package batch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

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
		text := "reference fixture"
		json.NewEncoder(w).Encode(retrieval.Packet{VersionID: id, Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query), Entries: []retrieval.Entry{{ID: "e_fixture"}}})
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

func TestRetrievalBatchRetryOnAnotherBackendIsRefused(t *testing.T) {
	root := initRepo(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q retrieval.Search
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		text := "reference fixture"
		if err := json.NewEncoder(w).Encode(retrieval.Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query), Entries: []retrieval.Entry{{ID: "e_fixture"}}}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	firstCalls, secondCalls := 0, 0
	agent.Register(stub{"retrieval-first", func(_ context.Context, task agent.Task) agent.Result {
		firstCalls++
		return agent.Result{Backend: "retrieval-first", Model: "fixture", Status: agent.StatusOK, Final: "retry"}
	}})
	agent.Register(stub{"retrieval-second", func(_ context.Context, task agent.Task) agent.Result {
		secondCalls++
		return agent.Result{Backend: "retrieval-second", Model: "fixture", Status: agent.StatusOK, Final: "done"}
	}})
	opts := agent.Options{Mode: agent.ModeReadOnly, Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	result, _ := run(t, Spec{Dir: root, Backend: "retrieval-first", Options: opts, Retry: []string{"retrieval-second"}, Tasks: []Task{{ID: "cross-backend-retry", Prompt: "original", Expect: "^done$"}}})
	tr := result.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 2 || firstCalls != 1 || secondCalls != 0 || !strings.Contains(tr.Attempts[1].Result.Error, "retrieval pin scope differs from execution") {
		t.Fatalf("cross-backend retry was not refused: task=%+v calls=%d,%d", tr, firstCalls, secondCalls)
	}
}
