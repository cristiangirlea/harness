package eval

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

type judgeKnowledgeProbe struct{ t *testing.T }

func (judgeKnowledgeProbe) Name() string            { return "knowledge-judge" }
func (judgeKnowledgeProbe) Detect() (string, error) { return "fixture", nil }
func (p judgeKnowledgeProbe) Run(ctx context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	if task.Knowledge == nil {
		p.t.Error("missing Knowledge")
	} else if s, err := task.Knowledge(); err != nil {
		p.t.Error(err)
	} else {
		_, err = s.SearchKnowledge(ctx, retrieval.Search{})
		if !errors.Is(err, retrieval.ErrUnavailable) {
			p.t.Errorf("search: %v", err)
		}
	}
	return agent.Result{Status: agent.StatusOK, Final: `{"score":8,"reason":"ok"}`}
}
func TestJudgeReceivesAWorkingKnowledgeFactory(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	got := judgeCell(context.Background(), judgeKnowledgeProbe{t}, &Judge{}, batch.TaskResult{ID: "cell"}, evidence{dir: t.TempDir(), prompt: "judge"})
	if !got.Scored() || hits.Load() != 1 {
		t.Fatalf("judgement=%+v searches=%d", got, hits.Load())
	}
}
