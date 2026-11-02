package execproto

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func refreshFixture(t *testing.T, dir string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q retrieval.Search
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		version := q.VersionID
		if version == "" {
			version = "active"
		}
		_ = json.NewEncoder(w).Encode(retrieval.Packet{VersionID: version, Digest: retrieval.TextDigest(""), QueryDigest: retrieval.TextDigest(q.Query)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("secret", 12))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	w, err := runlog.Create("codex", agent.Task{Prompt: "first", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "codex", Status: agent.StatusOK, Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "codex", Model: "model", Revision: "old-revision"}, Packet: &retrieval.Packet{VersionID: "v1"}}}); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func TestRetrievalRefreshInputs(t *testing.T) {
	registerFake(t, func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	old, _ := agent.Get("codex")
	agent.Register(fakeBackend{name: "codex", caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Options: []string{"mode"}}, run: func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
		return agent.Result{Backend: "codex", Status: agent.StatusOK}
	}})
	defer agent.Register(old)
	dir := t.TempDir()
	id := refreshFixture(t, dir)
	legacy, err := runlog.Create("codex", agent.Task{Prompt: "legacy", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(agent.Result{Backend: "codex", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"carried pin", map[string]any{"continue_run_id": id, "fresh": true}, ""},
		{"refresh", map[string]any{"continue_run_id": id, "fresh": true, "retrieval_refresh": true}, ""},
		{"resume refresh", map[string]any{"continue_run_id": id, "fresh": false, "retrieval_refresh": true}, "requires a fresh"},
		{"nothing to refresh", map[string]any{"continue_run_id": legacy.ID(), "retrieval_refresh": true}, "nothing to refresh"},
		{"missing continuation", map[string]any{"retrieval_refresh": true}, "retrieval_refresh needs continue_run_id"},
		{"backend switch", map[string]any{"continue_run_id": id, "fresh": true, "backend": "fake-exec"}, "cannot carry over"},
		{"legacy", map[string]any{"continue_run_id": legacy.ID(), "fresh": true}, ""},
		{"false without continuation", map[string]any{"retrieval_refresh": false}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(dir, "codex")
			fields := req["task"].(map[string]any)
			for k, v := range tc.fields {
				fields[k] = v
			}
			h.send(t, req)
			msg := h.nextAny(t)
			if tc.want != "" {
				if msg["type"] != "rejected" || !strings.Contains(msg["error"].(map[string]any)["message"].(string), tc.want) {
					t.Fatalf("response: %+v", msg)
				}
				if tc.name == "backend switch" {
					message := msg["error"].(map[string]any)["message"].(string)
					if !strings.Contains(message, "continue on codex to keep the pin") || strings.Contains(message, "retrieval off") {
						t.Fatalf("exec advice: %s", message)
					}
				}
				h.exit(t, 2)
				return
			}
			if msg["type"] != "accepted" {
				t.Fatalf("response: %+v", msg)
			}
			if tc.name == "refresh" || tc.name == "carried pin" {
				runID := msg["run_id"].(string)
				rec, err := runlog.Load(runID)
				if err != nil || !strings.Contains(rec.RouteNote, "pin v1") || rec.Task.Retrieval != "required" || rec.Task.RetrievalRevision != "old-revision" || (tc.name == "carried pin" && rec.Task.RetrievalVersion != "v1") || (tc.name == "refresh" && rec.Task.RetrievalVersion != "") {
					t.Fatalf("record: %+v %v", rec, err)
				}
			}
			found := false
			for {
				m := h.nextAny(t)
				if strings.Contains(fmt.Sprint(m), "retrieval refreshed: pin v1") {
					found = true
				}
				if m["type"] == "result" {
					break
				}
			}
			h.exit(t, 0)
			if tc.name == "refresh" && !found {
				t.Fatal("missing refresh event")
			}
		})
	}
}
