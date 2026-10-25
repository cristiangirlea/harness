package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

// cellsOf is a batch result with one cell per task and repeat; cell gives its
// tokens, whether it passed and whether it acted.
func cellsOf(eval string, tasks, repeats int, cell func(task, repeat int) (int64, bool, bool)) batch.Result {
	res := batch.Result{ID: "b-" + eval}
	for t := 0; t < tasks; t++ {
		for r := 1; r <= repeats; r++ {
			tokens, passed, acted := cell(t, r)
			tr := batch.TaskResult{ID: fmt.Sprintf("%s.task%d.claude.r%d", eval, t, r), Status: batch.StatusFail}
			if passed {
				tr.Status = batch.StatusPass
			}
			result := agent.Result{Backend: "claude", Status: agent.StatusOK, Turns: 1, InputTokens: tokens,
				Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
			if acted {
				result.Turns, result.ToolCalls, tr.Commit = 3, 2, "abc"
			}
			tr.Attempts = []batch.Attempt{{Backend: "claude", Result: result}}
			res.Tasks = append(res.Tasks, tr)
		}
	}
	return res
}

func compareCells(t *testing.T, base, cand func(task, repeat int) (int64, bool, bool)) SetupComparison {
	t.Helper()
	var spec Spec
	for i := 0; i < 4; i++ {
		spec.Tasks = append(spec.Tasks, batch.Task{ID: fmt.Sprintf("task%d", i), Gate: []string{"true"}})
	}
	spec.Matrix, spec.Repeats = []Variant{{Backend: "claude"}}, 3
	return CompareSetups(spec, cellsOf("aaaaa", 4, 3, base), cellsOf("bbbbb", 4, 3, cand))
}

func steady(n int64) func(int, int) (int64, bool, bool) {
	return func(int, int) (int64, bool, bool) { return n, true, true }
}

func TestASetupThatSavesMoreThanTheBaselinesNoiseIsBetter(t *testing.T) {
	c := compareCells(t, steady(1000), steady(800))
	if c.Verdict != SetupBetter || c.PassingPairs != 12 || c.PassingReduction == nil || *c.PassingReduction < 0.199 {
		t.Errorf("verdict = %s (%s), pairs %d", c.Verdict, c.Reason, c.PassingPairs)
	}
}

func TestASavingInsideTheBaselinesOwnMovementIsTheSame(t *testing.T) {
	moving := func(_ int, r int) (int64, bool, bool) { return []int64{0, 1000, 1400, 1000}[r], true, true }
	c := compareCells(t, moving, func(task, r int) (int64, bool, bool) {
		n, p, a := moving(task, r)
		return n * 85 / 100, p, a
	})
	if c.Verdict != SetupSame || c.TokenMovement == nil || *c.TokenMovement < 0.3 {
		t.Errorf("verdict = %s (%s), movement %v; a 15%% saving inside a 35%% movement is noise", c.Verdict, c.Reason, c.TokenMovement)
	}
}

func TestASetupThatPassesLessIsWorseWhateverItSaves(t *testing.T) {
	c := compareCells(t, steady(1000), func(task, r int) (int64, bool, bool) { return 100, task != 0, true })
	if c.Verdict != SetupWorse || !strings.Contains(c.Reason, "pass rate") {
		t.Errorf("verdict = %s (%s)", c.Verdict, c.Reason)
	}
}

func TestCellsThatNeverActedAreLeftOutOfTheComparison(t *testing.T) {
	c := compareCells(t, steady(1000), func(task, r int) (int64, bool, bool) {
		if task == 0 {
			return 34000, false, false // never acted: the setup wiring, not the change
		}
		return 1000, true, true
	})
	if c.Verdict != SetupSame || c.Candidate.DidNotAct != 3 || c.Candidate.Acted != 9 || c.Candidate.PassRate != 1 {
		t.Errorf("verdict = %s (%s), candidate %+v", c.Verdict, c.Reason, c.Candidate)
	}
}

func TestAComparisonWithTooFewPassingRepeatsIsInconclusive(t *testing.T) {
	only := func(_ int, r int) (int64, bool, bool) { return 1000, r == 1, true }
	c := compareCells(t, only, func(task, r int) (int64, bool, bool) { n, p, a := only(task, r); return n / 2, p, a })
	if c.Verdict != SetupInconclusive {
		t.Errorf("verdict = %s (%s); one passing repeat measures no noise", c.Verdict, c.Reason)
	}
}

func TestASetupComparisonReportsSpendOnFailuresAndPerSuccess(t *testing.T) {
	c := compareCells(t, steady(1000), func(task, r int) (int64, bool, bool) { return 500, task != 3, true })
	if c.Candidate.FailedTokens != 1500 || c.Candidate.TokensPerSuccess == nil || *c.Candidate.TokensPerSuccess != 6000.0/9 {
		t.Errorf("candidate = %+v; want 1500 tokens on failures and 6000/9 per success", c.Candidate)
	}
}

func TestAHeldOutSetIsTheSameForTheSameSeedAndCoversEveryPurpose(t *testing.T) {
	var tasks []batch.Task
	for i := 0; i < 6; i++ {
		tasks = append(tasks, batch.Task{ID: fmt.Sprintf("fix%d", i), Prompt: "Fix the bug in parse.go"}, batch.Task{ID: fmt.Sprintf("rev%d", i), Prompt: "Review the patch for races"})
	}
	a, b := HeldOut(tasks, 7, 0.5), HeldOut(tasks, 7, 0.5)
	if strings.Join(a, ",") != strings.Join(b, ",") || len(a) != 6 {
		t.Fatalf("held out %q then %q; want the same six", a, b)
	}
	fix, rev := 0, 0
	for _, id := range a {
		if strings.HasPrefix(id, "fix") {
			fix++
		} else {
			rev++
		}
	}
	if fix != 3 || rev != 3 {
		t.Errorf("held out %d fix and %d review tasks, want three of each", fix, rev)
	}
	if c := HeldOut(tasks, 8, 0.5); strings.Join(c, ",") == strings.Join(a, ",") {
		t.Errorf("another seed held out the same tasks: %q", c)
	}
}

// ev-setup does the work, and spends half as much when its AGENTS.md says "fast".
type setupStub struct{ stub }

func (s setupStub) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "Write", Text: "out.txt"})
	_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt), 0o644)
	tokens := int64(1000)
	if rules, _ := os.ReadFile(filepath.Join(task.Dir, "AGENTS.md")); strings.Contains(string(rules), "fast") {
		tokens = 500
	}
	return agent.Result{Backend: s.name, Status: agent.StatusOK, Final: "done", Turns: 2, InputTokens: tokens,
		Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
}

