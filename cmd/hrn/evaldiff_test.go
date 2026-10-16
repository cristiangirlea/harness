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

func TestEvalListShowsAKnownZeroTotalAndCountsCellsWithoutACost(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	_, b := diffPair()
	b.JudgeCostUSD = 0
	v := &b.Table.Variants[0]
	v.MedianCostUSD, v.TotalCostUSD, v.CostCounted, v.CellsWithoutCost = 0, 0, true, 1
	savedEval(t, b)

	var buf bytes.Buffer
	if err := listEvals(&buf, 0); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "$0.00") || !strings.Contains(out, "(1 of 2 cells without a cost)") {
		t.Errorf("list should show a known zero total and flag the cell without a cost:\n%s", out)
	}
}

func TestEvalDiffAndListRefuseBadArguments(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, _ := diffPair()
	savedEval(t, a) // so a bad second id is what fails, not a missing first one
	for _, args := range [][]string{{"diff"}, {"diff", "aaaaa"}, {"diff", "aaaaa", "../bbbbb"}, {"diff", "aaaaa", `..\bbbbb`}, {"diff", "a", "b", "c"}, {"list", "extra"}} {
		code, err := cmdEval(args)
		if err == nil || code == 0 {
			t.Errorf("%v: code=%d err=%v, want a refusal", args, code, err)
		}
	}
}

func TestEvalDiffColumnsLineUp(t *testing.T) {
	a, b := diffPair()
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	var head, row string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "variant ") {
			head = line
		}
		if strings.HasPrefix(strings.TrimSpace(line), "sonnet ") {
			row = line
		}
	}
	col := func(s, sub string) int { return len([]rune(s[:strings.Index(s, sub)])) }
	for _, pair := range [][2]string{{"median cost", "$0.15"}, {"median time", "13s"}, {"score", "6.0"}} {
		if !strings.Contains(head, pair[0]) || !strings.Contains(row, pair[1]) {
			t.Fatalf("missing %v:\n%s\n%s", pair, head, row)
		}
		if h, r := col(head, pair[0]), col(row, pair[1]); h != r {
			t.Errorf("%q starts at column %d but its value at %d:\n%s\n%s", pair[0], h, r, head, row)
		}
	}
}

func TestEvalListSaysWhatItCouldNotRead(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, _ := diffPair()
	savedEval(t, a)
	broken := filepath.Join(runlog.Home(), "evals", "ccccc.json")
	writeJSONFile(t, broken, "not a record")

	var buf bytes.Buffer
	if err := listEvals(&buf, 0); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "aaaaa") || !strings.Contains(out, "1 eval record could not be read") {
		t.Errorf("the list should name what it skipped:\n%s", out)
	}
}

func TestEvalTablesShowAKnownZeroCostAndDashOnlyWhenNoCostIsKnown(t *testing.T) {
	a, b := diffPair()
	a.Table.Variants[0].MedianCostUSD, a.Table.Variants[0].CellsWithoutCost = 0, 2
	b.Table.Variants[0].MedianCostUSD, b.Table.Variants[0].CellsWithoutCost = 0, 1
	a.Table.Variants[0].CostCounted, b.Table.Variants[0].CostCounted = true, true
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	if out := buf.String(); !strings.Contains(out, "- → $0.00") {
		t.Errorf("diff should show no known cost as - and a known zero as $0.00:\n%s", out)
	}
	buf.Reset()
	printEval(r, b)
	if out := buf.String(); !strings.Contains(out, "$0.00") || !strings.Contains(out, "1 of 2 cells without a cost") {
		t.Errorf("eval table should show a known zero median and count the cells without a cost:\n%s", out)
	}
}
