package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db             *pgxpool.Pool
	knowledge      *pgxpool.Pool
	retrievalSlots chan struct{}
}

func checkSchemaVersion(version int) error {
	if version < 7 {
		return fmt.Errorf("unsupported schema version %d: run the explicit migrate command", version)
	}
	if version != 7 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	return nil
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database is not initialized: run the explicit migrate command: %w", err)
	}
	if err := checkSchemaVersion(version); err != nil {
		pool.Close()
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, err
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "1500"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "250"
	knowledge, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{db: pool, knowledge: knowledge, retrievalSlots: make(chan struct{}, 2)}, nil
}

func (s *Store) Close() {
	if s.knowledge != nil {
		s.knowledge.Close()
	}
	s.db.Close()
}

func Migrate(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(831542661)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > 7 {
		return fmt.Errorf("database schema version %d is newer than this binary", version)
	}
	if version == 0 {
		script, err := migrationFiles.ReadFile("migrations/001_initial.sql")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (1)"); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		script, err := migrationFiles.ReadFile("migrations/002_job_repository.sql")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 2: %w", err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (2)"); err != nil {
			return err
		}
	}
	if version <= 2 {
		script, err := migrationFiles.ReadFile("migrations/003_project_knowledge.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (3)"); err != nil {
			return err
		}
	}
	if version <= 3 {
		script, err := migrationFiles.ReadFile("migrations/004_knowledge_admissions.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 4: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (4)"); err != nil {
			return err
		}
	}
	if version <= 4 {
		script, err := migrationFiles.ReadFile("migrations/005_knowledge_weighted_search.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (5)"); err != nil {
			return err
		}
	}
	if version <= 5 {
		script, err := migrationFiles.ReadFile("migrations/006_knowledge_ranking.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 6: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (6)"); err != nil {
			return err
		}
	}
	if version <= 6 {
		script, err := migrationFiles.ReadFile("migrations/007_tenant_budgets.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration 7: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES (7)"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateTenant(ctx context.Context, name string) (string, string, error) {
	if name == "" || len(name) > 100 || strings.TrimSpace(name) != name {
		return "", "", errors.New("tenant name must be 1..100 trimmed characters")
	}
	id, err := newID("t_")
	if err != nil {
		return "", "", err
	}
	token, digest, err := newToken()
	if err != nil {
		return "", "", err
	}
	_, err = s.db.Exec(ctx, "INSERT INTO tenants(id,name,token_hash) VALUES($1,$2,$3)", id, name, digest)
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

func (s *Store) TenantAuthorized(ctx context.Context, tenantID, token string) (bool, error) {
	if len(token) != 64 {
		return false, nil
	}
	var wanted []byte
	err := s.db.QueryRow(ctx, "SELECT token_hash FROM tenants WHERE id=$1", tenantID).Scan(&wanted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(wanted, digest[:]) == 1, nil
}

const tenantBudgetTotals = `
    SELECT COALESCE(sum(COALESCE(known_spend,reserved_usd)), 0) AS committed,
           COALESCE(sum(reserved_usd) FILTER (WHERE known_spend IS NULL), 0) AS reserved,
           COALESCE(sum(known_spend), 0) AS spent
    FROM (
        SELECT reserved_usd,
               CASE WHEN state IN ('succeeded','failed','canceled') THEN spent_usd END AS known_spend
        FROM jobs WHERE tenant_id=$1
    ) AS exposure`

func validBudget(budget *float64) bool {
	return budget == nil || (*budget > 0 && !math.IsInf(*budget, 0))
}

func (s *Store) SetTenantBudget(ctx context.Context, tenantID string, budget *float64) error {
	if !validBudget(budget) {
		return errors.New("budget_usd must be a positive finite number or null")
	}
	var amount any
	if budget != nil {
		amount = strconv.FormatFloat(*budget, 'g', -1, 64)
	}
	tag, err := s.db.Exec(ctx, "UPDATE tenants SET budget_usd=$2::text::numeric WHERE id=$1", tenantID, amount)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) TenantBudget(ctx context.Context, tenantID string) (TenantBudget, error) {
	var budget TenantBudget
	err := s.db.QueryRow(ctx, `SELECT id,budget_usd,committed,reserved,spent
        FROM tenants CROSS JOIN (`+tenantBudgetTotals+`) AS totals WHERE id=$1`, tenantID).
		Scan(&budget.TenantID, &budget.BudgetUSD, &budget.CommittedUSD, &budget.ReservedUSD, &budget.SpentUSD)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantBudget{}, ErrNotFound
	}
	return budget, err
}

func checkTenantBudget(ctx context.Context, tx pgx.Tx, tenantID string, budget pgtype.Numeric, maxCost float64, reservation string, required bool) error {
	if !budget.Valid {
		if required {
			return fmt.Errorf("%w: this tenant has no budget and the service requires one (HRN_REQUIRE_TENANT_BUDGET)", ErrTenantBudget)
		}
		return nil
	}
	var committed float64
	var exceeded bool
	err := tx.QueryRow(ctx, `SELECT committed, committed+$2::text::numeric > $3::numeric
        FROM (`+tenantBudgetTotals+`) AS totals`, tenantID, reservation, budget).Scan(&committed, &exceeded)
	if err != nil {
		return err
	}
	if exceeded {
		limit, err := budget.Float64Value()
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: max_cost_usd %g would pass the budget of %g USD (%g committed)", ErrTenantBudget, maxCost, limit.Float64, committed)
	}
	return nil
}

// CreateProject returns a worker token once. Only its digest is stored.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, string, error) {
	if err := validateProject(p); err != nil {
		return Project{}, "", err
	}
	id, err := newID("p_")
	if err != nil {
		return Project{}, "", err
	}
	token, digest, err := newToken()
	if err != nil {
		return Project{}, "", err
	}
	p.ID = id
	_, err = s.db.Exec(ctx, "INSERT INTO projects(id,tenant_id,name,repository_url,base_ref,backend,worker_token_hash) VALUES($1,$2,$3,$4,$5,$6,$7)",
		p.ID, p.TenantID, p.Name, p.RepositoryURL, p.BaseRef, p.Backend, digest)
	if err != nil {
		return Project{}, "", err
	}
	return p, token, nil
}

func (s *Store) WorkerAuthorized(ctx context.Context, projectID, token string) (bool, error) {
	if len(token) != 64 {
		return false, nil
	}
	var wanted []byte
	err := s.db.QueryRow(ctx, "SELECT worker_token_hash FROM projects WHERE id=$1", projectID).Scan(&wanted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(wanted, digest[:]) == 1, nil
}

// IdempotentJobExists reports whether a job was already submitted under this idempotency key.
func (s *Store) IdempotentJobExists(ctx context.Context, tenantID, projectID, key string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE tenant_id=$1 AND project_id=$2 AND idempotency_key=$3)", tenantID, projectID, key).Scan(&exists)
	return exists, err
}

func (s *Store) Submit(ctx context.Context, input Submission) (Job, bool, error) {
	return s.submit(ctx, input, false)
}

func (s *Store) submit(ctx context.Context, input Submission, budgetRequired bool) (Job, bool, error) {
	if err := validateSubmission(input); err != nil {
		return Job{}, false, err
	}
	hash, err := requestHash(input)
	if err != nil {
		return Job{}, false, err
	}
	options, err := json.Marshal(input.Options)
	if err != nil {
		return Job{}, false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback(ctx)
	var budget pgtype.Numeric
	// Serialize reservations and budget updates before looking up the project or job.
	err = tx.QueryRow(ctx, "SELECT budget_usd FROM tenants WHERE id=$1 FOR UPDATE", input.TenantID).Scan(&budget)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, ErrNotFound
	}
	if err != nil {
		return Job{}, false, err
	}
	var profile, base, repositoryURL string
	err = tx.QueryRow(ctx, "SELECT backend,base_ref,repository_url FROM projects WHERE id=$1 AND tenant_id=$2", input.ProjectID, input.TenantID).Scan(&profile, &base, &repositoryURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, ErrNotFound
	}
	if err != nil {
		return Job{}, false, err
	}
	if input.Backend != profile || input.BaseRef != base {
		return Job{}, false, ErrUnsupported
	}
	key := any(nil)
	if input.IdempotencyKey != "" {
		key = input.IdempotencyKey
		var id string
		var originalHash []byte
		err = tx.QueryRow(ctx, "SELECT id,request_hash FROM jobs WHERE tenant_id=$1 AND project_id=$2 AND idempotency_key=$3", input.TenantID, input.ProjectID, key).Scan(&id, &originalHash)
		if err == nil {
			if !bytes.Equal(hash, originalHash) {
				return Job{}, false, ErrConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return Job{}, false, err
			}
			job, err := s.GetJob(ctx, input.TenantID, input.ProjectID, id)
			return job, true, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, err
		}
	}
	maxCost := input.Options.MaxCostUSD
	reservation := strconv.FormatFloat(maxCost, 'g', -1, 64)
	if err := checkTenantBudget(ctx, tx, input.TenantID, budget, maxCost, reservation, budgetRequired); err != nil {
		return Job{}, false, err
	}
	id, err := newID("j_")
	if err != nil {
		return Job{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs(id,tenant_id,project_id,prompt,options,backend,base_ref,repository_url,request_hash,idempotency_key,reserved_usd)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::text::numeric)`,
		id, input.TenantID, input.ProjectID, input.Prompt, options, input.Backend, input.BaseRef, repositoryURL, hash, key, reservation); err != nil {
		return Job{}, false, err
	}
	if err := appendEvent(ctx, tx, input.TenantID, input.ProjectID, id, "queued", nil); err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	job, err := s.GetJob(ctx, input.TenantID, input.ProjectID, id)
	return job, false, err
}

func (s *Store) GetJob(ctx context.Context, tenantID, projectID, jobID string) (Job, error) {
	var job Job
	var options, result []byte
	err := s.db.QueryRow(ctx, `SELECT id,tenant_id,project_id,prompt,options,backend,base_ref,repository_url,state,
        owner_generation,COALESCE(owner_id,''),lease_until,checkpoint_seq,result
        FROM jobs WHERE id=$1 AND tenant_id=$2 AND project_id=$3`, jobID, tenantID, projectID).
		Scan(&job.ID, &job.TenantID, &job.ProjectID, &job.Prompt, &options, &job.Backend, &job.BaseRef, &job.RepositoryURL, &job.State,
			&job.Generation, &job.OwnerID, &job.LeaseUntil, &job.CheckpointSequence, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	job.Options, err = decodeStoredOptions(options)
	if err != nil {
		return Job{}, err
	}
	if len(result) != 0 {
		value, err := decodeResult(result, false, true)
		if err != nil {
			return Job{}, err
		}
		job.Result = &value
	}
	return job, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, tenantID, projectID, jobID, kind string, details any) error {
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	// Holding the job row until commit makes one job's event IDs commit in order,
	// so a reader's after cursor cannot pass an ID that is still uncommitted.
	if _, err = tx.Exec(ctx, "SELECT 1 FROM jobs WHERE id=$1 FOR NO KEY UPDATE", jobID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO job_events(tenant_id,project_id,job_id,kind,details) VALUES($1,$2,$3,$4,$5)",
		tenantID, projectID, jobID, kind, payload)
	return err
}
