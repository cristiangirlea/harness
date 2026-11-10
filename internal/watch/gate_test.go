package watch

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

const shapedResult = "summary\noriginal: 0123456789abcdef (40960 bytes); read_original reads it\n[exit code 0]"

func gatedRun(t *testing.T, backend, tool string, gate bool) *runlog.Writer {
	t.Helper()
	task := agent.Task{Prompt: "p", Dir: "/repo"}
	task.Gate, task.ShapeTools = gate, true
	w, err := runlog.Create(backend, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := "out\n[exit code 3]\nexit code 3"
	if backend == "codex" {
		failed = "out\n[exit code 3]" // the gate's tool server adds no error line that the output holds
	}
	for _, text := range []string{"hi\n\n[exit code 0]", failed, tools.GuardError("gh-publish").Error(), shapedResult} {
		w.Event(agent.Event{Kind: agent.EventToolCall, Tool: tool, Text: "echo hi"})
		w.Event(agent.Event{Kind: agent.EventToolResult, Tool: tool, Text: text})
	}
	return w
}

func runPageBody(t *testing.T, srv *httptest.Server, id string) string {
	t.Helper()
	code, body := get(t, srv.URL+"/run/"+id)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	return body
}

func TestAGatedRunShowsTheGatesRecordOfEachCommand(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for backend, tool := range map[string]string{"api": "run_command", "codex": "shell"} {
		w := gatedRun(t, backend, tool, true)
		if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK}); err != nil {
			t.Fatal(err)
		}
		body := runPageBody(t, srv, w.ID())
		for _, want := range []string{"gate: allowed · exit 0", "gate: allowed · exit 3", "guard: refused · gh-publish",
			"through adanext-exec · audit run_id " + w.ID()} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the run page lacks %q", backend, want)
			}
		}
		if strings.Contains(body, "shaped from") {
			t.Errorf("%s: a shaped size is shown for an original the run does not hold", backend)
		}
		dir := filepath.Join(runlog.Root(), w.ID(), "originals")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0123456789abcdef.txt"), make([]byte, 40960), 0o600); err != nil {
			t.Fatal(err)
		}
		if body := runPageBody(t, srv, w.ID()); !strings.Contains(body, "gate: allowed · exit 0 · shaped from 40.0 KB") {
			t.Errorf("%s: the run page lacks the shaped size of an original the run holds", backend)
		}
	}
}

func TestARunThatIsNotRoutedIsMarkedUngated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	w := gatedRun(t, "api", "run_command", false)
	if err := w.Close(agent.Result{Backend: "api", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	body := runPageBody(t, srv, w.ID())
	if !strings.Contains(body, "<dd>ungated</dd>") {
		t.Error("an ungated run's header does not say so")
	}
	if strings.Contains(body, "gate: ") || strings.Contains(body, "audit run_id") {
		t.Error("an ungated run shows a gate record it does not have")
	}
}

func TestAGatedClaudeRunLinksItsAuditBySessionAndClaimsNoAnswerItDidNotRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	w := gatedRun(t, "claude", "Bash", true)
	if err := w.Close(agent.Result{Backend: "claude", Status: agent.StatusOK, Session: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	body := runPageBody(t, srv, w.ID())
	if !strings.Contains(body, "through adanext-exec · audit session sess-1") {
		t.Error("the claude run's header lacks its audit session")
	}
	if strings.Contains(body, "gate: allowed") || strings.Contains(body, "audit run_id") {
		t.Error("the claude run shows a gate answer its record does not hold")
	}
}

func TestAGatedRunHrnStoppedSaysACommandMayHaveRunUngated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	w := gatedRun(t, "claude", "Bash", true)
	if err := w.Close(agent.Result{Backend: "claude", Status: agent.StatusError,
		Error: agent.GateStopped + "a Bash call came without the gate's answer, so its command may have run ungated"}); err != nil {
		t.Fatal(err)
	}
	if body := runPageBody(t, srv, w.ID()); !strings.Contains(body, "through adanext-exec · stopped: a command may have run ungated") {
		t.Error("the header of a stopped gated run does not say a command may have run ungated")
	}
}

func TestTheRunStreamCarriesTheGatesRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	w := gatedRun(t, "api", "run_command", true)
	defer w.Close(agent.Result{Backend: "api", Status: agent.StatusOK})
	s, stop := openStream(t, srv.URL+"/stream/run/"+w.ID(), "")
	defer stop()
	for range 8 {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("stream ended early")
		}
		if strings.Contains(m.data, "guard: refused · gh-publish") {
			return
		}
	}
	t.Fatal("the stream lacks the gate's record of a command")
}

func TestAnOriginalLineIsNotAShapedSizeOnARunThatDidNotShape(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	task := agent.Task{Prompt: "p", Dir: "/repo"}
	task.Gate = true
	w, err := runlog.Create("api", task, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolResult, Tool: "run_command", Text: shapedResult})
	if err := w.Close(agent.Result{Backend: "api", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runlog.Root(), w.ID(), "originals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123456789abcdef.txt"), make([]byte, 40960), 0o600); err != nil {
		t.Fatal(err)
	}
	body := runPageBody(t, srv, w.ID())
	if !strings.Contains(body, "gate: allowed · exit 0") || strings.Contains(body, "shaped from") {
		t.Error("a run without shape_tools shows a shaped size")
	}
}

func TestAGatedRunThatNeverReachedTheGateIsMarkedUngated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for backend, why := range map[string]string{
		"gemini": "gate: the gemini backend's commands do not run through adanext-exec yet",
		"api":    "gate: adanext-exec is not in PATH",
	} {
		w := gatedRun(t, backend, "run_command", true)
		if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusError, Error: why}); err != nil {
			t.Fatal(err)
		}
		body := runPageBody(t, srv, w.ID())
		if strings.Contains(body, "<dd>through adanext-exec") || strings.Contains(body, "audit run_id") || !strings.Contains(body, "<dd>ungated · ") {
			t.Errorf("%s: a run that never reached the gate claims it, or is not marked ungated", backend)
		}
	}
}

func TestTheGatesRecordShortensPathsLikeTheResult(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	dir := t.TempDir()
	task := agent.Task{Prompt: "p", Dir: dir}
	task.Gate = true
	w, err := runlog.Create("api", task, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolResult, Tool: "run_command", Text: tools.GuardError("x " + filepath.Join(dir, "notes.txt")).Error()})
	if err := w.Close(agent.Result{Backend: "api", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if body := runPageBody(t, srv, w.ID()); strings.Contains(body, "guard: refused · x "+dir) || !strings.Contains(body, "guard: refused · x .") {
		t.Error("the gate's record shows the workspace's whole path, or is missing")
	}
}

func TestACodexRunStoppedForAnUngatedCommandShowsNoGateRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	w := gatedRun(t, "codex", "shell", true)
	if err := w.Close(agent.Result{Backend: "codex", Status: agent.StatusError,
		Error: agent.GateStopped + "codex ran a command through its own shell tool, outside the gate"}); err != nil {
		t.Fatal(err)
	}
	if body := runPageBody(t, srv, w.ID()); strings.Contains(body, `class="gate-note"`) {
		t.Error("a codex run hrn stopped shows the gate's record on results that may be its own shell's")
	}
}
