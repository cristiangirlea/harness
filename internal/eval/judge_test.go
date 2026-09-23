package eval

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

// judgeStub scores 8 unless the rubric asks it to ramble, and keeps every
// task it was handed so a test can read what the judge was shown.
type judgeStub struct {
	mu    *sync.Mutex
	tasks *[]agent.Task
}

func (judgeStub) Name() string            { return "ev-judge" }
func (judgeStub) Detect() (string, error) { return "stub", nil }
func (j judgeStub) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	j.mu.Lock()
	*j.tasks = append(*j.tasks, task)
	j.mu.Unlock()
	final := "The diff writes the file.\n```json\n{\"score\": 8, \"reason\": \"does what was asked\"}\n```"
	if strings.Contains(task.Prompt, "RAMBLE") {
		final = "I think it is fine overall."
	}
	return agent.Result{Status: agent.StatusOK, Final: final, CostUSD: 0.01}
}

var judged = judgeStub{mu: &sync.Mutex{}, tasks: &[]agent.Task{}}

func init() { agent.Register(judged) }

func TestJudgeScoresPassingCells(t *testing.T) {
	dir := initRepo(t)
	judged.mu.Lock()
	*judged.tasks = nil
	judged.mu.Unlock()

	spec := testSpec(dir)
	spec.Judge = &Judge{Backend: "ev-judge", Model: "haiku", Rubric: "out.txt holds the prompt",
		Rubrics: map[string]string{"beta": "RAMBLE"}}
	var mu sync.Mutex
	var events []batch.Event
	rec, res, err := Run(context.Background(), spec, func(e batch.Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)

	// ev-ok passes 2 tasks x 2 repeats; lazy fails its gate and is not judged.
	if len(rec.Judgements) != 4 {
		t.Fatalf("judgements = %+v, want one per passing cell", rec.Judgements)
	}
	scored := 0
	for _, j := range rec.Judgements {
		if !strings.Contains(j.Cell, ".ev-ok.") {
			t.Errorf("judged %s, which failed its gate", j.Cell)
		}
		switch {
		case strings.Contains(j.Cell, ".alpha."):
			if !j.Scored() || j.Score != 8 || j.Reason != "does what was asked" || j.RunID == "" {
				t.Errorf("alpha verdict = %+v", j)
			}
			scored++
		case strings.Contains(j.Cell, ".beta."):
			if j.Scored() || !strings.Contains(j.Error, "no {\"score\"") {
				t.Errorf("an answer without a verdict must be an error, not a zero: %+v", j)
			}
		}
	}
	if scored != 2 || rec.JudgeCostUSD < 0.039 || rec.JudgeCostUSD > 0.041 {
		t.Errorf("scored = %d, judge cost = %v", scored, rec.JudgeCostUSD)
	}

	// What the judge was shown and how it was run.
	judged.mu.Lock()
	tasks := append([]agent.Task(nil), *judged.tasks...)
	judged.mu.Unlock()
	if len(tasks) != 4 {
		t.Fatalf("judge ran %d times", len(tasks))
	}
	for _, task := range tasks {
		if task.Mode != agent.ModeReadOnly || task.Model != "haiku" || !task.NoInstructions {
			t.Errorf("judge options = %+v: read-only, the judge's model, no house rules", task.Options)
		}
		if !strings.Contains(task.Dir, "worktrees") {
			t.Errorf("judge dir = %s, want the cell's worktree", task.Dir)
		}
		if !strings.Contains(task.Prompt, "+++ b/out.txt") || !strings.Contains(task.Prompt, "## Agent's final answer") {
			t.Errorf("the judge must see the diff and the answer:\n%s", task.Prompt)
		}
	}

	// The table carries the scores; beta's failed verdicts count for nothing.
	var ok VariantRow
	for _, v := range rec.Table.Variants {
		if v.Name == "ev-ok" {
			ok = v
		}
	}
	if ok.Judged != 2 || ok.MeanScore != 8 {
		t.Errorf("ev-ok row = %+v", ok)
	}
	rows := map[string]string{}
	for _, r := range rec.Table.Tasks {
		rows[r.Task+"/"+r.Variant] = r.Score()
	}
	if rows["alpha/ev-ok"] != "2/2 8.0" || rows["beta/ev-ok"] != "2/2" || rows["alpha/lazy"] != "0/2" {
		t.Errorf("task rows = %v", rows)
	}

	judgeEvents := 0
	for _, e := range events {
		if e.Kind == "judge" {
			judgeEvents++
		}
	}
	if judgeEvents != 4 {
		t.Errorf("judge events = %d, want one per verdict", judgeEvents)
	}

	loaded, err := LoadRecord(rec.ID)
	if err != nil || len(loaded.Judgements) != 4 || loaded.Spec.Judge == nil || loaded.Spec.Judge.Rubric == "" {
		t.Errorf("judgements did not round-trip: %+v %v", loaded.Judgements, err)
	}
}

func TestJudgeIsCheckedBeforeAnythingRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge Judge
		want  string
	}{
		{"no rubric", Judge{}, "task alpha has no rubric"},
		{"one task uncovered", Judge{Rubrics: map[string]string{"alpha": "x"}}, "task beta has no rubric"},
		{"rubric for no task", Judge{Rubric: "x", Rubrics: map[string]string{"gamma": "y"}}, `"gamma", which is not a task`},
		{"unknown backend", Judge{Rubric: "x", Backend: "nope"}, "judge: unknown backend"},
		{"negative timeout", Judge{Rubric: "x", Timeout: -1}, "negative"},
	} {
		spec := testSpec("somewhere")
		j := tc.judge
		spec.Judge = &j
		if _, err := Expand(spec, "abc12"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	for _, tc := range []struct {
		text   string
		score  int
		reason string
		err    string
	}{
		{`{"score": 7, "reason": "minor flaws"}`, 7, "minor flaws", ""},
		{"Looks good.\n```json\n{\"score\":10,\"reason\":\"exact\"}\n```", 10, "exact", ""},
		// The last verdict wins: a judge that quotes the format first.
		{`Format is {"score": 0}. My verdict: {"score": 4, "reason": "partial"}`, 4, "partial", ""},
		{`{"reason": "no score"} {"note": {"nested": 1}}`, 0, "", "no {"},
		{`{"score": 11}`, 0, "", "0 to 10"},
		{`{"score": 6.5}`, 0, "", "whole number"},
		{"no json at all", 0, "", "no {"},
	} {
		score, reason, err := parseVerdict(tc.text)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err = %v, want %q", tc.text, err, tc.err)
			}
			continue
		}
		if err != nil || score != tc.score || reason != tc.reason {
			t.Errorf("%q: got %d %q %v", tc.text, score, reason, err)
		}
	}
}

