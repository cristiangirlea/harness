package hrnmcp

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testRemoteTenant  = "t_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRemoteProject = "p_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRemoteJob     = "j_cccccccccccccccccccccccccccccccc"
	testRemoteRef     = "dddddddddddddddddddddddddddddddddddddddd"
	testRemoteToken   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

type seenRemoteRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   map[string]any
}

func remoteFixture(t *testing.T) (*session, *[]seenRemoteRequest) {
	t.Helper()
	var mu sync.Mutex
	seen := make([]seenRemoteRequest, 0, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil && r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		mu.Lock()
		seen = append(seen, seenRemoteRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q,"state":"queued"}`, testRemoteJob)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			fmt.Fprint(w, `{"events":[{"id":12,"kind":"checkpoint"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			fmt.Fprint(w, `{"state":"canceled"}`)
		default:
			fmt.Fprintf(w, `{"id":%q,"state":"running"}`, testRemoteJob)
		}
	}))
	t.Cleanup(server.Close)
	certPath := filepath.Join(t.TempDir(), "server-ca.pem")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if _, err := x509.ParseCertificate(server.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pemData, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", testRemoteToken)
	t.Setenv("HRN_SERVER_CA_FILE", certPath)
	return connect(t, t.TempDir()), &seen
}

func TestRemoteToolsSendScopedRequestsWithTheTenantToken(t *testing.T) {
	session, requests := remoteFixture(t)
	args := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"prompt":"inspect","base_ref":%q,"idempotency_key":"mcp-1","max_cost_usd":0.25}`,
		testRemoteTenant, testRemoteProject, testRemoteRef)
	submitted := session.call(t, "remote_submit", args)
	if submitted.IsError || submitted.StructuredContent.(map[string]any)["state"] != "queued" {
		t.Fatalf("submit: %+v", submitted)
	}
	jobArgs := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"job_id":%q}`, testRemoteTenant, testRemoteProject, testRemoteJob)
	status := session.call(t, "remote_status", jobArgs)
	if status.IsError || status.StructuredContent.(map[string]any)["state"] != "running" {
		t.Fatalf("status: %+v", status)
	}
	events := session.call(t, "remote_events", strings.TrimSuffix(jobArgs, "}")+`,"after":11}`)
	if events.IsError || len(events.StructuredContent.(map[string]any)["events"].([]any)) != 1 {
		t.Fatalf("events: %+v", events)
	}
	canceled := session.call(t, "remote_cancel", jobArgs)
	if canceled.IsError || canceled.StructuredContent.(map[string]any)["state"] != "canceled" {
		t.Fatalf("cancel: %+v", canceled)
	}
	if len(*requests) != 4 {
		t.Fatalf("requests: %+v", *requests)
	}
	for _, request := range *requests {
		if request.auth != "Bearer "+testRemoteToken {
			t.Errorf("authentication: %+v", request)
		}
	}
	if (*requests)[0].method != "POST" || (*requests)[0].path != "/v1/projects/"+testRemoteProject+"/jobs" {
		t.Errorf("submit request: %+v", (*requests)[0])
	}
	body := (*requests)[0].body
	if body["tenant_id"] != testRemoteTenant || body["project_id"] != testRemoteProject || body["backend"] != "api" || body["base_ref"] != testRemoteRef {
		t.Errorf("submit body: %+v", body)
	}
	options := body["options"].(map[string]any)
	if options["mode"] != "readonly" || options["max_cost_usd"] != 0.25 || options["max_turns"] != float64(3) {
		t.Errorf("submit options: %+v", options)
	}
	if (*requests)[1].path != "/v1/jobs/"+testRemoteJob || !strings.Contains((*requests)[1].query, "tenant_id="+testRemoteTenant) ||
		!strings.Contains((*requests)[1].query, "project_id="+testRemoteProject) {
		t.Errorf("status scope: %+v", (*requests)[1])
	}
	if (*requests)[2].path != "/v1/jobs/"+testRemoteJob+"/events" || !strings.Contains((*requests)[2].query, "after=11") {
		t.Errorf("events cursor: %+v", (*requests)[2])
	}
	if (*requests)[3].method != "POST" || (*requests)[3].path != "/v1/jobs/"+testRemoteJob+"/cancel" {
		t.Errorf("cancel request: %+v", (*requests)[3])
	}
}

