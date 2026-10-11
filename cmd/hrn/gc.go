package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/cristiangirlea/harness/internal/gc"
	"github.com/cristiangirlea/harness/internal/report"
)

const gcUsage = "usage: hrn gc [--apply] [--grace 1h] [PATH ...]"

// cmdGC lists the worktrees under PATH (default: the current repository) with
// what gc decided for each, and with --apply removes the ones whose work is
// proven merged. internal/gc holds the rules.
func cmdGC(args []string) (int, error) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "")
	grace := fs.Duration("grace", time.Hour, "")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, gcUsage)
	}
	if *grace < 0 {
		return 2, fmt.Errorf("--grace %s is negative\n%s", *grace, gcUsage)
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	wts, err := gc.Scan(ctx, paths, gc.Options{Grace: *grace})
	if err != nil {
		return 1, err
	}
	sizes := map[string]int64{}
	var count int
	var total int64
	for _, w := range wts {
		if w.Verdict == gc.Remove {
			sizes[w.Path] = gc.Size(w.Path)
			count++
			total += sizes[w.Path]
		}
	}
	printGC(os.Stdout, wts, sizes)

	if !*apply {
		if count == 0 {
			fmt.Printf("nothing to remove; %d worktrees stay\n", len(wts))
		} else {
			fmt.Printf("would remove %d of %d worktrees, freeing %s; hrn gc --apply removes them\n", count, len(wts), report.Bytes(total))
		}
		return 0, nil
	}
	removed, errs := gc.Apply(ctx, wts)
	var freed int64
	for _, w := range removed {
		freed += sizes[w.Path]
	}
	fmt.Printf("removed %d worktrees, freeing %s\n", len(removed), report.Bytes(freed))
	if len(errs) > 0 {
		return 1, errors.Join(errs...)
	}
	return 0, nil
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
