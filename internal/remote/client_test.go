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
