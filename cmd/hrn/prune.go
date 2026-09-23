package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const pruneUsage = "usage: hrn runs prune --older-than AGE [--keep N] [--dry-run]   (AGE like 30d or 36h)"

// cmdPrune removes old runs, batch records and eval records from HRN_HOME.
// Records reference each other, so it decides from the top down: an eval that
// stays keeps the batch it ran as, and a batch that stays keeps its runs. A
// batch whose worktrees are still on disk also stays, because hrn batch rm
// needs its record to remove them.
func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("runs prune", flag.ContinueOnError)
	olderThan := fs.String("older-than", "", "")
	keep := fs.Int("keep", 0, "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), pruneUsage)
	}
	if *olderThan == "" {
		return errors.New("hrn runs prune needs --older-than: it will not guess how much history to delete\n" + pruneUsage)
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		return err
	}
	if *keep < 0 {
		return fmt.Errorf("--keep %d is negative", *keep)
	}
	cutoff := time.Now().Add(-age)

	// Evals first: one that stays keeps its batch.
	evals, badEvals, err := eval.ListAll()
	if err != nil {
		return err
	}
	keptBatches := map[string]bool{}
	var oldEvals []string
	for _, rec := range evals {
		if rec.Started.Before(cutoff) {
			oldEvals = append(oldEvals, rec.ID)
		} else {
			keptBatches[rec.BatchID] = true
		}
	}

	// Then batches: one that stays keeps its runs.
	batches, badBatches, err := batch.List()
	if err != nil {
		return err
	}
	// A record that cannot be read may cite anything, so nothing is safe to go.
	if badEvals+badBatches > 0 {
		return fmt.Errorf("%d eval and %d batch records could not be read, and what they cite cannot be protected; "+
			"fix or remove them under %s, then prune again", badEvals, badBatches, runlog.Home())
	}
	keptRuns := map[string]bool{}
	var oldBatches, live []string
	for _, res := range batches {
		old := res.Started.Before(cutoff) && !keptBatches[res.ID]
		switch {
		case old && batch.WorktreesRemain(res):
			live = append(live, res.ID)
		case old:
			oldBatches = append(oldBatches, res.ID)
			continue
		}
		for _, t := range res.Tasks {
			for _, a := range t.Attempts {
				if a.RunID != "" {
					keptRuns[a.RunID] = true
				}
			}
		}
	}

	// Top down, records before what they cite: if a removal fails partway, what
	// is left can be unreferenced but never a record pointing at nothing.
	var bytes int64
	for _, id := range oldEvals {
		n, err := eval.RemoveRecord(id, *dryRun)
		if err != nil {
			return fmt.Errorf("eval record %s: %w", id, err)
		}
		bytes += n
	}
	for _, id := range oldBatches {
		n, err := batch.RemoveRecord(id, *dryRun)
		if err != nil {
			return fmt.Errorf("batch record %s: %w", id, err)
		}
		bytes += n
	}
	runs, err := runlog.Prune(cutoff, *keep, keptRuns, *dryRun)
	if err != nil {
		return err
	}
	bytes += runs.Bytes

	verb := "removed"
	if *dryRun {
		verb = "would remove"
		for _, id := range runs.Removed {
			fmt.Println("run   ", id)
		}
		for _, id := range oldBatches {
			fmt.Println("batch ", id)
		}
		for _, id := range oldEvals {
			fmt.Println("eval  ", id)
		}
	}
	fmt.Printf("%s %d runs, %d batch records and %d eval records older than %s (%s)\n",
		verb, len(runs.Removed), len(oldBatches), len(oldEvals), *olderThan, report.Bytes(bytes))
	for _, id := range live {
		fmt.Printf("kept batch %s: its worktrees are still on disk; hrn batch rm %s removes them\n", id, id)
	}
	return nil
}

// parseAge reads an age as a number of days ("30d") or a Go duration ("36h").
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("--older-than %q: want days like 30d or a duration like 36h", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("--older-than %q: want days like 30d or a duration like 36h", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("--older-than %q must be positive", s)
	}
	return d, nil
}
