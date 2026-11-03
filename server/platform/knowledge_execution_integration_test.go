package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if _, err = s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: seed.KnowledgeScope, Operation: "activate", VersionID: id}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&HTTPServer{Store: s}).Handler())
	defer srv.Close()
	query := KnowledgeSearch{KnowledgeScope: seed.KnowledgeScope, Query: "Explain ERR_BUILD safely"}
	payload, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/knowledge/search", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var packet KnowledgePacket
	err = json.NewDecoder(resp.Body).Decode(&packet)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status: %s", resp.Status)
	}
	if err == nil {
		err = KnowledgeValidatePacket(packet, id, query.Query)
	}

	if err != nil || packet.VersionID != id || !strings.Contains(packet.Text, "checked project build") {
		t.Fatalf("search: %+v, %v", packet, err)
	}
}
