// Package chaos implements Phase 9's ("Chaos, Load, and Failure Testing",
// docs/roadmap.md) deterministic fault-injection primitives: reusable
// helpers that manipulate real PostgreSQL state (never a mock, per
// docs/testing-strategy.md) to simulate the exact failure classes
// docs/failure-model.md documents as "handled in v1" -- lease expiry,
// database statement/transaction rollback, connection interruption, and
// a seeded, reproducible action picker for randomized-but-deterministic
// chaos campaigns.
//
// Every helper here operates at an "explicit testable boundary"
// (direct SQL against the test database, or PostgreSQL's own admin
// functions such as pg_terminate_backend) rather than adding a
// production-only crash hook to internal/store or internal/worker --
// docs/roadmap.md's Phase 9 scope explicitly warns against scattering
// "arbitrary production-only crash hooks through core code." Nothing in
// this package is imported by, or changes the behavior of, any
// production code path (cmd/api, cmd/worker, internal/store,
// internal/worker) -- it is exercised only from test binaries
// (internal/chaos's own _test.go files) and cmd/chaos's manual
// stress/soak harness, both of which already require a real,
// disposable/test PostgreSQL instance.
package chaos

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Rand is a concurrency-safe, seed-deterministic source of randomness for
// chaos campaigns -- the same shape and rationale as internal/retry.Rand
// (math/rand.Rand is not safe for concurrent use, and many chaos-campaign
// goroutines share one seeded source), kept as a separate type so a
// campaign's action-selection randomness is deterministic independently
// of any retry-backoff jitter the code under test also consumes.
//
// Per docs/roadmap.md's "Chaos Must Be Reproducible" requirement, every
// seeded chaos test in this repository logs its seed via t.Logf before
// running (see internal/chaos's own _test.go files) -- a failure
// reproduces exactly by rerunning with the same seed.
type Rand struct {
	mu   sync.Mutex
	src  *rand.Rand
	seed int64
}

// NewRand returns a Rand deterministically seeded from seed.
func NewRand(seed int64) *Rand {
	return &Rand{src: rand.New(rand.NewSource(seed)), seed: seed} //nolint:gosec // chaos action selection, not security-sensitive
}

// Seed returns the seed this Rand was constructed with, for logging on
// test failure (docs/roadmap.md: "A test failure must report enough
// information to reproduce the exact sequence").
func (r *Rand) Seed() int64 { return r.seed }

// Intn returns a pseudo-random int in [0, n).
func (r *Rand) Intn(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.src.Intn(n)
}

// Float64 returns a pseudo-random float64 in [0, 1).
func (r *Rand) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.src.Float64()
}

// Bool returns true with probability p (0 <= p <= 1).
func (r *Rand) Bool(p float64) bool {
	return r.Float64() < p
}

// Pick returns a pseudo-randomly chosen index in [0, len(weights)) per
// weights (which need not sum to 1 -- they are normalized internally).
// Used by chaos campaigns to pick a fault/action from a weighted menu
// deterministically. Panics if weights is empty or every weight is <= 0
// (a campaign construction bug, not a runtime condition to handle
// gracefully).
func (r *Rand) Pick(weights []float64) int {
	var total float64
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		panic("chaos: Pick called with no positive weight")
	}
	x := r.Float64() * total
	var cum float64
	for i, w := range weights {
		cum += w
		if x < cum {
			return i
		}
	}
	return len(weights) - 1
}

