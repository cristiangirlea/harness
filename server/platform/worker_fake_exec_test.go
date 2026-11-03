package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func init() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if strings.HasPrefix(name, "fakehrn-") {
		fakeHRN(strings.TrimPrefix(name, "fakehrn-"))
		os.Exit(0)
	}
}
func fakeHRN(scenario string) {
	if strings.Join(os.Args[1:], " ") != "exec --protocol v1 --worker" || os.Getenv("ANTHROPIC_BASE_URL") != "http://localhost:1234" || os.Getenv("ANTHROPIC_API_KEY") != "fixture-key" || os.Getenv("HRN_WORKER_TOKEN") != "" || os.Getenv("OTHER_SECRET") != "" {
		os.Exit(2)
	}
	if os.Getenv("HRN_HOME") != filepath.Join(filepath.Dir(os.Getenv("HOME")), ".hrn-home") {
		os.Exit(6)
	}
	in := bufio.NewReader(os.Stdin)
	if scenario == "handshake" {
		line, _ := in.ReadBytes('\n')
		if len(line) != 0 {
			os.Exit(7)
		}
		return
	}
	fmt.Println(`{"v":1,"seq":1,"type":"hello","protocol":"v1"}`)
	var start struct {
		Attempt string `json:"attempt_id"`
	}
	line, _ := in.ReadBytes('\n')
	if json.Unmarshal(line, &start) != nil || start.Attempt == "" {
		os.Exit(3)
	}
	if scenario == "refused" {
		fmt.Fprintln(os.Stderr, "fixture stderr tail")
		fmt.Printf("{\"v\":1,\"seq\":2,\"type\":\"rejected\",\"attempt_id\":%q,\"error\":{\"code\":\"storage\",\"message\":\"cannot write\"}}\n", start.Attempt)
		return
	}
	fmt.Printf("{\"v\":1,\"seq\":2,\"type\":\"accepted\",\"attempt_id\":%q}\n", start.Attempt)
	if scenario == "exit" {
		return
	}
	if scenario == "ignore" {
		for {
			time.Sleep(time.Second)
		}
	}
	if scenario == "cancel" {
		line, _ = in.ReadBytes('\n')
		if !strings.Contains(string(line), `"cancel"`) {
			os.Exit(4)
		}
		fmt.Printf("{\"v\":1,\"seq\":3,\"type\":\"result\",\"attempt_id\":%q,\"result\":{\"status\":\"canceled\",\"backend\":\"api\"}}\n", start.Attempt)
		return
	}
	fmt.Printf("{\"v\":1,\"seq\":3,\"type\":\"checkpoint\",\"attempt_id\":%q,\"checkpoint\":{\"version\":1,\"sequence\":1}}\n", start.Attempt)
	line, _ = in.ReadBytes('\n')
	kind := "ack"
	if scenario == "nack" {
		kind = "nack"
	}
	if !strings.Contains(string(line), `"`+kind+`"`) {
		os.Exit(5)
	}
	fmt.Printf("{\"v\":1,\"seq\":4,\"type\":\"result\",\"attempt_id\":%q,\"result\":{\"status\":\"ok\",\"backend\":\"api\"}}\n", start.Attempt)
}
func TestWorkerExecProtocol(t *testing.T) {
	for _, scenario := range []string{"saved", "nack", "refused", "cancel", "ignore", "exit", "handshake", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("OTHER_SECRET", "secret")
			t.Setenv("HRN_WORKER_TOKEN", "secret")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			// The scenario is the file name, so only Windows gets an extension: on Linux the test binary ends in .test.
			name := "fakehrn-" + scenario
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			path := filepath.Join(t.TempDir(), name)
			if err = os.WriteFile(path, data, 0700); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing" {
				path = filepath.Join(t.TempDir(), "missing")
			}
			job := workspaceJob(strings.Repeat("e", 32))
			job.State, job.Generation = Running, 7
			job.Options.Mode = agent.ModeReadOnly
			var result agent.Result
			checkpoints := 0
			finishes := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				code, body := 204, ""
				switch r.Method + " " + r.URL.Path {
				case "POST /internal/projects/" + job.ProjectID + "/claim":
					b, _ := json.Marshal(job)
					code, body = 200, string(b)
				case "GET /internal/projects/" + job.ProjectID + "/jobs/" + job.ID + "/checkpoint":
					code = 404
				case "POST /internal/jobs/" + job.ID + "/checkpoint":
					checkpoints++
					var req checkpointRequest
					if json.NewDecoder(r.Body).Decode(&req) != nil || req.Generation != 7 {
						t.Error("bad checkpoint")
					}
					if scenario == "nack" {
						code = 409
					}
				case "POST /internal/jobs/" + job.ID + "/finish":
					finishes++
					var req finishRequest
					if json.NewDecoder(r.Body).Decode(&req) != nil {
						t.Error("bad finish")
					}
					result = req.Result
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			w := Worker{ControlURL: "http://control", ProjectID: job.ProjectID, Token: strings.Repeat("a", 64), WorkerID: "worker", WorkspaceRoot: t.TempDir(), HRNPath: path, ModelURL: "http://localhost:1234", ModelKey: "fixture-key", HTTPClient: client}

			t.Setenv("HOME", filepath.Join(w.WorkspaceRoot, "unwritable"))
			t.Setenv("HRN_HOME", "")
			if err := os.Unsetenv("HRN_HOME"); err != nil {
				t.Fatal(err)
			}
			w.cancelGrace = 100 * time.Millisecond

			ctx := context.Background()
			if scenario == "cancel" || scenario == "ignore" || scenario == "handshake" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			}
			ran, err := w.RunOnce(ctx)
			if !ran || err != nil {
				t.Fatalf("run: %v %v", ran, err)
			}
			switch scenario {
			case "missing":
				if finishes != 1 || result.Status != agent.StatusError || result.RecoveryRequired || !strings.HasPrefix(result.Error, "start worker:") || strings.Contains(result.Error, "; stderr:") {
					t.Fatalf("finishes %d result %+v", finishes, result)
				}
			case "saved":
				if result.Status != agent.StatusOK || checkpoints != 1 {
					t.Fatalf("result %+v checkpoints %d", result, checkpoints)
				}
			case "nack":
				if !result.RecoveryRequired || checkpoints != 1 {
					t.Fatalf("result %+v", result)
				}
			case "refused":
				if result.Status != agent.StatusError || result.RecoveryRequired || !strings.Contains(result.Error, "refused: storage: cannot write") || !strings.Contains(result.Error, "fixture stderr tail") {
					t.Fatalf("result %+v", result)
				}
			case "cancel", "handshake":
				if result.Status != "canceled" || result.RecoveryRequired {
					t.Fatalf("result %+v", result)
				}
			case "ignore", "exit":
				if !result.RecoveryRequired {
					t.Fatalf("result %+v", result)
				}
			}
		})
	}
}
