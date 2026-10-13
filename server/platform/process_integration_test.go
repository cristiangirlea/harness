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
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestWorkerHelperProcess(t *testing.T) {
	if os.Getenv("HRN_WORKER_HELPER") != "1" {
		return
	}
	backend := agent.NewAPIBackend()
	backend.BaseURL = os.Getenv("HRN_WORKER_MODEL")
	backend.APIKey = "fixture"
	backend.Tools = nil
	worker := &Worker{ControlURL: os.Getenv("HRN_WORKER_CONTROL"),
		ProjectID: os.Getenv("HRN_WORKER_PROJECT"), Token: os.Getenv("HRN_WORKER_TOKEN"),
		WorkerID: "killed-worker", WorkspaceRoot: os.Getenv("HRN_WORKER_ROOT"), Backend: backend}
	ran, err := worker.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("helper worker: %v %v", ran, err)
	}
}

func TestPostgresKilledWorkerResumesAtSafeBoundary(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, workerToken := testProject(t, store)
	job, _, err := store.Submit(ctx, testSubmission(project, "killed-worker", "two turns"))
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan struct{}, 1)
	release := make(chan struct{})
	base := (&HTTPServer{Store: store, OperatorToken: "operator-token-abcdefghijklmnopqrstuvwxyz",
		ScalerToken: "scaler-token-abcdefghijklmnopqrstuvwxyz12"}).Handler()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		block := false
		if strings.HasSuffix(r.URL.Path, "/checkpoint") && r.Method == "POST" {
			raw, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, readErr.Error(), 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request checkpointRequest
			if json.Unmarshal(raw, &request) == nil && request.Checkpoint.Boundary == "turn_complete" {
				block = true
			}
		}
		if !block {
			base.ServeHTTP(w, r)
			return
		}
		record := httptest.NewRecorder()
		base.ServeHTTP(record, r)
		if record.Code == 200 {
			select {
			case saved <- struct{}{}:
			default:
			}
			<-release
		}
		for key, values := range record.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(record.Code)
		_, _ = w.Write(record.Body.Bytes())
	}))
	defer control.Close()
	defer close(release)
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "workspaces", job.ID, "untracked.txt")
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := os.WriteFile(marker, []byte("retained after worker death"), 0o600); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			fakeTurn(w, "pause", "pause_turn")
			return
		}
		fakeTurn(w, "finished after process restart", "end_turn")
	}))
	defer model.Close()
	if admitted, err := store.AdmitOne(ctx); err != nil || admitted != job.ID {
		t.Fatalf("admit: %s %v", admitted, err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWorkerHelperProcess$")
	child.Env = append(os.Environ(),
		"HRN_WORKER_HELPER=1",
		"HRN_WORKER_CONTROL="+control.URL,
		"HRN_WORKER_MODEL="+model.URL,
		"HRN_WORKER_PROJECT="+project.ID,
		"HRN_WORKER_TOKEN="+workerToken,
		"HRN_WORKER_ROOT="+workspace)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-saved:
	case err := <-done:
		t.Fatalf("worker exited before checkpoint: %v %s", err, stderr.String())
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill()
		<-done
		t.Fatal("worker never saved turn_complete")
	}
	cp, err := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || cp.Boundary != "turn_complete" || cp.Pending != nil || cp.NextTurn != 2 || calls.Load() != 1 {
		_ = child.Process.Kill()
		<-done
		t.Fatalf("checkpoint before kill: %+v %v calls=%d", cp, err, calls.Load())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("helper unexpectedly exited cleanly")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained after worker death" {
		t.Fatalf("untracked file lost after killed worker: %q %v", data, err)
	}
	if _, err := store.db.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, project.ID, "new-worker"); err != ErrNotFound {
		t.Fatalf("killed worker job auto-reassigned: %v", err)
	}
	state, err := store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "resume",
		fmt.Sprintf("helper process %d killed and Wait confirmed exit", child.Process.Pid))
	if err != nil || state != Queued {
		t.Fatalf("operator recovery: %s %v", state, err)
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	next := agent.NewAPIBackend()
	next.BaseURL = model.URL
	next.APIKey = "fixture"
	next.Tools = nil
	worker := &Worker{ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "new-worker", WorkspaceRoot: workspace, Backend: next}
	ran, err := worker.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("replacement worker: %v %v", ran, err)
	}
	after, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || after.State != Succeeded || after.Generation != 2 || after.Result == nil ||
		after.Result.Final != "finished after process restart" || after.Result.Turns != 2 || calls.Load() != 2 {
		t.Fatalf("process recovery: %+v %v model calls=%d", after, err, calls.Load())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained after worker death" {
		t.Fatalf("untracked file lost on replacement worker: %q %v", data, err)
	}
}
