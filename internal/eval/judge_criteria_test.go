package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

type criteriaJudgeStub struct{ final string }

func (criteriaJudgeStub) Name() string            { return "criteria-judge" }
func (criteriaJudgeStub) Detect() (string, error) { return "stub", nil }
func (b criteriaJudgeStub) Run(context.Context, agent.Task, func(agent.Event)) agent.Result {
	return agent.Result{Status: agent.StatusOK, Final: b.final}
}

func TestJudgeComputesScoresFromEvidenceForEveryCriterion(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	j := &Judge{Criteria: []Criterion{{ID: "a", Check: "a"}, {ID: "b", Check: "b"}, {ID: "c", Check: "c"}}}
	for _, tc := range []struct {
		name, final string
		score       int
		downgraded  bool
		bad         bool
	}{
		{"parsed", `prose
{"criteria":[{"id":"c","met":false,"evidence":""},{"id":"a","met":true,"evidence":"a.go:1"},{"id":"b","met":true,"evidence":"+fixed"}],"reason":"done"}`, 7, false, false},
		{"without evidence", `{"criteria":[{"id":"a","met":true,"evidence":" \t"},{"id":"b","met":false},{"id":"c","met":false}],"reason":"done"}`, 0, true, false},
		{"missing id", `{"criteria":[{"id":"a","met":false},{"id":"b","met":false}]}`, 0, false, true},
		{"duplicate id", `{"criteria":[{"id":"a","met":false},{"id":"a","met":false},{"id":"c","met":false}]}`, 0, false, true},
		{"unknown id", `{"criteria":[{"id":"a","met":false},{"id":"b","met":false},{"id":"z","met":false}]}`, 0, false, true},
		{"missing met", `{"criteria":[{"id":"a"},{"id":"b","met":false},{"id":"c","met":false}]}`, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeCell(context.Background(), criteriaJudgeStub{tc.final}, j, batch.TaskResult{ID: "cell"}, evidence{dir: t.TempDir(), diff: "+++ b/a.go\n@@ -1 +1 @@\n-old\n+fixed\n"})
			if tc.bad {
				if got.Scored() {
					t.Fatalf("invalid verdict scored: %+v", got)
				}
				return
			}
			if !got.Scored() || got.Score != tc.score || got.Reason != "done" || len(got.Criteria) != 3 {
				t.Fatalf("verdict = %+v", got)
			}
			if got.Criteria[0].ID != "a" || got.Criteria[0].Downgraded != tc.downgraded || (tc.downgraded && got.Criteria[0].Met) {
				t.Fatalf("criteria = %+v", got.Criteria)
			}
		})
	}
}

