package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestFlowTaskLanesAndActions(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	for _, parallel := range []bool{true, false} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_OWNER", "local:test")
			id := "20261022-120000-aabbcc"
			run := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK, Turns: 3, ToolCalls: 2, CostUSD: 0.25})
			start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			res := batch.Result{ID: id, Owner: owner.Local(), Started: start, Duration: time.Minute}
			if err := os.MkdirAll(filepath.Dir(batch.FlowPath(id)), 0o755); err != nil {
				t.Fatal(err)
			}
			var recorded bytes.Buffer
			seq := 0
			emit := func(e flow.Event) {
				t.Helper()
				e.Owner, e.Batch = owner.Local(), id
				seq++
				if err := json.NewEncoder(&recorded).Encode(struct {
					flow.Event
					Version int `json:"v"`
					Seq     int `json:"seq"`
				}{e, flow.Version, seq}); err != nil {
					t.Fatal(err)
				}
			}
			emit(flow.Event{Time: start, Step: "batch", Kind: flow.KindBatch, Status: flow.StatusStarted})
			for i := range 3 {
				task := fmt.Sprintf("task%d", i)
				at := start.Add(time.Duration(i) * time.Second)
				if !parallel {
					at = start.Add(time.Duration(i) * 10 * time.Second)
				}
				status, result := flow.StatusOK, batch.StatusPass
				if i == 0 {
					status, result = flow.StatusFailed, batch.StatusFail
				}
				res.Tasks = append(res.Tasks, batch.TaskResult{ID: task, Status: result, Commit: "abc"})
				emit(flow.Event{Time: at, Step: task, Parent: "batch", Kind: flow.KindTask, Task: task, Name: task, Status: flow.StatusStarted})
				emit(flow.Event{Time: at, Step: task + ":attempt", Parent: task, Kind: flow.KindAttempt, Status: flow.StatusStarted})
				if i == 1 {
					emit(flow.Event{Time: at, Step: task + ":run", Parent: task + ":attempt", Kind: flow.KindRun, Run: run, Status: flow.StatusStarted})
					emit(flow.Event{Time: at.Add(10 * time.Second), Step: task + ":run", Kind: flow.KindRun, Status: flow.StatusOK})
				}
				emit(flow.Event{Time: at.Add(10 * time.Second), Step: task + ":attempt", Kind: flow.KindAttempt, Status: status})
				emit(flow.Event{Time: at.Add(10 * time.Second), Step: task, Kind: flow.KindTask, Status: status, Next: "inspect with hrn batch show " + id + " " + task})
			}
			emit(flow.Event{Time: start.Add(time.Minute), Step: "batch", Kind: flow.KindBatch, Status: flow.StatusOK})
			if err := os.WriteFile(batch.FlowPath(id), recorded.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			saveBatch(t, res)
			srv := httptest.NewServer(New(context.Background(), Options{}))
			defer srv.Close()
			for _, level := range []string{"simple", "technical", "debug"} {
				code, body := get(t, srv.URL+"/flow/"+id+"?level="+level)
				if code != 200 {
					t.Fatalf("page status %d", code)
				}
				for _, want := range []string{`class="flow-node lane-0"`, `data-lane="0"`, "10.0s", `<code>hrn batch show ` + id + `</code>`, `<code>hrn batch merge ` + id + ` task1</code>`, `<p class="muted small">3 turns · 2 tool calls · $0.2500</p>`} {
					if !strings.Contains(body, want) {
						t.Errorf("%s missing %q", level, want)
					}
				}
				for _, lane := range []string{"1", "2"} {
					if strings.Contains(body, `class="flow-node lane-`+lane+`" data-lane="`+lane+`"`) != parallel {
						t.Errorf("%s lane %s: parallel=%v", level, lane, parallel)
					}
				}
				if strings.Contains(body, "3 tasks ran side by side") != parallel {
					t.Errorf("%s parallel summary missing or misleading", level)
				}
				for _, unsafe := range []string{"<style", " style=", "<script>", "<form", "hrn batch retry", "--tasks", "hrn batch show " + id + " task"} {
					if strings.Contains(body, unsafe) {
						t.Errorf("%s contains %q", level, unsafe)
					}
				}
			}
		})
	}
}

func TestFlowRunNumbersAtEveryLevel(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK, Turns: 7, ToolCalls: 4, CostUSD: 0.0123, CostEstimate: true, Duration: time.Second})
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for _, level := range []string{"simple", "technical", "debug"} {
		_, body := get(t, srv.URL+"/flow/"+id+"?level="+level)
		if !strings.Contains(body, `<p class="muted small">7 turns · 4 tool calls · ~$0.0123</p>`) {
			t.Errorf("%s missing run numbers", level)
		}
	}
}

