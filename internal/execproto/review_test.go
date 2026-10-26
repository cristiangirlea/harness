package execproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestStartKeysMustUseTheirExactNamesBeforeValidation(t *testing.T) {
	for _, key := range []string{"MAX_TURNS", "MCP", "Prompt", "Task", "TYPE", "Attempt_ID"} {
		t.Run(key, func(t *testing.T) {
			name := registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				return agent.Result{Backend: "codex", Status: agent.StatusOK}
			})
			// Preserve codex's real capability report but never run the installed CLI,
			// even if a malformed start incorrectly reaches the backend.
			codex, _ := agent.Get("codex")
			agent.Register(fakeBackend{name: "codex", caps: agent.BackendCapabilities(codex), run: func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				return agent.Result{Backend: "codex", Status: agent.StatusOK}
			}})
			t.Cleanup(func() { agent.Register(codex) })
			req := start(t.TempDir(), name)
			task := req["task"].(map[string]any)
			task["backend"] = "codex"
			switch key {
			case "MAX_TURNS":
				task[key] = 5
			case "MCP":
				task[key] = []string{"unused.json"}
			case "Prompt":
				delete(task, "prompt")
				task[key] = "work"
			case "Task":
				delete(req, "task")
				req[key] = task
			case "TYPE":
				delete(req, "type")
				req[key] = "start"
			case "Attempt_ID":
				delete(req, "attempt_id")
				req[key] = "att-01"
			}
			h := openHarness(t, nil)
			h.next(t, "hello")
			h.send(t, req)
			msg := h.next(t, "rejected")["error"].(map[string]any)
			if msg["code"] != "bad_request" || !strings.Contains(msg["message"].(string), fmt.Sprintf("%q", key)) {
				t.Fatalf("must name key %q: %+v", key, msg)
			}
			h.exit(t, 2)
			if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
				t.Fatalf("invalid keys created a record: %+v %v", recs, err)
			}
		})
	}
}

func TestCancelKeysAreCaseSensitiveAndASecondStartNamesTheAllowedMessage(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), "fake-exec")
	h.send(t, req)
	h.next(t, "accepted")
	for _, tc := range []struct {
		key string
		msg any
	}{
		{"TYPE", map[string]any{"TYPE": "cancel", "attempt_id": "att-01"}},
		{"Attempt_ID", map[string]any{"type": "cancel", "Attempt_ID": "att-01"}},
		{"task", map[string]any{"type": 42, "attempt_id": "att-01", "task": map[string]any{}}},
	} {
		h.send(t, tc.msg)
		msg := h.next(t, "error")
		err := msg["error"].(map[string]any)
		if msg["fatal"] != false || err["code"] != "bad_request" || !strings.Contains(err["message"].(string), fmt.Sprintf("%q", tc.key)) {
			t.Fatalf("must refuse cancel key %q without stopping: %+v", tc.key, msg)
		}
	}
	h.send(t, req)
	msg := h.next(t, "error")
	err := msg["error"].(map[string]any)
	if err["code"] != "bad_request" || err["message"] != "only cancel is accepted after start" {
		t.Fatalf("second-start error: %+v", msg)
	}
	h.send(t, map[string]any{"type": "cancel", "attempt_id": "att-01"})
	if msg := h.next(t, "result"); msg["result"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("result: %+v", msg)
	}
	h.exit(t, 0)
}

func TestUnknownKeysAreNamedBeforeDecodingTheirValuesOrRequiredFields(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	// The prompt is missing and the aliased option's value has the wrong type.
	// Neither may obscure the case-sensitive unknown-key error.
	h.send(t, map[string]any{"type": "start", "attempt_id": "att-01", "task": map[string]any{
		"backend": "codex", "mode": "auto", "MAX_TURNS": "five",
	}})
	err := h.next(t, "rejected")["error"].(map[string]any)
	if err["code"] != "bad_request" || !strings.Contains(err["message"].(string), `"MAX_TURNS"`) {
		t.Fatalf("unknown key must take precedence: %+v", err)
	}
	h.exit(t, 2)
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("unknown key created a record: %+v %v", recs, err)
	}
}

