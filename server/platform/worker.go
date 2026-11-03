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
	"os"
	"os/exec"
	"path/filepath"
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
	HRNPath       string
	ModelURL      string
	ModelKey      string
	HTTPClient    *http.Client
	cancelGrace   time.Duration
}

func (w *Worker) PrepareHRNHome() error {
	path := filepath.Join(w.WorkspaceRoot, ".hrn-home")
	root, err := openWorkspaceRoot(w.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("hrn home %s: %w", path, err)
	}
	defer root.Close()
	if err := ensureWorkspaceDirectory(root, ".hrn-home"); err != nil {
		return fmt.Errorf("hrn home %s: %w", path, err)
	}
	f, err := os.CreateTemp(path, ".write-check-")
	if err != nil {
		return fmt.Errorf("hrn home %s: %w", path, err)
	}
	name := f.Name()
	if err = f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("hrn home %s: %w", path, err)
	}
	if err = os.Remove(name); err != nil {
		return fmt.Errorf("hrn home %s: %w", path, err)
	}
	return nil
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= 4096 {
		b.data = append(b.data[:0], p[len(p)-4096:]...)
		return n, nil
	}
	if len(b.data)+len(p) > 4096 {
		b.data = append(b.data[:0], b.data[len(b.data)+len(p)-4096:]...)
	}
	b.data = append(b.data, p...)
	return n, nil
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
	if w.ProjectID == "" || w.WorkerID == "" || len(w.Token) != 64 || w.ModelURL == "" || w.WorkspaceRoot == "" {
		return false, errors.New("incomplete worker configuration")
	}
	endpoint, endpointErr := url.Parse(w.ModelURL)
	if endpointErr != nil || endpoint == nil || endpoint.Scheme != "http" || endpoint.User != nil ||
		!(endpoint.Hostname() == "localhost" || (net.ParseIP(endpoint.Hostname()) != nil && net.ParseIP(endpoint.Hostname()).IsLoopback())) {
		return false, fmt.Errorf("%w: initial worker requires a local fake API endpoint", ErrUnsupported)
	}
	if err := w.PrepareHRNHome(); err != nil {
		return false, err
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
	attemptID := fmt.Sprintf("%s-%d", job.ID, job.Generation)
	start, startErr := workerStart(job, attemptID, dir, checkpoint)
	result := agent.Result{Backend: "api", Status: agent.StatusError}
	if job.Options.AllowDangerous {
		result.Error = "allow_dangerous is not supported for durable jobs"
	} else if startErr != nil {
		result.Error = startErr.Error()
	} else {
		cmd := exec.CommandContext(runCtx, w.HRNPath, "exec", "--protocol", "v1", "--worker")
		for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE"} {
			if value, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		cmd.Env = append(cmd.Env, "HRN_HOME="+filepath.Join(w.WorkspaceRoot, ".hrn-home"), "ANTHROPIC_BASE_URL="+w.ModelURL, "ANTHROPIC_API_KEY="+w.ModelKey)
		var stderr tailBuffer
		cmd.Stderr = &stderr
		input, pipeErr := cmd.StdinPipe()
		if pipeErr == nil {
			var output io.ReadCloser
			output, pipeErr = cmd.StdoutPipe()
			if pipeErr == nil {
				client := workerClient{input: output, output: input, attemptID: attemptID, start: start, checkpoint: func(raw json.RawMessage) checkpointDecision {
					var cp agent.APICheckpoint
					if json.Unmarshal(raw, &cp) != nil {
						return checkpointRejected
					}
					if _, err := prepareWorkspace(w.WorkspaceRoot, job, true); err != nil {
						return checkpointRejected
					}
					_, err := w.call(runCtx, http.MethodPost, "/internal/jobs/"+job.ID+"/checkpoint", checkpointRequest{workerRequest: workerRequest{ProjectID: w.ProjectID, WorkerID: w.WorkerID, Generation: job.Generation}, Checkpoint: cp}, nil)
					if err != nil {
						return checkpointRejected
					}
					return checkpointSaved
				}}
				cmd.Cancel = func() error { return nil }
				grace := w.cancelGrace
				if grace <= 0 {
					grace = 5 * time.Second
				}
				cmd.WaitDelay = grace
				if pipeErr = cmd.Start(); pipeErr == nil {
					done := make(chan struct{})
					cancelDone := make(chan struct{})
					go func() {
						defer close(cancelDone)
						select {
						case <-runCtx.Done():
							cancelWriteDone := make(chan struct{})
							go func() { defer close(cancelWriteDone); _ = client.cancel() }()
							timer := time.NewTimer(grace)
							defer timer.Stop()
							select {
							case <-done:
							case <-timer.C:
								_ = cmd.Process.Kill()
							}
							<-cancelWriteDone
						case <-done:
						}
					}()
					outcome, runErr := client.run()
					close(done)
					_ = input.Close()
					waitErr := cmd.Wait()
					<-cancelDone
					result = client.result
					client.mu.Lock()
					canceledBeforeStart := client.canceledBeforeStart
					client.mu.Unlock()
					if canceledBeforeStart {
						result = agent.Result{Backend: "api", Status: "canceled"}
					} else if outcome == outcomeRecovery || runErr != nil || (waitErr != nil && !(outcome == outcomeCanceled && runCtx.Err() != nil)) {
						if result.Status == "" {
							result = agent.Result{Backend: "api", Status: agent.StatusError, Error: "worker execution requires recovery"}
						}
						result.RecoveryRequired = true
					} else if outcome == outcomeRefused {
						if result.Status == "" {
							result = agent.Result{Backend: "api", Status: agent.StatusError, Error: "worker refused start"}
						}
					}
				}
			}
		}
		if (result.Status == agent.StatusError || result.RecoveryRequired) && len(stderr.data) > 0 {
			result.Error += "; stderr: " + string(stderr.data)
		}
		if pipeErr != nil {
			result.Error = "start worker: " + pipeErr.Error()
		}
	}

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
