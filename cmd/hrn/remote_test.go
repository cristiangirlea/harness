package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func remoteFixture(t *testing.T, handler http.Handler) (string, string, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("a", 64))
	t.Setenv("HRN_SERVER_CA_FILE", caPath)
	return "t_" + strings.Repeat("1", 32), "p_" + strings.Repeat("2", 32), "j_" + strings.Repeat("3", 32)
}

func TestRemoteCommandsUseTLSAndScopedAPI(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var tenant, project, job string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			t.Errorf("missing tenant authorization")
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/"+project+"/jobs":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			options, _ := input["options"].(map[string]any)
			if input["tenant_id"] != tenant || input["project_id"] != project ||
				input["idempotency_key"] != "stable-key" || input["backend"] != "api" ||
				options["mode"] != "readonly" || options["max_cost_usd"] != float64(1) {
				t.Errorf("incorrect remote submission: %#v", input)
			}
			if _, exists := input["dir"]; exists {
				t.Error("remote submission sent a host path")
			}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":%q,"state":"queued"}`, job)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+job:
			if r.URL.Query().Get("tenant_id") != tenant || r.URL.Query().Get("project_id") != project {
				t.Error("show lacked tenant/project scope")
			}
			fmt.Fprintf(w, `{"id":%q,"state":"succeeded"}`, job)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+job+"/events":
			if r.URL.Query().Get("after") != "7" {
				t.Error("event cursor missing")
			}
			fmt.Fprint(w, `{"events":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/"+job+"/cancel":
			fmt.Fprint(w, `{"state":"canceled"}`)
		default:
			t.Errorf("unexpected remote request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
	tenant, project, job = remoteFixture(t, handler)
	common := []string{"--tenant", tenant, "--project", project}
	commands := [][]string{
		append(append([]string{"submit"}, common...), "--base-ref", strings.Repeat("a", 40),
			"--key", "stable-key", "--max-cost", "1", "hello", "fixture"),
		append(append([]string{"show"}, common...), job),
		append(append([]string{"events"}, common...), "--after", "7", job),
		append(append([]string{"cancel"}, common...), job),
	}
	for _, command := range commands {
		var output bytes.Buffer
		if err := runRemote(command, &output); err != nil {
			t.Fatalf("%v: %v", command, err)
		}
		if !json.Valid(output.Bytes()) {
			t.Fatalf("%v returned invalid JSON: %s", command, output.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("remote requests = %d, want 4", len(calls))
	}
}

func TestRemoteRejectsPlaintextAndRedirects(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "http://example.invalid")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("a", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	if _, err := newRemoteConnection(); err == nil {
		t.Fatal("remote client accepted non-loopback HTTP")
	}
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalled = true
		w.WriteHeader(200)
	}))
	defer target.Close()
	tenant, project, job := remoteFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	var output bytes.Buffer
	err := runRemote([]string{"show", "--tenant", tenant, "--project", project, job}, &output)
	if err == nil || targetCalled {
		t.Fatalf("remote redirect followed or ignored: err=%v targetCalled=%v", err, targetCalled)
	}
}

func TestRemoteSubmitRejectsUnsupportedOptionsBeforeSending(t *testing.T) {
	var calls int
	tenant, project, _ := remoteFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	base := []string{"submit", "--tenant", tenant, "--project", project,
		"--base-ref", strings.Repeat("a", 40), "--key", "stable-key", "--max-cost", "1"}
	for _, unsupported := range []string{"--no-switch", "--mode", "--backend", "--options"} {
		args := append(append([]string{}, base...), unsupported, "auto", "inspect")
		if err := runRemote(args, io.Discard); err == nil {
			t.Errorf("accepted unsupported option %s", unsupported)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid submissions sent %d requests", calls)
	}
}

func TestRemoteCommandsRejectNonServerShapedJobIDsBeforeSending(t *testing.T) {
	var calls int
	tenant, project, _ := remoteFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	for _, action := range []string{"show", "events", "cancel"} {
		for _, job := range []string{"j_../x", "j_" + strings.Repeat("A", 32), "p_" + strings.Repeat("a", 32)} {
			t.Run(action+"/"+job, func(t *testing.T) {
				err := runRemote([]string{action, "--tenant", tenant, "--project", project, job}, io.Discard)
				if err == nil || err.Error() != "show, events and cancel require one server-issued job ID" {
					t.Fatalf("invalid job ID error = %v", err)
				}
			})
		}
	}
	if calls != 0 {
		t.Fatalf("invalid job IDs sent %d requests", calls)
	}
}

func TestRemoteEventCursorAdvancesWithoutDuplicates(t *testing.T) {
	stream := "id: 4\nevent: checkpoint\ndata: {\"id\":4}\n\n" +
		"id: 4\nevent: checkpoint\ndata: {\"id\":4}\n\n" +
		"id: 5\nevent: succeeded\ndata: {\"id\":5}\n\n"
	var output bytes.Buffer
	after, err := readRemoteEvents(strings.NewReader(stream), 3, &output)
	if err != nil || after != 5 {
		t.Fatalf("cursor = %d, error = %v", after, err)
	}
	if output.String() != "{\"id\":4}\n{\"id\":5}\n" {
		t.Fatalf("unexpected event output %q", output.String())
	}
}

type cancelOnSecondEvent struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnSecondEvent) Write(data []byte) (int, error) {
	n, err := w.Buffer.Write(data)
	if bytes.Contains(data, []byte(`"id":2`)) {
		w.cancel()
	}
	return n, err
}

func TestRemoteWatchReconnectsWithDurableCursor(t *testing.T) {
	var mu sync.Mutex
	var cursors []string
	remoteFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cursors = append(cursors, r.Header.Get("Last-Event-ID"))
		count := len(cursors)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			fmt.Fprint(w, "id: 1\nevent: admitted\ndata: {\"id\":1}\n\n")
		} else {
			fmt.Fprint(w, "id: 2\nevent: succeeded\ndata: {\"id\":2}\n\n")
		}
	}))
	connection, err := newRemoteConnection()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := &cancelOnSecondEvent{cancel: cancel}
	if err := connection.watch(ctx, "/v1/jobs/j_fixture/events", 0, output); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cursors) < 2 || cursors[0] != "0" || cursors[1] != "1" {
		t.Fatalf("reconnect cursors = %#v", cursors)
	}
	if output.String() != "{\"id\":1}\n{\"id\":2}\n" {
		t.Fatalf("unexpected watch output %q", output.String())
	}
}
