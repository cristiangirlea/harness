package execproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Detection is faked too: hello must never inspect credentials or need an installed CLI.
type detectedBackend struct{ agent.Backend }

func (b detectedBackend) Detect() (string, error) { return "test detection", nil }
func (b detectedBackend) Capabilities() agent.Capabilities {
	return agent.BackendCapabilities(b.Backend)
}

func TestMain(m *testing.M) {
	for _, name := range agent.Names() {
		b, _ := agent.Get(name)
		agent.Register(detectedBackend{b})
	}
	os.Exit(m.Run())
}

type fakeBackend struct {
	name      string
	caps      agent.Capabilities
	run       func(context.Context, agent.Task, func(agent.Event)) agent.Result
	detectErr error
}

func (b fakeBackend) Name() string                     { return b.name }
func (b fakeBackend) Detect() (string, error)          { return "fake", b.detectErr }
func (b fakeBackend) Capabilities() agent.Capabilities { return b.caps }
func (b fakeBackend) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	if b.run != nil {
		return b.run(ctx, task, emit)
	}
	<-ctx.Done()
	return agent.Result{Backend: b.name, Status: agent.StatusCanceled}
}

func registerFake(t *testing.T, run func(context.Context, agent.Task, func(agent.Event)) agent.Result) string {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	name := "fake-exec"
	agent.Register(fakeBackend{name: name, caps: agent.Capabilities{
		Modes: []agent.Mode{agent.ModeAuto}, Efforts: agent.EffortLevels,
		Sessions: "fork", Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"},
	}, run: run})
	return name
}

type harness struct {
	in   *io.PipeWriter
	out  *bufio.Reader
	code chan int
}

func openHarness(t *testing.T, create recordFactory) *harness {
	return openHarnessContext(t, context.Background(), create)
}

func openHarnessContext(t *testing.T, ctx context.Context, create recordFactory) *harness {
	t.Helper()
	ir, iw := io.Pipe()
	or, ow := io.Pipe()
	h := &harness{in: iw, out: bufio.NewReader(or), code: make(chan int, 1)}
	t.Cleanup(func() { iw.Close(); ir.Close(); or.Close(); ow.Close() })
	go func() {
		defer ow.Close()
		if create == nil {
			h.code <- Run(ctx, ir, ow, func() time.Time { return time.Unix(123, 456).UTC() })
		} else {
			h.code <- run(ctx, ir, ow, time.Now, create)
		}
	}()
	return h
}

