package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/report"
)

// cmdReport prints one table over the finished runs and one over the batches.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	since := fs.String("since", "", "")
	by := fs.String("by", report.ByBackend, "")
	jsonOut := fs.Bool("json", false, "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid arguments")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	window, err := report.ParseSince(*since)
	if err != nil {
		return err
	}
	var from time.Time
	if window > 0 {
		from = time.Now().Add(-window)
	}
	data, err := report.Collect(from)
	if err != nil {
		return err
	}
	rep, err := data.Report(*by)
	if err != nil {
		return err
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	scope := "all time"
	if rep.Since != nil {
		scope = fmt.Sprintf("since %s (%s)", rep.Since.Format("2006-01-02 15:04"), *since)
	}
	if len(rep.Runs) == 0 {
		fmt.Printf("no finished runs, %s\n", scope)
	} else {
		fmt.Printf("runs by %s, %s\n", rep.By, scope)
		t := [][]string{{rep.By, "runs", "ok", "ok%", "med dur", "med turns", "total cost", "med cost", "cache"}}
		for _, r := range append(rep.Runs, rep.RunsAll) {
			t = append(t, []string{shortKey(r.Key, 48), strconv.Itoa(r.Runs), strconv.Itoa(r.OK), report.Percent(r.OK, r.Runs),
				r.MedianDuration.Round(time.Second).String(), report.Number(r.MedianTurns),
				report.Money(r.TotalCost, r.CostEstimated), report.Money(r.MedianCost, r.CostEstimated), report.Share(r.CacheShare)})
		}
		fmt.Print(table(t, 1))
	}
	if rep.Unfinished > 0 {
		fmt.Printf("%d unfinished run(s) not counted\n", rep.Unfinished)
	}
	if len(rep.Batches) > 0 {
		fmt.Printf("\nbatches by repository, %s\n", scope)
		t := [][]string{{"repository", "batches", "tasks", "pass%", "med attempts", "total cost"}}
		for _, r := range append(rep.Batches, rep.BatchesAll) {
			t = append(t, []string{shortKey(r.Key, 48), strconv.Itoa(r.Batches), strconv.Itoa(r.Tasks), report.Percent(r.Passed, r.Tasks),
				report.Number(r.MedianAttempts), report.Money(r.TotalCost, r.CostEstimated)})
		}
		fmt.Print(table(t, 1))
	}
	if rep.RunningBatches > 0 {
		fmt.Printf("%d batch(es) still running not counted\n", rep.RunningBatches)
	}
	if rep.Unreadable > 0 {
		fmt.Printf("%d unreadable record(s) skipped\n", rep.Unreadable)
	}
	return nil
}

// shortKey keeps the end of a long key: for a path, that is the part that
// tells two repositories apart.
func shortKey(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

// table renders rows as aligned columns. Columns before rightFrom are labels
// and align left; the rest are numbers and align right.
func table(rows [][]string, rightFrom int) string {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], len([]rune(c)))
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, c := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			pad := strings.Repeat(" ", widths[i]-len([]rune(c)))
			switch {
			case i >= rightFrom:
				b.WriteString(pad + c)
			case i < len(r)-1:
				b.WriteString(c + pad)
			default:
				b.WriteString(c)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
