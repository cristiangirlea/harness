package platform

import (
	"context"
	"errors"
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmissionRefusesRetrievalBeforeRemoteExecution(t *testing.T) {
	err := validateSubmission(Submission{Options: agent.Options{Retrieval: "required"}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("retrieval submission: %v, want unsupported", err)
	}
}

type knowledgeExecutionFixture struct {
	t     *testing.T
	calls int
}

func (b *knowledgeExecutionFixture) Name() string            { return "codex" }
func (b *knowledgeExecutionFixture) Detect() (string, error) { return "fixture", nil }
func (b *knowledgeExecutionFixture) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	b.calls++
	if !strings.Contains(task.Prompt, "checked project build") || task.Mode != agent.ModeReadOnly || task.OriginalPrompt != "Explain ERR_BUILD safely" {
		b.t.Error("retrieval did not cross execution boundary correctly")
	}
	if _, e := os.Stat(filepath.Join(task.RunDir, "retrieval.json")); e != nil {
		b.t.Error(e)
	}
	return agent.Result{Backend: "codex", Model: task.Model, Status: agent.StatusOK}
}
func TestKnowledgePostgresRootExecutionBoundary(t *testing.T) {
	s := testStore(t)
	p, token := testProject(t, s)
	seed := knowledgeFixture(p)
	ctx := context.Background()
	id, e := s.ImportKnowledge(ctx, seed)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ChangeKnowledge(ctx, retrieval.Change{Scope: seed.Scope, Operation: "activate", VersionID: id}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer((&HTTPServer{Store: s}).Handler())
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", token)
	t.Setenv("HRN_SERVER_CA_FILE", "")
	b := &knowledgeExecutionFixture{t: t}
	result := agent.Run(ctx, b, agent.Task{Prompt: "Explain ERR_BUILD safely", RunDir: t.TempDir(), Options: agent.Options{Model: seed.Model, Mode: agent.ModeReadOnly, Retrieval: "required", RetrievalTenant: p.TenantID, RetrievalProject: p.ID, RetrievalRevision: fixtureRef}}, func(agent.Event) {})
	if result.Status != agent.StatusOK || b.calls != 1 || result.Retrieval.Packet.VersionID != id {
		t.Fatalf("execution: %+v", result)
	}
}
