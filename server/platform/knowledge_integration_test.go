package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

func knowledgeFixture(p Project) retrieval.Import {
	return retrieval.Import{Scope: retrieval.Scope{TenantID: p.TenantID, ProjectID: p.ID, Backend: "codex", Model: "fixture-model", Revision: fixtureRef}, Origin: "reviewed_source", ReviewedBy: "fixture-reviewer", ReviewEvidence: "synthetic reviewed test source", Entries: []retrieval.Entry{
		{Key: "exact", Content: "Use the checked project build command.", Source: "docs/build.md", Evidence: "fixture check", Identifiers: []string{"ERR_BUILD", "src/build.go"}},
		{Key: "text", Content: "ERR_BUILD is a build error. build build", Source: "docs/errors.md", Evidence: "fixture check", Identifiers: []string{}},
	}}
}
func TestKnowledgeImportRejectsTextThatCanCloseOrForgeAReference(t *testing.T) {
	markers := []string{"<project-reference", "</project-reference>", "[Reference ", "[/Reference]"}
	for _, marker := range markers {
		for _, field := range []string{"content", "source", "evidence", "review_evidence"} {
			t.Run(field+"/"+marker, func(t *testing.T) {
				in := knowledgeFixture(Project{TenantID: "t_" + strings.Repeat("a", 32), ID: "p_" + strings.Repeat("b", 32)})
				switch field {
				case "content":
					in.Entries[0].Content += marker
				case "source":
					in.Entries[0].Source += marker
				case "evidence":
					in.Entries[0].Evidence += marker
				case "review_evidence":
					in.ReviewEvidence += marker
				}
				if _, err := (&Store{}).ImportKnowledge(context.Background(), in); !errors.Is(err, ErrUnsupported) {
					t.Fatalf("reference frame marker accepted: %v", err)
				}
			})
		}
	}
}

func TestKnowledgeReadRejectsShortCredentialsBeforeQueryingStorage(t *testing.T) {
	scope := retrieval.Scope{TenantID: "t_" + strings.Repeat("a", 32), ProjectID: "p_" + strings.Repeat("b", 32), Backend: "codex", Model: "fixture", Revision: fixtureRef}
	h := &HTTPServer{Store: &Store{}}
	for _, credential := range []string{"", "short"} {
		t.Run(credential, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", nil)
			if credential != "" {
				r.Header.Set("Authorization", "Bearer "+credential)
			}
			w := httptest.NewRecorder()
			if h.knowledgeReadAuth(w, r, scope) || w.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", w.Code)
			}
		})
	}
}

func TestKnowledgeReadReportsStorageFailureAsUnavailable(t *testing.T) {
	s := testStore(t)
	p, worker := testProject(t, s)
	scope := knowledgeFixture(p).Scope
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+worker)
	w := httptest.NewRecorder()
	if (&HTTPServer{Store: s}).knowledgeReadAuth(w, r, scope) || w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte(`"retrieval_busy"`)) {
		t.Fatalf("storage failure: status %d, body %s", w.Code, w.Body.String())
	}
}

