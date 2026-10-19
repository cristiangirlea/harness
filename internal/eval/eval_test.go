package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestVariantsOverrideLeanAndPreserveUnsetValues(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, taskLean := range []*bool{nil, agent.Bool(false), agent.Bool(true)} {
		var spec Spec
		if err := json.Unmarshal([]byte(`{"backend":"claude","lean":true,"matrix":[{"name":"inherit"},{"name":"lean","lean":true},{"name":"full","lean":false}],"tasks":[{"id":"one","prompt":"test"}]}`), &spec); err != nil {
			t.Fatal(err)
		}
		spec.Tasks[0].Lean = taskLean
		bs, err := Expand(spec, "abc12")
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []*bool{taskLean, agent.Bool(true), agent.Bool(false)} {
			got := bs.Tasks[i].Lean
			if (got == nil) != (want == nil) || got != nil && want != nil && *got != *want {
				t.Errorf("task lean %v, variant %s: lean = %v, want %v", taskLean, spec.Matrix[i].Name, got, want)
			}
		}
		if !bs.IsLean() {
			t.Error("spec lean default was lost")
		}
		data, err := json.Marshal(spec.Matrix)
		if err != nil {
			t.Fatal(err)
		}
		var variants []map[string]any
		if err := json.Unmarshal(data, &variants); err != nil {
			t.Fatal(err)
		}
		if _, present := variants[0]["lean"]; present || variants[1]["lean"] != true || variants[2]["lean"] != false {
			t.Errorf("variant lean values did not round-trip: %s", data)
		}
	}
}

func TestVariantLeanRequiresAResolvedBackendThatAppliesIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, source := range []string{"variant", "task", "spec", "default"} {
		for _, backend := range []string{"claude", "codex", "api", "gemini", "opencode", "agy"} {
			for _, lean := range []bool{false, true} {
				var spec Spec
				if err := json.Unmarshal([]byte(`{"matrix":[{"name":"test","lean":`+fmt.Sprint(lean)+`}],"tasks":[{"id":"one","prompt":"test"}]}`), &spec); err != nil {
					t.Fatal(err)
				}
				switch source {
				case "variant":
					spec.Matrix[0].Backend = backend
				case "task":
					spec.Tasks[0].Backend = backend
				case "spec":
					spec.Backend = backend
				case "default":
					backend = agent.DefaultBackendNameIn(runlog.Home())
				}
				_, err := Expand(spec, "abc12")
				if backend == "claude" || backend == "codex" {
					if err != nil {
						t.Errorf("%s lean=%v: %v", source, lean, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "lean") || !strings.Contains(err.Error(), "claude and codex") {
					t.Errorf("%s backend=%s lean=%v: got %v, want a claude-and-codex-only lean error", source, backend, lean, err)
				}
			}
		}
	}
}

func TestExpandShapeToolsVariants(t *testing.T) {
	spec := testSpec("somewhere")
	spec.ShapeTools = true
	for i := range spec.Tasks {
		spec.Tasks[i].ShapeTools = true
	}
	spec.Matrix = []Variant{{Name: "raw", Backend: "ev-ok"}, {Name: "shaped", Backend: "ev-ok", ShapeTools: true}}
	bs, err := Expand(spec, "abc12")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range bs.Tasks {
		if task.ShapeTools != strings.Contains(task.ID, ".shaped.") {
			t.Errorf("%s: ShapeTools=%v", task.ID, task.ShapeTools)
		}
	}
}

// Two stubs: one does the work the gate checks for, the other does nothing.
type stub struct {
	name  string
	write bool
}

func (s stub) Name() string            { return s.name }
func (s stub) Detect() (string, error) { return "stub", nil }
func (s stub) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	if s.write {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt), 0o644)
	}
	return agent.Result{Backend: s.name, Model: task.Model, Status: agent.StatusOK,
		Final: "done", Turns: 2, InputTokens: 100, OutputTokens: 10, CostUSD: 0.25, CostEstimate: true,
		Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
}

