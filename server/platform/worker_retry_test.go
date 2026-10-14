package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestWorkerRetriesTemporaryControlConnectionBeforeClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var attempts atomic.Int32
	responded := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return nil, syscall.ECONNREFUSED
		}
		close(responded)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	backend := agent.NewAPIBackend()
	backend.BaseURL = "http://127.0.0.1:9090"
	worker := &Worker{
		ControlURL:    "https://control.invalid",
		ProjectID:     "p_0123456789abcdef0123456789abcdef",
		Token:         strings.Repeat("a", 64),
		WorkerID:      "fixture-worker",
		WorkspaceRoot: t.TempDir(),
		Backend:       backend,
		HTTPClient:    client,
	}
	go func() {
		<-responded
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker did not remain running after retry: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("claim attempts = %d, want 2", got)
	}
}
