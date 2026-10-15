package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestWorkerRefusesStoredAllowDangerousAndStillRunsSafeJobs(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("allow_dangerous=%t", allow), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_GUARDED", "")
			// A claim represents an already stored job; submission validation is bypassed.
			job := workspaceJob(strings.Repeat("d", 32))
			job.State, job.Generation = Running, 1
			job.Options = agent.Options{Mode: agent.ModeReadOnly, MaxCostUSD: 1, AllowDangerous: allow}
			var result *agent.Result
			claims, finishes, checkpoints := 0, 0, 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				code, body := http.StatusNoContent, ""
				switch r.Method + " " + r.URL.Path {
				case "POST /internal/projects/" + job.ProjectID + "/claim":
					claims++
					if claims == 1 {
						data, err := json.Marshal(job)
						if err != nil {
							return nil, err
						}
						code, body = http.StatusOK, string(data)
					}
				case "GET /internal/projects/" + job.ProjectID + "/jobs/" + job.ID + "/checkpoint":
					code = http.StatusNotFound
				case "POST /internal/jobs/" + job.ID + "/checkpoint":
					checkpoints++
				case "POST /internal/jobs/" + job.ID + "/finish":
					var request finishRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					finishes++
					result = &request.Result
				default:
					return nil, fmt.Errorf("unexpected control request: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: code, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			var modelCalls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelCalls.Add(1)
				fakeModel(w, r)
			}))
			defer model.Close()
			backend := agent.NewAPIBackend()
			backend.BaseURL, backend.APIKey = model.URL, "fixture"
			worker := &Worker{ControlURL: "https://control.invalid", ProjectID: job.ProjectID,
				Token: strings.Repeat("a", 64), WorkerID: "fixture-worker", WorkspaceRoot: t.TempDir(),
				Backend: backend, HTTPClient: client}
			if ran, err := worker.RunOnce(context.Background()); !ran || err != nil {
				t.Fatalf("worker failed instead of finishing the job: ran=%v err=%v", ran, err)
			}
			if finishes != 1 || result == nil || result.RecoveryRequired {
				t.Fatalf("job did not receive one terminal result: finishes=%d result=%+v", finishes, result)
			}
			if allow {
				if result.Status != agent.StatusError || !strings.Contains(result.Error, "allow_dangerous is not supported for durable jobs") || modelCalls.Load() != 0 || checkpoints != 0 {
					t.Fatalf("stored dangerous job ran: result=%+v model_calls=%d checkpoints=%d", result, modelCalls.Load(), checkpoints)
				}
			} else if result.Status != agent.StatusOK || result.Final != "fixture completed" || modelCalls.Load() != 1 || checkpoints != 3 {
				t.Fatalf("safe job did not run: result=%+v model_calls=%d checkpoints=%d", result, modelCalls.Load(), checkpoints)
			}
			if ran, err := worker.RunOnce(context.Background()); ran || err != nil || finishes != 1 {
				t.Fatalf("worker could not poll after finishing: ran=%v err=%v finishes=%d", ran, err, finishes)
			}
		})
	}
}