func init() {
	agent.Register(stub{"ev-ok", true})
	agent.Register(stub{"ev-lazy", false})
}

func testSpec(dir string) Spec {
	var spec Spec
	spec.Dir = dir
	spec.Gate = []string{"test -f out.txt"}
	spec.Tasks = []batch.Task{{ID: "alpha", Prompt: "do alpha"}, {ID: "beta", Prompt: "do beta"}}
	spec.Matrix = []Variant{{Backend: "ev-ok"}, {Name: "lazy", Backend: "ev-lazy"}}
	spec.Repeats = 2
	return spec
}

func TestExpand(t *testing.T) {
	spec := testSpec("somewhere")
	spec.Timeout = agent.Duration(5 * time.Minute)
	spec.MaxCostUSD = 2
	spec.Lean = agent.Bool(false)
	bs, err := Expand(spec, "abc12")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs.Tasks) != 8 {
		t.Fatalf("tasks = %d, want 2 tasks x 2 variants x 2 repeats", len(bs.Tasks))
	}
	var ids []string
	for _, task := range bs.Tasks {
		ids = append(ids, task.ID)
		if task.Retry != nil {
			t.Errorf("%s: an eval measures single attempts, so retry must be nil", task.ID)
		}
	}
	joined := strings.Join(ids, " ")
	for _, want := range []string{"abc12.alpha.ev-ok.r1", "abc12.alpha.ev-ok.r2", "abc12.beta.lazy.r2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing id %s in %s", want, joined)
		}
	}
	if bs.Timeout != agent.Duration(5*time.Minute) || bs.MaxCostUSD != 2 || bs.Lean == nil || *bs.Lean || len(bs.Gate) != 1 || bs.Dir != "somewhere" {
		t.Errorf("spec fields must carry through: %+v", bs)
	}
	first := bs.Tasks[0]
	if first.Backend != "ev-ok" || first.Prompt != "do alpha" {
		t.Errorf("task = %+v", first)
	}
	if bs.Worktrees != batch.WorktreesFresh {
		t.Errorf("worktrees = %q, want fresh: the judge reads each cell's worktree after the batch", bs.Worktrees)
	}
	spec.Worktrees = batch.WorktreesSlots
	if bs, err := Expand(spec, "abc12"); err != nil || bs.Worktrees != batch.WorktreesSlots {
		t.Errorf("an eval without a judge may ask for slots: %q, %v", bs.Worktrees, err)
	}
}

func TestExpandDefaultCellsAreNotRouted(t *testing.T) {
	for _, backend := range []string{"", "ev-ok"} {
		t.Run("spec-"+backend, func(t *testing.T) {
			dir := initRepo(t)
			name := agent.DefaultBackendName()
			old, err := agent.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			agent.Register(stub{name, false})
			defer agent.Register(old)
			if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"ev-lazy","model":"routed","effort":"medium"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			spec := Spec{Spec: batch.Spec{Dir: dir, Backend: backend,
				Tasks: []batch.Task{{ID: "one", Prompt: "Implement a widget"}, {ID: "two", Prompt: "Implement another widget"}}},
				Matrix: []Variant{{Name: "default"}}, Repeats: 2}
			bs, err := Expand(spec, "routecheck")
			if err != nil {
				t.Fatal(err)
			}
			want := firstOf(backend, name)
			for _, cell := range bs.Tasks {
				if cell.Backend != want {
					t.Errorf("cell %s backend = %q, want %q", cell.ID, cell.Backend, want)
				}
			}
			res, err := batch.Run(context.Background(), bs, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := batch.Remove(res); err != nil {
					t.Error(err)
				}
			}()
			for _, tr := range res.Tasks {
				if tr.Status != batch.StatusPass || len(tr.Attempts) != 1 {
					t.Fatalf("cell: %+v", tr)
				}
				rec, err := runlog.Load(tr.Attempts[0].RunID)
				if err != nil {
					t.Fatal(err)
				}
				if rec.Backend != want || rec.RoutedBy != nil || rec.Task.Model != "" || rec.Task.Effort != "" {
					t.Errorf("cell was routed: %+v", rec)
				}
			}
		})
	}
}

