// Shared test helpers for Phase 2 lease/reclaim/fencing tests. Per
// docs/testing-strategy.md ("Time-dependent tests ... control time
// explicitly ... rather than relying on wall-clock waits"), lease
// expiration is simulated by directly rewinding lease_expires_at via SQL
// against PostgreSQL's own clock, never by sleeping past a short TTL —
// this keeps every reclaim/expiry test deterministic and fast.
package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// forceExpireLease rewinds jobID's lease_expires_at to one second in the
// past (PostgreSQL's clock), simulating a worker that stopped
// heartbeating long enough for its lease to expire, without any real
// sleep. The row must currently be RUNNING with a non-NULL
// lease_expires_at, or this fails loudly (a misused helper should not
// silently no-op).
func forceExpireLease(t *testing.T, db *sql.DB, jobID uuid.UUID) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE jobs SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1 AND state = 'RUNNING'`, jobID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forceExpireLease: job %s was not RUNNING", jobID)
}

// forceSetEligibleAt rewinds or fast-forwards jobID's eligible_at to
// exactly delta relative to PostgreSQL's own clock (negative delta ->
// past, positive -> future), simulating retry-backoff timing without a
// real sleep. Used for RETRY_WAIT eligibility-boundary tests, mirroring
// forceExpireLease's role for lease-expiry tests. The row must currently
// exist and be in RETRY_WAIT, or this fails loudly.
func forceSetEligibleAt(t *testing.T, db *sql.DB, jobID uuid.UUID, delta time.Duration) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE jobs SET eligible_at = now() + $2 * interval '1 second'
		WHERE id = $1 AND state = 'RETRY_WAIT'`, jobID, delta.Seconds())
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forceSetEligibleAt: job %s was not RETRY_WAIT", jobID)
}

// attemptRow mirrors one job_attempts row for test assertions.
type attemptRow struct {
	AttemptNumber   int
	LeaseGeneration int64
	WorkerID        string
	StartedAt       time.Time
	FinishedAt      *time.Time
	Outcome         *string
	ErrorClass      *string
	ErrorMessage    *string
}

// attemptsForJob returns every job_attempts row for jobID, ordered by
// attempt_number, for tests asserting the append-only ledger's shape
// (TF-INV-007's mechanism, insofar as Phase 2 populates it).
func attemptsForJob(t *testing.T, db *sql.DB, jobID uuid.UUID) []attemptRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT attempt_number, lease_generation, worker_id, started_at, finished_at, outcome, error_class, error_message
		FROM job_attempts WHERE job_id = $1 ORDER BY attempt_number ASC`, jobID)
	require.NoError(t, err)
	defer rows.Close()

	var out []attemptRow
	for rows.Next() {
		var (
			a          attemptRow
			finishedAt sql.NullTime
			outcome    sql.NullString
			errorClass sql.NullString
			errorMsg   sql.NullString
		)
		require.NoError(t, rows.Scan(&a.AttemptNumber, &a.LeaseGeneration, &a.WorkerID, &a.StartedAt, &finishedAt, &outcome, &errorClass, &errorMsg))
		if finishedAt.Valid {
			a.FinishedAt = &finishedAt.Time
		}
		if outcome.Valid {
			a.Outcome = &outcome.String
		}
		if errorClass.Valid {
			a.ErrorClass = &errorClass.String
		}
		if errorMsg.Valid {
			a.ErrorMessage = &errorMsg.String
		}
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}
