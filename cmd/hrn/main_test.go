package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type contextWarningBackend struct{ recorder }

func (r contextWarningBackend) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	fmt.Fprintln(os.Stderr, "context test backend started")
	return r.recorder.Run(ctx, task, emit)
}

func TestContinueWarnsOnStderrBeforeStartingOnlyAboveTheContextThreshold(t *testing.T) {
	agent.Register(contextWarningBackend{recorder{name: "stub-context"}})
	for _, jsonOut := range []bool{false, true} {
		for _, n := range []int64{0, 149999, 150000, 150001, 285000} {
			t.Run(fmt.Sprintf("json=%v/context=%d", jsonOut, n), func(t *testing.T) {
				t.Setenv("HRN_HOME", t.TempDir())
				w, err := runlog.Create("stub-context", agent.Task{Prompt: "first", Dir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub-context"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(agent.Result{Backend: "stub-context", Status: agent.StatusOK, ContextTokens: n}); err != nil {
					t.Fatal(err)
				}
				capture, err := os.CreateTemp(t.TempDir(), "stderr")
				if err != nil {
					t.Fatal(err)
				}
				defer capture.Close()
				prev := os.Stderr
				os.Stderr = capture
				defer func() { os.Stderr = prev }()
				args := []string{"--continue", w.ID(), "-b", "stub-context", "--no-switch"}
				if jsonOut {
					args = append(args, "--json")
				}
				code, err := cmdRun(append(args, "again"))
				if code != 0 || err != nil {
					t.Fatalf("continuation refused: code=%d error=%v", code, err)
				}
				data, err := os.ReadFile(capture.Name())
				if err != nil {
					t.Fatal(err)
				}
				got := string(data)
				start := strings.Index(got, "context test backend started")
				if start < 0 {
					t.Fatal("backend never started")
				}
				if n > 150000 {
					for _, want := range []string{"warning:", w.ID(), fmt.Sprintf("%d tokens", n), "fresh run", "short hand-over"} {
						if !strings.Contains(got[:start], want) {
							t.Errorf("stderr before start lacks %q: %q", want, got[:start])
						}
					}
				} else if strings.Contains(got, "short hand-over") {
					t.Errorf("unneeded warning: %q", got)
				}
			})
		}
	}
}

// recorder answers immediately and keeps the task it was given, so a test can
// see what the flags actually produced.
type recorder struct {
	name string
	last chan agent.Task
}

func (r recorder) Name() string            { return r.name }
func (r recorder) Detect() (string, error) { return "built-in stub", nil }
func (r recorder) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	select {
	case r.last <- task:
	default:
	}
	return agent.Result{Backend: r.name, Status: agent.StatusOK, Final: "done", Turns: 1}
}

var recorded = make(chan agent.Task, 4)

// A second backend, for continuing a run somewhere else.
func init() {
	agent.Register(recorder{"stub-cmd", recorded})
	agent.Register(recorder{"stub-cmd-b", recorded})
}

// drain empties what earlier tests left, so a test never reads their task.
func drain(t *testing.T) {
	t.Helper()
	for {
		select {
		case <-recorded:
		default:
			return
		}
	}
}

func TestRunRefusesAnUnknownEffortBeforeTheBackendStarts(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	code, err := cmdRun([]string{"-b", "stub-cmd", "-C", t.TempDir(), "--effort", "turbo", "do it"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), `unknown effort "turbo"`) {
		t.Fatalf("code=%d err=%v, want 2 and an unknown effort error", code, err)
	}
	select {
	case task := <-recorded:
		t.Fatalf("the backend ran with %+v", task)
	default:
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("a refused run left records: %v err=%v", recs, err)
	}
}

