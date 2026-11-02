package hrnmcp

import (
	"encoding/json"
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	dir := t.TempDir()
	s := connect(t, dir)
	old, _ := agent.Get("codex")
	agent.Register(sessionStub{"codex"})
	defer agent.Register(old)
	oldClaude, _ := agent.Get("claude")
	agent.Register(sessionStub{"claude"})
	defer agent.Register(oldClaude)
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
		{"carried", map[string]any{"continue_run_id": id}, ""},
		{"resume refresh", map[string]any{"continue_run_id": id, "fresh": false, "retrieval_refresh": true}, "requires a fresh"},
		{"nothing to refresh", map[string]any{"continue_run_id": legacy.ID(), "retrieval_refresh": true}, "nothing to refresh"},
		{"moved pin", map[string]any{"continue_run_id": id, "fresh": true, "retrieval_revision": "new-revision"}, "pinned at revision"},
		{"refresh", map[string]any{"continue_run_id": id, "fresh": true, "retrieval_refresh": true, "retrieval_revision": "new-revision"}, ""},
		{"missing continuation", map[string]any{"retrieval_refresh": true}, "retrieval_refresh needs continue_run_id"},
		{"backend off", map[string]any{"continue_run_id": id, "fresh": true, "backend": "claude", "retrieval": "off"}, ""},
		{"backend switch", map[string]any{"continue_run_id": id, "fresh": true, "retrieval_refresh": true, "backend": "claude", "retrieval": "required", "retrieval_tenant": "tenant", "retrieval_project": "project", "retrieval_revision": "old-revision", "model": "model"}, "cannot carry over"},
		{"legacy", map[string]any{"continue_run_id": legacy.ID(), "fresh": true}, ""},
		{"false without continuation", map[string]any{"backend": "stub", "retrieval_refresh": false}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fields["prompt"] = "work"
			tc.fields["no_switch"] = true
			b, _ := json.Marshal(tc.fields)
			r := s.call(t, "run", string(b))
			if tc.want != "" {
				if !r.IsError || !strings.Contains(r.Text(), tc.want) {
					t.Fatalf("result: %s", r.Text())
				}
				return
			}
			if r.IsError {
				t.Fatal(r.Text())
			}
			if tc.name == "refresh" || tc.name == "carried" {
				runID := r.StructuredContent.(map[string]any)["run_id"].(string)
				rec, err := runlog.Load(runID)
				if err != nil || !strings.Contains(rec.RouteNote, "pin v1") || rec.Task.Retrieval != "required" || (tc.name == "carried" && rec.Task.RetrievalRevision != "old-revision") || (tc.name == "refresh" && rec.Task.RetrievalRevision != "new-revision") || (tc.name == "carried" && rec.Task.RetrievalVersion != "v1") || (tc.name == "refresh" && rec.Task.RetrievalVersion != "") {
					t.Fatalf("record: %+v %v", rec, err)
				}
			}
			if tc.name == "refresh" {
				id := r.StructuredContent.(map[string]any)["run_id"].(string)
				data, err := os.ReadFile(filepath.Join(runlog.Root(), id, "events.jsonl"))
				if err != nil || !strings.Contains(string(data), "retrieval refreshed: pin v1") {
					t.Fatalf("refresh event: %s %v", data, err)
				}
			}
		})
	}
}