func init() {
	agent.Register(setupStub{stub{"ev-setup", false}})
	setupBackends = append(setupBackends, "ev-setup")
}

func TestASetupComparisonRunsTheSameCodeWithEachSidesSetupFiles(t *testing.T) {
	dir := initRepo(t)
	commit := func(rules string) string {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(rules), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "AGENTS.md"}, {"commit", "-q", "-m", rules}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}
	base := commit("be thorough\n")
	commit("be fast\n")
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-setup"}}
	run, err := RunSetupComparison(context.Background(), spec, SetupOptions{Source: base}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := run.Comparison
	if c.Verdict != SetupBetter || c.PassingBaselineMedian == nil || *c.PassingBaselineMedian != 1000 || *c.PassingCandidateMedian != 500 {
		t.Fatalf("comparison = %+v; want better, 1000 → 500 tokens", c)
	}
	if !slices.Equal(c.Changed, []string{"AGENTS.md"}) || c.Baseline.Eval != run.Baseline.ID || c.Candidate.Eval != run.Candidate.ID {
		t.Errorf("comparison = %+v", c)
	}
	loaded, err := LoadRecord(run.Candidate.ID)
	if err != nil || loaded.SetupComparison == nil || loaded.SetupComparison.Verdict != SetupBetter {
		t.Errorf("the candidate's saved record lacks the comparison: %+v, %v", loaded.SetupComparison, err)
	}
}

