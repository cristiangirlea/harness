package platform

import "context"

var metricJobStates = [...]string{
	"admitted", "canceled", "failed", "queued", "recovery_required", "running", "succeeded", "waiting_approval",
}

type StoreMetrics struct {
	Jobs                map[string]int64
	OldestQueuedSeconds float64
	Tenants             int64
	Projects            int64
	SchemaVersion       int
}

// Metrics reads service-wide counts in one database snapshot.
func (s *Store) Metrics(ctx context.Context) (StoreMetrics, error) {
	var m StoreMetrics
	var counts []int64
	err := s.db.QueryRow(ctx, `
		WITH totals AS (SELECT state, COUNT(*) AS count, MIN(created_at) AS oldest FROM jobs GROUP BY state)
		SELECT ARRAY(SELECT COALESCE(totals.count, 0)
			FROM unnest($1::text[]) WITH ORDINALITY AS states(name, position)
			LEFT JOIN totals ON totals.state = states.name ORDER BY states.position),
			COALESCE((SELECT GREATEST(0, EXTRACT(EPOCH FROM now() - oldest))::double precision
				FROM totals WHERE state = 'queued'), 0),
			(SELECT COUNT(*) FROM tenants),
			(SELECT COUNT(*) FROM projects),
			(SELECT COALESCE(MAX(version), 0) FROM schema_migrations)`,
		metricJobStates[:]).Scan(&counts, &m.OldestQueuedSeconds, &m.Tenants, &m.Projects, &m.SchemaVersion)
	if err != nil {
		return StoreMetrics{}, err
	}
	m.Jobs = make(map[string]int64, len(metricJobStates))
	for i, state := range metricJobStates {
		m.Jobs[state] = counts[i]
	}
	return m, nil
}