func TestRemoteMCPRejectsUnsupportedInput(t *testing.T) {
	session, requests := remoteFixture(t)
	valid := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"prompt":"inspect","base_ref":%q,"idempotency_key":"mcp-2","max_cost_usd":0.25`,
		testRemoteTenant, testRemoteProject, testRemoteRef)
	for _, args := range []string{
		valid + `,"mounts":["/host"]}`,
		valid + `,"mode":"auto"}`,
		valid + `,"no_switch":true}`,
		valid + `,"options":{"no_switch":true}}`,
		valid + `,"max_turns":-1}`,
		strings.Replace(valid+"}", testRemoteProject, "p_../other", 1),
	} {
		result := session.call(t, "remote_submit", args)
		if !result.IsError {
			t.Errorf("submit accepted %s: %+v", args, result)
		}
	}
	badCursor := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"job_id":%q,"after":-1}`, testRemoteTenant, testRemoteProject, testRemoteJob)
	if result := session.call(t, "remote_events", badCursor); !result.IsError {
		t.Errorf("negative cursor accepted: %+v", result)
	}
	if len(*requests) != 0 {
		t.Errorf("invalid requests reached control: %+v", *requests)
	}
}

func TestRemoteToolsRejectCredentialFieldsBeforeSendingRequests(t *testing.T) {
	session, requests := remoteFixture(t)
	submit := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"prompt":"inspect","base_ref":%q,"idempotency_key":"mcp-credential","max_cost_usd":0.25`,
		testRemoteTenant, testRemoteProject, testRemoteRef)
	job := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"job_id":%q`, testRemoteTenant, testRemoteProject, testRemoteJob)
	for _, tool := range []struct {
		name string
		args string
	}{
		{"remote_submit", submit},
		{"remote_status", job},
		{"remote_events", job},
		{"remote_cancel", job},
	} {
		for _, field := range []string{"token", "authorization"} {
			t.Run(tool.name+"/"+field, func(t *testing.T) {
				args := tool.args + fmt.Sprintf(`,%q:"supplied credential"}`, field)
				if result := session.call(t, tool.name, args); !result.IsError {
					t.Fatalf("credential field accepted: %+v", result)
				}
			})
		}
	}
	if len(*requests) != 0 {
		t.Fatalf("credential fields sent %d requests", len(*requests))
	}
}

func TestRemoteSubmitRefusesCostAboveTheClientCapBeforeSending(t *testing.T) {
	session, requests := remoteFixture(t)
	t.Setenv("HRN_REMOTE_MAX_COST_USD", "1")
	args := fmt.Sprintf(`{"tenant_id":%q,"project_id":%q,"prompt":"inspect","base_ref":%q,"idempotency_key":"mcp-cap","max_cost_usd":%%s}`,
		testRemoteTenant, testRemoteProject, testRemoteRef)
	result := session.call(t, "remote_submit", fmt.Sprintf(args, "5"))
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Content), "HRN_REMOTE_MAX_COST_USD") {
		t.Fatalf("cost above the cap: %+v", result)
	}
	if len(*requests) != 0 {
		t.Fatalf("cost above the cap reached control: %+v", *requests)
	}
	if result := session.call(t, "remote_submit", fmt.Sprintf(args, "1")); result.IsError {
		t.Fatalf("cost at the cap: %+v", result)
	}
	if len(*requests) != 1 {
		t.Fatalf("cost at the cap sent %d requests, want 1", len(*requests))
	}
}
