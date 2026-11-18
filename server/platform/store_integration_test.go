package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const fixtureRef = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("HRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HRN_TEST_DATABASE_URL for real PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := newID("hrn_test_")
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := target.Query()
	params.Set("search_path", schema)
	target.RawQuery = params.Encode()
	scopedDSN := target.String()
	if err = Migrate(ctx, scopedDSN); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return store
}
func testProject(t *testing.T, s *Store) (Project, string) {
	t.Helper()
	ctx := context.Background()
	name, err := newID("tenant_")
	if err != nil {
		t.Fatal(err)
	}
	tenant, _, err := s.CreateTenant(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	project, token, err := s.CreateProject(ctx, Project{
		TenantID: tenant, Name: "main", RepositoryURL: "https://example.invalid/example.git",
		BaseRef: fixtureRef, Backend: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	return project, token
}

func testSubmission(p Project, key, prompt string) Submission {
	return Submission{TenantID: p.TenantID, ProjectID: p.ID, Prompt: prompt,
		Options: JobOptions{Mode: ModeReadOnly, MaxCostUSD: 1, MaxTurns: 3},
		Backend: "api", BaseRef: p.BaseRef, IdempotencyKey: key}
}

func TestRepositoryMigrationRejectsLegacyJobs(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	if _, _, err := s.Submit(ctx, testSubmission(p, "legacy-job", "inspect")); err != nil {
		t.Fatal(err)
	}
	dropTenantBudgetSchema(t, s)
	if _, err := s.db.Exec(ctx, "ALTER TABLE jobs DROP COLUMN repository_url"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, "DROP TABLE knowledge_admissions,knowledge_changes,retrieval_records,knowledge_active_versions,knowledge_members,knowledge_versions,knowledge_entries; DROP FUNCTION knowledge_membership_creation_only(); DELETE FROM schema_migrations WHERE version>=2"); err != nil {
		t.Fatal(err)
	}
	err := Migrate(ctx, s.db.Config().ConnString())
	if err == nil || !strings.Contains(err.Error(), "empty jobs table") {
		t.Fatalf("legacy jobs were silently assigned a current project URL: %v", err)
	}
	var version int
	if err := s.db.QueryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 1 {
		t.Fatalf("failed migration changed schema version: %d %v", version, err)
	}
}

func TestRepositoryMigrationUpgradesEmptyLegacyStore(t *testing.T) {
	s := testStore(t)
	dropTenantBudgetSchema(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(ctx, "ALTER TABLE jobs DROP COLUMN repository_url"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, "DROP TABLE knowledge_admissions,knowledge_changes,retrieval_records,knowledge_active_versions,knowledge_members,knowledge_versions,knowledge_entries; DROP FUNCTION knowledge_membership_creation_only(); DELETE FROM schema_migrations WHERE version>=2"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
		t.Fatalf("empty legacy schema upgrade: %v", err)
	}
	var version int
	if err := s.db.QueryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 7 {
		t.Fatalf("migration version: %d %v", version, err)
	}
}

func TestRepositoryMigrationLeavesVersionSevenDatabaseUnchanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	type migration struct {
		version int
		applied time.Time
	}
	readVersions := func() []migration {
		t.Helper()
		rows, err := s.db.Query(ctx, "SELECT version, applied_at FROM schema_migrations ORDER BY version")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var versions []migration
		for rows.Next() {
			var row migration
			if err := rows.Scan(&row.version, &row.applied); err != nil {
				t.Fatal(err)
			}
			versions = append(versions, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return versions
	}
	before := readVersions()
	if len(before) != 7 {
		t.Fatalf("unexpected starting schema versions: %+v", before)
	}
	for i, record := range before {
		if record.version != i+1 {
			t.Fatalf("unexpected starting schema versions: %+v", before)
		}
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
			t.Fatalf("migration on version 7, pass %d: %v", i+1, err)
		}
	}
	after := readVersions()
	if len(after) != len(before) {
		t.Fatalf("migration changed version count: before=%+v after=%+v", before, after)
	}
	for i := range before {
		if before[i].version != after[i].version || !before[i].applied.Equal(after[i].applied) {
			t.Fatalf("migration changed version record: before=%+v after=%+v", before, after)
		}
	}
}

func TestRepositoryOpenRejectsVersionOneDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(ctx, "DELETE FROM schema_migrations WHERE version>=2"); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, s.db.Config().ConnString())
	if opened != nil {
		opened.Close()
		t.Fatal("Open accepted a version 1 database")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported schema version 1") {
		t.Fatalf("Open error for version 1: %v", err)
	}
}

func TestRepositoryOpenRejectsVersionTwoDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(ctx, "DELETE FROM schema_migrations WHERE version>=3"); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, s.db.Config().ConnString())
	if opened != nil {
		opened.Close()
		t.Fatal("Open accepted a version 2 database")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported schema version 2") {
		t.Fatalf("Open error for version 2: %v", err)
	}
}

