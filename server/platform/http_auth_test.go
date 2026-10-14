package platform

import (
	"context"
	"encoding/json"
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

func TestHTTPWorkerTokenCannotAuthorizeAnotherBodyProject(t *testing.T) {
	const goodToken = "good-token-abcdefghijklmnopqrstuvwxyz"
	server := &HTTPServer{workerAuthorized: func(_ context.Context, projectID, candidate string) (bool, error) {
		return projectID == "project" && candidate == goodToken, nil
	}}
	for _, route := range []string{"heartbeat", "checkpoint", "finish"} {
		t.Run(route, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/internal/jobs/job/"+route, strings.NewReader(`{"project_id":"other"}`))
			r.Header.Set("Authorization", "Bearer "+goodToken)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "project or resource not found") {
				t.Fatalf("status %d, want plain 404: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHTTPWorkerBadTokenWithUnusableProjectBodyGetsPlainNotFound(t *testing.T) {
	const badToken = "bad-token-abcdefghijklmnopqrstuvwxyz0"
	server := &HTTPServer{workerAuthorized: func(_ context.Context, _, _ string) (bool, error) {
		return false, nil
	}}
	for _, body := range []struct{ name, value string }{
		{"non-object", `[]`},
		{"no project ID", `{"worker_id":"worker"}`},
		{"malformed before project ID", `{"worker_id":`},
	} {
		for _, route := range []string{"heartbeat", "checkpoint", "finish"} {
			t.Run(body.name+"/"+route, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "/internal/jobs/job/"+route, strings.NewReader(body.value))
				r.Header.Set("Authorization", "Bearer "+badToken)
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r)
				const want = "{\"error\":{\"code\":\"not_found\",\"message\":\"project or resource not found\"}}\n"
				if w.Code != http.StatusNotFound || w.Body.String() != want {
					t.Fatalf("status %d, body %q; want plain 404 %q", w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

func TestHTTPWorkerProjectIDPreparseMatchesStrictDecode(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"case-insensitive field", `{"Project_ID":"project"}`},
		{"duplicate field", `{"project_id":"project","project_id":"project"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request workerRequest
			decoder := json.NewDecoder(strings.NewReader(tc.body))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				t.Fatal(err)
			}
			if got := workerProjectID([]byte(tc.body)); got != "project" || request.ProjectID != got {
				t.Fatalf("preparse project %q, decoded project %q; want project", got, request.ProjectID)
			}
		})
	}
}
