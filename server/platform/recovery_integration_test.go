package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

type lostCheckpointAck struct {
	base http.RoundTripper
	once atomic.Bool
}

func (l *lostCheckpointAck) RoundTrip(req *http.Request) (*http.Response, error) {
	lose := false
	if strings.HasSuffix(req.URL.Path, "/checkpoint") && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return nil, err
		}
		var record checkpointRequest
		if json.Unmarshal(raw, &record) == nil && record.Checkpoint.Boundary == "turn_complete" && !l.once.Swap(true) {
			lose = true
		}
	}
	response, err := l.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if lose {
		response.Body.Close()
		return nil, errors.New("injected lost acknowledgement after database commit")
	}
	return response, nil
}

func fakeTurn(w http.ResponseWriter, text, stop string) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":20}}`, stop),
		`{"type":"message_stop"}`,
	}
	for _, event := range events {
		var kind struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &kind)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind.Type, event)
	}
}

func TestHTTPManualRecoveryAfterSafeCheckpoint(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, tenantToken, err := store.CreateTenant(ctx, "recovery-fixture")
	if err != nil {
		t.Fatal(err)
	}
	project, workerToken, err := store.CreateProject(ctx, Project{
		TenantID: tenantID, Name: "project", RepositoryURL: "https://example.invalid/repo.git",
		BaseRef: fixtureRef, Backend: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Submit(ctx, testSubmission(project, "safe-checkpoint", "two turns"))
	if err != nil {
		t.Fatal(err)
	}
	operator := "operator-token-abcdefghijklmnopqrstuvwxyz"
	control := httptest.NewServer((&HTTPServer{Store: store, OperatorToken: operator,
		ScalerToken: "scaler-token-abcdefghijklmnopqrstuvwxyz12"}).Handler())
	defer control.Close()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := modelCalls.Add(1)
		if count == 1 {
			fakeTurn(w, "wait", "pause_turn")
			return
		}
		fakeTurn(w, "completed", "end_turn")
	}))
	defer model.Close()
	api := func() *agent.APIBackend {
		b := agent.NewAPIBackend()
		b.BaseURL = model.URL
		b.APIKey = "fixture"
		b.Tools = nil
		return b
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	lost := &lostCheckpointAck{base: http.DefaultTransport}
	first := &Worker{ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "worker-first", WorkspaceRoot: t.TempDir(), Backend: api(),
		HTTPClient: &http.Client{Transport: lost}}
	// Both attempts must use the same persistent workspace root.
	root := first.WorkspaceRoot
	ran, err := first.RunOnce(ctx)
	if err != nil || !ran || !lost.once.Load() {
		t.Fatalf("first attempt: %v %v", ran, err)
	}
	observed, err := store.GetJob(ctx, tenantID, project.ID, job.ID)
	if err != nil || observed.State != RecoveryRequired {
		t.Fatalf("lost ACK was silently replayed: %+v %v", observed, err)
	}
	cp, err := store.LatestCheckpoint(ctx, tenantID, project.ID, job.ID)
	if err != nil || cp.Boundary != "turn_complete" || cp.Pending != nil || cp.NextTurn != 2 || modelCalls.Load() != 1 {
		t.Fatalf("safe checkpoint: %+v %v calls=%d", cp, err, modelCalls.Load())
	}
	if _, err = store.Claim(ctx, project.ID, "worker-second"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncertain execution reclaimed automatically: %v", err)
	}
	resume := map[string]string{
		"tenant_id": tenantID, "project_id": project.ID, "action": "resume",
		"termination_evidence": "first worker RunOnce returned and its heartbeat goroutine ended",
	}
	status, raw := sendJSON(t, control.Client(), "POST", control.URL+"/v1/jobs/"+job.ID+"/resume", tenantToken, resume)
	if status != 401 {
		t.Fatalf("tenant could override recovery: %d %s", status, raw)
	}
	status, raw = sendJSON(t, control.Client(), "POST", control.URL+"/v1/jobs/"+job.ID+"/resume", operator, resume)
	if status != 200 {
		t.Fatalf("operator resume: %d %s", status, raw)
	}
	if _, err = store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "workspaces", job.ID, "untracked.txt")
	if err := os.WriteFile(marker, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := &Worker{ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "worker-second", WorkspaceRoot: root, Backend: api()}
	ran, err = second.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("second attempt: %v %v", ran, err)
	}
	observed, err = store.GetJob(ctx, tenantID, project.ID, job.ID)
	if err != nil || observed.State != Succeeded || observed.Generation != 2 || observed.Result == nil ||
		observed.Result.Turns != 2 || observed.Result.InputTokens != 200 || observed.Result.Final != "completed" {
		t.Fatalf("resume did not preserve execution: %+v %v", observed, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
		t.Fatalf("untracked workspace file lost on worker restart: %q %v", data, err)
	}
	if modelCalls.Load() != 2 {
		t.Fatalf("model request repeated: %d calls", modelCalls.Load())
	}
	events, err := store.Events(ctx, tenantID, project.ID, job.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "recovery_resolved" && bytes.Contains(event.Details, []byte("worker RunOnce returned")) {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery evidence not audited")
	}
}

func TestPostgresUnresolvedEffectCannotResume(t *testing.T) {
	store := testStore(t)
	project, _ := testProject(t, store)
	ctx := context.Background()
	job, _, err := store.Submit(ctx, testSubmission(project, "", "effect"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(ctx, project.ID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	cp := agent.APICheckpoint{Version: 1, Sequence: 1, Boundary: "tool_intent", TaskDigest: "digest",
		Pending: &agent.PendingOperation{Kind: "tool", ID: "tool-1"}, NextTurn: 1}
	if err = store.SaveCheckpoint(ctx, project.ID, job.ID, "worker-a", claim.Generation, cp); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, project.ID, job.ID, "worker-a", claim.Generation,
		agent.Result{Backend: "api", Status: agent.StatusError, RecoveryRequired: true}); err != nil {
		t.Fatal(err)
	}
	_, err = store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "resume", "old worker stopped")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("pending effect resumed: %v", err)
	}
	state, err := store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "fail", "old worker stopped; operation result unknown")
	if err != nil || state != Failed {
		t.Fatalf("failed recovery: %s %v", state, err)
	}
}
