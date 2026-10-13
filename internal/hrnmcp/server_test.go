package hrnmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type stubBackend struct{}

func (stubBackend) Name() string            { return "stub" }
func (stubBackend) Detect() (string, error) { return "built-in stub", nil }
func (stubBackend) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	if task.Prompt == "record-write-failure" {
		// Invalid tool input forces event encoding to fail without a real backend.
		for range 2 {
			emit(agent.Event{Kind: agent.EventToolCall, Input: json.RawMessage(`{`)})
		}
	}
	if task.Prompt == "result-write-failure" {
		if err := os.Mkdir(filepath.Join(task.RunDir, "result.json"), 0o755); err != nil {
			return agent.Result{Backend: "stub", Status: agent.StatusError, Error: err.Error()}
		}
	}
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "read_file", Text: "main.go"})
	emit(agent.Event{Kind: agent.EventToolResult, Tool: "read_file", Text: "package main"})
	if task.Prompt == "fail" {
		emit(agent.Event{Kind: agent.EventError, Text: "asked to fail"})
		return agent.Result{Backend: "stub", Status: agent.StatusError, Error: "asked to fail", Turns: 1}
	}
	emit(agent.Event{Kind: agent.EventText, Text: "stub says: " + task.Prompt})
	return agent.Result{Backend: "stub", Model: "stub-1", Status: agent.StatusOK, Final: "stub says: " + task.Prompt, Turns: 1, InputTokens: 10, OutputTokens: 5}
}

// leanBackend is a stub that applies lean, as claude does, so a test can tell
// whether a caller turned lean on by default.
type leanBackend struct{ stubBackend }

func (leanBackend) Name() string      { return "stub-lean" }
func (leanBackend) AppliesLean() bool { return true }

func init() { agent.Register(stubBackend{}); agent.Register(leanBackend{}) }

type session struct {
	cl       *mcp.Client
	mu       sync.Mutex
	progress []string
}

func connect(t *testing.T, defaultDir string) *session {
	t.Helper()
	return connectTo(t, NewServer(defaultDir))
}

// connectDangerous is connect to a server the operator started with
// --allow-dangerous-runs.
func connectDangerous(t *testing.T, defaultDir string) *session {
	t.Helper()
	return connectTo(t, NewDangerousServer(defaultDir))
}

func connectTo(t *testing.T, srv *mcp.Server) *session {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, toServer, serverOut) }()
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
	if names := s.cl.Tools(); len(names) != 7 || names[0].Name != "run" || !names[1].Annotations.ReadOnly() {
		t.Fatalf("tools = %+v", names)
	}

	res := s.call(t, "run", `{"prompt":"hello","backend":"stub","mode":"auto"}`)
	text := res.Text()
	if res.IsError || !strings.Contains(text, "stub says: hello") || !strings.Contains(text, "── ok · stub · stub-1 · 1 turns") {
		t.Fatalf("run result: %+v", res)
	}
	sc := res.StructuredContent.(map[string]any)
	if _, ok := sc["record_error"]; ok {
		t.Errorf("successful run must omit record_error: %v", sc)
	}
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

func TestRunReturnsRecordErrorsToTheHost(t *testing.T) {
	for _, tc := range []struct {
		prompt, prefix, want string
		isError              bool
	}{
		{"record-write-failure", "run record incomplete: ", "2 events were not recorded", false},
		{"result-write-failure", "saving run: ", "result.json", true},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			s := connect(t, t.TempDir())
			res := s.call(t, "run", `{"prompt":"`+tc.prompt+`","backend":"stub"}`)
			if res.IsError != tc.isError {
				t.Errorf("isError = %v, want %v: lost events leave an ok run ok, an unsaved result does not", res.IsError, tc.isError)
			}
			if !strings.Contains(res.Text(), tc.prefix) || !strings.Contains(res.Text(), tc.want) || !strings.Contains(res.Text(), "stub says: "+tc.prompt) {
				t.Errorf("run must return its answer and record error: %+v", res)
			}
			sc := res.StructuredContent.(map[string]any)
			recordError, _ := sc["record_error"].(string)
			if !strings.Contains(recordError, tc.want) {
				t.Errorf("structured result must carry the record error: %v", sc)
			}
			if result := sc["result"].(map[string]any); result["status"] != "ok" {
				t.Errorf("record failure must preserve the backend's result: %v", result)
			}
			if tc.prompt == "record-write-failure" {
				if rec, err := runlog.Load(sc["run_id"].(string)); err != nil || !rec.Done {
					t.Errorf("failed event recording must still finish the record: %+v, err=%v", rec, err)
				}
			}
		})
	}
}