func TestRunFlagsReachTheTask(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	code, err := cmdRun([]string{
		"-b", "stub-cmd", "-C", dir, "--mode", "auto", "-m", "some-model",
		"--effort", "high", "--max-turns", "7", "--no-instructions", "--no-context-edits", "--lean",
		"--timeout", "90s", "--max-cost", "1.5", "--task-budget", "30000",
		"hello", "world",
	})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var task agent.Task
	select {
	case task = <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend was never run")
	}
	if task.Prompt != "hello world" {
		t.Errorf("prompt = %q", task.Prompt)
	}
	for name, ok := range map[string]bool{
		"mode":             task.Mode == agent.ModeAuto,
		"model":            task.Model == "some-model",
		"effort":           task.Effort == "high",
		"max-turns":        task.MaxTurns == 7,
		"no-instructions":  task.NoInstructions,
		"no-context-edits": task.NoContextEdits,
		"lean":             task.Lean,
		"timeout":          task.Timeout == agent.Duration(90*time.Second),
		"max-cost":         task.MaxCostUSD == 1.5,
		"task-budget":      task.TaskBudget == 30000,
		"run-dir":          task.RunDir != "",
	} {
		if !ok {
			t.Errorf("--%s did not reach the task: %+v", name, task)
		}
	}
	if _, err := os.Stat(task.Dir); err != nil {
		t.Errorf("workspace %q: %v", task.Dir, err)
	}
	if recs, err := runlog.List(1); err != nil || len(recs) != 1 || recs[0].Task.Prompt != "hello world" {
		t.Errorf("the run should be recorded: %+v err=%v", recs, err)
	}
}

func TestRunRejectsBadFlagCombinations(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"budget below the minimum", []string{"-b", "stub-cmd", "-C", dir, "--task-budget", "100", "x"}, "20000"},
		{"negative cost", []string{"-b", "stub-cmd", "-C", dir, "--max-cost", "-1", "x"}, "negative"},
		{"missing workspace", []string{"-b", "stub-cmd", "-C", dir + "-nope", "x"}, "-C "},
		{"unknown backend", []string{"-b", "nope", "-C", dir, "x"}, "unknown backend"},
		{"continue a path-like id", []string{"-b", "stub-cmd", "-C", dir, "--continue", "../escape", "x"}, "invalid run id"},
		{"continue a run that is not there", []string{"-b", "stub-cmd", "-C", dir, "--continue", "20200101-000000-nope", "x"}, "20200101-000000-nope"},
	} {
		code, err := cmdRun(tc.args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: code=%d err=%v, want 2 and a message about %q", tc.name, code, err, tc.want)
		}
	}
}

// The other commands parse their own flags the same way, so the cheap half of
// that wiring, what they refuse, is worth a test now that the first one exists.
func TestCommandsRefuseBadArguments(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope.json")
	for _, tc := range []struct {
		name string
		run  func() (int, error)
		want string
	}{
		{"eval without a spec", func() (int, error) { return cmdEval(nil) }, "usage"},
		{"eval show without an id", func() (int, error) { return cmdEval([]string{"show"}) }, "usage"},
		{"eval with a missing spec", func() (int, error) { return cmdEval([]string{missing}) }, "nope.json"},
		{"batch without a spec", func() (int, error) { return cmdBatch(nil) }, "usage"},
		{"batch rm without an id", func() (int, error) { return cmdBatch([]string{"rm"}) }, "usage"},
		{"show without an id", func() (int, error) { return 0, cmdShow(nil) }, "usage"},
	} {
		code, err := tc.run()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
			t.Errorf("%s: code=%d err=%v, want a message about %q", tc.name, code, err, tc.want)
		}
	}
}

func TestContinueOnAnotherBackendInheritsWhatStillApplies(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	if code, err := cmdRun([]string{"-b", "stub-cmd", "-C", dir, "--mode", "auto", "-m", "stub-model", "first"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	<-recorded
	recs, err := runlog.List(1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}
	first := recs[0].ID

	// Nothing typed but the backend: workspace and mode carry over, the model
	// does not, and the backend is told what the first run did.
	if code, err := cmdRun([]string{"--continue", first, "-b", "stub-cmd-b", "second"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	task := <-recorded
	if task.Mode != agent.ModeAuto || task.Model != "" || task.Dir != recs[0].Task.Dir {
		t.Errorf("mode=%s model=%q dir=%q", task.Mode, task.Model, task.Dir)
	}
	if !strings.Contains(task.Prompt, "--- request\n> first\n--- answer\n> done") || !strings.HasSuffix(task.Prompt, "second") {
		t.Errorf("prompt:\n%s", task.Prompt)
	}

	// The stub saves no session, so the same backend has nothing to resume.
	if code, err := cmdRun([]string{"--continue", first, "x"}); code != 2 || err == nil || !strings.Contains(err.Error(), "no saved conversation") {
		t.Errorf("code=%d err=%v", code, err)
	}
}
