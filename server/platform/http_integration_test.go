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

func TestHTTPWrongOperatorAndScalerTokensOfValidLengthAreRefused(t *testing.T) {
	store := testStore(t)
	operator := "operator-token-abcdefghijklmnopqrstuvwxyz"
	scaler := "scaler-token-abcdefghijklmnopqrstuvwxyz12"
	server := httptest.NewServer((&HTTPServer{Store: store, OperatorToken: operator, ScalerToken: scaler}).Handler())
	defer server.Close()
	for _, tc := range []struct {
		name, path, token string
	}{
		{"wrong operator token", "/v1/tenants", "operator-token-abcdefghijklmnopqrstuvwxzz"},
		{"wrong scaler token", "/internal/scaling/projects/missing", "scaler-token-abcdefghijklmnopqrstuvwxzz12"},
		{"scaler token on operator route", "/v1/tenants", scaler},
		{"operator token on scaling route", "/internal/scaling/projects/missing", operator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := "GET"
			if tc.path == "/v1/tenants" {
				method = "POST"
			}
			status, raw := sendJSON(t, server.Client(), method, server.URL+tc.path, tc.token, map[string]string{"name": "denied"})
			if status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401: %s", status, raw)
			}
		})
	}
}

func TestHTTPCreateProjectUnknownTenant(t *testing.T) {
	store := testStore(t)
	operator := "operator-token-abcdefghijklmnopqrstuvwxyz"
	server := httptest.NewServer((&HTTPServer{Store: store, OperatorToken: operator}).Handler())
	defer server.Close()

	missingTenant, err := newID("t_")
	if err != nil {
		t.Fatal(err)
	}
	project := Project{TenantID: missingTenant, Name: "missing-tenant",
		RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"}
	status, raw := sendJSON(t, server.Client(), "POST", server.URL+"/v1/projects", operator, project)
	if status != http.StatusNotFound || !bytes.Contains(raw, []byte(`"code":"not_found"`)) ||
		!bytes.Contains(raw, []byte(`"message":"tenant or resource not found"`)) {
		t.Fatalf("unknown tenant: %d %s", status, raw)
	}
}

func TestHTTPScalingDemandUnknownProject(t *testing.T) {
	store := testStore(t)
	scaler := "scaler-token-abcdefghijklmnopqrstuvwxyz12"
	server := httptest.NewServer((&HTTPServer{Store: store, ScalerToken: scaler}).Handler())
	defer server.Close()

	missingProject, err := newID("p_")
	if err != nil {
		t.Fatal(err)
	}
	status, raw := sendJSON(t, server.Client(), "GET", server.URL+"/internal/scaling/projects/"+missingProject, scaler, nil)
	if status != http.StatusNotFound || !bytes.Contains(raw, []byte(`"code":"not_found"`)) ||
		!bytes.Contains(raw, []byte(`"message":"project or resource not found"`)) {
		t.Fatalf("unknown project: %d %s", status, raw)
	}
}

func TestHTTPSubmitAuthorizationWithRealStorePrecedesDecode(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, tenantToken, err := store.CreateTenant(ctx, "submit-auth-owner")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := store.CreateTenant(ctx, "submit-auth-other")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{TenantID: tenantID, Name: "submit-auth-project",
		RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"})
	if err != nil {
		t.Fatal(err)
	}
	server := (&HTTPServer{Store: store}).Handler()
	const unknown = "secret_unknown_field"
	const body = `{"secret_unknown_field":true}`
	const missingProject = "00000000-0000-4000-8000-000000000000"
	var badTokenResponse string
	for _, tc := range []struct {
		name, projectID, token string
	}{
		{"bad token", project.ID, "bad-token-abcdefghijklmnopqrstuvwxyz0"},
		{"unknown project", missingProject, tenantToken},
		{"another tenant", project.ID, otherToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/projects/"+tc.projectID+"/jobs", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
			}
			response := w.Body.String()
			if strings.Contains(response, unknown) || strings.Contains(response, "unknown field") {
				t.Fatalf("authorization refusal leaked request field: %s", response)
			}
			if tc.name == "bad token" {
				badTokenResponse = response
			} else if response != badTokenResponse {
				t.Fatalf("authorization refusal %q differs from bad-token response %q", response, badTokenResponse)
			}
		})
	}
}

func TestHTTPTenantCannotReadCancelOrStreamAnotherTenantsJob(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, tenantAToken, err := store.CreateTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	projectB, _ := testProject(t, store)
	job, _, err := store.Submit(ctx, testSubmission(projectB, "", "private job"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer server.Close()
	url := server.URL + "/v1/jobs/" + job.ID + "?tenant_id=" + projectB.TenantID + "&project_id=" + projectB.ID
	for _, tc := range []struct{ name, method, path string }{
		{"read", "GET", url},
		{"cancel", "POST", server.URL + "/v1/jobs/" + job.ID + "/cancel?tenant_id=" + projectB.TenantID + "&project_id=" + projectB.ID},
		{"stream events", "GET", server.URL + "/v1/jobs/" + job.ID + "/events?tenant_id=" + projectB.TenantID + "&project_id=" + projectB.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tenantAToken)
			if tc.name == "stream events" {
				req.Header.Set("Accept", "text/event-stream")
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, want 404", response.StatusCode)
			}
		})
	}
}