func (h *harness) next(t *testing.T, kind string) map[string]any {
	t.Helper()
	ch := make(chan map[string]any, 1)
	go func() {
		line, err := h.out.ReadBytes('\n')
		var msg map[string]any
		if err == nil {
			_ = json.Unmarshal(line, &msg)
		}
		ch <- msg
	}()
	select {
	case msg := <-ch:
		if msg["type"] != kind {
			t.Fatalf("want %s, got %+v", kind, msg)
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", kind)
		return nil
	}
}

func (h *harness) send(t *testing.T, msg any) {
	t.Helper()
	if err := json.NewEncoder(h.in).Encode(msg); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) exit(t *testing.T, want int) {
	t.Helper()
	select {
	case code := <-h.code:
		if code != want {
			t.Fatalf("exit = %d, want %d", code, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("protocol did not exit")
	}
	if line, err := h.out.ReadBytes('\n'); len(line) != 0 || err != io.EOF {
		t.Fatalf("result/rejected must be last: %s %v", line, err)
	}
}

func start(dir, backend string) map[string]any {
	return map[string]any{"type": "start", "attempt_id": "att-01", "task": map[string]any{
		"prompt": "work", "dir": dir, "backend": backend, "mode": "auto",
	}}
}

func TestHelloPrecedesInputAndReportsConservativeCapabilities(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := openHarness(t, nil)
	msg := h.next(t, "hello")
	if msg["seq"] != float64(1) || msg["v"] != float64(1) || msg["protocol"] != "v1" || msg["time"] != "1970-01-01T00:02:03.000000456Z" {
		t.Fatalf("bad envelope: %+v", msg)
	}
	wants := map[string]agent.Capabilities{
		"api":      {Modes: []agent.Mode{"readonly", "auto", "yolo"}, Efforts: agent.EffortLevels, Sessions: "fork", Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"}},
		"claude":   {Modes: []agent.Mode{"readonly", "auto", "yolo"}, Efforts: agent.EffortLevels, Sessions: "fork", Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"}},
		"codex":    {Modes: []agent.Mode{"readonly", "auto", "yolo"}, Efforts: agent.EffortLevels, Sessions: "fork", Options: []string{"model", "mode", "effort", "timeout", "mcp"}},
		"gemini":   {Modes: []agent.Mode{"auto", "yolo"}, Efforts: []string{}, Sessions: "none", Options: []string{"model", "mode", "timeout"}},
		"opencode": {Modes: []agent.Mode{"yolo"}, Efforts: []string{}, Sessions: "none", Options: []string{"model", "mode", "timeout"}},
	}
	for _, raw := range msg["backends"].([]any) {
		entry := raw.(map[string]any)
		name := entry["name"].(string)
		want, ok := wants[name]
		if !ok {
			continue
		}
		data, _ := json.Marshal(entry)
		var got agent.Capabilities
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s capabilities = %+v, want %+v", name, got, want)
		}
		if entry["available"] != true || entry["detail"] != "test detection" {
			t.Errorf("detection = %+v", entry)
		}
		delete(wants, name)
	}
	if len(wants) != 0 {
		t.Fatalf("missing backends: %v", wants)
	}
	h.in.Close()
	h.next(t, "rejected")
	h.exit(t, 2)
}

func TestAcceptedRunStreamsCompleteEventsAndRecordsTheAttempt(t *testing.T) {
	name := registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventText, Text: "fragment", Partial: true})
		emit(agent.Event{Kind: agent.EventText, Text: "first"})
		emit(agent.Event{Kind: agent.EventThinking, Text: "second"})
		return agent.Result{Backend: "fake-exec", Model: "resolved", ModelReported: true, Status: agent.StatusOK, Final: "done"}
	})
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), name)
	req["task"].(map[string]any)["model"] = "requested"
	h.send(t, req)
	accepted := h.next(t, "accepted")
	for i, text := range []string{"first", "second"} {
		msg := h.next(t, "event")
		if msg["seq"] != float64(i+3) || msg["attempt_id"] != "att-01" || msg["run_id"] != accepted["run_id"] || msg["event"].(map[string]any)["text"] != text {
			t.Fatalf("bad event: %+v", msg)
		}
	}
	res := h.next(t, "result")
	if res["seq"] != float64(5) || res["requested_model"] != "requested" || res["resolved_model"] != "resolved" || res["record_error"] != "" {
		t.Fatalf("bad result: %+v", res)
	}
	h.exit(t, 0)
	rec, err := runlog.Load(accepted["run_id"].(string))
	if err != nil || rec.Task.AttemptID != "att-01" || !rec.Done || rec.Result.Status != agent.StatusOK || !rec.Result.ModelReported {
		t.Fatalf("record: %+v %v", rec, err)
	}
}

func TestStartRecordsADeclaredPurposeOrTheHeader(t *testing.T) {
	name := registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	for _, tc := range []struct{ name, prompt, declared, want string }{
		{name: "declared", prompt: "Purpose: debug\nwork", declared: "review", want: "review"},
		{name: "header", prompt: "Purpose: debug\nwork", want: "debug"},
		{name: "neither", prompt: "work", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(t.TempDir(), name)
			req["task"].(map[string]any)["prompt"] = tc.prompt
			if tc.declared != "" {
				req["task"].(map[string]any)["purpose"] = tc.declared
			}
			h.send(t, req)
			accepted := h.next(t, "accepted")
			h.next(t, "result")
			h.exit(t, 0)
			rec, err := runlog.Load(accepted["run_id"].(string))
			if err != nil || rec.Task.Purpose != tc.want {
				t.Fatalf("purpose = %q, want %q (%v)", rec.Task.Purpose, tc.want, err)
			}
		})
	}
}

