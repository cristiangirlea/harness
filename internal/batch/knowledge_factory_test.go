package batch

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

func TestProposalReceivesAWorkingKnowledgeFactory(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	agent.Register(stub{"knowledge-proposal", func(ctx context.Context, task agent.Task) agent.Result {
		if task.Knowledge == nil {
			t.Error("missing Knowledge")
		} else if s, err := task.Knowledge(); err != nil {
			t.Error(err)
		} else {
			_, err = s.SearchKnowledge(ctx, retrieval.Search{})
			if !errors.Is(err, retrieval.ErrUnavailable) {
				t.Errorf("search: %v", err)
			}
		}
		return agent.Result{Status: agent.StatusOK, Final: `["one","two"]`}
	}})
	got, err := proposeAdditions(context.Background(), "knowledge-proposal", Task{Prompt: "task"}, TaskResult{}, 2, t.TempDir(), agent.Options{})
	if err != nil || len(got) != 2 || hits.Load() != 1 {
		t.Fatalf("additions=%v err=%v searches=%d", got, err, hits.Load())
	}
}