func TestJudgeRejectsInvalidCriteriaBeforeRunning(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, criteria := range [][]Criterion{
		{{ID: "", Check: "x"}}, {{ID: "bad.id", Check: "x"}}, {{ID: "a", Check: " \n"}}, {{ID: "a", Check: "x"}, {ID: "a", Check: "y"}},
		func() []Criterion {
			var out []Criterion
			for i := 0; i < 31; i++ {
				out = append(out, Criterion{ID: fmt.Sprint(i), Check: "x"})
			}
			return out
		}(),
	} {
		j := Judge{Backend: "ev-judge", Rubric: "x", Criteria: criteria}
		if err := j.check([]batch.Task{{ID: "a"}}); err == nil {
			t.Fatalf("accepted %+v", criteria)
		}
	}
	j := Judge{Backend: "ev-judge", Rubric: "x", Criteria: []Criterion{{ID: "A-z_09", Check: "x"}}}
	if err := j.check([]batch.Task{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeKeepsTheLegacyPromptAndVerdictWithoutCriteria(t *testing.T) {
	const originalInstructions = `You are grading one attempt by a coding agent. Score how well the change does
what the task asked, measured against the rubric. Judge the change itself: an
explanation that claims more than the diff shows earns nothing. An empty diff
is right only when the task needed no change.

Scores: 10 does exactly what was asked and nothing else; 7 does it with minor
flaws; 4 partly does it or adds unrequested changes; 0 does the wrong thing or
nothing.

Reply with one line of JSON as the last thing you write, and nothing after it:
{"score": <integer 0-10>, "reason": "<one sentence>"}
`
	want := originalInstructions + "\n## Task\n```\ntask\n```\n\n## Rubric\n```\nrubric\n```\n\n## Agent's final answer\n```\nfinal\n```\n\n## Diff\n```\n+diff\n```\n"
	if got := judgePrompt("task", "rubric", "final", "+diff", nil, nil); got != want {
		t.Fatalf("legacy prompt changed: %q", got)
	}
	t.Setenv("HRN_HOME", t.TempDir())
	got := judgeCell(context.Background(), criteriaJudgeStub{`{"score":8,"reason":"same"}`}, &Judge{}, batch.TaskResult{}, evidence{dir: t.TempDir()})
	if !got.Scored() || got.Score != 8 || got.Reason != "same" || len(got.Criteria) != 0 {
		t.Fatalf("legacy verdict = %+v", got)
	}
}

func TestJudgeRefusesExclusionsWithoutCriteria(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	j := Judge{Backend: "ev-judge", Rubric: "x", NotFlag: []string{"warnings"}}
	if err := j.check([]batch.Task{{ID: "a"}}); err == nil || err.Error() != "judge: not_flag needs criteria" {
		t.Fatalf("validation = %v", err)
	}
}

func TestCriteriaRequireReferencesAndStoreClippedEvidence(t *testing.T) {
	for _, tc := range []struct {
		evidence string
		met      bool
	}{
		{"yes", false}, {"n/a", false}, {"see diff", false}, {" \t", false},
		{"12:30", false}, {"fixed at 12:30", false}, {"http://x:80", false}, {"ratio 3:1", false},
		{" yes", false}, {"-", false}, {"- n/a", false}, {"other.go:12", false},
		{"internal/x.go:12", true}, {"+ return nil", true}, {"- return err", true}, {" return nil", true},
		{"new/file.go:9", true}, {"./internal/x.go:12", true}, {"b/internal/x.go:12", true}, {`internal\x.go:12`, true},
		{"[internal/x.go:12](source)", true}, {"{internal/x.go:12}", true}, {"http://internal/x.go:80", false}, {"notinternal/x.go:12", false},
		{"deleted.go:1", true}, {"space file.go:2", true}, {"from.go:1", true}, {"to.go:1", true},
		{"+ return nil  \t", true}, {"+++ b/internal/x.go", false}, {"+ missing", false},
		{"+ab", false}, {"- n/a  ", false}, {"+++ b/pretend.go:1", false},
		{"see internal/x.go:12 for details", true}, {"+" + strings.Repeat("é", 500), true},
	} {
		t.Run(clip(tc.evidence, 20), func(t *testing.T) {
			text, err := json.Marshal(map[string]any{"criteria": []map[string]any{{"id": "a", "met": true, "evidence": tc.evidence}}})
			if err != nil {
				t.Fatal(err)
			}
			diff := "diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -1,2 +1,3 @@\n- return err\n+ return nil\n return nil\n+ab\n+" + strings.Repeat("é", 500) + "\n" +
				"diff --git a/deleted.go b/deleted.go\n--- a/deleted.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-deleted\n" +
				"diff --git \"a/space file.go\" \"b/space file.go\"\n" +
				"diff --git a/from.go b/to.go\nsimilarity index 100%\nrename from from.go\nrename to to.go\n"
			score, _, got, err := parseCriteriaVerdict(string(text), []Criterion{{ID: "a", Check: "x"}}, evidence{diff: diff, untracked: []string{"new/file.go"}})
			if err != nil || len(got) != 1 {
				t.Fatalf("verdict = %+v, %v", got, err)
			}
			if got[0].Met != tc.met || got[0].Downgraded == tc.met || score != map[bool]int{true: 10, false: 0}[tc.met] {
				t.Fatalf("score = %d, verdict = %+v", score, got[0])
			}
			if got[0].Evidence != clip(tc.evidence, 500) || !utf8.ValidString(got[0].Evidence) {
				t.Fatalf("stored evidence = %q", got[0].Evidence)
			}
		})
	}
}

func TestCriteriaParserUsesTheLastVerdictAndRejectsTruncatedReplacements(t *testing.T) {
	valid := `{"criteria":[{"id":"a","met":true,"evidence":"+ return {x}"}],"reason":"first"}`
	last := `{"criteria":[{"id":"a","met":false,"evidence":"- removed {y}"}],"reason":"last"}`
	for _, tc := range []struct {
		name, text, reason string
		bad                bool
	}{
		{"braces in evidence", valid, "first", false},
		{"broken fragment before final verdict", `{"criteria":[` + "\n" + valid, "first", false},
		{"last complete object", valid + "\n" + last, "last", false},
		{"truncated replacement", valid + `\n{"criteria":[{"id":"a","met":`, "", true},
		{"truncated replacement after reason", valid + `\n{"reason":"unfinished","criteria":[`, "", true},
		{"invalid last complete object", valid + `\n{"criteria":[]}`, "", true},
		{"null criteria", `{"criteria":null}`, "", true},
		{"empty criteria", `{"criteria":[]}`, "", true},
		{"null replacement", valid + `\n{"criteria":null}`, "", true},
		{"trailing prose", valid + "\nSome trailing prose with {braces}.", "first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, _, err := parseCriteriaVerdict(tc.text, []Criterion{{ID: "a", Check: "x"}}, evidence{diff: "@@ -1 +1 @@\n+ return {x}\n"})
			if (err != nil) != tc.bad || (!tc.bad && reason != tc.reason) {
				t.Fatalf("reason = %q, error = %v", reason, err)
			}
		})
	}
}

func TestJudgeAllIncludesCriteriaInTheFakeJudgesPrompt(t *testing.T) {
	dir := initRepo(t)
	spec := testSpec(dir)
	spec.Judge = &Judge{Backend: "ev-judge", Rubric: "x", Criteria: []Criterion{{ID: "a", Check: "x"}}}
	judged.mu.Lock()
	*judged.tasks = nil
	judged.mu.Unlock()
	rec, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	for _, verdict := range rec.Judgements {
		if !verdict.Scored() || verdict.Score != 10 {
			t.Fatalf("changed-file evidence was lost: %+v", verdict)
		}
	}
	judged.mu.Lock()
	defer judged.mu.Unlock()
	if len(*judged.tasks) == 0 {
		t.Fatal("fake judge did not run")
	}
	for _, task := range *judged.tasks {
		if !strings.Contains(task.Prompt, "## Criteria\n") || !strings.Contains(task.Prompt, "- a: x") {
			t.Fatalf("missing criteria: %s", task.Prompt)
		}
	}
}

func TestJudgeListsCriteriaAndExclusionsInItsPrompt(t *testing.T) {
	j := &Judge{Criteria: []Criterion{{ID: "safe", Check: "preserves inputs"}}, NotFlag: []string{"existing warnings"}}
	got := judgePrompt("task", "rubric", "final", "+diff", nil, j)
	for _, want := range []string{"- safe: preserves inputs", "- existing warnings", `"criteria"`, "evidence"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, `"score"`) {
		t.Fatal("criteria prompt requests a holistic score")
	}
}

func TestVariantCountsCriterionVerdictsAndCellsWhoseRepeatsDisagree(t *testing.T) {
	spec := Spec{Matrix: []Variant{{Name: "v"}}, Judge: &Judge{Criteria: []Criterion{{ID: "a", Check: "x"}, {ID: "b", Check: "y"}}}}
	spec.Tasks = []batch.Task{{ID: "t"}}
	res := batch.Result{Tasks: []batch.TaskResult{{ID: "e.t.v.r1", Status: batch.StatusPass}, {ID: "e.t.v.r2", Status: batch.StatusPass}}}
	verdicts := []Judgement{
		{Cell: "e.t.v.r1", Repeat: 1, Criteria: []CriterionVerdict{{ID: "a", Met: true}, {ID: "b", Met: false, Downgraded: true}}},
		{Cell: "e.t.v.r1", Repeat: 2, Criteria: []CriterionVerdict{{ID: "a", Met: false}, {ID: "b", Met: false}}},
		{Cell: "e.t.v.r2", Repeat: 1, Criteria: []CriterionVerdict{{ID: "a", Met: true}, {ID: "b", Met: true}}},
		{Cell: "e.t.v.r2", Repeat: 2, Error: "invalid", Criteria: []CriterionVerdict{{ID: "a", Met: false}}},
	}
	want := []CriterionSummary{{ID: "a", Met: 2, Scored: 3, Disagreed: 1}, {ID: "b", Met: 1, Scored: 3}}
	if got := Summarize(spec, res, verdicts).Variants[0].Criteria; !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestOldJudgementsRoundTripWithoutCriteria(t *testing.T) {
	old := `{"cell":"e.t.v.r1","score":8,"reason":"ok"}`
	var got Judgement
	if err := json.Unmarshal([]byte(old), &got); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil || string(b) != old {
		t.Fatalf("round trip = %s, %v", b, err)
	}
}
