package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cristiangirlea/harness/internal/gc"
	"github.com/cristiangirlea/harness/internal/report"
)

const gcUsage = "usage: hrn gc [--apply] [--grace 1h] [--idle 24h] [--min-idle 2h] [--free-below SIZE] [PATH ...]"

// cmdGC lists the worktrees under PATH (default: the current repository) and
// the build caches there and inside them, with what gc decided for each, and
// with --apply removes the ones whose work is proven merged or whose build
// output is idle. internal/gc holds the rules.
func cmdGC(args []string) (int, error) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "")
	grace := fs.Duration("grace", time.Hour, "")
	idle := fs.Duration("idle", 24*time.Hour, "")
	minIdle := fs.Duration("min-idle", 2*time.Hour, "")
	freeBelow := fs.String("free-below", "", "")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, gcUsage)
	}
	switch {
	case *grace < 0:
		return 2, fmt.Errorf("--grace %s is negative\n%s", *grace, gcUsage)
	case *idle < 0:
		return 2, fmt.Errorf("--idle %s is negative\n%s", *idle, gcUsage)
	case *minIdle < 0:
		return 2, fmt.Errorf("--min-idle %s is negative\n%s", *minIdle, gcUsage)
	case *minIdle > *idle:
		return 2, fmt.Errorf("--min-idle %s is above --idle %s, so --free-below could take nothing\n%s", *minIdle, *idle, gcUsage)
	}
	below, err := parseSize(*freeBelow)
	if err != nil {
		return 2, fmt.Errorf("--free-below: %w\n%s", err, gcUsage)
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opt := gc.Options{Grace: *grace, Idle: *idle, MinIdle: *minIdle, FreeBelow: below}
	wts, err := gc.Scan(ctx, paths, opt)
	if err != nil {
		return 1, err
	}
	caches, err := gc.Caches(ctx, paths, wts, opt)
	if err != nil {
		return 1, err
	}
	sizes := map[string]int64{}
	for _, w := range wts {
		if w.Verdict == gc.Remove {
			sizes[w.Path] = gc.Size(w.Path)
		}
	}
	printGC(os.Stdout, wts, sizes)
	printCaches(os.Stdout, caches)

	if !*apply {
		fmt.Println(gcSummary(wts, caches, sizes))
		return 0, nil
	}
	removed, errs := gc.Apply(ctx, wts)
	removedCaches, cacheErrs := gc.ApplyCaches(ctx, caches)
	errs = append(errs, cacheErrs...)
	fmt.Printf("removed %d worktrees and %d caches, freeing %s\n", len(removed), len(removedCaches), report.Bytes(gc.Total(removed, sizes)+gc.TotalCaches(removedCaches)))
	if len(errs) > 0 {
		return 1, errors.Join(errs...)
	}
	return 0, nil
}

// gcSummary is the dry run's last line: what would go and what it frees, or
// that nothing would.
func gcSummary(wts []gc.Worktree, caches []gc.Cache, sizes map[string]int64) string {
	var going []gc.Worktree
	for _, w := range wts {
		if w.Verdict == gc.Remove {
			going = append(going, w)
		}
	}
	var goingCaches []gc.Cache
	for _, c := range caches {
		if c.Verdict == gc.Remove {
			goingCaches = append(goingCaches, c)
		}
	}
	if len(going)+len(goingCaches) == 0 {
		return fmt.Sprintf("nothing to remove; %d worktrees and %d caches stay", len(wts), len(caches))
	}
	freed := gc.Total(going, sizes) + gc.TotalCaches(goingCaches)
	return fmt.Sprintf("would remove %d of %d worktrees and %d of %d caches, freeing %s; hrn gc --apply removes them", len(going), len(wts), len(goingCaches), len(caches), report.Bytes(freed))
}

// printGC writes one line per worktree, the ones to remove first: the
// verdict, the folder, its branch or batch, the size it frees, why, and the
// pull request, which is the record once the folder is gone.
func printGC(w io.Writer, wts []gc.Worktree, sizes map[string]int64) {
	sorted := append([]gc.Worktree(nil), wts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Verdict == gc.Remove && sorted[j].Verdict != gc.Remove
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, t := range sorted {
		what := t.Branch
		switch {
		case t.Batch != "":
			what = "batch " + t.Batch
		case what == "":
			what = "(detached)"
		}
		size := ""
		if n, ok := sizes[t.Path]; ok {
			size = report.Bytes(n)
		}
		url := ""
		if t.PR != nil {
			url = t.PR.URL
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Verdict, t.Path, what, size, t.Reason, url)
	}
	tw.Flush()
}

// printCaches writes one line per build cache, the ones to remove first: the
// verdict, the folder, "cache", its size, when it was last written (a build,
// for a Cargo target with a profile), and why.
func printCaches(w io.Writer, caches []gc.Cache) {
	sorted := append([]gc.Cache(nil), caches...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Verdict == gc.Remove && sorted[j].Verdict != gc.Remove
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range sorted {
		written := "-"
		if !c.LastBuild.IsZero() {
			written = "written " + c.LastBuild.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Verdict, c.Path, "cache", report.Bytes(c.Size), written, c.Reason)
	}
	tw.Flush()
}

// parseSize reads a size such as 50G, 500M, 2k, 1.5G or 123 (bytes), in units
// of 1024 as the listing prints them. Empty is 0.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	num := strings.TrimSuffix(strings.ToUpper(s), "B")
	mult := int64(1)
	if num != "" {
		switch num[len(num)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			num = num[:len(num)-1]
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, fmt.Errorf("%q is not a size like 50G or 500M", s)
	}
	bytes := n * float64(mult)
	if bytes >= math.MaxInt64 {
		return 0, fmt.Errorf("%q is more than a size can be", s)
	}
	return int64(bytes), nil
}