func TestFlowPageShowsTheRunsIssueEscapedAndOnlyToItsOwner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:other")
	foreign := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK})
	t.Setenv("HRN_OWNER", "local:test")
	id := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK})
	for n, run := range map[int]string{1: id, 2: foreign} {
		il, err := ws.TakeIssue(t.Context(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: n},
			ws.Lease{Holder: "hrn run", Run: run, Backend: "codex", Branch: "fix<b>"}, ws.IssueOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { il.Release(t.Context(), "") })
	}
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	_, body := get(t, srv.URL+"/flow/"+id)
	if want := `<p class="flow-issue">issue o/n#1: held by hrn ` + id + ` (codex) on fix&lt;b&gt; since `; !strings.Contains(body, want) {
		t.Errorf("page lacks %q", want)
	}
	if strings.Contains(body, "o/n#2") {
		t.Error("the page shows another owner's issue")
	}
	if _, js := get(t, srv.URL+"/flow/"+id+"/json"); !strings.Contains(js, `"issues":["issue o/n#1: held by hrn `) {
		t.Errorf("json = %s", js)
	}
	for _, path := range []string{"/flow/" + foreign, "/flow/" + foreign + "/json"} {
		if code, body := get(t, srv.URL+path); code != 404 || strings.Contains(body, "o/n#2") {
			t.Errorf("%s: another owner's run gave %d with its issue shown %t", path, code, strings.Contains(body, "o/n#2"))
		}
	}
}

func TestFlowLanesReuseAndFold(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		ends  []int
		lanes []int
		peak  int
	}{
		{"reuse", []int{10, 2, 3, 4}, []int{0, 1, 1, 1}, 2},
		{"fold", []int{10, 10, 10, 10, 10, 10, 10}, []int{0, 1, 2, 3, 4, 4, 4}, 7},
		{"unfinished", []int{-1, 2, 3}, []int{0, 1, 1}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_OWNER", "local:test")
			events := []flow.Event{{Time: start, Step: "batch", Kind: flow.KindBatch, Status: flow.StatusStarted}}
			for i, end := range tc.ends {
				step := fmt.Sprintf("task%d", i)
				events = append(events, flow.Event{Time: start.Add(time.Duration(i) * time.Second), Step: step, Parent: "batch", Kind: flow.KindTask, Status: flow.StatusStarted})
				if end >= 0 {
					events = append(events, flow.Event{Time: start.Add(time.Duration(end) * time.Second), Step: step, Kind: flow.KindTask, Status: flow.StatusOK})
				}
			}
			tree := buildFlowTree(events, "simple")
			for i, n := range tree[0].Children {
				if n.Lane != tc.lanes[i] {
					t.Errorf("task %d lane = %d, want %d", i, n.Lane, tc.lanes[i])
				}
			}
			if got := flowSummary(flow.Build(events)); !strings.Contains(got, fmt.Sprintf("%d tasks ran side by side", tc.peak)) {
				t.Errorf("peak summary = %q", got)
			}
		})
	}
}

func TestFlowRunNumbersRespectViewerAccessAndMissingCost(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:other")
	id := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK, Turns: 7, ToolCalls: 4})
	t.Setenv("HRN_OWNER", "local:test")
	tree := []flowNode{{Node: &flow.Node{Kind: flow.KindRun, Run: id}}}
	s := &server{}
	s.flowDetails(owner.LocalViewer(), id, "run", tree)
	if tree[0].Numbers != "" {
		t.Fatal("another owner's run metrics are visible")
	}
	t.Setenv("HRN_OWNER", "local:other")
	s.flowDetails(owner.LocalViewer(), id, "run", tree)
	if tree[0].Numbers != "7 turns · 4 tool calls" {
		t.Fatalf("missing cost should stay absent: %q", tree[0].Numbers)
	}
}

