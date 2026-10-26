package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
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

func TestEvalDiffMarksAJudgeChangeWithinTheJudgesSpread(t *testing.T) {
	a, b := diffPair()
	a.Table.Variants[0].MeanScore, a.Table.Variants[0].JudgeRepeats, a.Table.Variants[0].ScoreMin, a.Table.Variants[0].ScoreMax = 8, 3, 7, 9
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	out := buf.String()
	if !strings.Contains(out, "8.0 (7.0-9.0) → 8.5 (within noise)") || !strings.Contains(out, "note: within noise:") {
		t.Errorf("a 0.5 move inside a 2-point spread should be marked:\n%s", out)
	}

	a.Table.Variants[0].ScoreMin, a.Table.Variants[0].ScoreMax = 7.9, 8.1
	buf.Reset()
	printEvalDiff(r, eval.Compare(a, b))
	if out := buf.String(); strings.Contains(out, "within noise") || !strings.Contains(out, "8.0 (7.9-8.1) → 8.5") {
		t.Errorf("a move larger than the spread is not noise:\n%s", out)
	}
}

func TestEvalTableShowsTheJudgeSpreadAndRepeatsPerCell(t *testing.T) {
	_, b := diffPair()
	b.Spec.Judge = &eval.Judge{Rubric: "r", Repeats: 3}
	b.Judgements = make([]eval.Judgement, 6)
	b.Table.Variants[0].JudgeRepeats, b.Table.Variants[0].ScoreMin, b.Table.Variants[0].ScoreMax = 3, 8, 9
	b.Table.Tasks[0].Judged, b.Table.Tasks[0].MeanScore = 2, 8.5
	b.Table.Tasks[0].JudgeRepeats, b.Table.Tasks[0].ScoreMin, b.Table.Tasks[0].ScoreMax = 3, 8, 9
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, b)
	out := buf.String()
	var head, row, taskHead, taskRow string
	for _, line := range strings.Split(out, "\n") {
		switch f := strings.Fields(line); {
		case len(f) > 0 && f[0] == "variant":
			head = line
		case len(f) > 0 && f[0] == "sonnet":
			row = line
		case len(f) > 0 && f[0] == "task":
			taskHead = line
		case len(f) > 0 && f[0] == "greet":
			taskRow = line
		}
	}
	if !strings.HasSuffix(head, "spread") || !strings.HasSuffix(row, "8.0-9.0") || len(head) != len(row) {
		t.Errorf("the spread column lines up at the end:\n%s\n%s", head, row)
	}
	if !strings.HasSuffix(taskRow, "2/2 8.5 (8.0-9.0)") || len(taskHead) != len(taskRow) {
		t.Errorf("the task column widens to its score:\n%s\n%s", taskHead, taskRow)
	}
	if !strings.Contains(out, "judge: 6 verdicts (3 per cell)") {
		t.Errorf("the judge note names the repeats:\n%s", out)
	}
}

func TestEvalDiffMarksATaskRowWithinNoise(t *testing.T) {
	a, b := diffPair()
	a.Table.Tasks[0].Judged, a.Table.Tasks[0].MeanScore = 1, 6
	a.Table.Tasks[0].JudgeRepeats, a.Table.Tasks[0].ScoreMin, a.Table.Tasks[0].ScoreMax = 2, 5, 7
	b.Table.Tasks[0].Judged, b.Table.Tasks[0].MeanScore = 2, 6.5
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	if out := buf.String(); !strings.Contains(out, "1/2 6.0 (5.0-7.0) → 2/2 6.5 (within noise)") {
		t.Errorf("the task row should be marked:\n%s", out)
	}
}

func TestEvalTableShowsADashForAVariantJudgedWithoutRepeats(t *testing.T) {
	a, _ := diffPair()
	a.Spec.Judge = &eval.Judge{Rubric: "r", Repeats: 2}
	a.Table.Variants[0].JudgeRepeats, a.Table.Variants[0].ScoreMin, a.Table.Variants[0].ScoreMax = 2, 5, 7
	a.Table.Variants[1].Judged, a.Table.Variants[1].MeanScore = 1, 9 // its verdicts never overlapped across repeats
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, a)
	for _, line := range strings.Split(buf.String(), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "haiku" && (f[len(f)-1] != "-" || f[len(f)-2] != "9.0") {
			t.Errorf("haiku row = %q, want its score and a dash for the spread", line)
		}
		if len(f) > 0 && f[0] == "sonnet" && f[len(f)-1] != "5.0-7.0" {
			t.Errorf("sonnet row = %q, want its spread", line)
		}
	}
}