func TestBatchIncludesRecordErrorsInTheRunResultLine(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := gitRepo(t)
	var lines []string
	res, err := batch.Run(context.Background(), batch.Spec{
		Dir: dir, Backend: "stub", Tasks: []batch.Task{{ID: "record", Prompt: "record-write-failure"}},
	}, func(e batch.Event) {
		if e.Kind == "run" {
			lines = append(lines, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "run record incomplete: 2 events were not recorded") {
		t.Errorf("run result line must carry Close's error: %q", lines)
	}
	if len(res.Tasks) != 1 || len(res.Tasks[0].Attempts) != 1 || !strings.Contains(res.Tasks[0].Attempts[0].Error, "2 events were not recorded") {
		t.Errorf("attempt must retain Close's error: %+v", res.Tasks)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Status != batch.StatusPass {
		t.Errorf("an ok agent must still pass when its record fails: %+v", res.Tasks)
	}
}

func TestShowIncludesTheSavedRecordError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"record-write-failure","backend":"stub"}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, err := runlog.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	// A marked incomplete record must remain inspectable despite a broken line.
	if err := os.WriteFile(filepath.Join(runlog.Root(), id, "events.jsonl"), []byte("{broken\n{\"kind\":\"text\",\"text\":\"recovered\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, events := range []string{"true", "false"} {
		shown := s.call(t, "show", `{"run_id":"`+id+`","events":`+events+`}`)
		if shown.IsError || !strings.Contains(shown.Text(), "run record incomplete: "+rec.RecordError) {
			t.Errorf("show must report the saved error, events=%s: %+v", events, shown)
		}
		if events == "true" && !strings.Contains(shown.Text(), "recovered") {
			t.Errorf("show must include intact events after a broken line: %+v", shown)
		}
	}
}

func TestRunErrors(t *testing.T) {
	s := connect(t, t.TempDir())
	for _, tc := range []struct{ args, want string }{
		{`{}`, "prompt is required"},
		{`{"prompt":"x","backend":"stub","dir":"/definitely/not/here"}`, "dir /definitely/not/here"},
		{`{"prompt":"x","backend":"nope"}`, "unknown backend"},
		{`{"prompt":"x","backend":"stub","mode":"weird"}`, "unknown mode"},
		{`{"prompt":"x","backend":"stub","effort":"turbo"}`, `unknown effort "turbo"`},
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

func TestRunPassesAllowDangerous(t *testing.T) {
	s := connectDangerous(t, t.TempDir())
	for _, tc := range []struct {
		args string
		want bool
	}{
		{`{"prompt":"x","backend":"stub"}`, false},
		{`{"prompt":"x","backend":"stub","allow_dangerous":true}`, true},
	} {
		res := s.call(t, "run", tc.args)
		id := res.StructuredContent.(map[string]any)["run_id"].(string)
		if rec, err := runlog.Load(id); err != nil || rec.Task.AllowDangerous != tc.want {
			t.Errorf("%s: AllowDangerous = %v, want %v (err=%v)", tc.args, rec.Task.AllowDangerous, tc.want, err)
		}
	}
}

func TestRunPassesLimits(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub","timeout":"90s","max_cost_usd":1.5,"task_budget":30000}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, err := runlog.Load(id)
	if err != nil || rec.Task.Timeout != agent.Duration(90*time.Second) || rec.Task.MaxCostUSD != 1.5 || rec.Task.TaskBudget != 30000 {
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

func TestRunPassesLean(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub-lean","lean":true}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || !rec.Task.IsLean() {
		t.Fatalf("lean should reach the task: %+v err=%v", rec.Task, err)
	}
	// Like hrn run, the tool is not lean unless asked.
	res = s.call(t, "run", `{"prompt":"x","backend":"stub-lean"}`)
	id = res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || rec.Task.Lean != nil {
		t.Fatalf("lean must stay unset: %+v err=%v", rec.Task, err)
	}
}

func TestRunPassesCompactWindow(t *testing.T) {
	s := connect(t, t.TempDir())
	res := s.call(t, "run", `{"prompt":"x","backend":"stub","compact_window":200000}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || rec.Task.CompactWindow != "200000" {
		t.Fatalf("compact_window should reach the task: %+v err=%v", rec.Task, err)
	}
	if r := s.call(t, "run", `{"prompt":"x","backend":"stub","compact_window":50000}`); !r.IsError || !strings.Contains(r.Text(), "compact_window") {
		t.Errorf("an out-of-range window must be refused before the run: %s", r.Text())
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

// askingBackend asks for permission once and reports what it was told.
type askingBackend struct{}

func (askingBackend) Name() string            { return "stub-ask" }
func (askingBackend) Detect() (string, error) { return "built-in stub", nil }
func (askingBackend) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	answer := "denied"
	if task.Approve != nil && task.Approve(agent.ApprovalRequest{Tool: "write_file", Summary: "notes.txt"}) {
		answer = "allowed"
	}
	return agent.Result{Backend: "stub-ask", Status: agent.StatusOK, Final: answer, Turns: 1}
}

func init() { agent.Register(askingBackend{}) }

// answering connects a session whose client answers every approval the same way.
func answering(t *testing.T, reply map[string]any) *session {
	return answeringWith(t, reply, nil)
}

// answeringWith also hands each elicitation's params to watch, so a test can
// assert what the person was actually asked.
func answeringWith(t *testing.T, reply map[string]any, watch func(json.RawMessage)) *session {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(t.TempDir()).Serve(ctx, toServer, serverOut) }()
	t.Cleanup(func() { cancel(); clientOut.Close(); serverOut.Close(); <-done })

	s := &session{}
	s.cl = &mcp.Client{Progress: func(m string) { s.mu.Lock(); s.progress = append(s.progress, m); s.mu.Unlock() }}
	if reply != nil {
		s.cl.Elicit = func(_ context.Context, params json.RawMessage) (any, error) {
			if watch != nil {
				watch(params)
			}
			return reply, nil
		}
	}
	if err := s.cl.Connect(ctx, toClient, clientOut); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAskModeGoesThroughTheHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply map[string]any
		want  string
	}{
		{"accepted", map[string]any{"action": "accept", "content": map[string]any{"approve": true}}, "allowed"},
		{"the box left unticked", map[string]any{"action": "accept", "content": map[string]any{"approve": false}}, "denied"},
		{"declined", map[string]any{"action": "decline"}, "denied"},
		{"host cannot ask", nil, "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := answering(t, tc.reply)
			res := s.call(t, "run", `{"prompt":"x","backend":"stub-ask","mode":"ask"}`)
			if got := res.Text(); !strings.Contains(got, tc.want) {
				t.Errorf("run said %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestApprovalPromptSaysWhatIsBeingApproved(t *testing.T) {
	var asked string
	s := answeringWith(t, map[string]any{"action": "accept", "content": map[string]any{"approve": true}},
		func(params json.RawMessage) { asked = string(params) })
	res := s.call(t, "run", `{"prompt":"x","backend":"stub-ask","mode":"ask"}`)
	id := res.StructuredContent.(map[string]any)["run_id"].(string)

	for _, want := range []string{"write_file", "notes.txt", id, `"approve"`, `"required"`} {
		if !strings.Contains(asked, want) {
			t.Errorf("the prompt must carry %q:\n%s", want, asked)
		}
	}
	s.mu.Lock()
	progress := strings.Join(s.progress, "|")
	s.mu.Unlock()
	if !strings.Contains(progress, "allowed write_file") {
		t.Errorf("the decision should reach the host: %q", progress)
	}
}

func TestRunContinueHandsTheConversationToAnotherBackend(t *testing.T) {
	s := connect(t, t.TempDir())
	first := s.call(t, "run", `{"prompt":"may I write?","backend":"stub-ask","mode":"auto"}`)
	id := first.StructuredContent.(map[string]any)["run_id"].(string)

	// stub echoes its prompt, so the answer shows what the backend was given.
	r := s.call(t, "run", `{"prompt":"and now?","backend":"stub","continue_run_id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("continue on another backend: %s", r.Text())
	}
	for _, want := range []string{"--- request\n> may I write?", "--- answer\n> denied", "=== now\n\nand now?"} {
		if !strings.Contains(r.Text(), want) {
			t.Errorf("the backend was not given %q:\n%s", want, r.Text())
		}
	}
	next := r.StructuredContent.(map[string]any)["run_id"].(string)
	rec, err := runlog.Load(next)
	if err != nil || rec.Continues != id || rec.Task.Prompt != "and now?" {
		t.Errorf("the record should keep the prompt as sent by the caller and link the chain: %+v err=%v", rec, err)
	}
	if rec.Task.Mode != agent.ModeAuto {
		t.Errorf("mode = %s, want auto inherited from the earlier run rather than the readonly default", rec.Task.Mode)
	}
	if _, err := os.Stat(filepath.Join(runlog.Root(), next, "handoff.md")); err != nil {
		t.Errorf("handoff.md: %v", err)
	}
	// A mode named in the call still wins.
	r = s.call(t, "run", `{"prompt":"look only","backend":"stub","mode":"readonly","continue_run_id":"`+id+`"}`)
	if rec, err := runlog.Load(r.StructuredContent.(map[string]any)["run_id"].(string)); err != nil || rec.Task.Mode != agent.ModeReadOnly {
		t.Errorf("an explicit mode should override the inherited one: %+v err=%v", rec.Task, err)
	}
}

func TestContinueIncludesAContextWarningInItsAnswerWithoutRefusing(t *testing.T) {
	for _, n := range []int64{0, 149999, 150000, 150001, 285000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := t.TempDir()
			s := connect(t, dir)
			w, err := runlog.Create("stub", agent.Task{Prompt: "first", Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub-1"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: "stub", Status: agent.StatusOK, ContextTokens: n}); err != nil {
				t.Fatal(err)
			}
			r := s.call(t, "run", `{"prompt":"again","backend":"stub","no_switch":true,"continue_run_id":"`+w.ID()+`"}`)
			if r.IsError {
				t.Fatalf("continuation refused: %s", r.Text())
			}
			if n > 150000 {
				for _, want := range []string{"warning:", w.ID(), fmt.Sprintf("%d tokens", n), "fresh run", "short hand-over"} {
					if !strings.Contains(r.Text(), want) {
						t.Errorf("answer lacks %q: %s", want, r.Text())
					}
				}
			} else if strings.Contains(r.Text(), "short hand-over") {
				t.Errorf("unneeded warning: %s", r.Text())
			}
		})
	}
}
