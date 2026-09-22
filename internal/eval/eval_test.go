package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

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
		Final: "done", Turns: 2, InputTokens: 100, OutputTokens: 10, CostUSD: 0.25, CostEstimate: true}
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
	if bs.Timeout != agent.Duration(5*time.Minute) || bs.MaxCostUSD != 2 || len(bs.Gate) != 1 || bs.Dir != "somewhere" {
		t.Errorf("spec fields must carry through: %+v", bs)
	}
	first := bs.Tasks[0]
	if first.Backend != "ev-ok" || first.Prompt != "do alpha" {
		t.Errorf("task = %+v", first)
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
	} {
		spec := testSpec("somewhere")
		tc.edit(&spec)
		if _, err := Expand(spec, "abc12"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
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
		t.Errorf("context should count prompt tokens and cache reads: %+v", ok)
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

// initRepo makes a repository with one commit and points HX_HOME at a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HX_HOME", t.TempDir())
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