func TestASetupComparisonRefusesWhatItCannotMeasureCheaply(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*Spec)
		want   string
	}{
		"two variants":    {func(s *Spec) { s.Matrix = append(s.Matrix, Variant{Name: "other", Backend: "ev-setup"}) }, "one variant"},
		"metered":         {func(s *Spec) { s.Matrix = []Variant{{Backend: "api"}} }, "plan backend"},
		"one repeat":      {func(s *Spec) { s.Repeats = 1 }, "at least 2"},
		"unnamed backend": {func(s *Spec) { s.Matrix = []Variant{{Name: "x"}} }, "plan backend"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec("somewhere")
			spec.Matrix = []Variant{{Backend: "ev-setup"}}
			c.change(&spec)
			if _, err := RunSetupComparison(context.Background(), spec, SetupOptions{Source: "main"}, nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestASetupThatMakesTheAgentGiveUpOnAHardTaskIsNotBetter(t *testing.T) {
	base := func(task, r int) (int64, bool, bool) { return 1000, task != 0, true } // tries task0 and fails
	cand := func(task, r int) (int64, bool, bool) {
		if task == 0 {
			return 30000, false, false // gives up on it without acting
		}
		return 1000, true, true
	}
	c := compareCells(t, base, cand)
	if c.Verdict == SetupBetter || c.Baseline.Acted != 9 || c.Candidate.Acted != 9 || c.Candidate.DidNotAct != 3 || c.Baseline.DidNotAct != 0 {
		t.Errorf("verdict = %s (%s), baseline %+v, candidate %+v; the pair must drop out of both sides", c.Verdict, c.Reason, c.Baseline, c.Candidate)
	}
}

func TestASetupThatCostsMoreThanTheThresholdIsWorse(t *testing.T) {
	c := compareCells(t, steady(1000), steady(1300))
	if c.Verdict != SetupWorse || !strings.Contains(c.Reason, "more median tokens") {
		t.Errorf("verdict = %s (%s)", c.Verdict, c.Reason)
	}
}

func TestACellOnOneSideOnlyIsLeftOut(t *testing.T) {
	var spec Spec
	spec.Tasks = []batch.Task{{ID: "task0", Gate: []string{"true"}}, {ID: "task1", Gate: []string{"true"}}}
	spec.Matrix, spec.Repeats = []Variant{{Backend: "claude"}}, 3
	base := cellsOf("aaaaa", 2, 3, func(int, int) (int64, bool, bool) { return 1000, true, true })
	cand := cellsOf("bbbbb", 2, 3, func(int, int) (int64, bool, bool) { return 800, true, true })
	cand.Tasks = cand.Tasks[:5]
	c := CompareSetups(spec, base, cand)
	if c.Unpaired != 1 || c.Baseline.Cells != 5 || !strings.Contains(c.Reason, "one side only") {
		t.Errorf("unpaired = %d, baseline cells %d, reason %q", c.Unpaired, c.Baseline.Cells, c.Reason)
	}
}

func TestEstimatedTokensStayOutOfThePairs(t *testing.T) {
	var spec Spec
	spec.Tasks = []batch.Task{{ID: "task0", Gate: []string{"true"}}}
	spec.Matrix, spec.Repeats = []Variant{{Backend: "claude"}}, 3
	base := cellsOf("aaaaa", 1, 3, func(int, int) (int64, bool, bool) { return 1000, true, true })
	cand := cellsOf("bbbbb", 1, 3, func(int, int) (int64, bool, bool) { return 100, true, true })
	for i := range cand.Tasks {
		cand.Tasks[i].Attempts[0].Result.Usage.Status = "estimated"
	}
	c := CompareSetups(spec, base, cand)
	if c.PassingPairs != 0 || c.Verdict != SetupInconclusive || c.Candidate.UnknownTokens != 3 {
		t.Errorf("pairs = %d, verdict %s (%s), candidate %+v", c.PassingPairs, c.Verdict, c.Reason, c.Candidate)
	}
}

func TestATokenVerdictNeedsAtLeastTwoPassingPairs(t *testing.T) {
	var spec Spec
	spec.Tasks = []batch.Task{{ID: "task0", Gate: []string{"true"}}, {ID: "task1", Gate: []string{"true"}}}
	spec.Matrix, spec.Repeats = []Variant{{Backend: "claude"}}, 2
	// Half pass on each side in every repeat, but only task0 in repeat 1 passes on both.
	base := cellsOf("aaaaa", 2, 2, func(task, r int) (int64, bool, bool) { return 1000, (task == 0) == (r == 1), true })
	cand := cellsOf("bbbbb", 2, 2, func(task, r int) (int64, bool, bool) { return 500, r == 1, true })
	c := CompareSetups(spec, base, cand)
	if c.Verdict != SetupInconclusive || c.PassingPairs != 1 || !strings.Contains(c.Reason, "pairs passed on both sides") {
		t.Errorf("verdict = %s (%s), pairs %d", c.Verdict, c.Reason, c.PassingPairs)
	}
}

func TestRunSetupComparisonRefusesMeteredTasksAndJudgesAndBadSplits(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*Spec, *SetupOptions)
		want   string
	}{
		"a task on api": {func(s *Spec, _ *SetupOptions) {
			s.Matrix[0].Backend = ""
			s.Backend = "ev-setup"
			s.Tasks[1].Backend = "api"
		}, "task beta"},
		"a judge on default": {func(s *Spec, _ *SetupOptions) { s.Judge = &Judge{Rubric: "r"} }, "judge backend"},
		"a judge on api":     {func(s *Spec, _ *SetupOptions) { s.Judge = &Judge{Backend: "api", Rubric: "r"} }, "judge backend"},
		"a share above one":  {func(_ *Spec, o *SetupOptions) { o.HeldOut = 1.5 }, "between 0 and 1"},
		"no source":          {func(_ *Spec, o *SetupOptions) { o.Source = "" }, "no setup source"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec("somewhere")
			spec.Matrix = []Variant{{Backend: "ev-setup"}}
			opts := SetupOptions{Source: "main"}
			c.change(&spec, &opts)
			if _, err := RunSetupComparison(context.Background(), spec, opts, nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestASetupComparisonOnAHeldOutShareRecordsHowToRebuildIt(t *testing.T) {
	dir := initRepo(t)
	write := func(rules string) string {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(rules), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "AGENTS.md"}, {"commit", "-q", "-m", rules}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}
	base := write("be thorough\n")
	write("be fast\n")
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-setup"}}
	run, err := RunSetupComparison(context.Background(), spec, SetupOptions{Source: base, HeldOut: 0.5, Seed: 1 << 63}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := run.Comparison
	if c.HeldOut != 0.5 || c.Seed == nil || *c.Seed != 1<<63 || len(c.Tasks) != 1 || len(run.Candidate.Spec.Tasks) != 1 || c.SourceCommit != base {
		t.Errorf("comparison = %+v; want the share, the seed, one task and the source commit", c)
	}
	if _, err := RunSetupComparison(context.Background(), spec, SetupOptions{Source: base, HeldOut: 0.1}, nil); err == nil || !strings.Contains(err.Error(), "leaves no task") {
		t.Errorf("err = %v, want a refusal for a share that leaves no task", err)
	}
}

func TestARerunPairsWithItsFirstRoundCellInASetupComparison(t *testing.T) {
	var spec Spec
	for i := 0; i < 4; i++ {
		spec.Tasks = append(spec.Tasks, batch.Task{ID: fmt.Sprintf("task%d", i), Gate: []string{"true"}})
	}
	spec.Matrix, spec.Repeats = []Variant{{Backend: "claude"}}, 3
	base, cand := cellsOf("aaaaa", 4, 3, steady(1000)), cellsOf("bbbbb", 4, 3, steady(1000))
	// One candidate cell failed to launch, and its rerun ran and passed.
	unstarted := &cand.Tasks[0]
	rerun := *unstarted
	rerun.ID += ".u1"
	unstarted.Status = batch.StatusFail
	unstarted.Attempts = []batch.Attempt{{Backend: "claude", DidNotStart: "launch refused",
		Result: agent.Result{Backend: "claude", Status: agent.StatusError, Error: "launch refused"}}}
	cand.Tasks = append(cand.Tasks, rerun)
	c := CompareSetups(spec, base, cand)
	if c.Candidate.Cells != 12 || c.Candidate.Acted != 12 || c.Unpaired != 0 {
		t.Fatalf("candidate %+v, unpaired %v: the rerun must stand in for its cell, not add one", c.Candidate, c.Unpaired)
	}
}
