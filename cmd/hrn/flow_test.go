package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestFlowRunTextAndJSON(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	w, err := runlog.Create("claude", agent.Task{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "go test"})
	w.Event(agent.Event{Kind: agent.EventToolResult, Tool: "shell", Text: "failed", IsError: true})
	w.Event(agent.Event{Kind: agent.EventError, Text: "backend stopped"})
	if err := w.Close(agent.Result{Status: agent.StatusError, Error: "backend stopped", Duration: time.Second}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdFlow([]string{w.ID()}) })
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.HasPrefix(text, "run "+w.ID()+": failed") || !strings.Contains(text, "tool shell") || !strings.Contains(text, "error") || strings.Contains(text, "thinking") {
		t.Fatalf("flow text = %s", text)
	}
	out, err = captureStdout(t, func() error { return cmdFlow([]string{"--json", w.ID()}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int          `json:"version"`
		Events  []flow.Event `json:"events"`
		Tree    []*flow.Node `json:"tree"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Version != 1 || len(doc.Events) == 0 || len(doc.Tree) == 0 {
		t.Fatalf("json = %s (%v)", out, err)
	}
	out, err = captureStdout(t, func() error { return cmdFlow([]string{w.ID(), "--level", "simple"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tool shell") {
		t.Fatalf("simple output shows tool: %s", out)
	}
}

func TestFlowBatchSummaryNamesFailedGateAfterRetry(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	res := batch.Result{ID: "b1", Owner: "local:one", Started: time.Now(), Tasks: []batch.TaskResult{{ID: "task-a", Status: batch.StatusFail, Attempts: []batch.Attempt{
		{Backend: "claude", Result: agent.Result{Status: agent.StatusError}},
		{Backend: "codex", Result: agent.Result{Status: agent.StatusError}, Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1, Output: "FAIL"}}},
	}}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdFlow([]string{"b1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "1 failed") || !strings.Contains(string(out), "go test ./...") || !strings.Contains(string(out), "exited 1") {
		t.Fatalf("output = %s", out)
	}
}

func TestFlowHidesMissingAndForeignRecords(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	for _, id := range []string{"missing", "../secret"} {
		if err := cmdFlow([]string{id}); err == nil || !strings.Contains(err.Error(), "hrn flow: no run or batch "+id) {
			t.Errorf("id %q error = %v", id, err)
		}
	}
	w, err := runlog.Create("claude", agent.Task{Prompt: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_OWNER", "local:two")
	if err := cmdFlow([]string{w.ID()}); err == nil || !strings.Contains(err.Error(), "hrn flow: no run or batch "+w.ID()) {
		t.Fatalf("foreign run error = %v", err)
	}
}

func TestFlowPrintsNoTerminalControlsOrSecretsFromRecords(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("codex", agent.Task{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "echo \x1b]52;c;cGF5bG9hZA==\x07 done"})
	if err := w.Close(agent.Result{Status: agent.StatusError, Error: "token=ghp_abcdefghijklmnop1234 \x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdFlow([]string{w.ID()}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(string(out), 0x1b) || strings.Contains(string(out), "ghp_abcdefghijklmnop1234") {
		t.Fatalf("flow printed a control sequence or a secret: %q", out)
	}
}

func TestFlowJSONCleansAStoredFlowFileAgain(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := batch.Result{ID: "20261020-150000-aabbcc", Started: time.Now(), Tasks: []batch.TaskResult{{ID: "t", Status: batch.StatusPass}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	line := `{"v":1,"seq":1,"step":"batch","kind":"batch","text":"token ghp_abcdefghijklmnop1234"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(batch.FlowPath(res.ID)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batch.FlowPath(res.ID), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdFlow([]string{"--json", res.ID}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ghp_abcdefghijklmnop1234") {
		t.Fatalf("--json printed a stored secret: %s", out)
	}
}

func TestFlowDebugPrintsNoControlsFromStepIds(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := batch.Result{ID: "20261021-120000-aabbcc", Started: time.Now(), Tasks: []batch.TaskResult{{ID: "t", Status: batch.StatusPass}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	line := `{"v":1,"seq":1,"step":"batch\u001b]0;owned\u0007","kind":"batch","run":"r\u001b[2J"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(batch.FlowPath(res.ID)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batch.FlowPath(res.ID), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdFlow([]string{"--level", "debug", res.ID}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(string(out), 0x1b) || strings.ContainsRune(string(out), 0x07) {
		t.Fatalf("debug output carried a control character: %q", out)
	}
}