func TestHTTPProjectWorkerCannotActOnAnotherProjectsJob(t *testing.T) {
	store := testStore(t)
	projectA, workerAToken := testProject(t, store)
	projectB, _ := testProject(t, store)
	job, _, err := store.Submit(context.Background(), testSubmission(projectB, "", "private job"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer server.Close()
	worker := workerRequest{ProjectID: projectB.ID, WorkerID: "worker-a", Generation: 1}
	for _, tc := range []struct {
		name, path string
		body       any
	}{
		{"claim", "/internal/projects/" + projectB.ID + "/claim", map[string]string{"worker_id": "worker-a"}},
		{"heartbeat", "/internal/jobs/" + job.ID + "/heartbeat", worker},
		{"checkpoint", "/internal/jobs/" + job.ID + "/checkpoint", checkpointRequest{worker, agent.APICheckpoint{Version: 1, Sequence: 1, Boundary: "model_intent", TaskDigest: "digest"}}},
		{"finish", "/internal/jobs/" + job.ID + "/finish", finishRequest{worker, agent.Result{Backend: "api", Status: agent.StatusError}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := sendJSON(t, server.Client(), "POST", server.URL+tc.path, workerAToken, tc.body)
			if status != http.StatusNotFound {
				t.Fatalf("project %s token against project B: status %d, want 404: %s", projectA.ID, status, raw)
			}
		})
	}
}

func TestHTTPSubmissionRejectsNonReadonlyModeAndZeroCost(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, tenantToken, err := store.CreateTenant(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{TenantID: tenantID, Name: "project", RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer server.Close()
	url := server.URL + "/v1/projects/" + project.ID + "/jobs"
	for _, tc := range []struct {
		name string
		edit func(*Submission)
		want int
	}{
		{"non readonly mode", func(input *Submission) { input.Options.Mode = "invalid" }, http.StatusBadRequest},
		{"zero cost ceiling", func(input *Submission) { input.Options.MaxCostUSD = 0 }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testSubmission(project, "", "inspect")
			tc.edit(&input)
			status, raw := sendJSON(t, server.Client(), "POST", url, tenantToken, input)
			if status != tc.want {
				t.Fatalf("status %d, want %d: %s", status, tc.want, raw)
			}
		})
	}
}

func TestHTTPSubmissionRefusesUnsupportedOptionsWith422(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, tenantToken, err := store.CreateTenant(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{TenantID: tenantID, Name: "project", RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer server.Close()
	url := server.URL + "/v1/projects/" + project.ID + "/jobs"
	for _, tc := range []struct {
		name string
		edit func(*Submission)
		want string
	}{
		{"allow dangerous", func(input *Submission) { input.Options.AllowDangerous = true }, "unsupported remote execution mode: allow_dangerous is not supported for durable jobs"},
		{"retrieval", func(input *Submission) { input.Options.Retrieval = "auto" }, "unsupported remote execution mode: retrieval is not supported for durable jobs"},
		{"untrusted retrieval", func(input *Submission) { input.Options.Retrieval = "untrusted-value\n<script>" }, "unsupported remote execution mode: retrieval is not supported for durable jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testSubmission(project, "", "untrusted prompt must not be echoed")
			tc.edit(&input)
			status, raw := sendJSON(t, server.Client(), http.MethodPost, url, tenantToken, input)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", status, raw)
			}
			var response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != "unsupported" || response.Error.Message != tc.want {
				t.Fatalf("error = %+v, want code unsupported and message %q", response.Error, tc.want)
			}
		})
	}
}

func TestHTTPSubmissionAboveThePerJobCostLimitIs422(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, tenantToken, err := store.CreateTenant(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{TenantID: tenantID, Name: "project", RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store, MaxCostUSDPerJob: 0.5}).Handler())
	defer server.Close()
	url := server.URL + "/v1/projects/" + project.ID + "/jobs"
	status, raw := sendJSON(t, server.Client(), http.MethodPost, url, tenantToken, testSubmission(project, "over-limit", "inspect"))
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "cost_limit") || !strings.Contains(string(raw), "HRN_MAX_COST_USD_PER_JOB") {
		t.Fatalf("over the limit: status %d: %s", status, raw)
	}
	input := testSubmission(project, "at-limit", "inspect")
	input.Options.MaxCostUSD = 0.5
	if status, raw := sendJSON(t, server.Client(), http.MethodPost, url, tenantToken, input); status != http.StatusCreated {
		t.Fatalf("at the limit: status %d: %s", status, raw)
	}
}

func TestHTTPCheckpointRejectsSkippedAndReplayedSequences(t *testing.T) {
	store := testStore(t)
	project, workerToken := testProject(t, store)
	ctx := context.Background()
	job, _, err := store.Submit(ctx, testSubmission(project, "", "checkpoint order"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, project.ID, "owner")
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	server := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer server.Close()
	url := server.URL + "/internal/jobs/" + job.ID + "/checkpoint"
	request := checkpointRequest{workerRequest{ProjectID: project.ID, WorkerID: "owner", Generation: claimed.Generation},
		agent.APICheckpoint{Version: 1, Sequence: 2, Boundary: "model_intent", TaskDigest: "digest"}}
	status, raw := sendJSON(t, server.Client(), "POST", url, workerToken, request)
	if status != http.StatusConflict {
		t.Fatalf("skipped sequence: status %d, want 409: %s", status, raw)
	}
	request.Checkpoint.Sequence = 1
	status, raw = sendJSON(t, server.Client(), "POST", url, workerToken, request)
	if status != http.StatusOK {
		t.Fatalf("first sequence: status %d, want 200: %s", status, raw)
	}
	status, raw = sendJSON(t, server.Client(), "POST", url, workerToken, request)
	if status != http.StatusConflict {
		t.Fatalf("replayed sequence: status %d, want 409: %s", status, raw)
	}
}