func TestPostgresMigrationAndScopedSubmission(t *testing.T) {
	s := testStore(t)
	p, token := testProject(t, s)
	ctx := context.Background()
	allowed, err := s.WorkerAuthorized(ctx, p.ID, token)
	if err != nil || !allowed {
		t.Fatalf("project token failed: %v", err)
	}
	allowed, err = s.WorkerAuthorized(ctx, p.ID, "wrong")
	if err != nil || allowed {
		t.Fatalf("wrong token authorized: %v", err)
	}
	input := testSubmission(p, "stable-key", "hello")
	const contenders = 12
	var wg sync.WaitGroup
	ids := make(chan string, contenders)
	failures := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, _, err := s.Submit(ctx, input)
			if err != nil {
				failures <- err
				return
			}
			ids <- job.ID
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate idempotent jobs: %s vs %s", first, id)
		}
	}
	input.Prompt = "different"
	_, _, err = s.Submit(ctx, input)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting content accepted: %v", err)
	}
	_, err = s.GetJob(ctx, "another-tenant", p.ID, first)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	_, err = s.GetJob(ctx, p.TenantID, "another-project", first)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read: %v", err)
	}
	events, err := s.Events(ctx, p.TenantID, p.ID, first, 0, 10)
	if err != nil || len(events) != 1 || events[0].Kind != "queued" {
		t.Fatalf("events: %+v %v", events, err)
	}
}

func TestPostgresAdmissionClaimFencingAndRecovery(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	submitted, _, err := s.Submit(ctx, testSubmission(p, "", "one"))
	if err != nil {
		t.Fatal(err)
	}
	demand, err := s.ScalingDemand(ctx, p.ID)
	if err != nil || demand != 0 {
		t.Fatalf("queued work activated scaling: %d %v", demand, err)
	}
	admitted, err := s.AdmitOne(ctx)
	if err != nil || admitted != submitted.ID {
		t.Fatalf("admission: %q %v", admitted, err)
	}
	demand, err = s.ScalingDemand(ctx, p.ID)
	if err != nil || demand != 1 {
		t.Fatalf("admitted work invisible: %d %v", demand, err)
	}
	claimed, err := s.Claim(ctx, p.ID, "worker-a")
	if err != nil || claimed.ID != submitted.ID || claimed.Generation != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	_, err = s.Claim(ctx, p.ID, "worker-b")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate owner: %v", err)
	}
	good, cancel, err := s.Heartbeat(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation)
	if err != nil || !good || cancel {
		t.Fatalf("heartbeat: %v %v %v", good, cancel, err)
	}
	cp := testCheckpoint(t, `{ "version":1, "sequence":1, "boundary":"model_intent", "task_digest":"digest", "pending":{"kind":"model","id":"turn-1"}, "next_turn":1 }`)
	if err = s.SaveCheckpoint(ctx, p.ID, claimed.ID, "worker-b", claimed.Generation, cp); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale worker write: %v", err)
	}
	if err = s.SaveCheckpoint(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation+1, cp); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale generation write: %v", err)
	}
	if err = s.SaveCheckpoint(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation, cp); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCheckpoint(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation, cp); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate checkpoint accepted: %v", err)
	}
	restored, err := s.LatestCheckpoint(ctx, p.TenantID, p.ID, claimed.ID)
	if err != nil || restored.Sequence != 1 || restored.Pending.ID != "turn-1" || string(restored.Bytes()) != string(cp.Bytes()) {
		t.Fatalf("checkpoint: %+v %v", restored, err)
	}
	_, err = s.LatestCheckpoint(ctx, "another-tenant", p.ID, claimed.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant checkpoint: %v", err)
	}
	_, err = s.db.Exec(ctx, "UPDATE checkpoints SET boundary='corrupted' WHERE job_id=$1", claimed.ID)
	if err == nil {
		t.Fatal("append-only checkpoint was mutable")
	}
	_, err = s.db.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	good, _, err = s.Heartbeat(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation)
	if err != nil || good {
		t.Fatalf("expired lease renewed: %v %v", good, err)
	}
	count, err := s.MarkExpired(ctx)
	if err != nil || count < 1 {
		t.Fatalf("expiration: %d %v", count, err)
	}
	observed, err := s.GetJob(ctx, p.TenantID, p.ID, claimed.ID)
	if err != nil || observed.State != RecoveryRequired {
		t.Fatalf("unproven stop was reassigned: %+v %v", observed, err)
	}
	if err = s.SaveCheckpoint(ctx, p.ID, claimed.ID, "worker-a", claimed.Generation, cp); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("expired writer advanced: %v", err)
	}
	if _, err = s.Claim(ctx, p.ID, "worker-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recovery job was reclaimed: %v", err)
	}
	demand, err = s.ScalingDemand(ctx, p.ID)
	if err != nil || demand != 0 {
		t.Fatalf("uncertain job drives new pod: %d %v", demand, err)
	}
}

