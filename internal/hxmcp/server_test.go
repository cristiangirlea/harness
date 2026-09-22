package hxmcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type stubBackend struct{}

func (stubBackend) Name() string            { return "stub" }
func (stubBackend) Detect() (string, error) { return "built-in stub", nil }
func (stubBackend) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "read_file", Text: "main.go"})
	emit(agent.Event{Kind: agent.EventToolResult, Tool: "read_file", Text: "package main"})
	if task.Prompt == "fail" {
		emit(agent.Event{Kind: agent.EventError, Text: "asked to fail"})
		return agent.Result{Backend: "stub", Status: agent.StatusError, Error: "asked to fail", Turns: 1}
	}
	emit(agent.Event{Kind: agent.EventText, Text: "stub says: " + task.Prompt})
	return agent.Result{Backend: "stub", Model: "stub-1", Status: agent.StatusOK, Final: "stub says: " + task.Prompt, Turns: 1, InputTokens: 10, OutputTokens: 5}
}

func init() { agent.Register(stubBackend{}) }

type session struct {
	cl       *mcp.Client
	mu       sync.Mutex
	progress []string
}

func connect(t *testing.T, defaultDir string) *session {
	t.Helper()
	t.Setenv("HX_HOME", t.TempDir())
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(defaultDir).Serve(ctx, toServer, serverOut) }()
	t.Cleanup(func() { cancel(); clientOut.Close(); serverOut.Close(); <-done })
	s := &session{}
	s.cl = &mcp.Client{Progress: func(m string) { s.mu.Lock(); s.progress = append(s.progress, m); s.mu.Unlock() }}
	if err := s.cl.Connect(ctx, toClient, clientOut); err != nil {
		t.Fatal(err)
	}
	return s
}

func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func (s *session) call(t *testing.T, name, args string) mcp.CallResult {
	t.Helper()
	res, err := s.cl.CallTool(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestRunThenInspect(t *testing.T) {
	dir := t.TempDir()
	s := connect(t, dir)
	if names := s.cl.Tools(); len(names) != 4 || names[0].Name != "run" || !names[1].Annotations.ReadOnly() {
		t.Fatalf("tools = %+v", names)
	}

	res := s.call(t, "run", `{"prompt":"hello","backend":"stub","mode":"auto"}`)
	text := res.Text()
	if res.IsError || !strings.Contains(text, "stub says: hello") || !strings.Contains(text, "── ok · stub · stub-1 · 1 turns") {
		t.Fatalf("run result: %+v", res)
	}
	sc := res.StructuredContent.(map[string]any)
	id, _ := sc["run_id"].(string)
	if id == "" {
		t.Fatalf("no run_id: %v", sc)
	}
	s.mu.Lock()
	progress := strings.Join(s.progress, "|")
	s.mu.Unlock()
	if progress != "→ read_file main.go" {
		t.Errorf("progress = %q", progress)
	}

	rec, err := runlog.Load(id)
	if err != nil || rec.Task.Mode != agent.ModeAuto || rec.Result.Status != agent.StatusOK {
		t.Fatalf("record: %+v err=%v", rec, err)
	}
	// The workspace is canonicalised, and a Windows temp path arrives as an 8.3
	// short name, so compare the directories themselves, not the strings.
	if !sameDir(rec.Task.Dir, dir) {
		t.Errorf("record dir = %s, want %s", rec.Task.Dir, dir)
	}

	if text := s.call(t, "runs", `{}`).Text(); !strings.Contains(text, id) || !strings.Contains(text, "stub") {
		t.Errorf("runs = %q", text)
	}
	text = s.call(t, "show", `{"run_id":"`+id+`"}`).Text()
	for _, want := range []string{"hello", "→ read_file main.go", "✓ package main", "── ok"} {
		if !strings.Contains(text, want) {
			t.Errorf("show missing %q:\n%s", want, text)
		}
	}
	if text := s.call(t, "backends", `{}`).Text(); !strings.Contains(text, "✓ stub") {
		t.Errorf("backends = %q", text)
	}
}

func TestRunErrors(t *testing.T) {
	s := connect(t, t.TempDir())
	for _, tc := range []struct{ args, want string }{
		{`{}`, "prompt is required"},
		{`{"prompt":"x","backend":"stub","dir":"/definitely/not/here"}`, "dir /definitely/not/here"},
		{`{"prompt":"x","backend":"nope"}`, "unknown backend"},
		{`{"prompt":"x","backend":"stub","mode":"weird"}`, "unknown mode"},
	} {
		res := s.call(t, "run", tc.args)
		if !res.IsError || !strings.Contains(res.Text(), tc.want) {
			t.Errorf("run %s: want error containing %q, got %+v", tc.args, tc.want, res)
		}
	}

	res := s.call(t, "run", `{"prompt":"fail","backend":"stub"}`)
	if !res.IsError || !strings.Contains(res.Text(), "status: error") || !strings.Contains(res.Text(), "asked to fail") {
		t.Errorf("failed run should be flagged: %+v", res)
	}
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || rec.Task.Mode != agent.ModeReadOnly {
		t.Errorf("default mode should be readonly: %+v err=%v", rec, err)
	}

	if res := s.call(t, "show", `{"run_id":"../etc"}`); !res.IsError {
		t.Error("show must reject path-like ids")
	}
}

func TestRunPassesNoInstructions(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub","no_instructions":true}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || !rec.Task.NoInstructions {
		t.Fatalf("no_instructions should reach the task: %+v err=%v", rec.Task, err)
	}
}

func TestRunPassesLimits(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub","timeout":"90s","max_cost_usd":1.5,"task_budget":30000}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, err := runlog.Load(id)
	if err != nil || rec.Task.Timeout != 90*time.Second || rec.Task.MaxCostUSD != 1.5 || rec.Task.TaskBudget != 30000 {
		t.Fatalf("limits should reach the task: %+v err=%v", rec.Task, err)
	}
	for _, args := range []string{
		`{"prompt":"x","backend":"stub","timeout":"soon"}`,
		`{"prompt":"x","backend":"stub","task_budget":100}`,
	} {
		if r := s.call(t, "run", args); !r.IsError {
			t.Errorf("%s should be rejected", args)
		}
	}
}

func TestRunPassesNoContextEdits(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub","no_context_edits":true}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || !rec.Task.NoContextEdits {
		t.Fatalf("no_context_edits should reach the task: %+v err=%v", rec.Task, err)
	}
}

func TestRunContinueRejectsWhatItCannot(t *testing.T) {
	s := connect(t, t.TempDir())
	if r := s.call(t, "run", `{"prompt":"x","continue_run_id":"nope"}`); !r.IsError || !strings.Contains(r.Text(), "nope") {
		t.Errorf("an unknown run id should be reported: %+v", r)
	}
	// A stub run saves no conversation, so continuing it is refused clearly.
	res := s.call(t, "run", `{"prompt":"x","backend":"stub"}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	r := s.call(t, "run", `{"prompt":"and now?","continue_run_id":"`+id+`"}`)
	if !r.IsError || !strings.Contains(r.Text(), "no saved conversation") {
		t.Errorf("continuing a run without a conversation: %+v", r)
	}
}
