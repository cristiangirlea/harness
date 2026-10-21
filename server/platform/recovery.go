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

// ResolveRecovery requires externally verified termination evidence and refuses unresolved model/tool intents.
func (s *Store) ResolveRecovery(ctx context.Context, tenantID, projectID, jobID, action, evidence string) (State, error) {
	if evidence == "" || len(evidence) > 1024 {
		return "", errors.New("termination evidence must be 1..1024 bytes")
	}
	if action != "resume" && action != "fail" && action != "complete" {
		return "", errors.New("action must be resume, fail or complete")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var state State
	var oldGeneration int64
	err = tx.QueryRow(ctx, "SELECT state,owner_generation FROM jobs WHERE id=$1 AND tenant_id=$2 AND project_id=$3 FOR UPDATE", jobID, tenantID, projectID).Scan(&state, &oldGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if state != RecoveryRequired {
		return "", ErrConflict
	}
	var cp agent.APICheckpoint
	var raw, digest []byte
	var sequence int64
	err = tx.QueryRow(ctx, "SELECT sequence,digest,payload FROM checkpoints WHERE job_id=$1 ORDER BY sequence DESC LIMIT 1", jobID).Scan(&sequence, &digest, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	hasCheckpoint := err == nil
	if hasCheckpoint {
		actual := sha256.Sum256(raw)
		if !equalDigest(digest, actual[:]) {
			return "", errors.New("checkpoint checksum mismatch")
		}
		if err = json.Unmarshal(raw, &cp); err != nil {
			return "", err
		}
		if cp.Sequence != uint64(sequence) {
			return "", errors.New("checkpoint sequence mismatch")
		}
	}
	next := Failed
	var result []byte
	switch action {
	case "resume":
		if hasCheckpoint && (cp.Pending != nil || cp.Finished) {
			return "", ErrConflict
		}
		next = Queued
	case "complete":
		if !hasCheckpoint || !cp.Finished || cp.Pending != nil {
			return "", ErrConflict
		}
		switch cp.Result.Status {
		case agent.StatusOK:
			next = Succeeded
		case agent.StatusCanceled:
			next = Canceled
		default:
			next = Failed
		}
		result, err = json.Marshal(cp.Result)
		if err != nil {
			return "", err
		}
	case "fail":
		next = Failed
	}
	_, err = tx.Exec(ctx, "UPDATE jobs SET state=$2,owner_id=NULL,lease_until=NULL,available_at=now(),result=$3,updated_at=now() WHERE id=$1", jobID, next, result)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE attempts SET ended_at=now() WHERE job_id=$1 AND generation=$2", jobID, oldGeneration); err != nil {
		return "", err
	}
	if err = appendEvent(ctx, tx, tenantID, projectID, jobID, "recovery_resolved",
		map[string]string{"action": action, "evidence": evidence, "state": string(next)}); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("resolve recovery: %w", err)
	}
	return next, nil
}
