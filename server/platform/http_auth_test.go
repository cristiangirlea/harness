package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPOIDCAuthorization(t *testing.T) {
	f := newOIDCFixture(t)
	claims := f.claims()
	claims["roles"] = "operator"
	operator := f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256")
	delete(claims, "roles")
	claims["tenants"] = []string{"own"}
	tenant := f.sign(t, oidcHeader("ES256", "ec"), claims, "ES256")
	claims["tenants"] = ""
	emptyTenant := f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256")
	claims["tenants"] = []string{""}
	emptyTenantArray := f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256")
	server := &HTTPServer{
		Store:            &Store{},
		OIDC:             f.verifier(t),
		OperatorToken:    "operator-token-abcdefghijklmnopqrstuvwxyz",
		ScalerToken:      "scaler-token-abcdefghijklmnopqrstuvwxyz",
		workerAuthorized: func(context.Context, string, string) (bool, error) { return false, nil },
	}
	handler := server.Handler()
	const budget = "/v1/tenants/own/budget"
	const events = "/v1/jobs/job/events?after=bad&tenant_id="
	const worker = "/internal/projects/project/claim"
	const scaler = "/internal/scaling/projects/project"
	const wrong = "wrong-static-token-abcdefghijklmnopqrstuvwxyz"
	call := func(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method, path, token, downstream string
	}{
		{"PUT", budget, operator, "budget_usd must be"},
		{"POST", "/v1/jobs/job/resume", operator, "invalid recovery action"},
		{"GET", events + "own", tenant, "invalid_cursor"},
	} {
		w := call(handler, tc.method, tc.path, tc.token)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.downstream) {
			t.Errorf("%s %s: did not reach handler validation: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		name, method, path, token string
		handler                   http.Handler
		status                    int
	}{
		{"tenant on operator route", "PUT", budget, tenant, handler, 401},
		{"other tenant", "GET", events + "other", tenant, handler, 404},
		{"other tenant budget", "GET", "/v1/tenants/other/budget", tenant, handler, 404},
		{"operator has no tenant scope", "GET", events + "own", operator, handler, 404},
		{"empty tenant string", "GET", events, emptyTenant, handler, 404},
		{"empty tenant array entry", "GET", events, emptyTenantArray, handler, 404},
		{"worker", "POST", worker, operator, handler, 404},
		{"scaler", "GET", scaler, operator, handler, 401},
		{"tenant on worker route", "POST", worker, tenant, handler, 404},
		{"tenant on scaler route", "GET", scaler, tenant, handler, 401},
		{"feature off", "PUT", budget, operator, (&HTTPServer{}).Handler(), 401},
		{"tenant feature off", "GET", events + "own", tenant, (&HTTPServer{Store: &Store{}}).Handler(), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signed := call(tc.handler, tc.method, tc.path, tc.token)
			static := call(tc.handler, tc.method, tc.path, wrong)
			if signed.Code != tc.status || signed.Code != static.Code || signed.Body.String() != static.Body.String() {
				t.Fatalf("signed refusal %d %q differs from static %d %q", signed.Code, signed.Body.String(), static.Code, static.Body.String())
			}
		})
	}
	f.discovery.Store("invalid discovery")
	unavailable := (&HTTPServer{Store: &Store{}, OIDC: f.verifier(t), OperatorToken: server.OperatorToken}).Handler()
	for _, tc := range []struct{ method, path, token string }{
		{"PUT", budget, operator},
		{"GET", events + "own", tenant},
	} {
		signed := call(unavailable, tc.method, tc.path, tc.token)
		static := call(unavailable, tc.method, tc.path, wrong)
		if signed.Code != static.Code || signed.Body.String() != static.Body.String() {
			t.Fatalf("issuer failure changed refusal: signed %d %q, static %d %q", signed.Code, signed.Body.String(), static.Code, static.Body.String())
		}
	}
	w := call(unavailable, "PUT", budget, server.OperatorToken)
	if w.Code != 400 || f.documents.Load() != 2 {
		t.Fatalf("static operator with unavailable issuer: status %d, discovery requests %d", w.Code, f.documents.Load())
	}
}

func TestEventDetailsWithActor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
		want  string
	}{
		{"nil", nil, `{"actor":"person","actor_issuer":"issuer"}`},
		{"string map", map[string]string{"action": "resume"}, `{"action":"resume","actor":"person","actor_issuer":"issuer"}`},
		{"arbitrary map", map[string]any{"actor": "ignored", "number": int64(9007199254740993)}, `{"actor":"person","actor_issuer":"issuer","number":9007199254740993}`},
		{"other object", json.RawMessage(`{"ok":true}`), `{"actor":"person","actor_issuer":"issuer","ok":true}`},
		{"array", []string{"value"}, `["value"]`},
		{"scalar", "value", `"value"`},
		{"JSON null", json.RawMessage("null"), "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eventDetails(WithActor(context.Background(), "person", "issuer"), tc.input)
			if err != nil || string(got) != tc.want {
				t.Fatalf("details = %s, %v, want %s", got, err, tc.want)
			}
		})
	}
	got, err := eventDetails(context.Background(), nil)
	if err != nil || string(got) != "{}" {
		t.Fatalf("details without actor = %s, %v", got, err)
	}
}

func TestHTTPBudgetPutValidation(t *testing.T) {
	const operator = "operator-token-abcdefghijklmnopqrstuvwxyz"
	handler := (&HTTPServer{OperatorToken: operator}).Handler()
	for _, body := range []string{
		"", "null", "[]", "true", "{}", `{"unknown":1}`, `{"budget_usd":1,"unknown":2}`,
		`{"budget_usd":0}`, `{"budget_usd":-1}`, `{"budget_usd":NaN}`, `{"budget_usd":Inf}`,
		`{"budget_usd":1e999}`, `{"budget_usd":"1"}`, `{"budget_usd":1} {}`,
	} {
		r := httptest.NewRequest(http.MethodPut, "/v1/tenants/tenant/budget", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+operator)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400: %s", body, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodPut, "/v1/tenants/tenant/budget", strings.NewReader("invalid"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated put: status %d, want 401", w.Code)
	}
}

func TestHTTPBudgetErrorIs422(t *testing.T) {
	w := httptest.NewRecorder()
	invalid(w, fmt.Errorf("%w: limit reached", ErrTenantBudget))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"code":"tenant_budget"`) {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

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