func TestExpandRejectsBadSpecs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"no matrix", func(s *Spec) { s.Matrix = nil }, "matrix"},
		{"no tasks", func(s *Spec) { s.Tasks = nil }, "tasks"},
		{"duplicate variant", func(s *Spec) { s.Matrix = []Variant{{Backend: "ev-ok"}, {Backend: "ev-ok"}} }, "duplicate"},
		{"unknown backend", func(s *Spec) { s.Matrix = []Variant{{Backend: "nope"}} }, "unknown backend"},
		{"bad repeats", func(s *Spec) { s.Repeats = -1 }, "repeats"},
		{"dotted task id", func(s *Spec) { s.Tasks[0].ID = "api.vet" }, "no dots"},
		{"dotted variant", func(s *Spec) { s.Matrix[0].Name = "opus.high" }, "no dots"},
		{"spaced variant", func(s *Spec) { s.Matrix[0].Name = "opus high" }, "variant"},
		{"spec retry", func(s *Spec) { s.Retry = []string{"ev-ok"} }, "retry does not belong"},
		{"task retry", func(s *Spec) { s.Tasks[0].Retry = []string{"ev-ok"} }, "retry does not belong"},
		{"slots with a judge", func(s *Spec) { s.Worktrees = batch.WorktreesSlots; s.Judge = &Judge{Rubric: "r"} }, "fresh"},
	} {
		spec := testSpec("somewhere")
		tc.edit(&spec)
		if _, err := Expand(spec, "abc12"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// A cell with its own empty gate list would run gateless but be saved and
// diffed as gated, so the eval is refused before any cell runs or is recorded.
func TestAnEvalRefusesATaskWithAnEmptyGateList(t *testing.T) {
	dir := initRepo(t)
	p := filepath.Join(t.TempDir(), "eval.json")
	body := `{"dir":` + strconv.Quote(dir) + `,"gate":["test -f out.txt"],"matrix":[{"backend":"ev-ok"}],` +
		`"tasks":[{"id":"alpha","prompt":"do alpha"},{"id":"beta","prompt":"do beta","gate":[]}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, res, err := Run(context.Background(), spec, nil); err == nil || !strings.Contains(err.Error(), "beta") ||
		!strings.Contains(err.Error(), "empty gate list") || len(res.Tasks) != 0 {
		t.Fatalf("err = %v, tasks = %d; want beta refused before anything ran", err, len(res.Tasks))
	}
	if recs, _, err := ListAll(); err != nil || len(recs) != 0 {
		t.Errorf("no eval should be recorded: %d records, err = %v", len(recs), err)
	}
}

func TestRunScoresEachVariant(t *testing.T) {
	dir := initRepo(t)
	rec, res, err := Run(context.Background(), testSpec(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" || rec.BatchID != res.ID || len(res.Tasks) != 8 {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Table.Variants) != 2 {
		t.Fatalf("variants = %+v", rec.Table.Variants)
	}
	byName := map[string]VariantRow{}
	for _, v := range rec.Table.Variants {
		byName[v.Name] = v
	}
	ok, lazy := byName["ev-ok"], byName["lazy"]
	if ok.Attempts != 4 || ok.Passed != 4 || lazy.Attempts != 4 || lazy.Passed != 0 {
		t.Errorf("ok = %+v, lazy = %+v", ok, lazy)
	}
	if ok.MedianContext != 100 {
		t.Errorf("context should count every input token once: %+v", ok)
	}
	if ok.MedianCostUSD != 0.25 || !ok.CostEstimate {
		t.Errorf("cost should come from the attempts: %+v", ok)
	}
	if ok.Statuses["ok"] != 4 {
		t.Errorf("status histogram = %v", ok.Statuses)
	}
	// Per task and variant: alpha passed twice on ev-ok, never on lazy.
	counts := map[string]string{}
	for _, row := range rec.Table.Tasks {
		counts[row.Task+"/"+row.Variant] = row.Score()
	}
	if counts["alpha/ev-ok"] != "2/2" || counts["alpha/lazy"] != "0/2" {
		t.Errorf("task rows = %v", counts)
	}

	// The record is on disk and reads back.
	loaded, err := LoadRecord(rec.ID)
	if err != nil || loaded.BatchID != rec.ID && loaded.BatchID != res.ID {
		t.Fatalf("loaded = %+v err=%v", loaded, err)
	}
	if len(loaded.Table.Variants) != 2 {
		t.Errorf("table did not round-trip: %+v", loaded.Table)
	}
	// Eval leaves the worktrees for the caller to remove.
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("worktree should still exist until the caller removes it: %v", err)
	}
	if err := batch.Remove(res); err != nil {
		t.Errorf("remove: %v", err)
	}
}

// initRepo makes a repository with one commit and points HRN_HOME at a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestAnEvalNeverSwitchesBackends(t *testing.T) {
	bs, err := Expand(testSpec("somewhere"), "abc12")
	if err != nil {
		t.Fatal(err)
	}
	// A cell measures its own backend; one carried on elsewhere would score
	// the other one under its name.
	if !bs.NoSwitch {
		t.Error("the batch an eval runs must set no_switch")
	}
}

func TestAnEvalFailsACellThatChangedAProtectedFileUnlessItSaysOtherwise(t *testing.T) {
	bs, err := Expand(testSpec("somewhere"), "abc12")
	if err != nil {
		t.Fatal(err)
	}
	if bs.ProtectMode != batch.ProtectFail {
		t.Errorf("protect_mode = %q, want %q by default in an eval", bs.ProtectMode, batch.ProtectFail)
	}
	spec := testSpec("somewhere")
	spec.ProtectMode = batch.ProtectReport
	if bs, err = Expand(spec, "abc12"); err != nil || bs.ProtectMode != batch.ProtectReport {
		t.Errorf("protect_mode = %q (%v), want the spec's own %q", bs.ProtectMode, err, batch.ProtectReport)
	}
}

// ev-tamper does the work and also changes README.md, which a spec may protect.
type tamperStub struct{ stub }

func (s tamperStub) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	_ = os.WriteFile(filepath.Join(task.Dir, "README.md"), []byte("weakened\n"), 0o644)
	return s.stub.Run(ctx, task, emit)
}

func init() { agent.Register(tamperStub{stub{"ev-tamper", true}}) }

func TestAnEvalCellThatChangedAProtectedFileFailsByDefault(t *testing.T) {
	for _, c := range []struct {
		name     string
		protect  []string
		specMode string
		taskMode string
		passed   int
	}{
		{"changed, eval default", []string{"README.md"}, "", "", 0},
		{"changed, spec says report", []string{"README.md"}, batch.ProtectReport, "", 1},
		{"changed, task says report", []string{"README.md"}, "", batch.ProtectReport, 1},
		{"only an added file is protected", []string{"out.txt"}, "", "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var spec Spec
			spec.Dir = initRepo(t)
			spec.Gate = []string{"test -f out.txt"}
			spec.Protect, spec.ProtectMode = c.protect, c.specMode
			spec.Tasks = []batch.Task{{ID: "alpha", Prompt: "do alpha", ProtectMode: c.taskMode}}
			spec.Matrix = []Variant{{Backend: "ev-tamper"}}
			rec, res, err := Run(context.Background(), spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := rec.Table.Variants[0].Passed; got != c.passed {
				t.Errorf("passed = %d, want %d: %+v", got, c.passed, res.Tasks[0].Attempts)
			}
		})
	}
}

// ev-half does task alpha at a full context and, after one look, gives up on the others cheaply.
type halfStub struct{ stub }

func (s halfStub) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "Read", Text: "out.txt"})
	tokens := int64(10)
	if strings.Contains(task.Prompt, "alpha") {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt), 0o644)
		tokens = 1000
	}
	return agent.Result{Backend: s.name, Status: agent.StatusOK, Final: "done", Turns: 1,
		InputTokens: tokens, CostUSD: float64(tokens) / 1000, Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
}

func init() { agent.Register(halfStub{stub{"ev-half", false}}) }

func TestAVariantsMediansOverPassingCellsLeaveCheapFailuresOut(t *testing.T) {
	spec := testSpec(initRepo(t))
	spec.Matrix = []Variant{{Backend: "ev-half"}}
	rec, _, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := rec.Table.Variants[0]
	if v.Passed != 2 || v.MedianContext != 505 || v.MedianContextPassed != 1000 || v.MedianCostPassedUSD != 1 || !v.PassedCostKnown {
		t.Errorf("row = %+v; want 2 passes, all-cells context 505, passing context 1000 and cost $1", v)
	}
}

// ev-idle works on alpha and, on any other task, answers in one turn without a tool call or a change.
type idleStub struct{ stub }

func (s idleStub) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	if strings.Contains(task.Prompt, "alpha") {
		emit(agent.Event{Kind: agent.EventToolCall, Tool: "Write", Text: "out.txt"})
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt), 0o644)
		return s.stub.Run(ctx, task, emit)
	}
	return agent.Result{Backend: s.name, Status: agent.StatusOK, Final: "I can't access the repository files.", Turns: 1,
		InputTokens: 34000, Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
}

func init() { agent.Register(idleStub{stub{"ev-idle", false}}) }

func TestACellThatNeverActedIsCountedApartFromFailures(t *testing.T) {
	spec := testSpec(initRepo(t))
	spec.Matrix = []Variant{{Backend: "ev-idle"}}
	rec, _, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := rec.Table.Variants[0]
	if v.Attempts != 4 || v.Passed != 2 || v.DidNotAct != 2 || v.PassRate() != 1 {
		t.Errorf("row = %+v (pass rate %v); want 4 cells, 2 passes, 2 that did not act, a pass rate of 1 over the cells that acted", v, v.PassRate())
	}
	if v.MedianContext != 100 || v.MedianTurns != 2 {
		t.Errorf("medians = context %v, turns %v; the cells that never acted must stay out of them", v.MedianContext, v.MedianTurns)
	}
	for _, row := range rec.Table.Tasks {
		want := map[string]string{"alpha": "2/2", "beta": "0/0 +2 did not act"}[row.Task]
		if row.Score() != want {
			t.Errorf("task %s scores %q, want %q", row.Task, row.Score(), want)
		}
	}
}

func TestAnAnswerOnlyTaskThatFailsWithoutAToolCallStillActed(t *testing.T) {
	var spec Spec
	spec.Dir = initRepo(t)
	spec.Tasks = []batch.Task{{ID: "beta", Prompt: "is there a bug in beta?", Expect: "^yes"}}
	spec.Matrix = []Variant{{Backend: "ev-idle"}}
	rec, _, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := rec.Table.Variants[0]; v.DidNotAct != 0 || v.Attempts != 1 || v.Passed != 0 {
		t.Errorf("row = %+v; the answer is the work, so a wrong one is a failure", v)
	}
}

func TestATasksScoreKeepsTheCellsThatNeverActedBesideItsCount(t *testing.T) {
	r := TaskRow{Passed: 1, Total: 2, DidNotAct: 1, Judged: 1, MeanScore: 8}
	if got := r.Score(); got != "1/1 +1 did not act 8.0" {
		t.Errorf("score = %q", got)
	}
}
