package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPostgresKnowledgeRankingReadmission(t *testing.T) {
	for _, ranking := range []string{"simple-or-exact-v3", ""} {
		t.Run("ranking="+ranking, func(t *testing.T) {
			s := testStore(t)
			p, _ := testProject(t, s)
			ctx := context.Background()
			in := knowledgeFixture(p)
			baseline, err := s.ImportKnowledge(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: baseline})
			if err != nil || active.VersionID != baseline {
				t.Fatalf("current activation: %+v %v", active, err)
			}
			packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, Query: "ERR_BUILD"})
			if err != nil || packet.VersionRanking != KnowledgeRankingVersion || packet.RankingMismatch {
				t.Fatalf("creation ranking: %+v %v", packet, err)
			}
			id, err := newID("v_")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, `INSERT INTO knowledge_versions(tenant_id,project_id,id,backend,model,revision,reviewed_by,review_evidence,ranking) SELECT tenant_id,project_id,$4,backend,model,revision,reviewed_by,review_evidence,NULLIF($5,'') FROM knowledge_versions WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, p.TenantID, p.ID, baseline, id, ranking)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO knowledge_members(tenant_id,project_id,version_id,entry_id,entry_key) SELECT tenant_id,project_id,$4,entry_id,entry_key FROM knowledge_members WHERE tenant_id=$1 AND project_id=$2 AND version_id=$3`, p.TenantID, p.ID, baseline, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			activate := func() error {
				_, err := s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: id, ExpectedGeneration: active.Generation})
				return err
			}
			if err := activate(); !errors.Is(err, ErrRankingChanged) || !strings.Contains(err.Error(), "hrn knowledge admit") {
				t.Fatalf("stale activation: %v", err)
			}
			admission := KnowledgeAdmission{KnowledgeScope: in.KnowledgeScope, VersionID: id, BaselineVersionID: baseline}
			// Safety controls must still work for a version that predates ranking tracking.
			_, err = s.db.Exec(ctx, `UPDATE knowledge_active_versions SET previous_id=$3 WHERE tenant_id=$1 AND project_id=$2`, p.TenantID, p.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			active, err = s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "rollback", ExpectedGeneration: active.Generation})
			if err != nil || active.VersionID != id {
				t.Fatalf("stale rollback: %+v %v", active, err)
			}
			if err := activate(); !errors.Is(err, ErrRankingChanged) {
				t.Fatalf("stale active no-op: %v", err)
			}
			for _, pin := range []string{"", id} {
				packet, err = s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: pin, Query: "ERR_BUILD"})
				if err != nil || !packet.RankingMismatch || packet.VersionRanking != ranking || packet.Ranking != KnowledgeRankingVersion || len(packet.Entries) == 0 {
					t.Fatalf("stale search: %+v %v", packet, err)
				}
				var raw []byte
				if err := s.db.QueryRow(ctx, `SELECT packet FROM retrieval_records WHERE id=$1`, packet.AuditID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var saved KnowledgePacket
				if err := json.Unmarshal(raw, &saved); err != nil || saved.VersionRanking != ranking || !saved.RankingMismatch {
					t.Fatalf("audit: %+v %v", saved, err)
				}
			}
			active, err = s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "disable", ExpectedGeneration: active.Generation})
			if err != nil || active.VersionID != "" {
				t.Fatalf("stale disable: %+v %v", active, err)
			}
			active, err = s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: baseline, ExpectedGeneration: active.Generation})
			if err != nil {
				t.Fatal(err)
			}
			for _, verdict := range []string{"rejected", "pending"} {
				admission.Verdict = verdict
				if err := s.RecordAdmission(ctx, admission); err != nil {
					t.Fatal(err)
				}
				got, err := s.ActiveKnowledge(ctx, in.KnowledgeScope)
				if err != nil || got != active {
					t.Fatalf("%s changed active: %+v %v", verdict, got, err)
				}
				packet, err = s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: id, Query: "ERR_BUILD"})
				if err != nil || packet.VersionRanking != KnowledgeRankingVersion || packet.RankingMismatch {
					t.Fatalf("%s ranking: %+v %v", verdict, packet, err)
				}
				if err := activate(); err != nil {
					t.Fatalf("%s operator activation: %v", verdict, err)
				}
				active, err = s.ActiveKnowledge(ctx, in.KnowledgeScope)
				if err != nil {
					t.Fatal(err)
				}
				active, err = s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: baseline, ExpectedGeneration: active.Generation})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(ctx, `UPDATE knowledge_admissions SET ranking='older-admission' WHERE tenant_id=$1 AND project_id=$2 AND version_id=$3`, p.TenantID, p.ID, id); err != nil {
				t.Fatal(err)
			}
			packet, err = s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: id, Query: "ERR_BUILD"})
			if err != nil || packet.VersionRanking != "older-admission" || !packet.RankingMismatch {
				t.Fatalf("old admission ranking: %+v %v", packet, err)
			}
			if err := activate(); !errors.Is(err, ErrRankingChanged) {
				t.Fatalf("old admission activation: %v", err)
			}
			admission.Verdict = "admitted"
			if err := s.RecordAdmission(ctx, admission); err != nil {
				t.Fatal(err)
			}
			active, err = s.ActiveKnowledge(ctx, in.KnowledgeScope)
			if err != nil || active.VersionID != id {
				t.Fatalf("admission activation: %+v %v", active, err)
			}
			if err := activate(); err != nil {
				t.Fatalf("readmitted activation: %v", err)
			}
			packet, err = s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: id, Query: "ERR_BUILD"})
			if err != nil || packet.VersionRanking != KnowledgeRankingVersion || packet.RankingMismatch {
				t.Fatalf("admitted ranking: %+v %v", packet, err)
			}
			var recorded string
			if err := s.db.QueryRow(ctx, `SELECT ranking FROM knowledge_admissions WHERE tenant_id=$1 AND project_id=$2 AND version_id=$3`, p.TenantID, p.ID, id).Scan(&recorded); err != nil || recorded != KnowledgeRankingVersion {
				t.Fatalf("recorded ranking: %q %v", recorded, err)
			}
		})
	}
}

func TestPostgresKnowledgeRankingMigration(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	baseline, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeKnowledge(ctx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: baseline}); err != nil {
		t.Fatal(err)
	}
	in.Entries[0].Content = "candidate"
	id, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAdmission(ctx, KnowledgeAdmission{KnowledgeScope: in.KnowledgeScope, VersionID: id, BaselineVersionID: baseline, Verdict: "admitted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `ALTER TABLE knowledge_versions DROP COLUMN ranking; ALTER TABLE knowledge_admissions DROP COLUMN ranking; DELETE FROM schema_migrations WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
			t.Fatal(err)
		}
	}
	var untracked bool
	if err := s.db.QueryRow(ctx, `SELECT v.ranking IS NULL AND a.ranking IS NULL FROM knowledge_versions v JOIN knowledge_admissions a ON (a.tenant_id,a.project_id,a.version_id)=(v.tenant_id,v.project_id,v.id) WHERE v.tenant_id=$1 AND v.project_id=$2 AND v.id=$3`, p.TenantID, p.ID, id).Scan(&untracked); err != nil || !untracked {
		t.Fatalf("migration backfilled ranking: %v %v", untracked, err)
	}
	packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, Query: "ERR_BUILD"})
	if err != nil || packet.VersionID != id || packet.VersionRanking != "" || !packet.RankingMismatch {
		t.Fatalf("legacy active search: %+v %v", packet, err)
	}
}