func TestKnowledgeImportsRemainImmutableAcrossActivationRollbackAndDisable(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	id, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.ImportKnowledge(ctx, in)
	if err != nil || id2 != id {
		t.Fatalf("idempotence %s %s %v", id, id2, err)
	}
	in.Origin = "model_generated"
	if _, err = s.ImportKnowledge(ctx, in); err == nil {
		t.Fatal("generated source approved")
	}
	in.Origin = "reviewed_source"
	badSeed := knowledgeFixture(p)
	badSeed.Entries[0].Source = ".codex/auth.json"
	if _, err = s.ImportKnowledge(ctx, badSeed); err == nil {
		t.Fatal("private credential source accepted")
	}
	v, err := s.InspectKnowledge(ctx, in.Scope, id)
	if err != nil || len(v.Entries) != 2 {
		t.Fatalf("inspect %#v %v", v, err)
	}
	for _, q := range []string{`UPDATE knowledge_entries SET content='mutated'`, `DELETE FROM knowledge_members`, `UPDATE knowledge_versions SET review_evidence='mutated'`} {
		if _, err = s.db.Exec(ctx, q); err == nil {
			t.Fatalf("mutable: %s", q)
		}
	}
	if _, err = s.db.Exec(ctx, `INSERT INTO knowledge_members(tenant_id,project_id,version_id,entry_id,entry_key) VALUES($1,$2,$3,$4,'late')`, p.TenantID, p.ID, id, v.Entries[0].ID); err == nil {
		t.Fatal("membership not sealed")
	}
	active, err := s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "activate", VersionID: id})
	if err != nil {
		t.Fatal(err)
	}
	search := retrieval.Search{Scope: in.Scope, Query: "ERR_BUILD", RunRef: "fixture-run", Attempt: 1}
	packet, err := s.SearchKnowledge(ctx, search)
	if err != nil || len(packet.Entries) != 2 || packet.Entries[0].Key != "exact" {
		t.Fatalf("ranking %#v %v", packet, err)
	}
	again, err := s.SearchKnowledge(ctx, search)
	if err != nil || again.Digest != packet.Digest {
		t.Fatal("non deterministic ranking", err)
	}
	var saved retrieval.Packet
	var raw []byte
	if err = s.db.QueryRow(ctx, `SELECT packet FROM retrieval_records WHERE id=$1`, packet.AuditID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &saved); err != nil || saved.Digest != packet.Digest || saved.Bytes != len(saved.Text) {
		t.Fatal("audit mismatch", err)
	}
	for _, q := range []string{"", "thisdoesnotexist", "src/build.go"} {
		search.Query = q
		got, e := s.SearchKnowledge(ctx, search)
		if e != nil {
			t.Fatal(e)
		}
		if q == "src/build.go" {
			if len(got.Entries) != 1 {
				t.Fatal("exact path not matched")
			}
		} else if got.Text != "" {
			t.Fatal("no-match returned content")
		}
	}
	// New membership supersedes exact while an old pin remains recoverable.
	in.Entries = in.Entries[:1]
	in.Entries[0].Content = "Replacement verified build instructions."
	next, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	active, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "activate", VersionID: next, ExpectedGeneration: active.Generation})
	if err != nil {
		t.Fatal(err)
	}
	search.Query = "ERR_BUILD"
	got, err := s.SearchKnowledge(ctx, search)
	if err != nil || len(got.Entries) != 1 || got.VersionID != next {
		t.Fatalf("superseded %v %v", got, err)
	}
	search.VersionID = id
	got, err = s.SearchKnowledge(ctx, search)
	if err != nil || got.Digest != packet.Digest {
		t.Fatal("pin mutated", err)
	}
	active, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "rollback", ExpectedGeneration: active.Generation})
	if err != nil || active.VersionID != id {
		t.Fatal("rollback", err)
	}
	active, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "disable", ExpectedGeneration: active.Generation})
	if err != nil {
		t.Fatal(err)
	}
	search.VersionID = ""
	got, err = s.SearchKnowledge(ctx, search)
	if err != nil || got.Text != "" || got.EmptyReason != "disabled_or_unconfigured" {
		t.Fatal("disable", err)
	}
	search.VersionID = id
	got, err = s.SearchKnowledge(ctx, search)
	if err != nil || got.Digest != packet.Digest {
		t.Fatal("disable destroyed pin", err)
	}
	for _, mutate := range []func(*retrieval.Search){func(q *retrieval.Search) { q.Model = "wrong" }, func(q *retrieval.Search) { q.Backend = "api" }, func(q *retrieval.Search) { q.Revision = strings.Repeat("b", 40) }, func(q *retrieval.Search) { q.VersionID = "v_' OR 1=1--" }} {
		q := search
		mutate(&q)
		if _, e := s.SearchKnowledge(ctx, q); !errors.Is(e, ErrNotFound) {
			t.Fatalf("applicability %v", e)
		}
	}
}
func TestKnowledgeConcurrentImportsDeduplicateAndRetrievalStaysBounded(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); id, e := s.ImportKnowledge(ctx, in); ids <- id; errs <- e }()
	}
	wg.Wait()
	id, id2 := <-ids, <-ids
	if id != id2 {
		t.Fatal("concurrent ingestion duplicates")
	}
	for i := 0; i < 2; i++ {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "activate", VersionID: id})
			errs <- e
		}()
	}
	wg.Wait()
	e1, e2 := <-errs, <-errs
	if !((e1 == nil && errors.Is(e2, ErrConflict)) || (e2 == nil && errors.Is(e1, ErrConflict))) {
		t.Fatalf("activation %v %v", e1, e2)
	}
	// Oversized first hit is skipped, labels/evidence count, and multibyte text stays valid.
	in.Entries = []retrieval.Entry{{Key: "a", Content: strings.Repeat("界", 2700), Source: strings.Repeat("s", 500), Evidence: "fixture", Identifiers: []string{"CAP"}}, {Key: "b", Content: "small", Source: "source", Evidence: "checked", Identifiers: []string{"CAP"}}}
	capID, e := s.ImportKnowledge(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.SearchKnowledge(ctx, retrieval.Search{Scope: in.Scope, VersionID: capID, Query: "CAP"})
	if e != nil || got.Bytes > 8192 || len(got.Entries) != 1 || got.Entries[0].Key != "b" {
		t.Fatalf("cap %#v %v", got, e)
	}
	// Saturating retrieval admission cannot consume queue-pool connections.
	s.retrievalSlots <- struct{}{}
	s.retrievalSlots <- struct{}{}
	if _, e = s.SearchKnowledge(ctx, retrieval.Search{Scope: in.Scope, Query: "CAP"}); !errors.Is(e, ErrRetrievalBusy) {
		t.Fatal("unbounded admission", e)
	}
	if _, _, e = s.Submit(ctx, testSubmission(p, "during-search", "fixture")); e != nil {
		t.Fatal("queue starved", e)
	}
	<-s.retrievalSlots
	<-s.retrievalSlots
	if s.knowledge.Config().MaxConns != 2 || s.knowledge.Config().ConnConfig.RuntimeParams["statement_timeout"] != "1500" {
		t.Fatal("unbounded pool")
	}
}
func TestKnowledgeCredentialsStayWithinProjectAndRole(t *testing.T) {
	s := testStore(t)
	p, worker := testProject(t, s)
	ctx := context.Background()
	otherTenantID, otherTenantToken, e := s.CreateTenant(ctx, "other knowledge tenant")
	if e != nil {
		t.Fatal(e)
	}
	other, otherWorker, e := s.CreateProject(ctx, Project{TenantID: otherTenantID, Name: "other", RepositoryURL: p.RepositoryURL, BaseRef: p.BaseRef, Backend: p.Backend})
	if e != nil {
		t.Fatal(e)
	}
	in := knowledgeFixture(p)
	id, e := s.ImportKnowledge(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	h := (&HTTPServer{Store: s, OperatorToken: strings.Repeat("o", 64)}).Handler()
	request := func(path, token string, input any) int {
		b, _ := json.Marshal(input)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	query := retrieval.Search{Scope: in.Scope, VersionID: id, Query: "ERR_BUILD"}
	if code := request("/v1/knowledge/search", worker, query); code != 200 {
		t.Fatalf("owner %d", code)
	}
	for _, path := range []string{"/v1/knowledge/search", "/v1/knowledge/inspect", "/v1/knowledge/active"} {
		var input any = query
		if strings.HasSuffix(path, "inspect") {
			input = map[string]any{"tenant_id": p.TenantID, "project_id": p.ID, "backend": in.Backend, "model": in.Model, "revision": in.Revision, "version_id": id}
		} else if strings.HasSuffix(path, "active") {
			input = in.Scope
		}
		for _, credential := range []string{otherWorker, otherTenantToken} {
			if code := request(path, credential, input); code != 404 {
				t.Fatalf("cross tenant %s %d", path, code)
			}
		}
	}
	// Same tenant, different project worker cannot widen authorization.
	sibling, siblingToken, e := s.CreateProject(ctx, Project{TenantID: p.TenantID, Name: "sibling", RepositoryURL: p.RepositoryURL, BaseRef: p.BaseRef, Backend: p.Backend})
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/v1/knowledge/search", "/v1/knowledge/inspect", "/v1/knowledge/active"} {
		var input any = query
		if strings.HasSuffix(path, "inspect") {
			input = map[string]any{"tenant_id": p.TenantID, "project_id": p.ID, "backend": in.Backend, "model": in.Model, "revision": in.Revision, "version_id": id}
		} else if strings.HasSuffix(path, "active") {
			input = in.Scope
		}
		if code := request(path, siblingToken, input); code != 404 {
			t.Fatalf("cross project %s %d", path, code)
		}
	}
	bad := query
	bad.ProjectID = sibling.ID
	if _, e = s.SearchKnowledge(ctx, bad); !errors.Is(e, ErrNotFound) {
		t.Fatal("version scope leak", e)
	}
	bad = query
	bad.TenantID = other.TenantID
	if code := request("/v1/knowledge/search", worker, bad); code != 404 {
		t.Fatalf("tenant substitution %d", code)
	}
	for _, malicious := range []string{"../projects", "p_' OR true--", strings.Repeat("x", 4096)} {
		bad = query
		bad.ProjectID = malicious
		if code := request("/v1/knowledge/search", worker, bad); code != 404 {
			t.Fatalf("malicious id %d", code)
		}
	}
	if code := request("/v1/knowledge/import", worker, in); code != 401 {
		t.Fatal("worker could approve", code)
	}
	for _, operation := range []string{"activate", "disable", "rollback"} {
		if code := request("/v1/knowledge/change", worker, retrieval.Change{Scope: in.Scope, Operation: operation, VersionID: id}); code != 401 {
			t.Fatalf("worker could %s: %d", operation, code)
		}
	}
}

func TestKnowledgeVerboseTasksFindRelevantReferences(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	in := knowledgeFixture(p)
	ctx := context.Background()
	id, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"Please investigate why our build is failing after yesterday changes and suggest the checked command to run safely", "Please locate src/build.go and explain how ERR_BUILD relates to the failing step without modifying files"} {
		packet, e := s.SearchKnowledge(ctx, retrieval.Search{Scope: in.Scope, VersionID: id, Query: q})
		if e != nil || len(packet.Entries) == 0 {
			t.Fatalf("verbose task missed: %s %v", q, e)
		}
		if strings.Contains(q, "ERR_BUILD") && packet.Entries[0].Key != "exact" {
			t.Fatal("exact symbol priority lost")
		}
		if packet.Digest != retrieval.TextDigest(packet.Text) || len(retrieval.ReferenceBlock(packet.Text)) > 8192 {
			t.Fatal("digest/framing mismatch")
		}
	}
}

