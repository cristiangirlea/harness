package platform

import (
	"context"
	"encoding/json"
	"github.com/cristiangirlea/harness/internal/agent"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerExecMissingBinaryFinishesFailed(t *testing.T) {
	job := workspaceJob(strings.Repeat("d", 32))
	job.State, job.Generation = Running, 7
	var result agent.Result
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, body := 204, ""
		switch r.Method + " " + r.URL.Path {
		case "POST /internal/projects/" + job.ProjectID + "/claim":
			b, _ := json.Marshal(job)
			code, body = 200, string(b)
		case "GET /internal/projects/" + job.ProjectID + "/jobs/" + job.ID + "/checkpoint":
			code = 404
		case "POST /internal/jobs/" + job.ID + "/finish":
			var req finishRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			result = req.Result
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	w := Worker{ControlURL: "http://control", ProjectID: job.ProjectID, Token: strings.Repeat("a", 64), WorkerID: "worker", WorkspaceRoot: t.TempDir(), HRNPath: filepath.Join(t.TempDir(), "missing"), ModelURL: "http://localhost:1234", ModelKey: "fixture-key", HTTPClient: client}
	ran, err := w.RunOnce(context.Background())
	if !ran || err != nil || result.Status != agent.StatusError {
		t.Fatalf("run: %v %v result=%+v", ran, err, result)
	}
}