func TestInvalidStartsAreRejectedWithoutRecords(t *testing.T) {
	name := registerFake(t, nil)
	agent.Register(fakeBackend{name: "fake-no-effort", caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Sessions: "none", Options: []string{"model", "mode", "timeout"}}})
	agent.Register(fakeBackend{name: "fake-unavailable", caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Options: []string{"mode"}}, detectErr: errors.New("not installed")})
	for _, tc := range []struct {
		name, code string
		edit       func(map[string]any)
		raw        string
	}{
		{name: "malformed", code: "bad_request", raw: "{\n"},
		{name: "unknown field", code: "bad_request", edit: func(m map[string]any) { m["typo"] = true }},
		{name: "unknown task field", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["max_cost_usd"] = 1 }},
		{name: "missing prompt", code: "bad_request", edit: func(m map[string]any) { delete(m["task"].(map[string]any), "prompt") }},
		{name: "missing dir", code: "bad_request", edit: func(m map[string]any) { delete(m["task"].(map[string]any), "dir") }},
		{name: "missing mode", code: "bad_request", edit: func(m map[string]any) { delete(m["task"].(map[string]any), "mode") }},
		{name: "missing task", code: "bad_request", edit: func(m map[string]any) { delete(m, "task") }},
		{name: "unknown purpose", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["purpose"] = "unknown" }},
		{name: "purpose typo", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["purpose"] = "Review" }},
		{name: "model with ampersand", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["model"] = "x&calc" }},
		{name: "model with variable", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["model"] = "%PATH%" }},
		{name: "model with quote", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["model"] = `a"b` }},
		{name: "model with space", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["model"] = "a b" }},
		{name: "model as flag", code: "bad_request", edit: func(m map[string]any) { m["task"].(map[string]any)["model"] = "-x" }},
		{name: "bad attempt", code: "bad_request", edit: func(m map[string]any) { m["attempt_id"] = "bad/attempt" }},
		{name: "long attempt", code: "bad_request", edit: func(m map[string]any) { m["attempt_id"] = strings.Repeat("a", 129) }},
		{name: "empty attempt", code: "bad_request", edit: func(m map[string]any) { m["attempt_id"] = "" }},
		{name: "unknown backend", code: "unsupported", edit: func(m map[string]any) { m["task"].(map[string]any)["backend"] = "unknown" }},
		{name: "ask", code: "unsupported", edit: func(m map[string]any) { m["task"].(map[string]any)["mode"] = "ask" }},
		{name: "effort unsupported", code: "unsupported", edit: func(m map[string]any) {
			m["task"].(map[string]any)["backend"] = "fake-no-effort"
			m["task"].(map[string]any)["effort"] = "high"
		}},
		{name: "effort typo", code: "unsupported", edit: func(m map[string]any) { m["task"].(map[string]any)["effort"] = "HIGH" }},
		{name: "codex max turns", code: "unsupported", edit: func(m map[string]any) {
			m["task"].(map[string]any)["backend"] = "codex"
			m["task"].(map[string]any)["max_turns"] = 0
		}},
		{name: "unavailable", code: "unavailable", edit: func(m map[string]any) { m["task"].(map[string]any)["backend"] = "fake-unavailable" }},
		{name: "cancel first", code: "bad_request", raw: `{"type":"cancel","attempt_id":"att-01"}` + "\n"},
		{name: "two objects", code: "bad_request", raw: `{} {}` + "\n"},
		{name: "too large", code: "bad_request", raw: strings.Repeat("x", (4<<20)+1) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			h := openHarness(t, nil)
			h.next(t, "hello")
			if tc.raw != "" {
				go func() { _, _ = io.WriteString(h.in, tc.raw) }()
			} else {
				m := start(t.TempDir(), name)
				tc.edit(m)
				h.send(t, m)
			}
			msg := h.next(t, "rejected")
			if msg["error"].(map[string]any)["code"] != tc.code {
				t.Fatalf("bad rejection: %+v", msg)
			}
			h.exit(t, 2)
			if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
				t.Fatalf("rejected start created records: %+v %v", recs, err)
			}
		})
	}
}

