package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// varyingJudge gives the same cell 6, then 8, then 7, so its repeats disagree.
type varyingJudge struct {
	mu    *sync.Mutex
	seen  map[string]int
	tasks *[]agent.Task
}

func (varyingJudge) Name() string            { return "ev-judge-vary" }
func (varyingJudge) Detect() (string, error) { return "stub", nil }
func (j varyingJudge) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	j.mu.Lock()
	n := j.seen[task.Dir]
	j.seen[task.Dir]++
	*j.tasks = append(*j.tasks, task)
	j.mu.Unlock()
	score := []int{6, 8, 7}[n%3]
	return agent.Result{Status: agent.StatusOK, Final: fmt.Sprintf(`{"score": %d, "reason": "r"}`, score), CostUSD: 0.01}
}

var varying = varyingJudge{mu: &sync.Mutex{}, seen: map[string]int{}, tasks: &[]agent.Task{}}

func init() { agent.Register(varying) }

func TestJudgeRepeatsScoreEachPassingCellNTimesOnTheSameEvidence(t *testing.T) {
	dir := initRepo(t)
	spec := testSpec(dir)
	spec.Judge = &Judge{Backend: "ev-judge-vary", Rubric: "out.txt holds the prompt", Repeats: 3}
	var mu sync.Mutex
	var labels []string
	rec, res, err := Run(context.Background(), spec, func(e batch.Event) {
		if e.Kind == "judge" {
			mu.Lock()
			labels = append(labels, e.Text)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)

	// ev-ok passes 2 tasks x 2 repeats; each of those 4 cells is judged 3 times.
	if len(rec.Judgements) != 12 {
		t.Fatalf("judgements = %d, want 12", len(rec.Judgements))
	}
	runs := map[string]bool{}
	repeats := map[string][]int{}
	for _, j := range rec.Judgements {
		if !j.Scored() || j.RunID == "" || runs[j.RunID] {
			t.Errorf("each repeat is its own scored run: %+v", j)
		}
		runs[j.RunID] = true
		repeats[j.Cell] = append(repeats[j.Cell], j.Repeat)
	}
	for cell, rs := range repeats {
		if len(rs) != 3 || rs[0] != 1 || rs[1] != 2 || rs[2] != 3 {
			t.Errorf("%s repeats = %v, want 1, 2, 3", cell, rs)
		}
	}
	varying.mu.Lock()
	prompts := map[string]map[string]bool{}
	for _, task := range *varying.tasks {
		if prompts[task.Dir] == nil {
			prompts[task.Dir] = map[string]bool{}
		}
		prompts[task.Dir][task.Prompt] = true
	}
	varying.mu.Unlock()
	for cell, ps := range prompts {
		if len(ps) != 1 {
			t.Errorf("%s: the repeats saw %d different prompts, want the same evidence each time", cell, len(ps))
		}
	}
	if len(labels) != 12 || !strings.HasPrefix(labels[0], "judge ") || !strings.Contains(labels[0], "/3: ") {
		t.Errorf("judge events = %v, want one per verdict naming its repeat", labels)
	}

	var ok VariantRow
	for _, v := range rec.Table.Variants {
		if v.Name == "ev-ok" {
			ok = v
		}
	}
	// Every cell got 6, 8 and 7 in some order, so the mean is 7 whatever the order.
	if ok.Judged != 4 || ok.MeanScore != 7 || ok.JudgeRepeats != 3 || ok.ScoreMin < 6 || ok.ScoreMax > 8 || ok.ScoreMin > ok.ScoreMax {
		t.Errorf("ev-ok row = %+v", ok)
	}

	loaded, err := LoadRecord(rec.ID)
	if err != nil || len(loaded.Judgements) != 12 || loaded.Spec.Judge.Repeats != 3 {
		t.Errorf("repeats did not round-trip: %d verdicts, %v", len(loaded.Judgements), err)
	}
}

func TestSummarizeShowsTheJudgeSpreadAcrossRepeats(t *testing.T) {
	spec := Spec{Matrix: []Variant{{Backend: "ev-ok"}}}
	spec.Tasks = []batch.Task{{ID: "a"}}
	res := batch.Result{Tasks: []batch.TaskResult{
		{ID: "e.a.ev-ok.r1", Status: batch.StatusPass},
		{ID: "e.a.ev-ok.r2", Status: batch.StatusPass},
	}}
	verdicts := []Judgement{
		{Cell: "e.a.ev-ok.r1", Score: 6, Repeat: 1}, {Cell: "e.a.ev-ok.r1", Score: 8, Repeat: 2},
		{Cell: "e.a.ev-ok.r2", Score: 8, Repeat: 1}, {Cell: "e.a.ev-ok.r2", Score: 9, Repeat: 2},
		{Cell: "e.a.ev-ok.r2", Error: "timeout", Repeat: 3}, // no score, so no third repeat
	}
	table := Summarize(spec, res, verdicts)
	row := table.Variants[0]
	// Repeat 1 averages 7, repeat 2 averages 8.5.
	if row.Judged != 2 || row.MeanScore != 7.75 || row.JudgeRepeats != 2 || row.ScoreMin != 7 || row.ScoreMax != 8.5 {
		t.Errorf("row = %+v", row)
	}
	if got := table.Tasks[0].Score(); got != "2/2 7.8 (7.0-8.5)" {
		t.Errorf("task score = %q", got)
	}

	// A record from before repeats: one verdict per cell, no repeat number, no spread.
	old := Summarize(spec, res, []Judgement{{Cell: "e.a.ev-ok.r1", Score: 6}, {Cell: "e.a.ev-ok.r2", Score: 8}})
	if r := old.Variants[0]; r.Judged != 2 || r.MeanScore != 7 || r.JudgeRepeats != 0 || r.ScoreMin != 0 || r.ScoreMax != 0 {
		t.Errorf("old row = %+v", r)
	}
	if got := old.Tasks[0].Score(); got != "2/2 7.0" {
		t.Errorf("old task score = %q", got)
	}
}

func TestCompareMarksAJudgeScoreChangeWithinTheJudgesSpreadAsNoise(t *testing.T) {
	row := func(mean float64, repeats int, lo, hi float64) *VariantRow {
		return &VariantRow{Name: "v", Judged: 2, MeanScore: mean, JudgeRepeats: repeats, ScoreMin: lo, ScoreMax: hi}
	}
	for _, tc := range []struct {
		name string
		a, b *VariantRow
		want bool
	}{
		{"smaller than a's spread", row(7, 3, 6, 8), row(7.5, 0, 0, 0), true},
		{"smaller than b's spread", row(7, 0, 0, 0), row(7.5, 2, 7, 8), true},
		{"as large as the spread", row(7, 2, 6.5, 7.5), row(8, 2, 7.5, 8.5), false},
		{"no repeats on either side", row(7, 0, 0, 0), row(7.1, 0, 0, 0), false},
		{"one side not judged", row(7, 3, 6, 8), &VariantRow{Name: "v"}, false},
	} {
		a, b := Record{ID: "a"}, Record{ID: "b"}
		a.Table.Variants = []VariantRow{*tc.a}
		b.Table.Variants = []VariantRow{*tc.b}
		a.Table.Tasks = []TaskRow{{Task: "t", Variant: "v", Judged: tc.a.Judged, MeanScore: tc.a.MeanScore, JudgeRepeats: tc.a.JudgeRepeats, ScoreMin: tc.a.ScoreMin, ScoreMax: tc.a.ScoreMax}}
		b.Table.Tasks = []TaskRow{{Task: "t", Variant: "v", Judged: tc.b.Judged, MeanScore: tc.b.MeanScore, JudgeRepeats: tc.b.JudgeRepeats, ScoreMin: tc.b.ScoreMin, ScoreMax: tc.b.ScoreMax}}
		d := Compare(a, b)
		if d.Variants[0].WithinNoise != tc.want || d.Tasks[0].WithinNoise != tc.want {
			t.Errorf("%s: within noise = %v / %v, want %v", tc.name, d.Variants[0].WithinNoise, d.Tasks[0].WithinNoise, tc.want)
		}
	}
}

func TestCompareDoesNotCountJudgeRepeatsAsAChangedJudgeButCountsHarnessFields(t *testing.T) {
	a, b := twoEvals()
	a.Spec.Judge = &Judge{Rubric: "strict"}
	b.Spec.Judge = &Judge{Rubric: "strict", Repeats: 3}
	if Compare(a, b).JudgeChanged {
		t.Error("more repeats measure the same judge, on the same scale")
	}
	for _, edit := range []func(*Variant){
		func(v *Variant) { v.PromptPrefix = "Be brief." },
		func(v *Variant) { v.AppendSystemPrompt = "Run the tests." },
		func(v *Variant) { v.NoSkills = true },
	} {
		a.Spec.Matrix = []Variant{{Name: "sonnet", Backend: "claude"}}
		changed := Variant{Name: "sonnet", Backend: "claude"}
		edit(&changed)
		b.Spec.Matrix = []Variant{changed}
		if got := join(Compare(a, b).VariantChanged); got != "sonnet" {
			t.Errorf("%+v: variant changed = %q, want sonnet", changed, got)
		}
	}
}

func TestJudgeRepeatsMustNotBeNegative(t *testing.T) {
	spec := testSpec("somewhere")
	spec.Judge = &Judge{Rubric: "x", Repeats: -1}
	if _, err := Expand(spec, "abc12"); err == nil || !strings.Contains(err.Error(), "judge: repeats -1 is negative") {
		t.Errorf("err = %v", err)
	}
}

func TestPromptPrefixGoesBeforeEveryTaskOnEveryBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, backend := range []string{"claude", "codex", "api", "gemini", "opencode", "agy"} {
		spec := testSpec("somewhere")
		spec.Matrix = []Variant{{Name: "plain", Backend: backend}, {Name: "briefed", Backend: backend, PromptPrefix: "Read the tests first."}}
		bs, err := Expand(spec, "abc12")
		if err != nil {
			t.Fatalf("%s: %v", backend, err)
		}
		for _, task := range bs.Tasks {
			original := "do alpha"
			if strings.Contains(task.ID, ".beta.") {
				original = "do beta"
			}
			want := original
			if strings.Contains(task.ID, ".briefed.") {
				want = "Read the tests first.\n\n" + original
			}
			if task.Prompt != want {
				t.Errorf("%s %s: prompt = %q, want %q", backend, task.ID, task.Prompt, want)
			}
		}
	}
}

func TestTheJudgeGradesTheTasksOwnPromptNotThePrefixedOne(t *testing.T) {
	dir := initRepo(t)
	judged.mu.Lock()
	*judged.tasks = nil
	judged.mu.Unlock()
	spec := testSpec(dir)
	spec.Repeats = 1
	spec.Matrix = []Variant{{Backend: "ev-ok", PromptPrefix: "PREFIX-ONLY"}}
	spec.Judge = &Judge{Backend: "ev-judge", Rubric: "out.txt holds the prompt"}
	_, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	judged.mu.Lock()
	defer judged.mu.Unlock()
	if len(*judged.tasks) != 2 {
		t.Fatalf("judge ran %d times", len(*judged.tasks))
	}
	for _, task := range *judged.tasks {
		section := task.Prompt[strings.Index(task.Prompt, "## Task"):strings.Index(task.Prompt, "## Rubric")]
		if strings.Contains(section, "PREFIX-ONLY") || !strings.Contains(section, "do ") {
			t.Errorf("the judge's task section = %q, want the task's own prompt", section)
		}
	}
	// The agent itself was given the prefix: ev-ok writes its prompt to out.txt.
	diff, _, err := batch.Diff(context.Background(), res, res.Tasks[0].ID)
	if err != nil || !strings.Contains(diff, "PREFIX-ONLY") {
		t.Errorf("the agent's prompt should carry the prefix: %v\n%s", err, diff)
	}
}

func TestHarnessVariantsReachTheirCellsOnlyOnBackendsThatApplyThem(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	type field struct {
		name     string
		json     string
		backends []string // the ones that apply it
		set      func(agent.Options) bool
	}
	fields := []field{
		{"append_system_prompt", `"append_system_prompt":"Run the tests."`, []string{"claude", "api"},
			func(o agent.Options) bool { return o.AppendSystemPrompt == "Run the tests." }},
		{"no_skills", `"no_skills":true`, []string{"claude"},
			func(o agent.Options) bool { return o.NoSkills }},
	}
	for _, f := range fields {
		for _, source := range []string{"variant", "task", "spec"} {
			for _, backend := range []string{"claude", "codex", "api", "gemini", "opencode", "agy"} {
				variant, task, top := `{"name":"v","backend":"`+backend+`"}`, `{"id":"one","prompt":"p"}`, ""
				switch source {
				case "variant":
					variant = `{"name":"v","backend":"` + backend + `",` + f.json + `}`
				case "task":
					task = `{"id":"one","prompt":"p",` + f.json + `}`
				case "spec":
					top = f.json + ","
				}
				var spec Spec
				if err := json.Unmarshal([]byte(`{`+top+`"matrix":[`+variant+`],"tasks":[`+task+`]}`), &spec); err != nil {
					t.Fatal(err)
				}
				bs, err := Expand(spec, "abc12")
				applies := slices.Contains(f.backends, backend)
				if !applies {
					if err == nil || !strings.Contains(err.Error(), f.name+" is only supported by") || !strings.Contains(err.Error(), "uses "+backend) {
						t.Errorf("%s from the %s on %s: err = %v, want a refusal naming the backend", f.name, source, backend, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s from the %s on %s: %v", f.name, source, backend, err)
					continue
				}
				// A spec-wide value reaches the cell through the batch's own inheritance.
				if got := bs.Tasks[0].Options.Over(bs.Options); !f.set(got) {
					t.Errorf("%s from the %s on %s did not reach the cell: %+v", f.name, source, backend, got)
				}
			}
		}
	}
	// Nothing set, nothing refused, whatever the default backend is.
	spec := testSpec("somewhere")
	spec.Matrix = []Variant{{Name: "default"}}
	if _, err := Expand(spec, "abc12"); err != nil && strings.Contains(err.Error(), "only supported") {
		t.Errorf("default backend %s: %v", agent.DefaultBackendNameIn(runlog.Home()), err)
	}
}
