package main

import (
	"encoding/json"
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
	t.Setenv("HRN_HOME", t.TempDir())
	drain(t)
	old, _ := agent.Get("codex")
	agent.Register(recorder{"codex", recorded})
	defer agent.Register(old)
	oldClaude, _ := agent.Get("claude")
	agent.Register(recorder{"claude", recorded})
	defer agent.Register(oldClaude)
	id := refreshFixture(t, t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"carried", []string{"--continue", id, "--no-switch", "work"}, ""},
		{"moved pin", []string{"--continue", id, "--fresh", "--no-switch", "--retrieval-revision", "new-revision", "work"}, "pinned at revision"},
		{"refresh", []string{"--continue", id, "--fresh", "--retrieval-refresh", "--retrieval-revision", "new-revision", "--no-switch", "work"}, ""},
		{"resume refresh", []string{"--continue", id, "--resume", "--retrieval-refresh", "--no-switch", "work"}, "requires a fresh"},
		{"missing continuation", []string{"--retrieval-refresh", "work"}, "--retrieval-refresh needs --continue"},
		{"backend off", []string{"--continue", id, "--fresh", "--no-switch", "-b", "claude", "--retrieval", "off", "work"}, ""},
		{"backend switch", []string{"--continue", id, "--fresh", "--retrieval-refresh", "--no-switch", "-b", "claude", "--retrieval", "required", "work"}, "cannot carry over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := cmdRun(tc.args)
			if tc.want != "" {
				if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("code=%d err=%v", code, err)
				}
				return
			}
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			task := <-recorded
			if tc.name == "backend off" {
				if task.Retrieval != "off" {
					t.Fatalf("off task: %+v", task.Options)
				}
				return
			}
			if (tc.name == "carried" && task.RetrievalRevision != "old-revision") || (tc.name == "refresh" && task.RetrievalRevision != "new-revision") || (tc.name == "carried" && task.RetrievalVersion != "v1") || (tc.name == "refresh" && task.RetrievalVersion != "") {
				t.Fatalf("refresh task: %+v", task.Options)
			}
			recs, err := runlog.List(0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, rec := range recs {
				if rec.Continues == id && rec.Task.RetrievalVersion == task.RetrievalVersion && strings.Contains(rec.RouteNote, "pin v1") {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing recorded retrieval decision: %+v", recs)
			}
		})
	}
}
