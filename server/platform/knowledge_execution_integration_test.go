package platform

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestSubmissionRefusesRetrievalBeforeRemoteExecution(t *testing.T) {
	err := validateSubmission(Submission{Options: JobOptions{Retrieval: "required"}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("retrieval submission: %v, want unsupported", err)
	}
}

func TestKnowledgePostgresRemoteExecutionBoundary(t *testing.T) {
	s := testStore(t)
	p, token := testProject(t, s)
	seed := knowledgeFixture(p)
	ctx := context.Background()
	id, err := s.ImportKnowledge(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: seed.Scope, Operation: "activate", VersionID: id}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&HTTPServer{Store: s}).Handler())
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", token)
	t.Setenv("HRN_SERVER_CA_FILE", "")
	searcher, err := remote.SearcherFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	packet, err := searcher.SearchKnowledge(ctx, retrieval.Search{Scope: seed.Scope, Query: "Explain ERR_BUILD safely"})
	if err != nil || packet.VersionID != id || !strings.Contains(packet.Text, "checked project build") {
		t.Fatalf("search: %+v, %v", packet, err)
	}
}
