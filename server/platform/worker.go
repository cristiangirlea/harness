package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

// Worker talks only to the control service: no database connection and, for now, no tools.
type Worker struct {
	ControlURL    string
	ProjectID     string
	Token         string
	WorkerID      string
	WorkspaceRoot string
	Backend       *agent.APIBackend
	HTTPClient    *http.Client
}

func (w *Worker) client() *http.Client {
	if w.HTTPClient != nil {
		return w.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (w *Worker) call(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.ControlURL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+w.Token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *w.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return response.StatusCode, fmt.Errorf("control service %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	if output != nil && response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(response.Body, 20<<20)).Decode(output); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.ProjectID == "" || w.WorkerID == "" || len(w.Token) != 64 || w.Backend == nil || w.WorkspaceRoot == "" {
		return false, errors.New("incomplete worker configuration")
	}
	endpoint, endpointErr := url.Parse(w.Backend.BaseURL)
	if endpointErr != nil || endpoint == nil || endpoint.Scheme != "http" || endpoint.User != nil ||
		!(endpoint.Hostname() == "localhost" || (net.ParseIP(endpoint.Hostname()) != nil && net.ParseIP(endpoint.Hostname()).IsLoopback())) {
		return false, fmt.Errorf("%w: initial worker requires a local fake API endpoint", ErrUnsupported)
	}
	root, err := openWorkspaceRoot(w.WorkspaceRoot)
	if err != nil {
		return false, err
	}
	root.Close()
	var job Job
	code, err := w.call(ctx, http.MethodPost, "/internal/projects/"+w.ProjectID+"/claim", map[string]string{"worker_id": w.WorkerID}, &job)
	if err != nil {
		return false, err
	}
	if code == http.StatusNoContent {
		return false, nil
	}
	if job.ProjectID != w.ProjectID || job.State != Running || job.Backend != "api" {
		return true, errors.New("control service returned an invalid claim")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var state struct {
					CancelRequested bool `json:"cancel_requested"`
				}
				_, err := w.call(runCtx, http.MethodPost, "/internal/jobs/"+job.ID+"/heartbeat",
					workerRequest{ProjectID: w.ProjectID, WorkerID: w.WorkerID, Generation: job.Generation}, &state)
				if err != nil || state.CancelRequested {
					cancel()
					return
				}
			}
		}
	}()
	var checkpoint *agent.APICheckpoint
	status, loadErr := w.call(runCtx, http.MethodGet, "/internal/projects/"+w.ProjectID+"/jobs/"+job.ID+"/checkpoint", nil, &checkpoint)
	if loadErr != nil && status != http.StatusNotFound {
		cancel()
		<-heartbeatDone
		return true, loadErr
	}
	dir, workspaceErr := prepareWorkspace(w.WorkspaceRoot, job, checkpoint != nil)
	if workspaceErr != nil {
		cancel()
		<-heartbeatDone
		if finishErr := w.finishWorkspaceRecovery(job); finishErr != nil {
			return true, fmt.Errorf("workspace validation: %w; recovery update: %v", workspaceErr, finishErr)
		}
		return true, fmt.Errorf("workspace validation: %w", workspaceErr)
	}
	task := agent.Task{Prompt: job.Prompt, Dir: dir, Options: job.Options}
	task.Durable = &agent.DurableExecution{Resume: checkpoint, Save: func(saveCtx context.Context, cp agent.APICheckpoint) error {
		if _, err := prepareWorkspace(w.WorkspaceRoot, job, true); err != nil {
			return fmt.Errorf("workspace changed before checkpoint: %w", err)
		}
		_, err := w.call(saveCtx, http.MethodPost, "/internal/jobs/"+job.ID+"/checkpoint",
			checkpointRequest{workerRequest: workerRequest{ProjectID: w.ProjectID, WorkerID: w.WorkerID, Generation: job.Generation}, Checkpoint: cp}, nil)
		return err
	}}
	// There is no trusted tool-execution sidecar yet. Keep the model in a
	// reasoning-only mode even if an operator accidentally supplied tools.
	w.Backend.Tools = nil
	result := agent.Run(runCtx, w.Backend, task, func(agent.Event) {})
	cancel()
	<-heartbeatDone
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer finishCancel()
	_, err = w.call(finishCtx, http.MethodPost, "/internal/jobs/"+job.ID+"/finish",
		finishRequest{workerRequest: workerRequest{ProjectID: w.ProjectID, WorkerID: w.WorkerID, Generation: job.Generation}, Result: result}, nil)
	if err != nil {
		return true, err
	}
	return true, nil
}

func (w *Worker) finishWorkspaceRecovery(job Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := w.call(ctx, http.MethodPost, "/internal/jobs/"+job.ID+"/finish",
		finishRequest{workerRequest: workerRequest{ProjectID: w.ProjectID, WorkerID: w.WorkerID, Generation: job.Generation},
			Result: agent.Result{Backend: "api", Status: agent.StatusError, RecoveryRequired: true,
				Error: "workspace identity could not be verified"}}, nil)
	return err
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	transportFailures := 0
	for {
		didWork, err := w.RunOnce(ctx)
		if err != nil {
			var transportErr *url.Error
			if !didWork && errors.As(err, &transportErr) && ctx.Err() == nil && transportFailures < 5 {
				transportFailures++
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(transportFailures) * time.Second):
					continue
				}
			}
			return err
		}
		transportFailures = 0
		if didWork {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