func TestCancellationAndCallerErrorsPreserveTheAcceptedRun(t *testing.T) {
	for _, action := range []string{"cancel", "eof", "wrong attempt", "repeated cancel", "second start", "malformed cancel"} {
		t.Run(action, func(t *testing.T) {
			release := make(chan struct{})
			registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				<-release
				<-ctx.Done()
				return agent.Result{Backend: "fake-exec", Status: agent.StatusCanceled}
			})
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(t.TempDir(), "fake-exec")
			h.send(t, req)
			h.next(t, "accepted")
			cancel := map[string]any{"type": "cancel", "attempt_id": "att-01"}
			switch action {
			case "eof":
				h.in.Close()
			case "wrong attempt":
				h.send(t, map[string]any{"type": "cancel", "attempt_id": "another"})
				msg := h.next(t, "error")
				if msg["fatal"] != false || msg["error"].(map[string]any)["code"] != "bad_request" {
					t.Fatalf("bad caller error: %+v", msg)
				}
				h.send(t, cancel)
			case "second start":
				h.send(t, req)
				h.next(t, "error")
				h.send(t, cancel)
			case "malformed cancel":
				h.send(t, map[string]any{"type": "cancel", "attempt_id": "att-01", "typo": true})
				h.next(t, "error")
				h.send(t, cancel)
			case "repeated cancel":
				h.send(t, cancel)
				h.send(t, cancel)
			default:
				h.send(t, cancel)
			}
			close(release)
			msg := h.next(t, "result")
			if msg["result"].(map[string]any)["status"] != "canceled" {
				t.Fatalf("bad canceled result: %+v", msg)
			}
			h.exit(t, 0)
		})
	}
}

// Close the real events file before the next Event, as runlog's file-failure tests do.
type closedEvents struct {
	*runlog.Writer
	once sync.Once
}

func (r *closedEvents) Event(e agent.Event) {
	r.once.Do(func() { _ = r.Writer.Close(agent.Result{Status: agent.StatusCanceled}) })
	r.Writer.Event(e)
}

func TestStorageFailureCancelsAndStillReportsTheRecordError(t *testing.T) {
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventText, Text: "cannot record"})
		<-ctx.Done()
		return agent.Result{Backend: "fake-exec", Status: agent.StatusCanceled}
	})
	h := openHarness(t, func(backend string, task agent.Task, continues, route string, routed *runlog.RoutedBy, note string) (record, error) {
		w, err := runlog.CreateRouted(backend, task, continues, route, routed, note)
		return &closedEvents{Writer: w}, err
	})
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	msg := h.next(t, "error")
	if msg["fatal"] != true || msg["error"].(map[string]any)["code"] != "storage" {
		t.Fatalf("bad storage error: %+v", msg)
	}
	h.next(t, "event")
	msg = h.next(t, "result")
	if msg["record_error"] == "" || msg["result"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("bad storage result: %+v", msg)
	}
	h.exit(t, 0)
	rec, err := runlog.Load(accepted["run_id"].(string))
	if err != nil || !rec.Done || rec.RecordError == "" {
		t.Fatalf("bad record: %+v %v", rec, err)
	}
}

func TestEventTextIsClippedOnARuneBoundaryOnlyInTheProtocol(t *testing.T) {
	text := strings.Repeat("x", (256<<10)-1) + "🙂tail"
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventText, Text: text})
		return agent.Result{Status: agent.StatusOK}
	})
	h := openHarness(t, nil)
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	msg := h.next(t, "event")["event"].(map[string]any)
	clipped := msg["text"].(string)
	if msg["truncated"] != true || len(clipped) != (256<<10)-1 || !utf8.ValidString(clipped) {
		t.Fatalf("bad clipped event: len %d, truncated %v", len(clipped), msg["truncated"])
	}
	h.next(t, "result")
	h.exit(t, 0)
	events, err := runlog.Events(accepted["run_id"].(string))
	if err != nil || len(events) != 1 || events[0].Text != text {
		t.Fatal("record must keep full text", err)
	}
}

