package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func sendJSON(t *testing.T, client *http.Client, method, url, token string, input any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, output
}

func fakeModel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fixture completed"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(line), &event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, line)
	}
}

func TestHTTPClientDisconnectControlRestartAndWorkerCompletion(t *testing.T) {
	store := testStore(t)
	operator := "operator-token-abcdefghijklmnopqrstuvwxyz"
	scaler := "scaler-token-abcdefghijklmnopqrstuvwxyz12"
	handler := (&HTTPServer{Store: store, OperatorToken: operator, ScalerToken: scaler}).Handler()
	first := httptest.NewServer(handler)
	client := first.Client()
	status, raw := sendJSON(t, client, "POST", first.URL+"/v1/tenants", operator, map[string]string{"name": "fixture-tenant"})
	if status != 201 {
		t.Fatalf("create tenant %d: %s", status, raw)
	}
	var tenant struct {
		ID    string `json:"id"`
		Token string `json:"tenant_token"`
	}
	if err := json.Unmarshal(raw, &tenant); err != nil {
		t.Fatal(err)
	}
	registration := Project{TenantID: tenant.ID, Name: "fixture-project",
		RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"}
	status, raw = sendJSON(t, client, "POST", first.URL+"/v1/projects", operator, registration)
	if status != 201 {
		t.Fatalf("create project %d: %s", status, raw)
	}
	var created struct {
		Project Project `json:"project"`
		Token   string  `json:"worker_token"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	input := testSubmission(created.Project, "disconnect-key", "explain the fixture")
	status, raw = sendJSON(t, client, "POST", first.URL+"/v1/projects/"+created.Project.ID+"/jobs", tenant.Token, input)
	if status != 201 {
		t.Fatalf("submit %d: %s", status, raw)
	}
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	status, _ = sendJSON(t, client, "GET", first.URL+"/v1/jobs/"+job.ID+"?tenant_id="+tenant.ID+"&project_id="+created.Project.ID, "wrong-tenant-token", nil)
	if status != 404 {
		t.Fatalf("wrong tenant token read job: %d", status)
	}
	first.Close() // Client connection and original control process are gone.
	restarted := httptest.NewServer(handler)
	defer restarted.Close()
	client = restarted.Client()
	status, raw = sendJSON(t, client, "GET", restarted.URL+"/v1/jobs/"+job.ID+"?tenant_id="+tenant.ID+"&project_id="+created.Project.ID, tenant.Token, nil)
	if status != 200 {
		t.Fatalf("accepted job lost on control restart: %d %s", status, raw)
	}
	status, raw = sendJSON(t, client, "GET", restarted.URL+"/internal/scaling/projects/"+created.Project.ID, scaler, nil)
	if status != 200 || !bytes.Contains(raw, []byte(`"value":0`)) {
		t.Fatalf("queued job woke worker: %d %s", status, raw)
	}
	admitted, err := store.AdmitOne(context.Background())
	if err != nil || admitted != job.ID {
		t.Fatalf("admission: %s %v", admitted, err)
	}
	status, raw = sendJSON(t, client, "GET", restarted.URL+"/internal/scaling/projects/"+created.Project.ID, scaler, nil)
	if status != 200 || !bytes.Contains(raw, []byte(`"value":1`)) {
		t.Fatalf("admitted job invisible: %d %s", status, raw)
	}
	model := httptest.NewServer(http.HandlerFunc(fakeModel))
	defer model.Close()
	backend := agent.NewAPIBackend()
	backend.BaseURL = model.URL
	backend.APIKey = "fixture"
	backend.Tools = nil
	worker := &Worker{ControlURL: restarted.URL, ProjectID: created.Project.ID, Token: created.Token,
		WorkerID: "worker-fixture", WorkspaceRoot: t.TempDir(), Backend: backend, HTTPClient: client}
	ran, err := worker.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("worker: ran=%v err=%v", ran, err)
	}
	status, raw = sendJSON(t, client, "GET", restarted.URL+"/v1/jobs/"+job.ID+"?tenant_id="+tenant.ID+"&project_id="+created.Project.ID, tenant.Token, nil)
	if status != 200 {
		t.Fatalf("read final: %d %s", status, raw)
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != Succeeded || job.Result == nil || job.Result.Final != "fixture completed" || job.CheckpointSequence != 3 {
		t.Fatalf("job did not finish durably: %+v", job)
	}
	cp, err := store.LatestCheckpoint(context.Background(), tenant.ID, created.Project.ID, job.ID)
	if err != nil || !cp.Finished || cp.Result.Status != agent.StatusOK {
		t.Fatalf("final checkpoint: %+v %v", cp, err)
	}
	status, raw = sendJSON(t, client, "GET", restarted.URL+"/internal/scaling/projects/"+created.Project.ID, scaler, nil)
	if status != 200 || !bytes.Contains(raw, []byte(`"value":0`)) {
		t.Fatalf("completed job kept worker awake: %d %s", status, raw)
	}
	eventsURL := restarted.URL + "/v1/jobs/" + job.ID + "/events?tenant_id=" + tenant.ID + "&project_id=" + created.Project.ID
	status, raw = sendJSON(t, client, "GET", eventsURL, tenant.Token, nil)
	if status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	var page struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) < 5 {
		t.Fatalf("missing durable events: %+v", page.Events)
	}
	req, _ := http.NewRequest("GET", eventsURL+"&after=0", nil)
	req.Header.Set("Authorization", "Bearer "+tenant.Token)
	req.Header.Set("Last-Event-ID", "1")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("mismatched cursor: %d", response.StatusCode)
	}
	streamReq, _ := http.NewRequest("GET", eventsURL+"&after=0", nil)
	streamReq.Header.Set("Authorization", "Bearer "+tenant.Token)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := client.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(streamResp.Body)
	line, err := reader.ReadString('\n')
	streamResp.Body.Close()
	if err != nil || !strings.HasPrefix(line, "id: ") {
		t.Fatalf("SSE event: %q %v", line, err)
	}
}