func TestAnOversizedLineAfterAcceptanceStillAllowsCancellation(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	h.next(t, "accepted")
	// Send the following cancel with the oversized line: failure must not strand
	// its writer or disable the only remaining control channel.
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(h.in, strings.Repeat("x", MaxInputLine+1)+"\n"+`{"type":"cancel","attempt_id":"another"}`+"\n"+`{"type":"cancel","attempt_id":"att-01"}`+"\n")
		written <- err
	}()
	msg := h.next(t, "error")
	if msg["fatal"] != false || msg["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatalf("oversized line error: %+v", msg)
	}
	// A wrong-attempt cancel must still be read and answered before the real
	// cancel. A canceled result alone could come from the broken reader's exit.
	if next := h.next(t, "error"); next["fatal"] != false || next["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatalf("following control message: %+v", next)
	}
	if msg := h.next(t, "result"); msg["result"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("result: %+v", msg)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the following cancel was not read")
	}
	h.exit(t, 0)
}

func TestSecondStartExplainsThatOnlyCancelIsAccepted(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), "fake-exec")
	h.send(t, req)
	h.next(t, "accepted")
	h.send(t, req)
	err := h.next(t, "error")["error"].(map[string]any)
	if err["code"] != "bad_request" || err["message"] != "only cancel is accepted after start" {
		t.Errorf("second start: %+v", err)
	}
	h.send(t, map[string]any{"type": "cancel", "attempt_id": "att-01"})
	h.next(t, "result")
	h.exit(t, 0)
}

func TestDuplicatedTaskObjectsCannotBypassTheOptionGate(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	// encoding/json merges repeated task objects when decoding into a struct,
	// but a map retains only the last one. Refuse this ambiguous framing.
	data := fmt.Sprintf(`{"type":"start","attempt_id":"att-01","task":{"max_turns":5},"task":{"prompt":"work","dir":%q,"backend":"fake-exec","mode":"auto"}}`+"\n", t.TempDir())
	if _, err := io.WriteString(h.in, data); err != nil {
		t.Fatal(err)
	}
	err := h.next(t, "rejected")["error"].(map[string]any)
	if err["code"] != "bad_request" || !strings.Contains(err["message"].(string), `"task"`) {
		t.Fatalf("duplicate task: %+v", err)
	}
	h.exit(t, 2)
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("duplicate task created records: %+v %v", recs, err)
	}
}

func TestAnOversizedLineIsDiscardedOnlyThroughItsNewline(t *testing.T) {
	input := strings.Repeat("x", MaxInputLine+8192) + "\n" + `{"type":"cancel","attempt_id":"att-01"}` + "\n"
	done := make(chan struct{})
	defer close(done)
	// First supply a valid start frame so the reader recovers later oversized lines.
	lines := readInput(strings.NewReader("{}\n"+input), done)
	if first := <-lines; first.err != nil {
		t.Fatal(first.err)
	}
	if large := <-lines; large.err == nil {
		t.Fatal("oversized frame was accepted")
	}
	var next inputLine
	select {
	case next = <-lines:
	case <-time.After(time.Second):
		t.Fatal("reader stopped after the oversized frame")
	}
	if next.err != nil || string(next.data) != `{"type":"cancel","attempt_id":"att-01"}` {
		t.Fatalf("reader lost next frame: %s %v", next.data, next.err)
	}
}

func TestUnterminatedInputAfterAcceptanceCancelsTheRun(t *testing.T) {
	for _, size := range []int{10, MaxInputLine + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			registerFake(t, nil)
			h := openHarness(t, nil)
			h.next(t, "hello")
			h.send(t, start(t.TempDir(), "fake-exec"))
			h.next(t, "accepted")
			go func() { _, _ = io.WriteString(h.in, strings.Repeat("x", size)); _ = h.in.Close() }()
			// An error about the incomplete frame may precede the EOF cancellation.
			line, err := h.out.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var msg map[string]any
			if err := json.Unmarshal(line, &msg); err != nil {
				t.Fatal(err)
			}
			if msg["type"] == "error" {
				msg = h.next(t, "result")
			}
			if msg["type"] != "result" || msg["result"].(map[string]any)["status"] != "canceled" {
				t.Fatalf("unterminated frame: %+v", msg)
			}
			h.exit(t, 0)
		})
	}
}

type closeOnlyFailure struct{ *runlog.Writer }

func (r closeOnlyFailure) Close(res agent.Result) error {
	// All events reached the real file. Closing the already closed events file
	// makes the failure occur only in the protocol's Close, and persists the error.
	if err := r.Writer.Close(res); err != nil {
		return err
	}
	return r.Writer.Close(res)
}

