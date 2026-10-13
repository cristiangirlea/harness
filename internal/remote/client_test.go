package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReadEventsRejectsMissingID(t *testing.T) {
	for _, stream := range []string{
		"data: {}\n\n",
		"id: 0\ndata: {}\n\n",
	} {
		_, err := ReadEvents(strings.NewReader(stream), 0, io.Discard)
		if !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("stream %q: got %v", stream, err)
		}
	}
}

func TestReadEventsReportsOutputFailure(t *testing.T) {
	_, err := ReadEvents(strings.NewReader("id: 1\ndata: {}\n\n"), 0, brokenWriter{})
	if !errors.Is(err, ErrEventOutput) || !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("output error = %v", err)
	}
}

func TestRequestRejectsOversizedControlResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", (4<<20)+1)))
	}))
	defer server.Close()
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Request(context.Background(), http.MethodGet, "/v1/jobs/j_1", nil); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Errorf("large response = %v", err)
	}
}

func TestNewFromEnvAllowsPlainHTTPOnlyForLoopbackIPLiteral(t *testing.T) {
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	for _, origin := range []string{"http://localhost:1", "http://10.0.0.1:1", "http://192.168.1.1:1"} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("HRN_SERVER_URL", origin)
			if _, err := NewFromEnv(); err == nil || err.Error() != "remote control requires HTTPS or loopback HTTP" {
				t.Fatalf("NewFromEnv() error = %v", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	t.Setenv("HRN_SERVER_URL", server.URL)
	if _, err := NewFromEnv(); err != nil {
		t.Fatalf("loopback IP URL %q: %v", server.URL, err)
	}
}

func TestWatchErrorNamesTheStatusWithoutTheResponseBody(t *testing.T) {
	const body = "untrusted response with credentials and terminal output"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, body)
	}))
	defer server.Close()
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	err = client.Watch(context.Background(), "/v1/jobs/j_fixture/events", 0, io.Discard)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden || strings.Contains(err.Error(), body) {
		t.Fatalf("watch error = %v, want HTTP 403 without response body", err)
	}
}
