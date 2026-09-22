package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// recorder answers immediately and keeps the task it was given, so a test can
// see what the flags actually produced.
type recorder struct{ last chan agent.Task }

func (r recorder) Name() string            { return "stub-cmd" }
func (r recorder) Detect() (string, error) { return "built-in stub", nil }
func (r recorder) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	select {
	case r.last <- task:
	default:
	}
	return agent.Result{Backend: "stub-cmd", Status: agent.StatusOK, Final: "done", Turns: 1}
}

var recorded = make(chan agent.Task, 4)

func init() { agent.Register(recorder{recorded}) }

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

func TestRunFlagsReachTheTask(t *testing.T) {
	drain(t)
	t.Setenv("HX_HOME", t.TempDir())
	dir := t.TempDir()
	code, err := cmdRun([]string{
		"-b", "stub-cmd", "-C", dir, "--mode", "auto", "-m", "some-model",
		"--effort", "high", "--max-turns", "7", "--no-instructions", "--no-context-edits",
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
		"timeout":          task.Timeout == 90*time.Second,
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
	t.Setenv("HX_HOME", t.TempDir())
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
	t.Setenv("HX_HOME", t.TempDir())
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
