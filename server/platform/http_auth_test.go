package platform

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type countedBody struct {
	reader io.Reader
	reads  int
}

func (b *countedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func (b *countedBody) Close() error { return nil }

func TestHTTPBodyRoutesAuthorizeBeforeDecode(t *testing.T) {
	const goodToken = "good-token-abcdefghijklmnopqrstuvwxyz"
	const badToken = "bad-token-abcdefghijklmnopqrstuvwxyz0"
	const unknown = "secret_unknown_field"
	server := &HTTPServer{
		submissionAuthorized: func(_ context.Context, projectID, candidate string) (bool, error) {
			return projectID == "project" && candidate == goodToken, nil
		},
		workerAuthorized: func(_ context.Context, projectID, candidate string) (bool, error) {
			return projectID == "project" && candidate == goodToken, nil
		},
	}
	for _, tc := range []struct {
		name, path, body, refusal string
	}{
		{"submit", "/v1/projects/project/jobs", `{"tenant_id":"tenant","project_id":"project","secret_unknown_field":true}`, "tenant or resource not found"},
		{"heartbeat", "/internal/jobs/job/heartbeat", `{"project_id":"project","secret_unknown_field":true}`, "project or resource not found"},
		{"checkpoint", "/internal/jobs/job/checkpoint", `{"project_id":"project","secret_unknown_field":true}`, "project or resource not found"},
		{"finish", "/internal/jobs/job/finish", `{"project_id":"project","secret_unknown_field":true}`, "project or resource not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, auth := range []struct {
				name, token string
				status      int
			}{
				{"bad token", badToken, http.StatusNotFound},
				{"good token", goodToken, http.StatusBadRequest},
			} {
				t.Run(auth.name, func(t *testing.T) {
					body := &countedBody{reader: strings.NewReader(tc.body)}
					r := httptest.NewRequest(http.MethodPost, tc.path, nil)
					r.Body = body
					r.Header.Set("Authorization", "Bearer "+auth.token)
					w := httptest.NewRecorder()
					server.Handler().ServeHTTP(w, r)
					if w.Code != auth.status {
						t.Fatalf("status %d, want %d: %s", w.Code, auth.status, w.Body.String())
					}
					if auth.token == badToken {
						if tc.name == "submit" && body.reads != 0 {
							t.Fatalf("unauthorized request body read %d times", body.reads)
						}
						if !strings.Contains(w.Body.String(), tc.refusal) || strings.Contains(w.Body.String(), unknown) || strings.Contains(w.Body.String(), "unknown field") {
							t.Fatalf("unexpected authorization refusal: %s", w.Body.String())
						}
					} else if !strings.Contains(w.Body.String(), "unknown field") || !strings.Contains(w.Body.String(), unknown) {
						t.Fatalf("strict decode error missing: %s", w.Body.String())
					}
				})
			}
		})
	}
}

func TestHTTPWorkerBadBodyAfterProjectID(t *testing.T) {
	const goodToken = "good-token-abcdefghijklmnopqrstuvwxyz"
	server := &HTTPServer{workerAuthorized: func(_ context.Context, projectID, candidate string) (bool, error) {
		return projectID == "project" && candidate == goodToken, nil
	}}
	for _, tc := range []struct {
		name, body string
	}{
		{"malformed JSON", `{"project_id":"project","checkpoint":`},
		{"over 1 MiB", `{"project_id":"project","padding":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, token := range []string{goodToken, "bad-token-abcdefghijklmnopqrstuvwxyz0"} {
				r := httptest.NewRequest(http.MethodPost, "/internal/jobs/job/checkpoint", strings.NewReader(tc.body))
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r)
				want := http.StatusBadRequest
				if token != goodToken {
					want = http.StatusNotFound
				}
				if w.Code != want {
					t.Fatalf("token %q: status %d, want %d: %s", token, w.Code, want, w.Body.String())
				}
			}
		})
	}
}