// ForceExpireLease rewinds jobID's lease_expires_at to one second in the
// past (PostgreSQL's own clock), simulating a worker that stopped
// heartbeating long enough for its lease to expire -- the exact
// mechanism internal/store's own test helpers use (never a real sleep),
// exported here for reuse by chaos campaigns. The row must currently be
// RUNNING, or this returns an error (a misused helper should not
// silently no-op).
func ForceExpireLease(ctx context.Context, db *sql.DB, jobID uuid.UUID) error {
	res, err := db.ExecContext(ctx, `
		UPDATE jobs SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1 AND state = 'RUNNING'`, jobID)
	if err != nil {
		return fmt.Errorf("chaos: force expire lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("chaos: force expire lease: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("chaos: force expire lease: job %s was not RUNNING", jobID)
	}
	return nil
}

// ForceExpireAllRunningLeases rewinds every currently-RUNNING job's
// lease_expires_at into the past at once, simulating a mass worker-fleet
// crash (docs/failure-model.md F1/F2, at scale) -- used by "multiple
// workers/process-like instances restart" campaigns. Returns the number
// of rows affected.
func ForceExpireAllRunningLeases(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
	if err != nil {
		return 0, fmt.Errorf("chaos: force expire all running leases: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("chaos: force expire all running leases: %w", err)
	}
	return n, nil
}

// ForceSetEligibleAt rewinds or fast-forwards jobID's eligible_at to
// exactly delta relative to PostgreSQL's own clock, simulating retry
// backoff or scheduling timing without a real sleep. The row must
// currently be RETRY_WAIT or QUEUED, or this returns an error.
func ForceSetEligibleAt(ctx context.Context, db *sql.DB, jobID uuid.UUID, delta time.Duration) error {
	res, err := db.ExecContext(ctx, `
		UPDATE jobs SET eligible_at = now() + $2 * interval '1 second'
		WHERE id = $1 AND state IN ('RETRY_WAIT', 'QUEUED')`, jobID, delta.Seconds())
	if err != nil {
		return fmt.Errorf("chaos: force set eligible_at: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("chaos: force set eligible_at: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("chaos: force set eligible_at: job %s was not RETRY_WAIT/QUEUED", jobID)
	}
	return nil
}

// ForceAllEligibleNow fast-forwards every job currently in fromState
// (e.g. "RETRY_WAIT") to immediate eligibility, simulating "time passes"
// for an entire pool at once without a real sleep. Returns the number of
// rows affected.
func ForceAllEligibleNow(ctx context.Context, db *sql.DB, fromState string) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() WHERE state = $1`, fromState)
	if err != nil {
		return 0, fmt.Errorf("chaos: force all eligible now: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("chaos: force all eligible now: %w", err)
	}
	return n, nil
}

// PoisonAttemptInsert seeds a conflicting job_attempts row for jobID's
// next attempt so that the next Claim() call's own INSERT collides on
// UNIQUE(job_id, attempt_number) and its enclosing transaction rolls
// back entirely -- the exact mechanism
// internal/store's TestClaim_RollbackOnAttemptConflictLeavesJobRowUnchanged
// uses, exported here for chaos campaigns exercising
// docs/failure-model.md F5 ("Database transaction rollback") at the
// claim boundary. nextAttemptNumber must equal the job's current
// attempt_count + 1 (Claim's own computation) for the collision to land.
func PoisonAttemptInsert(ctx context.Context, db *sql.DB, jobID uuid.UUID, nextAttemptNumber int) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, $3, 0, 'chaos-poison', now())`, uuid.New(), jobID, nextAttemptNumber)
	if err != nil {
		return fmt.Errorf("chaos: poison attempt insert: %w", err)
	}
	return nil
}

// UnpoisonAttemptInserts removes every row PoisonAttemptInsert seeded,
// clearing the way for the job to be claimed normally again.
func UnpoisonAttemptInserts(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM job_attempts WHERE worker_id = 'chaos-poison'`); err != nil {
		return fmt.Errorf("chaos: unpoison attempt inserts: %w", err)
	}
	return nil
}

// BackendPID returns the PostgreSQL backend process ID serving conn, via
// pg_backend_pid(). Used together with TerminateBackend to simulate a
// severed database connection (docs/failure-model.md F6, "Temporary
// database unavailability") at a precise, deterministic point in an
// in-flight operation, rather than an approximate sleep-based race.
func BackendPID(ctx context.Context, conn *sql.Conn) (int32, error) {
	var pid int32
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return 0, fmt.Errorf("chaos: backend pid: %w", err)
	}
	return pid, nil
}

// TerminateBackend forcibly kills the PostgreSQL backend process pid via
// pg_terminate_backend, from a connection distinct from pid's own (a
// backend cannot terminate itself). This is a real, severed connection --
// any in-flight statement/transaction on that connection fails
// immediately with a connection-closed error, exactly as it would if the
// network link between a worker and PostgreSQL were cut -- not a
// simulated or injected Go-level error.
func TerminateBackend(ctx context.Context, db *sql.DB, pid int32) error {
	var terminated bool
	if err := db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
		return fmt.Errorf("chaos: terminate backend %d: %w", pid, err)
	}
	if !terminated {
		return fmt.Errorf("chaos: terminate backend %d: pg_terminate_backend reported false (already gone?)", pid)
	}
	return nil
}