func TestFlowBatchPageAndJSON(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := "20261020-120000-aabbcc"
	saveBatch(t, batch.Result{ID: id, Owner: owner.Local(), Started: time.Now().Add(-time.Minute), Duration: time.Minute, Tasks: []batch.TaskResult{{ID: "fix", Prompt: "fix", Status: batch.StatusFail, Attempts: []batch.Attempt{{Backend: "claude", Result: agent.Result{Status: agent.StatusOK}, Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1, Output: "API_TOKEN=supersecretvalue123"}}}, {Backend: "codex", Result: agent.Result{Status: agent.StatusError, Error: "gate failed"}}}}}})
	events, err := batch.Timeline(owner.LocalViewer(), id)
	if err != nil {
		t.Fatal(err)
	}
	w, err := flow.Open(batch.FlowPath(id), flow.Event{Owner: owner.Local(), Batch: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		w.Emit(e)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/flow/"+id)
	if code != 200 {
		t.Fatalf("page status %d", code)
	}
	if strings.Contains(body, `data-stream=`) {
		t.Fatal("finished batch flow should not have a stream")
	}
	for _, want := range []string{"fix", "go test ./...", "Why:", "0 of 1 tasks passed.", "API_TOKEN="} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "supersecretvalue123") || !strings.Contains(body, "API_TOKEN=") {
		t.Fatal("secret mask failed")
	}
	code, jsonBody := get(t, srv.URL+"/flow/"+id+"/json")
	var data struct {
		Version int         `json:"version"`
		Tree    []flow.Node `json:"tree"`
	}
	if err := json.Unmarshal([]byte(jsonBody), &data); err != nil || data.Version != 1 || code != 200 {
		t.Fatalf("JSON %d %s %v", code, jsonBody, err)
	}
}

func TestFlowRunStreamsChangesUntilClosed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	w, err := runlog.Create("fake", agent.Task{Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	id := w.ID()
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/flow/"+id+"?level=debug")
	if code != 200 || !strings.Contains(body, `data-stream="/stream/flow/`+id+`?level=debug"`) {
		t.Fatalf("running flow page does not stream: %d\n%s", code, body)
	}
	s, stop := openStream(t, srv.URL+"/stream/flow/"+id+"?level=debug", "")
	defer stop()
	first, ok := s.next(t)
	if !ok || first.event != "replace" {
		t.Fatalf("first flow event = %#v, %v", first, ok)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Input: json.RawMessage(`{"command":"echo live"}`)})
	update, ok := s.next(t)
	if !ok || update.event != "replace" || !strings.Contains(update.data, "shell") {
		t.Fatalf("tool step update = %#v, %v", update, ok)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	for {
		end, ok := s.next(t)
		if !ok {
			t.Fatal("flow stream ended without end event")
		}
		if end.event == "end" {
			if end.data != "done" {
				t.Fatalf("flow end = %#v", end)
			}
			break
		}
	}
}

func TestFlowStreamWakesBeforeTheNextPoll(t *testing.T) {
	for _, kind := range []string{"run", "batch"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_OWNER", "local:test")
			var id string
			var appendStep func()
			if kind == "run" {
				w, err := runlog.Create("fake", agent.Task{Prompt: "prompt"})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close(agent.Result{})
				id = w.ID()
				appendStep = func() {
					w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "new live step"})
				}
			} else {
				id = "20261021-120000-aabbcc"
				saveBatch(t, batch.Result{ID: id, Owner: owner.Local(), Started: time.Now(), Tasks: []batch.TaskResult{{ID: "t", Status: batch.StatusRunning}}})
				w, err := flow.Open(batch.FlowPath(id), flow.Event{Owner: owner.Local(), Batch: id})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				w.Emit(flow.Event{Step: batch.BatchStep, Kind: flow.KindBatch, Status: flow.StatusStarted})
				appendStep = func() {
					w.Emit(flow.Event{Step: "live", Parent: batch.BatchStep, Kind: flow.KindGate, Status: flow.StatusStarted, Name: "new live step"})
				}
			}
			srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Second}))
			defer srv.Close()
			s, stop := openStream(t, srv.URL+"/stream/flow/"+id+"?level=debug", "")
			defer stop()
			if first, ok := s.next(t); !ok || first.event != "replace" {
				t.Fatalf("first flow event = %#v, %v", first, ok)
			}
			started := time.Now()
			appendStep()
			update, ok := s.next(t)
			elapsed := time.Since(started)
			if !ok || update.event != "replace" || !strings.Contains(update.data, "new live step") {
				t.Fatalf("step update = %#v, %v", update, ok)
			}
			if elapsed >= 2*time.Second {
				t.Fatalf("step arrived after %s, want less than 2s", elapsed)
			}
		})
	}
}

