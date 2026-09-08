// Phase 8: read-only queries backing the two durable-state gauges
// docs/observability.md requires (taskforge_jobs_by_state,
// taskforge_active_workers) -- see internal/metrics's stateCollector,
// which calls these at scrape time rather than maintaining any in-memory
// bookkeeping that could drift after a restart, crash, or rolled-back
// transaction (docs/roadmap.md's "STATE METRICS" requirement). Both are
// plain reads: no locking, no side effects, exactly like GetByID/GetWorkflow.
package store

import (
	"context"
	"fmt"
	"time"
)

// JobStateCounts returns the current count of jobs in each state
// (SELECT state, count(*) FROM jobs GROUP BY state), per
// docs/observability.md's taskforge_jobs_by_state gauge. A state with
// zero current jobs is simply absent from the returned map.
func (s *Store) JobStateCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("store: job state counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("store: job state counts: scan: %w", err)
		}
		counts[state] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: job state counts: %w", err)
	}
	return counts, nil
}

// ActiveWorkerCount returns the number of distinct lease_owner values
// with a heartbeat_at within the last window, per
// docs/observability.md's taskforge_active_workers gauge ("distinct
// lease_owner values with a heartbeat_at within the last
// lease-extension interval"). window is a caller-supplied
// implementation choice (see internal/metrics.NewStateCollector's doc
// comment) -- v1 has no separate lease-extension-interval configuration
// surface to derive this from automatically.
func (s *Store) ActiveWorkerCount(ctx context.Context, window time.Duration) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT lease_owner)
		FROM jobs
		WHERE lease_owner IS NOT NULL
		  AND heartbeat_at >= now() - make_interval(secs => $1::double precision)`,
		window.Seconds(),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: active worker count: %w", err)
	}
	return count, nil
}