func TestContinuationAppliesThePlanAndReturnsItsNotes(t *testing.T) {
	registerFake(t, nil)
	// The fake advertises the same native forking contract followup uses.
	agent.Register(forkingFake{fakeBackend{name: "fake-exec", caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Sessions: "fork", Options: []string{"model", "mode", "timeout"}}, run: func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		if task.ForkSession != "session-01" {
			t.Errorf("fork = %q", task.ForkSession)
		}
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	}}})
	// Recorded as hrn records a run's directory: in long names, where the
	// temp dir can be spelt in short ones (the Windows runner's is).
	rawDir := t.TempDir()
	dir, err := filepath.EvalSymlinks(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := runlog.Create("fake-exec", agent.Task{Prompt: "earlier", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(agent.Result{Backend: "fake-exec", Status: agent.StatusOK, Session: "session-01"}); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(rawDir, "fake-exec")
	req["task"].(map[string]any)["continue_run_id"] = prev.ID()
	h.send(t, req)
	msg := h.next(t, "accepted")
	if msg["continues"] != prev.ID() || len(msg["notes"].([]any)) == 0 {
		t.Fatalf("bad continuation: %+v", msg)
	}
	h.next(t, "event")
	h.next(t, "result")
	h.exit(t, 0)
	rec, err := runlog.Load(msg["run_id"].(string))
	if err != nil || rec.Continues != prev.ID() || rec.Task.Dir != dir {
		t.Fatalf("bad continuation record: %+v %v", rec, err)
	}
}

func TestAContinuationOfARecordWithABadModelIsRejectedWithoutARecord(t *testing.T) {
	name := registerFake(t, nil)
	dir := t.TempDir()
	prev, err := runlog.Create(name, agent.Task{Prompt: "earlier", Dir: dir, Options: agent.Options{Model: "x&calc"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(agent.Result{Backend: name, Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(dir, name)
	req["task"].(map[string]any)["continue_run_id"] = prev.ID()
	req["task"].(map[string]any)["fresh"] = true
	h.send(t, req)
	msg := h.next(t, "rejected")
	if e := msg["error"].(map[string]any); e["code"] != "bad_request" || !strings.Contains(e["message"].(string), "is not a model id") {
		t.Fatalf("bad rejection: %+v", msg)
	}
	h.exit(t, 2)
	if recs, err := runlog.List(0); err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v %v, want only the earlier run", recs, err)
	}
}

type forkingFake struct{ fakeBackend }

func TestContinuationWarnsInAcceptedNotesAndRecordsOneInfoEventOnlyAboveTheThreshold(t *testing.T) {
	for _, n := range []int64{0, 149999, 150000, 150001, 285000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			name := registerFake(t, func(context.Context, agent.Task, func(agent.Event)) agent.Result {
				return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
			})
			rawDir := t.TempDir()
			dir, err := filepath.EvalSymlinks(rawDir)
			if err != nil {
				t.Fatal(err)
			}
			prev, err := runlog.Create(name, agent.Task{Prompt: "first", Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(prev.Dir(), "messages.json"), []byte(`{"model":"fake"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := prev.Close(agent.Result{Backend: name, Status: agent.StatusOK, ContextTokens: n}); err != nil {
				t.Fatal(err)
			}
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(rawDir, name)
			req["task"].(map[string]any)["continue_run_id"] = prev.ID()
			h.send(t, req)
			accepted := h.next(t, "accepted")
			warning := ""
			for _, note := range accepted["notes"].([]any) {
				if strings.Contains(note.(string), "short hand-over") {
					if warning != "" {
						t.Error("warning repeated in accepted notes")
					}
					warning = note.(string)
				}
			}
			if n > 150000 {
				for _, want := range []string{"warning:", prev.ID(), fmt.Sprintf("%d tokens", n), "fresh run", "short hand-over"} {
					if !strings.Contains(warning, want) {
						t.Errorf("accepted warning lacks %q: %s", want, warning)
					}
				}
			} else if warning != "" {
				t.Errorf("unneeded accepted warning: %s", warning)
			}
			// Apply emits the plan notes followed by the optional warning.
			h.next(t, "event")
			if warning != "" {
				msg := h.next(t, "event")
				e := msg["event"].(map[string]any)
				if e["kind"] != string(agent.EventInfo) || e["text"] != warning {
					t.Errorf("warning event = %+v", e)
				}
			}
			h.next(t, "result")
			h.exit(t, 0)
			events, err := runlog.Events(accepted["run_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, e := range events {
				if e.Kind == agent.EventInfo && strings.Contains(e.Text, "short hand-over") {
					count++
					if e.Text != warning {
						t.Errorf("record warning differs from accepted notes: %s", e.Text)
					}
				}
			}
			want := 0
			if n > 150000 {
				want = 1
			}
			if count != want {
				t.Errorf("recorded warnings = %d, want %d", count, want)
			}
		})
	}
}

func (forkingFake) ForksSessions() bool { return true }

func TestOldRecordsLoadWithoutAnAttemptID(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := "20260101-000000-aaaaaa"
	dir := filepath.Join(runlog.Root(), id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":%q,"backend":"old","task":{"prompt":"old","dir":"."},"result":{"backend":"old","status":"ok","turns":1,"duration_ns":42}}`, id)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	rec, err := runlog.Load(id)
	if err != nil || !rec.Done || rec.Task.AttemptID != "" || rec.Result.ModelReported || rec.Result.Duration != 42 {
		t.Fatalf("old record: %+v %v", rec, err)
	}
}

func TestResolvedModelRequiresAnExplicitBackendReport(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(fmt.Sprint(reported), func(t *testing.T) {
			registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				return agent.Result{Backend: "fake-exec", Model: task.Model, ModelReported: reported, Status: agent.StatusOK}
			})
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(t.TempDir(), "fake-exec")
			req["task"].(map[string]any)["model"] = "requested"
			h.send(t, req)
			h.next(t, "accepted")
			msg := h.next(t, "result")
			want := ""
			if reported {
				want = "requested"
			}
			if msg["requested_model"] != "requested" || msg["resolved_model"] != want {
				t.Fatalf("reported %v: %+v", reported, msg)
			}
			h.exit(t, 0)
		})
	}
}

type failingOutput struct {
	bytes.Buffer
	writes, failAt int
	short          bool
}

func (w *failingOutput) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		if w.short {
			return len(data) / 2, nil
		}
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(data)
}

func TestStdoutFailureCancelsTheRunClosesItsRecordAndExitsOne(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprintf("short=%v", short), func(t *testing.T) {
			canceled := make(chan struct{})
			registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
				emit(agent.Event{Kind: agent.EventText, Text: "before stdout fails"})
				<-ctx.Done()
				close(canceled)
				emit(agent.Event{Kind: agent.EventInfo, Text: "canceled"})
				return agent.Result{Backend: "fake-exec", Status: agent.StatusCanceled}
			})
			in, peer := io.Pipe()
			t.Cleanup(func() { peer.Close(); in.Close() })
			out := &failingOutput{failAt: 3, short: short}
			code := make(chan int, 1)
			go func() { code <- Run(context.Background(), in, out, nil) }()
			if err := json.NewEncoder(peer).Encode(start(t.TempDir(), "fake-exec")); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-code:
				if got != 1 {
					t.Fatalf("exit %d, want 1", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stdout failure did not stop the run")
			}
			select {
			case <-canceled:
			default:
				t.Fatal("backend context was not canceled")
			}
			recs, err := runlog.List(0)
			if err != nil || len(recs) != 1 || !recs[0].Done || recs[0].Result.Status != agent.StatusCanceled {
				t.Fatalf("record not closed: %+v %v", recs, err)
			}
			events, err := runlog.Events(recs[0].ID)
			if err != nil || len(events) != 2 {
				t.Fatalf("events must still be recorded after stdout fails: %+v %v", events, err)
			}
			if out.writes != 3 {
				t.Fatalf("wrote again after stdout failed: %d writes", out.writes)
			}
		})
	}
}

func TestInterruptContextCancelsWithoutClosingStdin(t *testing.T) {
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		emit(agent.Event{Kind: agent.EventInfo, Text: "running"})
		<-ctx.Done()
		return agent.Result{Backend: "fake-exec", Status: agent.StatusCanceled}
	})
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	h := openHarnessContext(t, ctx, nil)
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	h.next(t, "event")
	interrupt()
	msg := h.next(t, "result")
	if msg["result"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("interrupt result: %+v", msg)
	}
	h.exit(t, 0)
	rec, err := runlog.Load(accepted["run_id"].(string))
	if err != nil || !rec.Done || rec.Result.Status != agent.StatusCanceled {
		t.Fatalf("interrupt record: %+v %v", rec, err)
	}
}

