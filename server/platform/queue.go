package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/jackc/pgx/v5"
)

func (s *Store) AdmitOne(ctx context.Context) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(831542662)"); err != nil {
		return "", err
	}
	if err = expireAdmissions(ctx, tx); err != nil {
		return "", err
	}
	var active int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE state IN ('admitted','running','recovery_required')").Scan(&active); err != nil {
		return "", err
	}
	if active >= GlobalSlots {
		return "", tx.Commit(ctx)
	}
	var id, tenant, project string
	const candidate = "SELECT j.id,j.tenant_id,j.project_id FROM jobs j " +
		"WHERE j.state='queued' AND j.available_at<=now() " +
		"AND (SELECT count(*) FROM jobs a WHERE a.tenant_id=j.tenant_id AND a.state IN ('admitted','running','recovery_required')) < $1 " +
		"AND (SELECT count(*) FROM jobs a WHERE a.project_id=j.project_id AND a.state IN ('admitted','running','recovery_required')) < $2 " +
		"ORDER BY COALESCE((SELECT max(x.admitted_at) FROM jobs x WHERE x.tenant_id=j.tenant_id),'-infinity'::timestamptz),j.created_at,j.id " +
		"FOR UPDATE OF j SKIP LOCKED LIMIT 1"
	err = tx.QueryRow(ctx, candidate, TenantSlots, ProjectSlots).Scan(&id, &tenant, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", tx.Commit(ctx)
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE jobs SET state='admitted',admitted_at=now(),admission_deadline=now()+interval '5 minutes',admission_attempts=admission_attempts+1,updated_at=now() WHERE id=$1", id); err != nil {
		return "", err
	}
	if err = appendEvent(ctx, tx, tenant, project, id, "admitted", nil); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func expireAdmissions(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, "UPDATE jobs SET state=CASE WHEN admission_attempts>=3 THEN 'failed' ELSE 'queued' END, "+
		"available_at=now()+LEAST(admission_attempts*interval '30 seconds',interval '5 minutes'),"+
		"admission_deadline=NULL,updated_at=now() "+
		"WHERE state='admitted' AND admission_deadline<now() RETURNING id,tenant_id,project_id,state")
	if err != nil {
		return err
	}
	type changed struct{ id, tenant, project, state string }
	var list []changed
	for rows.Next() {
		var c changed
		if err = rows.Scan(&c.id, &c.tenant, &c.project, &c.state); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range list {
		if err = appendEvent(ctx, tx, c.tenant, c.project, c.id, "admission_timeout", map[string]string{"state": c.state}); err != nil {
			return err
		}
	}
	return nil
}

// Claim never steals a job. A timed out owner first enters recovery_required.
func (s *Store) Claim(ctx context.Context, projectID, workerID string) (Job, error) {
	if workerID == "" || len(workerID) > 128 {
		return Job{}, errors.New("invalid worker ID")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	var id, tenant string
	err = tx.QueryRow(ctx, "SELECT id,tenant_id FROM jobs WHERE project_id=$1 AND state='admitted' AND admission_deadline>now() AND NOT EXISTS (SELECT 1 FROM jobs blocked WHERE blocked.project_id=$1 AND blocked.state='recovery_required') ORDER BY admitted_at,id FOR UPDATE SKIP LOCKED LIMIT 1", projectID).Scan(&id, &tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	var generation int64
	_, err = tx.Exec(ctx, "UPDATE jobs SET state='running',owner_id=$2,owner_generation=owner_generation+1,lease_until=now()+interval '60 seconds',updated_at=now() WHERE id=$1", id, workerID)
	if err != nil {
		return Job{}, err
	}
	if err = tx.QueryRow(ctx, "SELECT owner_generation FROM jobs WHERE id=$1", id).Scan(&generation); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO attempts(job_id,generation,worker_id) VALUES($1,$2,$3)", id, generation, workerID); err != nil {
		return Job{}, err
	}
	if err = appendEvent(ctx, tx, tenant, projectID, id, "running", map[string]any{"generation": generation}); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Job{}, err
	}
	return s.GetJob(ctx, tenant, projectID, id)
}

