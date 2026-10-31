package platform

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

const oldKnowledgeSearchSQL = `SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers
 FROM knowledge_members m JOIN knowledge_entries e ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id)
 WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3
 AND (e.identifiers && $5::text[] OR e.search_vector @@ to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),'')))
 ORDER BY (e.identifiers && $5::text[]) DESC, ts_rank_cd(e.search_vector,to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),''))) DESC,e.entry_key COLLATE "C",e.id LIMIT 100`

func TestKnowledgeSearchMembersFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _ := testProject(t, s)
	other, _ := testProject(t, s)
	in := knowledgeFixture(p)
	in.Entries = append(in.Entries,
		retrieval.Entry{Key: "tie-a", Content: "build", Source: "docs/a", Evidence: "checked", Identifiers: []string{}},
		retrieval.Entry{Key: "tie-b", Content: "build", Source: "docs/b", Evidence: "checked", Identifiers: []string{"ERR_BUILD"}},
	)
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(ctx, `INSERT INTO knowledge_entries(tenant_id,project_id,id,entry_key,content,source,evidence,identifiers)
 SELECT $1,$2,'bulk-'||n,'bulk-'||n,CASE WHEN n%100=0 THEN 'build' ELSE 'unrelated' END,'fixture','checked',
 CASE WHEN n%100=0 THEN ARRAY['ERR_BUILD'] ELSE ARRAY[]::text[] END FROM generate_series(1,20000) n`, other.TenantID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_versions(tenant_id,project_id,id,backend,model,revision,reviewed_by,review_evidence) VALUES($1,$2,'bulk-version','codex','fixture','revision','reviewer','checked')`, other.TenantID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_members(tenant_id,project_id,version_id,entry_id,entry_key) SELECT $1,$2,'bulk-version',id,entry_key FROM knowledge_entries WHERE tenant_id=$1 AND project_id=$2`, other.TenantID, other.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(ctx, `ANALYZE knowledge_entries; ANALYZE knowledge_members`); err != nil {
		t.Fatal(err)
	}
	args := []any{p.TenantID, p.ID, version, "ERR_BUILD build", knowledgeQueryIdentifiers("ERR_BUILD build")}
	var raw []byte
	if err = s.db.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+knowledgeSearchSQL, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Plan planNode `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("plan: %s: %v", raw, err)
	}
	var memberIndex, entryGIN, materialized bool
	var walk func(planNode)
	walk = func(n planNode) {
		if n.Subplan == "CTE members" {
			materialized = true
		}
		if n.Relation == "knowledge_members" && (n.NodeType == "Index Scan" || n.NodeType == "Index Only Scan" || n.NodeType == "Bitmap Index Scan") {
			memberIndex = true
		}
		if n.Index == "knowledge_members_pkey" || n.Index == "knowledge_members_tenant_id_project_id_version_id_entry_key_key" {
			memberIndex = true
		}
		if n.Index == "knowledge_text_gin" || n.Index == "knowledge_identifiers_gin" {
			entryGIN = true
		}
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(plan[0].Plan)
	if !materialized || !memberIndex || entryGIN {
		t.Fatalf("expected members index without entries GIN scans: %s", raw)
	}
	for _, query := range []string{"ERR_BUILD build", "build", "src/build.go", "missing", ""} {
		args[3], args[4] = query, knowledgeQueryIdentifiers(query)
		read := func(sql string) []retrieval.Entry {
			rows, err := s.db.Query(ctx, sql, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			result := []retrieval.Entry{}
			for rows.Next() {
				var e retrieval.Entry
				if err := rows.Scan(&e.ID, &e.Key, &e.Content, &e.Source, &e.Evidence, &e.Identifiers); err != nil {
					t.Fatal(err)
				}
				result = append(result, e)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return result
		}
		before, after := read(oldKnowledgeSearchSQL), read(knowledgeSearchSQL)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("query %q: old=%+v new=%+v", query, before, after)
		}
	}
}

type planNode struct {
	Subplan  string     `json:"Subplan Name"`
	NodeType string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Index    string     `json:"Index Name"`
	Plans    []planNode `json:"Plans"`
}
