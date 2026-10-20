package watch

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
)

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
