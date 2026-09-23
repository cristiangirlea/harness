package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/report"
)

// printEvalDiff sets two evals side by side, variant by variant and then task
// by task. Each cell reads "before → after".
func printEvalDiff(r *renderer, d eval.Diff) {
	r.Line("")
	r.Line(fmt.Sprintf("  eval %s → %s", d.A, d.B))
	r.Line("")
	head := fmt.Sprintf("  %-20s %-16s %-18s %-14s", "variant", "pass", "median cost", "median time")
	if d.Judged {
		head += " score"
	}
	r.Line(strings.TrimRight(head, " "))
	for _, v := range d.Variants {
		switch {
		case v.A == nil:
			r.Line(fmt.Sprintf("  %-20s only in %s (%s pass, %s)", v.Name, d.B, report.Share(v.B.PassRate()), report.Money(v.B.MedianCostUSD, v.B.CostEstimate)))
		case v.B == nil:
			r.Line(fmt.Sprintf("  %-20s only in %s (%s pass, %s)", v.Name, d.A, report.Share(v.A.PassRate()), report.Money(v.A.MedianCostUSD, v.A.CostEstimate)))
		default:
			line := fmt.Sprintf("  %-20s %-16s %-18s %-14s", v.Name,
				arrow(report.Share(v.A.PassRate()), report.Share(v.B.PassRate())),
				arrow(report.Money(v.A.MedianCostUSD, v.A.CostEstimate), report.Money(v.B.MedianCostUSD, v.B.CostEstimate)),
				arrow(v.A.MedianDuration.Round(time.Second).String(), v.B.MedianDuration.Round(time.Second).String()))
			if d.Judged {
				line += " " + arrow(score(v.A.Judged, v.A.MeanScore), score(v.B.Judged, v.B.MeanScore))
			}
			r.Line(strings.TrimRight(line, " "))
		}
	}

	r.Line("")
	r.Line(fmt.Sprintf("  %-20s %-20s %s", "task", "variant", "passed"))
	for _, t := range d.Tasks {
		before, after := "-", "-"
		if t.A != nil {
			before = t.A.Score()
		}
		if t.B != nil {
			after = t.B.Score()
		}
		r.Line(fmt.Sprintf("  %-20s %-20s %s", t.Task, t.Variant, arrow(before, after)))
	}
	for _, id := range d.PromptChanged {
		r.Line(r.c(dim, fmt.Sprintf("  note: the prompt differs for task %s, so its rows compare different tasks", id)))
	}
}

func arrow(before, after string) string { return before + " → " + after }

// score renders a variant's mean judge score, or "-" when nothing was judged.
func score(judged int, mean float64) string {
	if judged == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", mean)
}

// listEvals prints the saved evals, newest first; n limits how many, 0 is all.
func listEvals(w io.Writer, n int) error {
	recs, err := eval.List(n)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Fprintln(w, "no evals yet")
		return nil
	}
	fmt.Fprintf(w, "%-6s %-16s %6s %6s %9s  %s\n", "id", "started", "cells", "pass", "cost", "batch")
	for _, rec := range recs {
		cells, passed := 0, 0
		cost, estimate := rec.JudgeCostUSD, false
		for _, v := range rec.Table.Variants {
			cells += v.Attempts
			passed += v.Passed
			cost += v.TotalCostUSD
			estimate = estimate || v.CostEstimate
		}
		fmt.Fprintf(w, "%-6s %-16s %6d %6s %9s  %s\n", rec.ID, rec.Started.Local().Format("2006-01-02 15:04"),
			cells, report.Percent(passed, cells), report.Money(cost, estimate), rec.BatchID)
	}
	return nil
}