// Heartbeat reports cancellation alongside ownership. A failed check stops the worker.
func (s *Store) Heartbeat(ctx context.Context, projectID, jobID, workerID string, generation int64) (bool, bool, error) {
	var cancel bool
	err := s.db.QueryRow(ctx, "UPDATE jobs SET lease_until=now()+interval '60 seconds' WHERE id=$1 AND owner_id=$2 AND owner_generation=$3 AND project_id=$4 AND state='running' AND lease_until>now() RETURNING cancel_requested", jobID, workerID, generation, projectID).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, cancel, nil
}

func (s *Store) SaveCheckpoint(ctx context.Context, projectID, jobID, workerID string, generation int64, cp agent.APICheckpoint) error {
	if cp.Version != 1 || cp.Sequence == 0 || cp.Boundary == "" || cp.TaskDigest == "" {
		return errors.New("invalid API checkpoint")
	}
	payload, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if len(payload) > 16*1024*1024 {
		return errors.New("checkpoint exceeds 16 MiB")
	}
	digest := sha256.Sum256(payload)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tenant, project string
	var prev int64
	err = tx.QueryRow(ctx, "SELECT tenant_id,project_id,checkpoint_seq FROM jobs WHERE id=$1 AND owner_id=$2 AND owner_generation=$3 AND project_id=$4 AND state='running' AND lease_until>now() FOR UPDATE", jobID, workerID, generation, projectID).Scan(&tenant, &project, &prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotOwner
	}
	if err != nil {
		return err
	}
	if cp.Sequence != uint64(prev+1) {
		return fmt.Errorf("%w: expected checkpoint sequence %d, got %d", ErrConflict, prev+1, cp.Sequence)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO checkpoints(job_id,sequence,generation,boundary,digest,payload) VALUES($1,$2,$3,$4,$5,$6)", jobID, cp.Sequence, generation, cp.Boundary, digest[:], payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE jobs SET checkpoint_seq=$2,updated_at=now() WHERE id=$1", jobID, cp.Sequence); err != nil {
		return err
	}
	if err = appendEvent(ctx, tx, tenant, project, jobID, "checkpoint", map[string]any{"sequence": cp.Sequence, "boundary": cp.Boundary}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) LatestCheckpoint(ctx context.Context, tenantID, projectID, jobID string) (*agent.APICheckpoint, error) {
	var seq int64
	var expected, raw []byte
	err := s.db.QueryRow(ctx, "SELECT c.sequence,c.digest,c.payload FROM checkpoints c JOIN jobs j ON j.id=c.job_id WHERE c.job_id=$1 AND j.tenant_id=$2 AND j.project_id=$3 ORDER BY c.sequence DESC LIMIT 1", jobID, tenantID, projectID).Scan(&seq, &expected, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(raw)
	if len(expected) != len(actual) || !equalDigest(expected, actual[:]) {
		return nil, errors.New("checkpoint checksum mismatch")
	}
	var cp agent.APICheckpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	if cp.Sequence != uint64(seq) {
		return nil, errors.New("checkpoint sequence mismatch")
	}
	return &cp, nil
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *Store) Finish(ctx context.Context, projectID, jobID, workerID string, generation int64, result agent.Result) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tenant, project string
	var checkpointSeq int64
	var cancelRequested bool
	err = tx.QueryRow(ctx, "SELECT tenant_id,project_id,checkpoint_seq,cancel_requested FROM jobs WHERE id=$1 AND owner_id=$2 AND owner_generation=$3 AND project_id=$4 AND state='running' AND lease_until>now() FOR UPDATE", jobID, workerID, generation, projectID).Scan(&tenant, &project, &checkpointSeq, &cancelRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotOwner
	}
	if err != nil {
		return err
	}
	next := Failed
	switch {
	case result.RecoveryRequired:
		next = RecoveryRequired
	case result.Status == agent.StatusOK:
		var finished bool
		err = tx.QueryRow(ctx, "SELECT true FROM checkpoints WHERE job_id=$1 AND sequence=$2 AND boundary='finished'", jobID, checkpointSeq).Scan(&finished)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("successful run has no finished checkpoint")
		}
		if err != nil {
			return err
		}
		next = Succeeded
	case result.Status == agent.StatusCanceled:
		if cancelRequested {
			next = Canceled
		} else {
			// A local shutdown or heartbeat failure is not a tenant cancellation.
			next = RecoveryRequired
		}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE jobs SET state=$2,result=$3,owner_id=NULL,lease_until=NULL,updated_at=now() WHERE id=$1", jobID, next, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE attempts SET ended_at=now() WHERE job_id=$1 AND generation=$2", jobID, generation); err != nil {
		return err
	}
	var details any
	if next == RecoveryRequired && !result.RecoveryRequired {
		details = map[string]string{"reason": "worker stopped without a tenant cancellation; executor termination must be verified"}
	}
	if err = appendEvent(ctx, tx, tenant, project, jobID, string(next), details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) MarkExpired(ctx context.Context) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "UPDATE jobs SET state='recovery_required',owner_id=NULL,lease_until=NULL,updated_at=now() WHERE state='running' AND lease_until<now() RETURNING id,tenant_id,project_id")
	if err != nil {
		return 0, err
	}
	type item struct{ id, tenant, project string }
	var found []item
	for rows.Next() {
		var row item
		if err = rows.Scan(&row.id, &row.tenant, &row.project); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, row := range found {
		if err = appendEvent(ctx, tx, row.tenant, row.project, row.id, "recovery_required", map[string]string{"reason": "lease expired; executor termination is unconfirmed"}); err != nil {
			return 0, err
		}
	}
	return len(found), tx.Commit(ctx)
}

