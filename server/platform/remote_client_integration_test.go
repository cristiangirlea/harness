package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRemoteClientAgainstPostgresControl(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	operatorToken := strings.Repeat("o", 64)
	scalerToken := strings.Repeat("s", 64)
	control := httptest.NewTLSServer((&HTTPServer{
		Store: store, OperatorToken: operatorToken, ScalerToken: scalerToken,
	}).Handler())
	defer control.Close()
	tenant, tenantToken, err := store.CreateTenant(ctx, "remote-a")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{
		TenantID: tenant, Name: "remote-project",
		RepositoryURL: "https://example.invalid/fixture.git",
		BaseRef:       fixtureRef, Backend: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &remoteRecording{}
	client := fixtureClient(control.URL, tenantToken, control.Client(), record)
	submission := testSubmission(project, "client-integration", "inspect")
	postPath := "/v1/projects/" + project.ID + "/jobs"
	raw, err := client.Request(ctx, http.MethodPost, postPath, submission)
	if err != nil {
		t.Fatal(err)
	}
	var created Job
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != Queued || created.Options.Mode != ModeReadOnly {
		t.Fatalf("submitted job: %+v", created)
	}
	raw, err = client.Request(ctx, http.MethodPost, postPath, submission)
	if err != nil {
		t.Fatal(err)
	}
	var duplicate Job
	if err := json.Unmarshal(raw, &duplicate); err != nil || duplicate.ID != created.ID {
		t.Fatalf("idempotent retry: %+v %v", duplicate, err)
	}
	submission.Prompt = "different"
	if _, err := client.Request(ctx, http.MethodPost, postPath, submission); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("conflicting retry: %v", err)
	}
	scope := "?tenant_id=" + tenant + "&project_id=" + project.ID
	raw, err = client.Request(ctx, http.MethodGet, "/v1/jobs/"+created.ID+scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	var shown Job
	if err := json.Unmarshal(raw, &shown); err != nil || shown.ID != created.ID {
		t.Fatalf("status: %+v %v", shown, err)
	}
	raw, err = client.Request(ctx, http.MethodGet, "/v1/jobs/"+created.ID+"/events"+scope+"&after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Events) != 1 || page.Events[0].Kind != "queued" {
		t.Fatalf("events: %+v %v", page, err)
	}
	otherTenant, otherToken, err := store.CreateTenant(ctx, "remote-b")
	if err != nil {
		t.Fatal(err)
	}
	otherClient := fixtureClient(control.URL, otherToken, control.Client(), record)
	if _, err := otherClient.Request(ctx, http.MethodGet, "/v1/jobs/"+created.ID+scope, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("other tenant read: %v", err)
	}
	if _, err := otherClient.Request(ctx, http.MethodGet, "/v1/jobs/"+created.ID+"?tenant_id="+otherTenant+"&project_id="+project.ID, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("other tenant ID substitution: %v", err)
	}
	raw, err = client.Request(ctx, http.MethodPost, "/v1/jobs/"+created.ID+"/cancel"+scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	var canceled map[string]string
	if err := json.Unmarshal(raw, &canceled); err != nil || canceled["state"] != "canceled" {
		t.Fatalf("cancel: %+v %v", canceled, err)
	}
	record.save(t, strings.NewReplacer(tenant, "<tenant-id>", project.ID, "<project-id>", created.ID, "<job-id>", otherTenant, "<other-tenant-id>", tenantToken, "<tenant-token>", otherToken, "<other-tenant-token>", control.URL, "<server-url>"))
}

type remoteExchange struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}
type remoteRecording struct{ exchanges []remoteExchange }

var remoteTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

func (r *remoteRecording) save(t *testing.T, replacements *strings.Replacer) {
	t.Helper()
	raw, err := json.Marshal(r.exchanges)
	if err != nil {
		t.Fatal(err)
	}
	normalized := replacements.Replace(string(raw))
	normalized = remoteTimestamp.ReplaceAllString(normalized, "<time>")
	var value any
	if err := json.Unmarshal([]byte(normalized), &value); err != nil {
		t.Fatal(err)
	}
	raw, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", "remote", "client.json")
	if os.Getenv("HRN_UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	expected, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Fatalf("missing %s; run with HRN_UPDATE_FIXTURES=1", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(expected, raw) {
		t.Errorf("fixture %s differs\n- expected:\n%s\n+ actual:\n%s", path, expected, raw)
	}
}

type testRemoteClient struct {
	url, token string
	client     *http.Client
	record     *remoteRecording
}

func fixtureClient(url, token string, client *http.Client, record *remoteRecording) *testRemoteClient {
	return &testRemoteClient{url, token, client, record}
}
func (c *testRemoteClient) Request(ctx context.Context, method, path string, value any) ([]byte, error) {
	var body *bytes.Reader
	if value != nil {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	requestBody := json.RawMessage("null")
	if value != nil {
		requestBody, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("response is not JSON: status %d", resp.StatusCode)
	}
	c.record.exchanges = append(c.record.exchanges, remoteExchange{method, path, requestBody, resp.StatusCode, json.RawMessage(b)})
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return b, nil
}