func TestEvalDiffNotesASetupChangeAndMixedCells(t *testing.T) {
	a, b := diffPair()
	a.Table.Variants[0].Setup = &agent.Setup{BaseDigest: "1", Files: map[string]string{"AGENTS.md": "x"}}
	a.Table.Variants[0].SetupMixed = true
	b.Table.Variants[0].Setup = &agent.Setup{BaseDigest: "2", Files: map[string]string{"AGENTS.md": "y"}, Skills: []string{"deliver"}}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	out := buf.String()
	for _, want := range []string{
		"variant sonnet started with another agent setup in bbbbb, beyond its variant fields: files AGENTS.md; skills deliver",
		"variant sonnet's cells in aaaaa did not all start with the same agent setup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
}

func TestEvalDiffListsAtMostFiveFilesOfASetupChange(t *testing.T) {
	got := setupNames("files", []string{"a", "b", "c", "d", "e", "f", "g"})
	if got != "files a, b, c, d, e and 2 more" {
		t.Errorf("got %q", got)
	}
}

func TestTheEvalTableShowsPassingCellMediansWhenSomeCellsFailed(t *testing.T) {
	_, b := diffPair()
	v := &b.Table.Variants[0]
	v.Attempts, v.Passed = 4, 2
	v.MedianContextPassed, v.MedianCostPassedUSD, v.PassedCostKnown = 1000, 1, true
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, b)
	if out := buf.String(); !strings.Contains(out, "passing cells: median cost $1.00, context 1000") {
		t.Errorf("eval table should show the passing cells' medians:\n%s", out)
	}
	for name, change := range map[string]func(*eval.VariantRow){
		"an older record": func(v *eval.VariantRow) {
			v.MedianContextPassed, v.MedianCostPassedUSD, v.PassedCostKnown = 0, 0, false
		},
		"every cell passed": func(v *eval.VariantRow) { v.Passed = v.Attempts },
		"no cell passed":    func(v *eval.VariantRow) { v.Passed = 0 },
	} {
		row := *v
		change(&row)
		rec := b
		rec.Table.Variants = []eval.VariantRow{row}
		buf.Reset()
		printEval(r, rec)
		if strings.Contains(buf.String(), "passing cells") {
			t.Errorf("%s: the table should not show passing medians:\n%s", name, buf.String())
		}
	}
}

func TestTheEvalTablesSayWhichCellsNeverActedAndToRerunThem(t *testing.T) {
	a, b := diffPair()
	b.Table.Variants[0].DidNotAct = 1
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, b)
	if out := buf.String(); !strings.Contains(out, "1 did not act") || !strings.Contains(out, "1 cell did not act (no tool call, no change): re-run it") {
		t.Errorf("eval table should count and explain the cells that never acted:\n%s", out)
	}
	buf.Reset()
	printEvalDiff(r, eval.Compare(a, b))
	if out := buf.String(); !strings.Contains(out, "did not act") {
		t.Errorf("diff should say a side has cells that never acted:\n%s", out)
	}
}

func TestEvalTablesAndDiffCountCellsThatDidNotStart(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, b := diffPair()
	b.Table.Variants[0] = eval.VariantRow{Name: "sonnet", Attempts: 4, Passed: 1, DidNotAct: 1, DidNotStart: 1}
	b.Table.Tasks = []eval.TaskRow{{Task: "task", Variant: "sonnet", Total: 4, Passed: 1, DidNotAct: 1, DidNotStart: 1}}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, b)
	for _, want := range []string{"50%", "1 did not act", "1 did not start", "1/2 +1 did not act +1 did not start"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table missing %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	printEvalDiff(r, eval.Compare(a, b))
	if out := buf.String(); !strings.Contains(out, "1 cell of sonnet in "+b.ID+" did not start") || !strings.Contains(out, "1/2 +1 did not act +1 did not start") {
		t.Errorf("diff lost did-not-start counts:\n%s", out)
	}
	b.Table.Variants = b.Table.Variants[:1]
	savedEval(t, b)
	buf.Reset()
	if err := listEvals(&buf, 0); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "50%") {
		t.Errorf("list must exclude unstarted cells:\n%s", out)
	}
}

func TestEvalTablesAndDiffReportRerunsAndStartRate(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, b := diffPair()
	b.Table.Variants[0].Reruns = 2
	b.Table.Variants[0].RerunsExhausted = 1
	b.Table.Variants[0].BackendStartRate = .5
	b.Table.Tasks[0].Reruns = 2
	b.Table.Tasks[0].RerunsExhausted = 1
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	for _, print := range []func(){func() { printEval(r, b) }, func() { printEvalDiff(r, eval.Compare(a, b)) }} {
		buf.Reset()
		print()
		for _, want := range []string{"start rate", "50%", "2 reruns", "1 exhausted"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("missing %q:\n%s", want, buf.String())
			}
		}
	}
}

