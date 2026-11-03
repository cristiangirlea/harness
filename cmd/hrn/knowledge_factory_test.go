package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestRunAndCarryOnReceiveWorkingKnowledgeFactories(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	probe := func(task agent.Task) {
		if task.Knowledge == nil {
			t.Error("missing Knowledge")
			return
		}
		s, err := task.Knowledge()
		if err != nil {
			t.Error(err)
			return
		}
		_, err = s.SearchKnowledge(context.Background(), retrieval.Search{})
		if !errors.Is(err, retrieval.ErrUnavailable) {
			t.Errorf("search: %v", err)
		}
	}
	agent.Register(scripted{"codex", func(task agent.Task) agent.Result {
		probe(task)
		return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit."}
	}})
	agent.Register(scripted{"claude", func(task agent.Task) agent.Result { probe(task); return agent.Result{Status: agent.StatusOK} }})
	code, err := cmdRun([]string{"-b", "codex", "-C", t.TempDir(), "--mode", "auto", "work"})
	if code != 0 || err != nil || hits.Load() != 2 {
		t.Fatalf("code=%d err=%v searches=%d", code, err, hits.Load())
	}
}
