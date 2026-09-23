package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func savedEval(t *testing.T, rec eval.Record) {
	t.Helper()
	writeJSONFile(t, filepath.Join(runlog.Home(), "evals", rec.ID+".json"), rec)
}

func diffPair() (eval.Record, eval.Record) {
	a := eval.Record{ID: "aaaaa", BatchID: "20260923-100000-aaaaaa", Started: time.Now().Add(-2 * time.Hour),
		Spec: eval.Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "greet", Prompt: "say hi"}}}},
		Table: eval.Table{
			Variants: []eval.VariantRow{
				{Name: "sonnet", Attempts: 2, Passed: 1, MedianCostUSD: 0.15, TotalCostUSD: 0.30, MedianDuration: 13 * time.Second, Judged: 1, MeanScore: 6},
				{Name: "haiku", Attempts: 2, Passed: 2, MedianCostUSD: 0.05, TotalCostUSD: 0.10},
			},
			Tasks: []eval.TaskRow{
				{Task: "greet", Variant: "sonnet", Passed: 1, Total: 2},
				{Task: "greet", Variant: "haiku", Passed: 2, Total: 2},
			},
		}}
	b := eval.Record{ID: "bbbbb", BatchID: "20260923-110000-bbbbbb", Started: time.Now().Add(-time.Hour),
		Spec: eval.Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "greet", Prompt: "say hello"}}}},
		Table: eval.Table{
			Variants: []eval.VariantRow{
				{Name: "sonnet", Attempts: 2, Passed: 2, MedianCostUSD: 0.12, TotalCostUSD: 0.24, MedianDuration: 11 * time.Second, Judged: 2, MeanScore: 8.5},
			},
			Tasks: []eval.TaskRow{{Task: "greet", Variant: "sonnet", Passed: 2, Total: 2}},
		}, JudgeCostUSD: 0.02}
	return a, b
}

func TestEvalDiffPrintsBothSidesAndWhatChanged(t *testing.T) {
	a, b := diffPair()
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	out := buf.String()
	for _, want := range []string{
		"aaaaa → bbbbb",
		"50% → 100%",     // sonnet's pass rate
		"$0.15 → $0.12",  // its median cost
		"13s → 11s",      // its median time
		"6.0 → 8.5",      // its judge score
		"only in aaaaa",  // haiku was dropped
		"1/2 → 2/2",      // greet under sonnet
		"greet",          // named in the prompt-changed note
		"prompt differs", // and the note says what differs
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}

func TestEvalListShowsTheNewestFirst(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, b := diffPair()
	// Eval ids are random: the older one sorting last by name must not matter.
	a.ID = "zzzzz"
	savedEval(t, a)
	savedEval(t, b)

	var buf bytes.Buffer
	if err := listEvals(&buf, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	ia, ib := strings.Index(out, "zzzzz"), strings.Index(out, "bbbbb")
	if ia < 0 || ib < 0 || ib > ia {
		t.Fatalf("want both, newest first:\n%s", out)
	}
	// b: 2 of 2 cells passed; cost is the variants plus the judge.
	for _, want := range []string{"100%", "$0.26", "75%", "$0.40"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := listEvals(&buf, 1); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "bbbbb") || strings.Contains(out, "zzzzz") {
		t.Errorf("-n 1 should be the newest eval:\n%s", out)
	}
}

func TestEvalDiffAndListRefuseBadArguments(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, args := range [][]string{{"diff"}, {"diff", "aaaaa"}, {"diff", "aaaaa", "../bbbbb"}, {"diff", "a", "b", "c"}, {"list", "extra"}} {
		code, err := cmdEval(args)
		if err == nil || code == 0 {
			t.Errorf("%v: code=%d err=%v, want a refusal", args, code, err)
		}
	}
}