func TestFlowStreamMasksToolSecrets(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	w, err := runlog.Create("fake", agent.Task{Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Input: json.RawMessage(`{"API_TOKEN":"supersecretvalue123"}`), Text: "API_TOKEN=supersecretvalue123"})
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/flow/"+w.ID()+"?level=debug", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" || strings.Contains(m.data, "supersecretvalue123") || !strings.Contains(m.data, "••••") {
		t.Fatalf("masked flow = %#v, %v", m, ok)
	}
}

func TestRunFlowLinksEvidenceAndLocalAccess(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK})
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for _, path := range []string{"/flow/" + id, "/run/" + id} {
		code, body := get(t, srv.URL+path)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		if strings.Contains(path, "flow") && !strings.Contains(body, "/run/"+id) {
			t.Fatal("flow missing run evidence link")
		}
	}
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	if body := viewAs(t, h, "/flow/"+id, "192.168.1.20:4567"); !strings.Contains(body, "/run/"+id) {
		t.Fatal("LAN viewer cannot read local flow")
	}
}

func TestFlowSummaryBlamesOnlyTheLastAttemptOfAFailedTask(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	gateFail := batch.Attempt{Backend: "codex", Result: agent.Result{Status: agent.StatusOK}, Gates: []batch.GateResult{{Command: "go vet", ExitCode: 1}}}
	gatePass := batch.Attempt{Backend: "claude", Result: agent.Result{Status: agent.StatusOK}, Gates: []batch.GateResult{{Command: "go vet"}}}
	saveBatch(t, batch.Result{ID: "20261020-130000-aabbcc", Started: time.Now().Add(-time.Minute), Duration: time.Minute, Tasks: []batch.TaskResult{
		{ID: "retried", Status: batch.StatusPass, Commit: "abc", Attempts: []batch.Attempt{gateFail, gatePass}},
		{ID: "broken", Status: batch.StatusFail, Attempts: []batch.Attempt{gatePass, gateFail}},
	}})
	events, err := batch.Timeline(owner.LocalViewer(), "20261020-130000-aabbcc")
	if err != nil {
		t.Fatal(err)
	}
	got := flowSummary(flow.Build(events))
	want := "1 of 2 tasks passed. Task broken failed at gate go vet (exited 1). Next: review the diff with hrn batch show 20261020-130000-aabbcc retried, then merge it."
	if got != want {
		t.Fatalf("summary =\n%q\nwant\n%q", got, want)
	}
}

func TestFlowSummaryOfARunSaysHowItEnded(t *testing.T) {
	rec := []flow.Event{{Step: "run:r", Kind: flow.KindRun, Status: flow.StatusFailed, Text: "error · 2 turns · 1 tool calls · boom"}}
	if got := flowSummary(flow.Build(rec)); got != "The run failed: error · 2 turns · 1 tool calls · boom." {
		t.Fatalf("summary = %q", got)
	}
}

func TestFlowStreamRidesOutAResultCaughtMidWrite(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	w, err := runlog.Create("fake", agent.Task{Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/flow/"+w.ID(), "")
	defer stop()
	if first, ok := s.next(t); !ok || first.event != "replace" {
		t.Fatalf("first flow event = %#v, %v", first, ok)
	}
	half := filepath.Join(w.Dir(), "result.json")
	if err := os.WriteFile(half, []byte(`{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	for {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("the stream ended without an end event")
		}
		if m.event == "problem" {
			t.Fatalf("a half-written result ended the stream with a problem: %s", m.data)
		}
		if m.event == "end" {
			if m.data != "done" {
				t.Fatalf("end = %q", m.data)
			}
			return
		}
	}
}

func TestFlowStreamRidesOutABatchCaughtMidSave(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := "20261021-110000-aabbcc"
	running := batch.Result{ID: id, Owner: owner.Local(), Started: time.Now(), Tasks: []batch.TaskResult{{ID: "t", Status: batch.StatusRunning}}}
	saveBatch(t, running)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/flow/"+id, "")
	defer stop()
	if first, ok := s.next(t); !ok || first.event != "replace" {
		t.Fatalf("first flow event = %#v, %v", first, ok)
	}
	if err := os.WriteFile(filepath.Join(runlog.Home(), "batches", id+".json"), []byte(`{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	done := running
	done.Tasks = []batch.TaskResult{{ID: "t", Status: batch.StatusPass, Duration: time.Second}}
	done.Duration = time.Second
	saveBatch(t, done)
	for {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("the stream ended without an end event")
		}
		if m.event == "problem" {
			t.Fatalf("a batch caught mid-save ended the stream: %s", m.data)
		}
		if m.event == "end" {
			return
		}
	}
}