func (s *Store) Cancel(ctx context.Context, tenantID, projectID, jobID string) (State, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var current State
	err = tx.QueryRow(ctx, "SELECT state FROM jobs WHERE id=$1 AND tenant_id=$2 AND project_id=$3 FOR UPDATE", jobID, tenantID, projectID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	switch current {
	case Queued, Admitted, WaitingApproval:
		_, err = tx.Exec(ctx, "UPDATE jobs SET state='canceled',updated_at=now() WHERE id=$1", jobID)
		if err == nil {
			err = appendEvent(ctx, tx, tenantID, projectID, jobID, "canceled", nil)
		}
		current = Canceled
	case Running:
		_, err = tx.Exec(ctx, "UPDATE jobs SET cancel_requested=true,updated_at=now() WHERE id=$1", jobID)
		if err == nil {
			err = appendEvent(ctx, tx, tenantID, projectID, jobID, "cancel_requested", nil)
		}
	}
	if err != nil {
		return "", err
	}
	return current, tx.Commit(ctx)
}

func (s *Store) ScalingDemand(ctx context.Context, projectID string) (int, error) {
	var count int
	err := s.db.QueryRow(ctx, "SELECT count(j.id) FILTER (WHERE j.state IN ('admitted','running')) FROM projects p LEFT JOIN jobs j ON j.project_id=p.id WHERE p.id=$1 GROUP BY p.id", projectID).Scan(&count)
	return count, err
}

func (s *Store) Events(ctx context.Context, tenantID, projectID, jobID string, after int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("event page size must be 1..1000")
	}
	var exists bool
	if err := s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1 AND tenant_id=$2 AND project_id=$3)", jobID, tenantID, projectID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.Query(ctx, "SELECT id,job_id,kind,details,created_at FROM job_events WHERE tenant_id=$1 AND project_id=$2 AND job_id=$3 AND id>$4 ORDER BY id LIMIT $5", tenantID, projectID, jobID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.JobID, &event.Kind, &event.Details, &event.Created); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
