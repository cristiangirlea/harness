package followup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

type pinBackend struct{ called *bool }

func (b pinBackend) Name() string            { return "codex" }
func (b pinBackend) Detect() (string, error) { return "fixture", nil }
func (b pinBackend) Run(_ context.Context, _ agent.Task, _ func(agent.Event)) agent.Result {
	*b.called = true
	return agent.Result{Status: agent.StatusOK}
}

func TestFreshPinIsRevalidatedByRetrievalServer(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("secret", 12))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	var got retrieval.Search
	disabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = retrieval.Search{}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		if disabled {
			http.Error(w, "disabled", http.StatusNotFound)
			return
		}
		version := got.VersionID
		if version == "" {
			version = "active"
		}
		body := fmt.Sprintf("[Reference e_fixture; source=%q; revision=%s; evidence=%q]\nfixture\n[/Reference]\n", "fixture.md", got.Revision, "synthetic")
		_ = json.NewEncoder(w).Encode(retrieval.Packet{VersionID: version, AuditID: "r_fixture", Text: body, Bytes: len(body), Digest: retrieval.TextDigest(body), QueryDigest: retrieval.TextDigest(got.Query), Entries: []retrieval.Entry{{ID: "e_fixture", Source: "fixture.md", Content: "fixture", Evidence: "synthetic"}}})
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	scope := retrieval.Scope{TenantID: "t_" + strings.Repeat("a", 32), ProjectID: "p_" + strings.Repeat("b", 32), Backend: "codex", Model: "fixture-model", Revision: strings.Repeat("c", 40)}
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: scope, Packet: &retrieval.Packet{VersionID: "v1"}}})
	run := func(req Request) agent.Result {
		t.Helper()
		req.RunID = id
		p, err := Make(req)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release()
		task := agent.Task{Prompt: "work", Dir: p.Dir, RunDir: t.TempDir()}
		p.ApplyRetrieval(&task.Options)
		p.Apply(&task, task.RunDir, func(agent.Event) {})
		called := false
		result := agent.Run(context.Background(), pinBackend{&called}, task, func(agent.Event) {})
		if disabled && called {
			t.Error("backend ran after disabled pin")
		}
		return result
	}
	if result := run(Request{}); result.Status != agent.StatusOK || got.VersionID != "v1" || got.Scope != scope {
		t.Fatalf("carry: result=%+v search=%+v", result, got)
	}
	disabled = true
	if result := run(Request{}); result.Status != agent.StatusError || !strings.Contains(result.Error, "404") {
		t.Fatalf("disabled: %+v", result)
	}
	disabled = false
	next := strings.Repeat("d", 40)
	if result := run(Request{RetrievalRevision: next, RetrievalRefresh: true}); result.Status != agent.StatusOK || got.VersionID != "" || got.Revision != next || result.Retrieval.Packet.VersionID != "active" {
		t.Fatalf("refresh: result=%+v search=%+v", result, got)
	}
}
