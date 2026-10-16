package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func retrievalFixtureText(revision string) string {
	return fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", "e_fixture", "", revision, "", "reference fixture")
}

func TestRetrievalBatchRetryPinsVersion(t *testing.T) {
	root := initRepo(t)
	var mu sync.Mutex
	var queries []retrieval.Search
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q retrieval.Search
		json.NewDecoder(r.Body).Decode(&q)
		mu.Lock()
		queries = append(queries, q)
		n := len(queries)
		mu.Unlock()
		id := q.VersionID
		if id == "" {
			id = "v_first"
			if n > 1 {
				id = "v_changed"
			}
		}
		text := retrievalFixtureText(q.Revision)
		json.NewEncoder(w).Encode(retrieval.Packet{VersionID: id, Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query), Entries: []retrieval.Entry{{ID: "e_fixture", Content: "reference fixture"}}})
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	count := 0
	agent.Register(stub{"retrieval-batch", func(_ context.Context, task agent.Task) agent.Result {
		count++
		if task.OriginalPrompt != "original" || task.Mode != agent.ModeReadOnly || strings.Count(task.Prompt, "<project-reference>") != 1 {
			t.Error("bad composed task")
		}
		final := "bad"
		if count > 1 {
			final = "done"
		}
		return agent.Result{Backend: "retrieval-batch", Model: "fixture", Status: agent.StatusOK, Final: final}
	}})
	opts := agent.Options{Mode: agent.ModeReadOnly, Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	result, _ := run(t, Spec{Dir: root, Backend: "retrieval-batch", Options: opts, Retry: []string{"retrieval-batch"}, Tasks: []Task{{ID: "retrieval-retry", Prompt: "original", Expect: "^done$"}}})
	if result.Tasks[0].Status != StatusPass || len(result.Tasks[0].Attempts) != 2 {
		t.Fatalf("%+v", result.Tasks[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 || queries[1].VersionID != "v_first" || !strings.Contains(queries[1].Query, "gate") {
		t.Fatalf("retry lost real failure/pin: %+v", queries)
	}
	for _, a := range result.Tasks[0].Attempts {
		rec, e := runlog.Load(a.RunID)
		if e != nil || rec.Task.OriginalPrompt != "original" || rec.Result.Retrieval.Packet.VersionID != "v_first" {
			t.Fatal("run provenance lost", e)
		}
	}
}

func TestRetrievalBatchRetryOnAnotherBackendIsRefusedBeforeItRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	agent.Register(stub{"retrieval-first", nil})
	agent.Register(stub{"retrieval-second", nil})
	opts := agent.Options{Mode: agent.ModeReadOnly, Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"spec retry", Spec{Backend: "retrieval-first", Options: opts, Retry: []string{"retrieval-first", "retrieval-second"}, Tasks: []Task{{ID: "a", Prompt: "p"}}}, "task a: retry names retrieval-second"},
		{"task retry", Spec{Backend: "retrieval-first", Options: opts, Tasks: []Task{{ID: "b", Prompt: "p", Retry: []string{"retrieval-second"}}}}, "task b: retry names retrieval-second"},
		{"task backend", Spec{Backend: "retrieval-first", Options: opts, Retry: []string{"retrieval-first"}, Tasks: []Task{{ID: "c", Prompt: "p", Backend: "retrieval-second"}}}, "task c: retry names retrieval-first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.normalize()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "retrieval") {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	same := Spec{Backend: "retrieval-first", Options: opts, Retry: []string{"retrieval-first"}, Tasks: []Task{{ID: "d", Prompt: "p"}}}
	if err := same.normalize(); err != nil {
		t.Fatalf("same-backend retry refused: %v", err)
	}
	off := Spec{Backend: "retrieval-first", Retry: []string{"retrieval-second"}, Tasks: []Task{{ID: "e", Prompt: "p"}}}
	if err := off.normalize(); err != nil {
		t.Fatalf("retry without retrieval refused: %v", err)
	}
}

// retrievalServer answers knowledge searches with one fixed packet, failing
// the requests fail picks with 503.
func retrievalServer(t *testing.T, fail func(n int) bool) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if fail != nil && fail(cur) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var q retrieval.Search
		json.NewDecoder(r.Body).Decode(&q)
		text := retrievalFixtureText(q.Revision)
		json.NewEncoder(w).Encode(retrieval.Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query), Entries: []retrieval.Entry{{ID: "e_fixture", Content: "reference fixture"}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
}

func TestRetrievalBatchRetryAfterAFailedRequiredRetrievalSearchesAfresh(t *testing.T) {
	root := initRepo(t)
	retrievalServer(t, func(n int) bool { return n == 1 })
	calls := 0
	agent.Register(stub{"retrieval-fresh", func(_ context.Context, task agent.Task) agent.Result {
		calls++
		if strings.Count(task.Prompt, "<project-reference>") != 1 {
			t.Error("retry ran without the fresh reference")
		}
		return agent.Result{Backend: "retrieval-fresh", Model: "fixture", Status: agent.StatusOK, Final: "done"}
	}})
	opts := agent.Options{Mode: agent.ModeReadOnly, Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	result, _ := run(t, Spec{Dir: root, Backend: "retrieval-fresh", Options: opts, Retry: []string{"retrieval-fresh"}, Tasks: []Task{{ID: "fresh", Prompt: "original", Expect: "^done$"}}})
	tr := result.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || calls != 1 || !strings.Contains(tr.Attempts[0].Result.Error, "retrieval request rejected or unavailable") {
		t.Fatalf("task=%+v calls=%d", tr, calls)
	}
	if p := tr.Attempts[1].Result.Retrieval; p == nil || p.Replayed || p.Packet == nil || p.Packet.VersionID != "v_fixture" {
		t.Fatalf("retry did not pin a fresh packet: %+v", p)
	}
}

func TestRetrievalBatchSkipsTheSwitchAtAUsageLimit(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	retrievalServer(t, nil)
	opts := agent.Options{Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Options: opts, Retry: []string{"codex"}, Tasks: []Task{{ID: "pinned", Prompt: "write both halves"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 || tr.Attempts[0].Result.Status != agent.StatusLimited {
		t.Fatalf("task = %+v", tr)
	}
	if !strings.Contains(tr.Error, "switch to claude skipped") || !strings.Contains(tr.Error, "retrieval pin is bound to codex") {
		t.Fatalf("reason = %q", tr.Error)
	}
}

func TestRetrievalBatchSkipsTheSwitchFromABackendAlreadyLimited(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	retrievalServer(t, nil)
	claude, err := agent.Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	agent.Register(stub{"claude", func(_ context.Context, task agent.Task) agent.Result {
		_ = limits.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
		return agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "not yet"}
	}})
	t.Cleanup(func() { agent.Register(claude) })
	opts := agent.Options{Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	res, _ := run(t, Spec{Dir: dir, Backend: "claude", Options: opts, Retry: []string{"claude"}, Tasks: []Task{{ID: "pinned", Prompt: "answer", Expect: "^done$"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 || !strings.Contains(tr.Error, "switch to codex skipped") || !strings.Contains(tr.Error, "retrieval pin is bound to claude") {
		t.Fatalf("task = %+v", tr)
	}
}