func TestConcurrentEventsHaveOneSequenceAndKeepTheirRecordOrder(t *testing.T) {
	const count = 32
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func() {
				defer wg.Done()
				emit(agent.Event{Kind: agent.EventText, Text: fmt.Sprint(i)})
			}()
		}
		wg.Wait()
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	h := openHarness(t, nil)
	h.next(t, "hello")
	h.send(t, start(t.TempDir(), "fake-exec"))
	accepted := h.next(t, "accepted")
	var texts []string
	for i := range count {
		msg := h.next(t, "event")
		if msg["seq"] != float64(i+3) {
			t.Fatalf("event sequence: %+v", msg)
		}
		texts = append(texts, msg["event"].(map[string]any)["text"].(string))
	}
	if msg := h.next(t, "result"); msg["seq"] != float64(count+3) {
		t.Fatalf("result sequence: %+v", msg)
	}
	h.exit(t, 0)
	events, err := runlog.Events(accepted["run_id"].(string))
	if err != nil || len(events) != count {
		t.Fatalf("recorded events: %d %v", len(events), err)
	}
	for i, e := range events {
		if e.Text != texts[i] {
			t.Fatalf("event %d: protocol %q record %q", i, texts[i], e.Text)
		}
	}
}

func TestWrongCancelAndSecondStartLeaveTheRunRunning(t *testing.T) {
	release := make(chan struct{})
	registerFake(t, func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
		<-release
		if ctx.Err() != nil {
			return agent.Result{Backend: "fake-exec", Status: agent.StatusCanceled}
		}
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), "fake-exec")
	h.send(t, req)
	h.next(t, "accepted")
	for _, msg := range []any{map[string]any{"type": "cancel", "attempt_id": "another"}, req} {
		h.send(t, msg)
		err := h.next(t, "error")
		if err["fatal"] != false || err["error"].(map[string]any)["code"] != "bad_request" {
			t.Fatalf("caller error: %+v", err)
		}
	}
	close(release)
	res := h.next(t, "result")
	if res["result"].(map[string]any)["status"] != "ok" {
		t.Fatalf("caller mistake stopped the run: %+v", res)
	}
	h.exit(t, 0)
	if recs, err := runlog.List(0); err != nil || len(recs) != 1 {
		t.Fatalf("second start must not create another record: %+v %v", recs, err)
	}
}

