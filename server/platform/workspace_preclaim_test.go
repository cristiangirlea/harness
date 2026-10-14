package platform

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestWorkerDoesNotClaimWithoutMountedWorkspace(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected claim")
	})}
	backend := agent.NewAPIBackend()
	backend.BaseURL = "http://127.0.0.1:9090"
	worker := &Worker{
		ControlURL:    "https://control.invalid",
		ProjectID:     "p_" + strings.Repeat("a", 32),
		Token:         strings.Repeat("b", 64),
		WorkerID:      "fixture-worker",
		WorkspaceRoot: filepath.Join(t.TempDir(), "missing-volume"),
		Backend:       backend, HTTPClient: client,
	}
	didWork, err := worker.RunOnce(context.Background())
	if didWork || err == nil || calls.Load() != 0 {
		t.Fatalf("worker claimed without a volume: didWork=%v err=%v calls=%d", didWork, err, calls.Load())
	}
}
