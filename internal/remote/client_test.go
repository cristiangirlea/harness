package remote

import (
	"bufio"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestNewFromEnvRejectsNonOriginServerURL(t *testing.T) {
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	for _, origin := range []string{
		"https://host?", "https:host", "https://host/a%2Fb", "https://host/%2F",
		"https://host#", "https://host/#",
		"https://user@host", "https://host/#fragment", "https://host/?q=1",
		"https://:443", "https://host/prefix",
	} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("HRN_SERVER_URL", origin)
			if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), "HRN_SERVER_URL") {
				t.Fatalf("NewFromEnv(%q) error = %v, want HRN_SERVER_URL refusal", origin, err)
			}
		})
	}
}

func TestNewFromEnvBuildsOriginFromParsedURL(t *testing.T) {
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	for _, raw := range []string{"HTTPS://host", "HTTPS://host/"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("HRN_SERVER_URL", raw)
			client, err := NewFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if client.origin != "https://host" {
				t.Fatalf("origin = %q, want https://host", client.origin)
			}
		})
	}
}

func TestAScopedIPv6OriginStaysEscapedInRequestURLs(t *testing.T) {
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	t.Setenv("HRN_SERVER_URL", "https://[fe80::1%25en0]:443")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(client.origin + "/v1/jobs")
	if err != nil {
		t.Fatalf("a request URL from origin %q does not parse: %v", client.origin, err)
	}
	if u.Hostname() != "fe80::1%en0" || u.Port() != "443" {
		t.Fatalf("request host = %q port %q, want fe80::1%%en0 and 443", u.Hostname(), u.Port())
	}
}

func TestNewFromEnvRejectsNonRegularCAFile(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "https://host")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Run("directory", func(t *testing.T) {
		t.Setenv("HRN_SERVER_CA_FILE", t.TempDir())
		if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") {
			t.Fatalf("NewFromEnv() error = %v, want HRN_SERVER_CA_FILE refusal", err)
		}
	})
	t.Run("FIFO", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.fifo")
		makeCAFIFO(t, path)
		t.Setenv("HRN_SERVER_CA_FILE", path)
		done := make(chan error, 1)
		go func() {
			_, err := NewFromEnv()
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") {
				t.Fatalf("NewFromEnv() error = %v, want HRN_SERVER_CA_FILE refusal", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("opening a FIFO blocked")
		}
	})
}

func TestNewFromEnvLoadsRegularCAFile(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(path, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", path)
	client, err := NewFromEnv()
	if err != nil {
		t.Fatalf("regular CA file: %v", err)
	}
	if _, err := client.Request(context.Background(), http.MethodGet, "/", nil); err != nil {
		t.Fatalf("request using regular CA file: %v", err)
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "ca-link.pem")
		if err := os.Symlink(path, link); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}
		t.Setenv("HRN_SERVER_CA_FILE", link)
		if _, err := NewFromEnv(); err != nil {
			t.Fatalf("symlink to regular CA file: %v", err)
		}
	})
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

func TestWatchStopsOnOversizedEventLine(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: "+strings.Repeat("x", 1<<20)+"\n\n")
	}))
	defer server.Close()
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = client.Watch(ctx, "/v1/jobs/j_fixture/events", 0, io.Discard)
	if ctx.Err() != nil {
		t.Fatalf("Watch reached the context deadline instead of returning the line error: %v", err)
	}
	if !errors.Is(err, bufio.ErrTooLong) || !strings.Contains(err.Error(), "event line") || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("Watch error = %v, want an event line over the 1 MiB limit", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestRemoteTokenNeverAppearsInClientErrors(t *testing.T) {
	token := strings.Repeat("secret", 8)
	t.Setenv("HRN_TENANT_TOKEN", token)
	t.Setenv("HRN_SERVER_CA_FILE", "")
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("error is missing or contains the tenant token: %v", err)
		}
	}
	t.Run("constructor", func(t *testing.T) {
		t.Setenv("HRN_SERVER_URL", "http://localhost:1")
		_, err := NewFromEnv()
		check(t, err)
	})
	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, token)
		}))
		defer server.Close()
		t.Setenv("HRN_SERVER_URL", server.URL)
		client, err := NewFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Request(context.Background(), http.MethodGet, "/v1/jobs/j_fixture", nil)
		check(t, err)
	})
	t.Run("transport error", func(t *testing.T) {
		t.Setenv("HRN_SERVER_URL", "http://127.0.0.1:0")
		client, err := NewFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Request(context.Background(), http.MethodGet, "/v1/jobs/j_fixture", nil)
		check(t, err)
	})
	t.Run("watch HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, token)
		}))
		defer server.Close()
		t.Setenv("HRN_SERVER_URL", server.URL)
		client, err := NewFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		check(t, client.Watch(context.Background(), "/v1/jobs/j_fixture/events", 0, io.Discard))
	})
}