func TestKnowledgeSearchTreatsTsqueryOperatorsAsPlainTextAndAcceptsLongQueries(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	in := knowledgeFixture(p)
	id, err := s.ImportKnowledge(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var words []string
	for n := 0; ; n++ {
		word := "word" + strconv.Itoa(n)
		if len(strings.Join(words, " "))+len(word)+1 > 2048 {
			break
		}
		words = append(words, word)
	}
	longQuery := strings.Join(words, " ")
	longQuery += strings.Repeat(" ", 2048-len(longQuery))
	for _, query := range []string{"'", `\`, "!", "&", ":*", "<->", `'\!&:*<->`, longQuery} {
		packet, err := s.SearchKnowledge(context.Background(), retrieval.Search{Scope: in.Scope, VersionID: id, Query: query})
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if err := retrieval.ValidatePacket(packet, id, query); err != nil {
			t.Fatalf("query %q: invalid packet: %v", query, err)
		}
	}
}

func TestKnowledgeSearchMatchesMultiWordAndPunctuatedIdentifiersThroughSQL(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	in := knowledgeFixture(p)
	in.Entries = append(in.Entries, retrieval.Entry{Key: "dial", Content: "Start the database fixture first.", Source: "docs/dial.md", Evidence: "fixture check", Identifiers: []string{"connection refused", "main.go"}})
	id, err := s.ImportKnowledge(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"Why does it say connection refused", "Please inspect main.go."} {
		packet, err := s.SearchKnowledge(context.Background(), retrieval.Search{Scope: in.Scope, VersionID: id, Query: query})
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if len(packet.Entries) == 0 || packet.Entries[0].Key != "dial" {
			t.Fatalf("query %q missed the exact identifier: %+v", query, packet.Entries)
		}
	}
}