func TestACloseOnlyStorageFailureReportsOneFatalErrorAndTheRecordError(t *testing.T) {
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventText, Text: "saved"})
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	h := openHarness(t, func(backend string, task agent.Task, continues, route string, routed *runlog.RoutedBy, note string) (record, error) {
		w, err := runlog.CreateRouted(backend, task, continues, route, routed, note)
		return closeOnlyFailure{w}, err
	})
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	h.next(t, "event")
	errMsg := h.next(t, "error")
	if errMsg["fatal"] != true || errMsg["error"].(map[string]any)["code"] != "storage" {
		t.Fatalf("close failure: %+v", errMsg)
	}
	res := h.next(t, "result")
	if res["record_error"] == "" || strings.Contains(res["record_error"].(string), "not recorded") {
		t.Fatalf("events were all saved: %+v", res)
	}
	h.exit(t, 0)
	rec, err := runlog.Load(accepted["run_id"].(string))
	if err != nil || !rec.Done || rec.RecordError != res["record_error"] {
		t.Fatalf("close failure record: %+v %v", rec, err)
	}
	events, err := runlog.Events(rec.ID)
	if err != nil || len(events) != 1 || events[0].Text != "saved" {
		t.Fatalf("close failure lost events: %+v %v", events, err)
	}
}

func TestSessionEventsPassThroughExecAndItsRecord(t *testing.T) {
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventSession, Text: "native-session"})
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK, Session: "native-session"}
	})
	h := openHarness(t, nil)
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	msg := h.next(t, "event")
	event := msg["event"].(map[string]any)
	if event["kind"] != "session" || event["text"] != "native-session" || msg["run_id"] != accepted["run_id"] || msg["attempt_id"] != "att-01" {
		t.Fatalf("session event: %+v", msg)
	}
	h.next(t, "result")
	h.exit(t, 0)
	events, err := runlog.Events(accepted["run_id"].(string))
	if err != nil || len(events) != 1 || events[0].Kind != agent.EventSession || events[0].Text != "native-session" {
		t.Fatalf("session record: %+v %v", events, err)
	}
}

func TestAContinuationReportsItsInheritedRequestedModel(t *testing.T) {
	registerFake(t, nil)
	agent.Register(forkingFake{fakeBackend{name: "fake-exec", caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Sessions: "fork", Options: []string{"mode", "model", "timeout"}}, run: func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		if task.Model != "inherited-model" {
			t.Errorf("backend task model: %q", task.Model)
		}
		return agent.Result{Backend: "fake-exec", Model: "resolved-model", ModelReported: true, Status: agent.StatusOK}
	}}})
	// Recorded as hrn records a run's directory: in long names, where the
	// temp dir can be spelt in short ones (the Windows runner's is).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prev, err := runlog.Create("fake-exec", agent.Task{Prompt: "earlier", Dir: dir, Options: agent.Options{Model: "inherited-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(agent.Result{Backend: "fake-exec", Status: agent.StatusOK, Session: "saved-session"}); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(dir, "fake-exec")
	req["task"].(map[string]any)["continue_run_id"] = prev.ID()
	h.send(t, req)
	accepted := h.next(t, "accepted")
	if accepted["requested"].(map[string]any)["model"] != "inherited-model" {
		t.Errorf("accepted model: %+v", accepted)
	}
	h.next(t, "event")
	res := h.next(t, "result")
	if res["requested_model"] != "inherited-model" || res["resolved_model"] != "resolved-model" {
		t.Errorf("requested and resolved model: %+v", res)
	}
	h.exit(t, 0)
}

func TestOversizedEventInputIsOmittedOnlyFromTheProtocol(t *testing.T) {
	for _, size := range []int{MaxEventText, MaxEventText + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			input := json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
			registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				emit(agent.Event{Kind: agent.EventToolCall, Text: "write file", Tool: "write_file", Input: input})
				return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
			})
			h := openHarness(t, nil)
			h.next(t, "hello")
			h.send(t, start(t.TempDir(), "fake-exec"))
			accepted := h.next(t, "accepted")
			msg := h.next(t, "event")["event"].(map[string]any)
			_, hasInput := msg["input"]
			if msg["truncated"] != (size > MaxEventText) || hasInput != (size <= MaxEventText) || msg["text"] != "write file" {
				t.Fatalf("input size %d: present %v truncated %v", size, hasInput, msg["truncated"])
			}
			h.next(t, "result")
			h.exit(t, 0)
			events, err := runlog.Events(accepted["run_id"].(string))
			if err != nil || len(events) != 1 || !bytes.Equal(events[0].Input, input) {
				t.Fatal("record must retain the full tool input", err)
			}
		})
	}
}

func TestInputAtTheLimitStillFitsOneFrame(t *testing.T) {
	data := strings.Repeat("x", MaxInputLine)
	line, err := readLine(bufio.NewReader(strings.NewReader(data+"\n")), false)
	if err != nil || string(line) != data {
		t.Fatalf("at-limit frame: %d bytes %v", len(line), err)
	}
}