func TestTheEvalTableCountsCellsThatCommittedOnTheirOwn(t *testing.T) {
	_, b := diffPair()
	b.Table.Variants[0].AgentCommitted = 1
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, b)
	want := fmt.Sprintf("1 of %d cells committed on their own", b.Table.Variants[0].Attempts)
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("eval table should say %q:\n%s", want, out)
	}
}

func TestEvalListLeavesCellsThatNeverActedOutOfThePassShare(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	_, b := diffPair()
	b.Table.Variants = []eval.VariantRow{{Name: "haiku", Attempts: 4, Passed: 2, DidNotAct: 2}}
	savedEval(t, b)
	var buf bytes.Buffer
	if err := listEvals(&buf, 0); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "100%") {
		t.Errorf("list should count only the cells that acted:\n%s", out)
	}
}

func TestASetupComparisonPrintsBothSidesAndItsVerdict(t *testing.T) {
	per := 750.0
	c := eval.SetupComparison{Source: "origin/main", Changed: []string{"AGENTS.md"},
		Baseline:  eval.SetupSide{Eval: "aaaaa", Acted: 4, Passed: 4, TokensPerSuccess: &per},
		Candidate: eval.SetupSide{Eval: "bbbbb", Acted: 3, Passed: 3, DidNotAct: 1, FailedTokens: 0},
		Verdict:   eval.SetupSame, Reason: "the same pass rate"}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printSetupComparison(r, c)
	out := buf.String()
	buf.Reset()
	c.Candidate.UnknownTokens, c.Candidate.FailedTokens = 2, 300
	printSetupComparison(r, c)
	out += buf.String()
	for _, want := range []string{"origin/main's setup files → this branch's (AGENTS.md)", "eval aaaaa · 4/4 passed", "750 per success", "1 did not act", "verdict: same, the same pass rate", "never change the verdict", "at least 300 (2 cells unknown) tokens on failures"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// fakeSetup replaces the comparison with one that returns run and err, and
// records which batches the command removes.
func fakeSetup(t *testing.T, run eval.SetupRun, err error) (*[]string, string) {
	t.Helper()
	oldRun, oldRemove := runSetupComparison, removeBatch
	t.Cleanup(func() { runSetupComparison, removeBatch = oldRun, oldRemove })
	runSetupComparison = func(context.Context, eval.Spec, eval.SetupOptions, func(batch.Event)) (eval.SetupRun, error) {
		return run, err
	}
	var removed []string
	removeBatch = func(res batch.Result) error { removed = append(removed, res.ID); return nil }
	spec := filepath.Join(t.TempDir(), "eval.json")
	body := `{"matrix": [{"backend": "claude"}], "repeats": 2, "tasks": [{"id": "a", "prompt": "do a"}]}`
	if werr := os.WriteFile(spec, []byte(body), 0o644); werr != nil {
		t.Fatal(werr)
	}
	return &removed, spec
}

func TestEvalSetupRemovesBothBatchesEvenWhenTheCandidateFailed(t *testing.T) {
	run := eval.SetupRun{BaselineRes: batch.Result{ID: "base-batch"}, CandidateRes: batch.Result{ID: "cand-batch"}}
	removed, spec := fakeSetup(t, run, errors.New("candidate: boom"))
	var out, errOut bytes.Buffer
	if code, err := evalSetup([]string{spec}, &out, &errOut); code != 1 || err == nil {
		t.Fatalf("code = %d, err = %v; want the candidate's failure", code, err)
	}
	if strings.Join(*removed, ",") != "base-batch,cand-batch" {
		t.Errorf("removed = %q, want both batches", *removed)
	}
}

func TestEvalSetupKeepsTheWorktreesWithKeep(t *testing.T) {
	run := eval.SetupRun{BaselineRes: batch.Result{ID: "base-batch"}, CandidateRes: batch.Result{ID: "cand-batch"},
		Comparison: eval.SetupComparison{Verdict: eval.SetupSame}}
	removed, spec := fakeSetup(t, run, nil)
	var out, errOut bytes.Buffer
	if code, err := evalSetup([]string{"--keep", spec}, &out, &errOut); code != 0 || err != nil {
		t.Fatalf("code = %d, err = %v", code, err)
	}
	if len(*removed) != 0 {
		t.Errorf("removed = %q with --keep", *removed)
	}
}

func TestEvalSetupJSONPrintsOnlyTheComparison(t *testing.T) {
	run := eval.SetupRun{Comparison: eval.SetupComparison{Verdict: eval.SetupBetter, Reason: "fewer tokens"}}
	_, spec := fakeSetup(t, run, nil)
	var out, errOut bytes.Buffer
	if code, err := evalSetup([]string{"--json", spec}, &out, &errOut); code != 0 || err != nil {
		t.Fatalf("code = %d, err = %v", code, err)
	}
	var got eval.SetupComparison
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Verdict != eval.SetupBetter {
		t.Errorf("stdout = %q (%v); want only the comparison as JSON", out.String(), err)
	}
}