func TestExecTakesNoRetrievalOptInAndHandsAPinnedRunToAnotherBackend(t *testing.T) {
	var got agent.Task
	registerFake(t, func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
		got = task
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	dir := t.TempDir()
	prev, err := runlog.Create("api", agent.Task{Prompt: "earlier", Dir: dir, Options: agent.Options{Model: "m", Retrieval: "required"}})
	if err != nil {
		t.Fatal(err)
	}
	pin := &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "api", Model: "m"}, Packet: &retrieval.Packet{VersionID: "kv-7"}}
	if err := prev.Close(agent.Result{Backend: "api", Status: agent.StatusOK, Final: "a", Retrieval: pin}); err != nil {
		t.Fatal(err)
	}

	// exec has no retrieval field, so it never reaches followup's pin refusal.
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(dir, "fake-exec")
	req["task"].(map[string]any)["continue_run_id"] = prev.ID()
	req["task"].(map[string]any)["retrieval"] = "required"
	h.send(t, req)
	if e := h.next(t, "rejected")["error"].(map[string]any); e["code"] != "bad_request" || !strings.Contains(fmt.Sprint(e["message"]), `unknown field "retrieval"`) {
		t.Fatalf("bad rejection: %+v", e)
	}
	h.exit(t, 2)
	if recs, err := runlog.List(0); err != nil || len(recs) != 1 {
		t.Fatalf("rejected start created records: %+v %v", recs, err)
	}

	// With retrieval off the transcript is handed over as before.
	h = openHarness(t, nil)
	h.next(t, "hello")
	delete(req["task"].(map[string]any), "retrieval")
	h.send(t, req)
	h.next(t, "accepted")
	h.next(t, "event") // the plan's notes: a new session, and the turn handed over
	h.next(t, "event")
	h.next(t, "result")
	h.exit(t, 0)
	if !strings.Contains(got.Prompt, "--- request\n> earlier") || got.RetrievalState != nil {
		t.Fatalf("the hand-over should carry the transcript and no pin: %+v", got)
	}
}