func TestJudgePromptClipsAndFences(t *testing.T) {
	diff := strings.Repeat("é", maxJudgeDiff) // two bytes each, so the cut lands mid-rune
	p := judgePrompt("fix ```it```", "rubric", "", diff, []string{"new.txt"})
	if !strings.Contains(p, "````\nfix ```it```\n````") {
		t.Errorf("a prompt holding a fence needs a longer one:\n%s", p[:600])
	}
	if !strings.Contains(p, "more bytes not shown]") || !strings.Contains(p, "new.txt") {
		t.Error("a clipped diff says so, and untracked files are listed")
	}
	if strings.Contains(p, "(no changes)") {
		t.Error("a diff with content is not reported as empty")
	}
	if !strings.Contains(judgePrompt("a", "b", "c", " \n", nil), "(no changes)") {
		t.Error("an empty diff is spelled out, so the judge cannot read silence as a missing section")
	}
	if strings.ContainsRune(p, utf8.RuneError) {
		t.Error("clipping split a rune")
	}
}

func TestSummarizeCountsUnchangedPassesAndToolCalls(t *testing.T) {
	spec := Spec{Matrix: []Variant{{Backend: "ev-ok"}}}
	spec.Tasks = []batch.Task{{ID: "a"}}
	att := func(calls int) []batch.Attempt {
		return []batch.Attempt{{Result: agent.Result{Status: agent.StatusOK, ToolCalls: calls}}}
	}
	res := batch.Result{Tasks: []batch.TaskResult{
		{ID: "e.a.ev-ok.r1", Status: batch.StatusPass, Commit: "abc", Attempts: att(3)},
		{ID: "e.a.ev-ok.r2", Status: batch.StatusPass, Attempts: att(8)},
		{ID: "e.a.ev-ok.r3", Status: batch.StatusFail, Attempts: att(5)},
	}}
	row := Summarize(spec, res, nil).Variants[0]
	if row.Passed != 2 || row.Unchanged != 1 {
		t.Errorf("row = %+v: one of the two passes committed nothing", row)
	}
	if row.MedianTools != 5 {
		t.Errorf("median tool calls = %v, want 5", row.MedianTools)
	}
}
