package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestPostgresJobSnapshotsRegisteredRepository(t *testing.T) {
	store := testStore(t)
	project, _ := testProject(t, store)
	ctx := context.Background()
	job, _, err := store.Submit(ctx, testSubmission(project, "repo-identity", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	if job.RepositoryURL != project.RepositoryURL {
		t.Fatalf("job lost registered repository: %q", job.RepositoryURL)
	}
	root := t.TempDir()
	dir, err := prepareWorkspace(root, job, false)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(ctx, "UPDATE projects SET repository_url=$1 WHERE id=$2 AND tenant_id=$3",
		"https://example.invalid/other.git", project.ID, project.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.RepositoryURL != job.RepositoryURL {
		t.Fatalf("accepted job inherited a later project URL: %q", retained.RepositoryURL)
	}
	if _, err := prepareWorkspace(root, retained, true); err != nil {
		t.Fatalf("accepted job lost its bound workspace: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
		t.Fatalf("untracked file changed: %q %v", data, err)
	}
	newJob, _, err := store.Submit(ctx, testSubmission(project, "new-repo-identity", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	if newJob.RepositoryURL != "https://example.invalid/other.git" {
		t.Fatalf("new job did not capture updated project URL: %q", newJob.RepositoryURL)
	}
}

func TestWorkerStopsOrphanWorkspaceBeforeModelCall(t *testing.T) {
	store := testStore(t)
	project, workerToken := testProject(t, store)
	ctx := context.Background()
	job, _, err := store.Submit(ctx, testSubmission(project, "orphan-workspace", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workspaces", job.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	control := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer control.Close()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		fakeModel(w, r)
	}))
	defer model.Close()
	backend := agent.NewAPIBackend()
	backend.BaseURL = model.URL
	backend.APIKey = "fixture"
	worker := &Worker{
		ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "workspace-fixture", WorkspaceRoot: root, Backend: backend,
	}
	ran, err := worker.RunOnce(ctx)
	if !ran || err == nil || !strings.Contains(err.Error(), "workspace validation") {
		t.Fatalf("orphaned workspace: ran=%v err=%v", ran, err)
	}
	if modelCalls.Load() != 0 {
		t.Fatalf("model called before workspace verification: %d", modelCalls.Load())
	}
	observed, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || observed.State != RecoveryRequired || observed.Result == nil || !observed.Result.RecoveryRequired {
		t.Fatalf("job was not fenced for recovery: %+v %v", observed, err)
	}
}

func TestWorkspaceLossDuringModelCallRequiresRecovery(t *testing.T) {
	store := testStore(t)
	project, workerToken := testProject(t, store)
	ctx := context.Background()
	job, _, err := store.Submit(ctx, testSubmission(project, "workspace-loss", "two turns"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manifest := filepath.Join(root, ".hrn-control", job.ID+".json")
	control := httptest.NewServer((&HTTPServer{Store: store}).Handler())
	defer control.Close()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		if err := os.Remove(manifest); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fakeTurn(w, "pause", "pause_turn")
	}))
	defer model.Close()
	backend := agent.NewAPIBackend()
	backend.BaseURL = model.URL
	backend.APIKey = "fixture"
	worker := &Worker{
		ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "workspace-loss-fixture", WorkspaceRoot: root, Backend: backend,
	}
	ran, err := worker.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("worker result: ran=%v err=%v", ran, err)
	}
	if modelCalls.Load() != 1 {
		t.Fatalf("workspace loss allowed another model call: %d", modelCalls.Load())
	}
	observed, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || observed.State != RecoveryRequired || observed.Result == nil || !observed.Result.RecoveryRequired {
		t.Fatalf("lost workspace was not fenced: %+v %v", observed, err)
	}
	cp, err := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || cp.Boundary != "model_intent" || cp.Pending == nil || cp.Pending.Kind != "model" {
		t.Fatalf("model outcome was falsely acknowledged: %+v %v", cp, err)
	}
}