func TestPostgresAdmissionCapsAcrossProjectsAndTenants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var projects []Project
	for i := 0; i < 3; i++ {
		p, _ := testProject(t, s)
		projects = append(projects, p)
		for j := 0; j < 3; j++ {
			_, _, err := s.Submit(ctx, testSubmission(p, "", fmt.Sprintf("%d-%d", i, j)))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	admitted := make(chan string, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.AdmitOne(ctx)
			if err != nil {
				failures <- err
				return
			}
			if id != "" {
				admitted <- id
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	close(admitted)
	seen := map[string]bool{}
	for id := range admitted {
		seen[id] = true
	}
	if len(seen) != GlobalSlots {
		t.Fatalf("admitted %d jobs, want %d", len(seen), GlobalSlots)
	}
	total := 0
	for _, p := range projects {
		n, err := s.ScalingDemand(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		if n > 1 {
			t.Fatalf("global admission did not distribute across tenants: project %s has %d", p.ID, n)
		}
	}
	if total != GlobalSlots {
		t.Fatalf("scaling demand %d differs from admission count", total)
	}
}

func TestPostgresSuccessfulFinishRequiresCheckpoint(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	job, _, err := s.Submit(ctx, testSubmission(p, "", "finish"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, p.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	ok := JobResult{Backend: "api", Status: StatusOK}
	if err = s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, ok); err == nil {
		t.Fatal("success without durable finished boundary")
	}
	cp := testCheckpoint(t, `{"version":1,"sequence":1,"boundary":"finished","task_digest":"digest","finished":true,"result":{"backend":"api","status":"ok"},"next_turn":1}`)
	if err = s.SaveCheckpoint(ctx, p.ID, job.ID, "owner", claim.Generation, cp); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, ok); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetJob(ctx, p.TenantID, p.ID, job.ID)
	if err != nil || after.State != Succeeded {
		t.Fatalf("finish: %+v %v", after, err)
	}
	if err = s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, ok); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale finalization: %v", err)
	}
}

func TestAdmissionMigrationUpgradesVersionThree(t *testing.T) {
	s := testStore(t)
	dropTenantBudgetSchema(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(ctx, "DROP TABLE knowledge_admissions; ALTER TABLE knowledge_versions DROP COLUMN ranking; DELETE FROM schema_migrations WHERE version>=4"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 7 {
		t.Fatalf("version %d: %v", version, err)
	}
}

func TestKnowledgeMigrationWeightsKeysInAPopulatedVersionFourDatabase(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(ctx, `DROP INDEX knowledge_text_gin;
ALTER TABLE knowledge_entries DROP COLUMN search_vector;
ALTER TABLE knowledge_entries ADD COLUMN search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;
CREATE INDEX knowledge_text_gin ON knowledge_entries USING gin(search_vector)`); err != nil {
		t.Fatal(err)
	}
	in := knowledgeFixture(p)
	in.Entries = []KnowledgeEntry{
		{Key: "migrationneedle", Content: "key match only", Source: "docs/key.md", Evidence: "fixture"},
		{Key: "aaa", Content: "migrationneedle", Source: "docs/body.md", Evidence: "fixture"},
	}
	id, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	search := KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: id, Query: "migrationneedle"}
	before, err := s.SearchKnowledge(ctx, search)
	if err != nil || len(before.Entries) != 1 || before.Entries[0].Key != "aaa" {
		t.Fatalf("legacy ranking: %#v %v", before, err)
	}
	dropTenantBudgetSchema(t, s)
	if _, err := s.db.Exec(ctx, "ALTER TABLE knowledge_versions DROP COLUMN ranking; ALTER TABLE knowledge_admissions DROP COLUMN ranking; DELETE FROM schema_migrations WHERE version>=5"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 7 {
		t.Fatalf("version %d: %v", version, err)
	}
	after, err := s.SearchKnowledge(ctx, search)
	if err != nil || len(after.Entries) != 2 || after.Entries[0].Key != "migrationneedle" || after.Entries[1].Key != "aaa" {
		t.Fatalf("migrated ranking: %#v %v", after, err)
	}
}

func dropTenantBudgetSchema(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(context.Background(), `ALTER TABLE tenants DROP COLUMN budget_usd;
        ALTER TABLE jobs DROP COLUMN reserved_usd, DROP COLUMN spent_usd;
        DELETE FROM schema_migrations WHERE version=7`); err != nil {
		t.Fatal(err)
	}
}

func TestTenantBudgetMigrationBackfillsVersionSixJobs(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	cases := []struct {
		name     string
		state    State
		result   string
		reserved float64
		spent    *float64
	}{
		{name: "queued", state: Queued, reserved: 2.75},
		{name: "canceled without attempt", state: Canceled, reserved: 2.75, spent: new(0.0)},
		{name: "below ceiling", state: Succeeded, result: `{"cost_usd":0.25,"cost_is_estimate":true}`, reserved: 2.75, spent: new(0.25)},
		{name: "above ceiling", state: Succeeded, result: `{"cost_usd":4}`, reserved: 2.75, spent: new(2.75)},
		{name: "estimated zero", state: Failed, result: `{"cost_usd":0,"cost_is_estimate":true}`, reserved: 2.75, spent: new(0.0)},
		{name: "missing cost", state: Failed, result: `{"status":"error"}`, reserved: 2.75},
		{name: "malformed cost", state: Failed, result: `{"cost_usd":"unknown","cost_is_estimate":true}`, reserved: 2.75},
		{name: "malformed ceiling", state: Queued},
	}
	var ids []string
	for _, tc := range cases {
		input := testSubmission(p, "", tc.name)
		input.Options.MaxCostUSD = 2.75
		job, _, err := s.Submit(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}
	dropTenantBudgetSchema(t, s)
	for i, tc := range cases {
		id := ids[i]
		if _, err := s.db.Exec(ctx, `UPDATE jobs SET state=$2, result=NULLIF($3::text, '')::jsonb WHERE id=$1`, id, tc.state, tc.result); err != nil {
			t.Fatal(err)
		}
		if tc.result != "" {
			if _, err := s.db.Exec(ctx, "INSERT INTO attempts(job_id,generation,worker_id,ended_at) VALUES($1,1,'legacy-worker',now())", id); err != nil {
				t.Fatal(err)
			}
		}
		if tc.reserved == 0 {
			if _, err := s.db.Exec(ctx, `UPDATE jobs SET options='{"max_cost_usd":"unknown"}' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := Migrate(ctx, s.db.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 7 {
		t.Fatalf("version %d: %v", version, err)
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reserved float64
			var spent *float64
			if err := s.db.QueryRow(ctx, "SELECT reserved_usd,spent_usd FROM jobs WHERE id=$1", ids[i]).Scan(&reserved, &spent); err != nil {
				t.Fatal(err)
			}
			if reserved != tc.reserved || (spent == nil) != (tc.spent == nil) || (spent != nil && *spent != *tc.spent) {
				t.Fatalf("reserved %g, spent %v; want reserved %g, spent %v", reserved, spent, tc.reserved, tc.spent)
			}
		})
	}
	assertTenantExposure(t, s, p.TenantID, 8.25, 3)
	var indexed bool
	if err := s.db.QueryRow(ctx, "SELECT to_regclass('jobs_tenant_exposure') IS NOT NULL").Scan(&indexed); err != nil || !indexed {
		t.Fatalf("tenant exposure index present: %v, err %v", indexed, err)
	}
}

func assertTenantExposure(t *testing.T, s *Store, tenantID string, reserved, spent float64) {
	t.Helper()
	got, err := s.TenantBudget(context.Background(), tenantID)
	if err != nil || got.TenantID != tenantID || got.ReservedUSD != reserved || got.SpentUSD != spent || got.CommittedUSD != reserved+spent {
		t.Fatalf("budget = %+v, %v; want reserved %g, spent %g", got, err, reserved, spent)
	}
}

func TestTenantBudgetExactFitAndRetry(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	limit := 0.3
	if err := s.SetTenantBudget(ctx, p.TenantID, &limit); err != nil {
		t.Fatal(err)
	}
	first := testSubmission(p, "first", "inspect")
	first.Options.MaxCostUSD = 0.1
	if _, reused, err := s.Submit(ctx, first); err != nil || reused {
		t.Fatalf("below budget: reused %t, err %v", reused, err)
	}
	second := testSubmission(p, "second", "inspect")
	second.Options.MaxCostUSD = 0.2
	job, reused, err := s.Submit(ctx, second)
	if err != nil || reused {
		t.Fatalf("exact fit: reused %t, err %v", reused, err)
	}
	retry, reused, err := s.Submit(ctx, second)
	if err != nil || !reused || retry.ID != job.ID {
		t.Fatalf("retry: job %s, reused %t, err %v", retry.ID, reused, err)
	}
	second.Prompt = "different"
	if _, _, err := s.Submit(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	newJob := testSubmission(p, "over-budget", "inspect")
	if _, _, err := s.Submit(ctx, newJob); !errors.Is(err, ErrTenantBudget) ||
		err.Error() != "tenant budget: max_cost_usd 1 would pass the budget of 0.3 USD (0.3 committed)" {
		t.Fatalf("above budget: %v", err)
	}
	if exists, err := s.IdempotentJobExists(ctx, p.TenantID, p.ID, newJob.IdempotencyKey); err != nil || exists {
		t.Fatalf("refused job exists = %t, err = %v", exists, err)
	}
	assertTenantExposure(t, s, p.TenantID, 0.3, 0)
}

func TestTenantBudgetConcurrentSubmissions(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	limit := 5.0
	if err := s.SetTenantBudget(ctx, p.TenantID, &limit); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 20)
	for range 20 {
		go func() {
			<-start
			_, _, err := s.Submit(ctx, testSubmission(p, "", "inspect"))
			results <- err
		}()
	}
	close(start)
	var accepted, refused int
	for range 20 {
		switch err := <-results; {
		case err == nil:
			accepted++
		case errors.Is(err, ErrTenantBudget):
			refused++
		default:
			t.Errorf("submit: %v", err)
		}
	}
	if accepted != 5 || refused != 15 {
		t.Fatalf("accepted %d, refused %d", accepted, refused)
	}
	assertTenantExposure(t, s, p.TenantID, 5, 0)
}

func TestTenantBudgetReleasesJobsWithoutAttempts(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		timeout     bool
	}{
		{name: "queued cancel"},
		{name: "admitted cancel", setup: "state='admitted'"},
		{name: "waiting approval cancel", setup: "state='waiting_approval'"},
		{name: "admission timeout", setup: "state='admitted', admission_attempts=3, admission_deadline=now()-interval '1 second'", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			p, _ := testProject(t, s)
			ctx := context.Background()
			limit := 1.0
			if err := s.SetTenantBudget(ctx, p.TenantID, &limit); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Submit(ctx, testSubmission(p, "", "inspect"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.setup != "" {
				if _, err := s.db.Exec(ctx, "UPDATE jobs SET "+tc.setup+" WHERE id=$1", job.ID); err != nil {
					t.Fatal(err)
				}
			}
			want := Canceled
			if tc.timeout {
				want = Failed
				if _, err := s.AdmitOne(ctx); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.Cancel(ctx, p.TenantID, p.ID, job.ID); err != nil {
				t.Fatal(err)
			}
			var state State
			var spent *float64
			if err := s.db.QueryRow(ctx, "SELECT state,spent_usd FROM jobs WHERE id=$1", job.ID).Scan(&state, &spent); err != nil || state != want || spent == nil || *spent != 0 {
				t.Fatalf("state %s, spent %v, err %v; want %s, spent 0", state, spent, err, want)
			}
			assertTenantExposure(t, s, p.TenantID, 0, 0)
			if _, _, err := s.Submit(ctx, testSubmission(p, "", "replacement")); err != nil {
				t.Fatalf("reservation not released: %v", err)
			}
		})
	}
}

func TestTenantBudgetFinishAccountsForWorkerCost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   JobResult
		want     State
		reserved float64
		spent    float64
	}{
		{"below", JobResult{Status: StatusOK, CostUSD: 0.25}, Succeeded, 0, 0.25},
		{"above", JobResult{Status: StatusError, CostUSD: 2}, Failed, 0, 1},
		{"negative", JobResult{Status: StatusCanceled, CostUSD: -0.25}, Canceled, 0, 0},
		{"recovery", JobResult{Status: StatusError, CostUSD: 0.25, RecoveryRequired: true}, RecoveryRequired, 1, 0},
		{"unknown", JobResult{Status: StatusError}, Failed, 1, 0},
		{"estimated zero", JobResult{Status: StatusError, CostEstimate: true}, Failed, 0, 0},
		{"waiting approval with attempt", JobResult{}, Canceled, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			p, _ := testProject(t, s)
			ctx := context.Background()
			limit := 1.0
			if err := s.SetTenantBudget(ctx, p.TenantID, &limit); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Submit(ctx, testSubmission(p, "", "inspect"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AdmitOne(ctx); err != nil {
				t.Fatal(err)
			}
			claim, err := s.Claim(ctx, p.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			switch tc.result.Status {
			case "":
				if _, err := s.db.Exec(ctx, "UPDATE jobs SET state='waiting_approval' WHERE id=$1", job.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Cancel(ctx, p.TenantID, p.ID, job.ID); err != nil {
					t.Fatal(err)
				}
			case StatusOK:
				cp := testCheckpoint(t, `{"version":1,"sequence":1,"boundary":"finished","task_digest":"digest","finished":true,"result":{"backend":"api","status":"ok"},"next_turn":1}`)
				if err := s.SaveCheckpoint(ctx, p.ID, job.ID, "owner", claim.Generation, cp); err != nil {
					t.Fatal(err)
				}
			case StatusCanceled:
				if _, err := s.Cancel(ctx, p.TenantID, p.ID, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.result.Status != "" {
				tc.result.Backend = "api"
				if err := s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, tc.result); err != nil {
					t.Fatal(err)
				}
			}
			var state State
			var spent *float64
			if err := s.db.QueryRow(ctx, "SELECT state,spent_usd FROM jobs WHERE id=$1", job.ID).Scan(&state, &spent); err != nil || state != tc.want {
				t.Fatalf("state %s, err %v; want %s", state, err, tc.want)
			}
			if (tc.reserved != 0 && spent != nil) || (tc.reserved == 0 && (spent == nil || *spent != tc.spent)) {
				t.Fatalf("spent %v, want %g or NULL for reserved %g", spent, tc.spent, tc.reserved)
			}
			assertTenantExposure(t, s, p.TenantID, tc.reserved, tc.spent)
			if tc.want == Succeeded {
				next := testSubmission(p, "", "remaining budget")
				next.Options.MaxCostUSD = 0.75
				if _, _, err := s.Submit(ctx, next); err != nil {
					t.Fatalf("released amount unavailable: %v", err)
				}
				assertTenantExposure(t, s, p.TenantID, 0.75, 0.25)
			}
		})
	}
}

func TestTenantBudgetRecoveryKeepsReservation(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   State
	}{
		{"complete", Succeeded},
		{"fail", Failed},
		{"resume", Queued},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s := testStore(t)
			p, _ := testProject(t, s)
			ctx := context.Background()
			job, _, err := s.Submit(ctx, testSubmission(p, "", "inspect"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AdmitOne(ctx); err != nil {
				t.Fatal(err)
			}
			claim, err := s.Claim(ctx, p.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if tc.action == "complete" {
				cp := testCheckpoint(t, `{"version":1,"sequence":1,"boundary":"finished","task_digest":"digest","finished":true,"result":{"backend":"api","status":"ok"},"next_turn":1}`)
				if err := s.SaveCheckpoint(ctx, p.ID, job.ID, "owner", claim.Generation, cp); err != nil {
					t.Fatal(err)
				}
			}
			result := JobResult{Backend: "api", Status: StatusError, CostUSD: 0.25, RecoveryRequired: true}
			if err := s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, result); err != nil {
				t.Fatal(err)
			}
			state, err := s.ResolveRecovery(ctx, p.TenantID, p.ID, job.ID, tc.action, "verified")
			if err != nil || state != tc.want {
				t.Fatalf("recovery state %s, err %v; want %s", state, err, tc.want)
			}
			want := tc.want
			if tc.action == "resume" {
				if _, err := s.Cancel(ctx, p.TenantID, p.ID, job.ID); err != nil {
					t.Fatal(err)
				}
				want = Canceled
			}
			var spent *float64
			if err := s.db.QueryRow(ctx, "SELECT state,spent_usd FROM jobs WHERE id=$1", job.ID).Scan(&state, &spent); err != nil || state != want || spent != nil {
				t.Fatalf("state %s, spent %v, err %v; want %s, spent NULL", state, spent, err, want)
			}
			assertTenantExposure(t, s, p.TenantID, 1, 0)
		})
	}
}

func TestTenantBudgetLoweringAllowsRunningJobToFinish(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	if err := s.SetTenantBudget(ctx, p.TenantID, new(1.0)); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Submit(ctx, testSubmission(p, "", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, p.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	assertTenantExposure(t, s, p.TenantID, 1, 0)
	if err := s.SetTenantBudget(ctx, p.TenantID, new(0.5)); err != nil {
		t.Fatal(err)
	}
	result := JobResult{Backend: "api", Status: StatusError, CostUSD: 0.75}
	if err := s.Finish(ctx, p.ID, job.ID, "owner", claim.Generation, result); err != nil {
		t.Fatal(err)
	}
	assertTenantExposure(t, s, p.TenantID, 0, 0.75)
	next := testSubmission(p, "", "new job")
	next.Options.MaxCostUSD = 0.25
	if _, _, err := s.Submit(ctx, next); !errors.Is(err, ErrTenantBudget) {
		t.Fatalf("new submission err %v; want %v", err, ErrTenantBudget)
	}
}

func TestStoredUnknownFields(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	job, _, err := s.Submit(ctx, testSubmission(p, "old-fields", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(ctx, `UPDATE jobs SET options='{"mode":"readonly","old_key":1}', result='{"status":"ok","old_key":1}', state='recovery_required' WHERE id=$1`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":1,"sequence":1,"boundary":"finished","task_digest":"d","finished":true,"old_key":1,"result":{"status":"ok","old_key":1}}`)
	digest := sha256.Sum256(raw)
	_, err = s.db.Exec(ctx, `INSERT INTO checkpoints(job_id,sequence,generation,boundary,digest,payload) VALUES($1,1,1,'finished',$2,$3)`, job.ID, digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, p.TenantID, p.ID, job.ID)
	if err != nil || got.Result == nil || got.Result.Status != StatusOK {
		t.Fatalf("job %+v: %v", got, err)
	}
	cp, err := s.LatestCheckpoint(ctx, p.TenantID, p.ID, job.ID)
	if err != nil || !cp.Finished {
		t.Fatalf("checkpoint %+v: %v", cp, err)
	}
	state, err := s.ResolveRecovery(ctx, p.TenantID, p.ID, job.ID, "complete", "verified")
	if err != nil || state != Succeeded {
		t.Fatalf("state %s: %v", state, err)
	}
}
