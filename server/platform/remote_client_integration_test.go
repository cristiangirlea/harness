package platform

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/remote"
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
	caPath := filepath.Join(t.TempDir(), "control-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: control.Certificate().Raw,
	}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", control.URL)
	t.Setenv("HRN_SERVER_CA_FILE", caPath)
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
	t.Setenv("HRN_TENANT_TOKEN", tenantToken)
	client, err := remote.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
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
	if created.ID == "" || created.State != Queued || created.Options.Mode != agent.ModeReadOnly {
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
	t.Setenv("HRN_TENANT_TOKEN", otherToken)
	otherClient, err := remote.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
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
}
