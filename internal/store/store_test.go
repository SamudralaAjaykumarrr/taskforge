// Integration tests against a real PostgreSQL instance (see
// internal/testutil), per docs/testing-strategy.md: persistence
// correctness is never asserted against a mock.
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db := testutil.DB(t)
	return store.New(db)
}

func newJobParams(jobType string) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{"k":"v"}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	}
}

// TestInsert_ReturnsCommittedRow proves the mechanism behind TF-INV-001:
// Insert only returns successfully once the row is durably committed, and
// the returned row is exactly what was committed (queryable independently
// via GetByID). See docs/invariants.md TF-INV-001.
func TestInsert_ReturnsCommittedRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.insert"))
	require.NoError(t, err)
	require.Equal(t, jobstate.Queued, created.State)
	require.Equal(t, 0, created.AttemptCount)
	require.False(t, created.CreatedAt.IsZero())

	// Independently re-read: proves the commit already happened, not
	// merely that Insert's own connection remembers it.
	fetched, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, jobstate.Queued, fetched.State)
}

// TestInsert_CancelledContextLeavesNoRow proves TF-INV-013 at the
// submission boundary: if the INSERT statement cannot complete (here,
// forced by an already-cancelled context), no row is left behind in any
// state — the caller never observes a job that "sort of" exists.
func TestInsert_CancelledContextLeavesNoRow(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Insert(ctx, newJobParams("test.insert.cancelled"))
	require.Error(t, err)

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE job_type = $1`, "test.insert.cancelled",
	).Scan(&count))
	require.Equal(t, 0, count, "a failed Insert must leave no row behind")
}

// TestGetByID_NotFound proves GET /jobs/{id} semantics at the store layer:
// an unknown ID is a clean, explicit "not found", never a zero-value job
// or a swallowed error.
func TestGetByID_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetByID(context.Background(), uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestClaim_NothingEligible proves Claim distinguishes "nothing to do
// right now" from an error: both the returned job and the ok flag must
// reflect that cleanly.
func TestClaim_NothingEligible(t *testing.T) {
	s := newStore(t)
	j, ok, err := s.Claim(context.Background(), "worker-1")
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, j)
}

// TestClaim_TransitionsQueuedToRunning is SF-001's claim step: a QUEUED
// job becomes RUNNING under the claiming worker, with attempt_count and
// lease fields set per docs/worker-protocol.md's claim query.
func TestClaim_TransitionsQueuedToRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.claim"))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)
	require.Equal(t, jobstate.Running, claimed.State)
	require.Equal(t, 1, claimed.AttemptCount)
	require.Equal(t, int64(1), claimed.LeaseGeneration)
	require.NotNil(t, claimed.LeaseOwner)
	require.Equal(t, "worker-1", *claimed.LeaseOwner)
	require.NotNil(t, claimed.LeaseExpiresAt)
}

// TestClaim_SkipsAlreadyClaimedJob proves a second claim attempt does not
// re-claim a RUNNING job whose lease is still valid (a direct, cheap
// analogue of TF-INV-002 — full concurrent-worker proof is
// TestClaim_ConcurrentWorkersRaceForSameJob / SF-006). Phase 2 does add a
// reclaim path (see TestClaim_ReclaimsExpiredLease), but it only fires
// once lease_expires_at has passed; a job claimed moments ago with the
// default (much longer) execution_timeout_seconds must not be touched.
func TestClaim_SkipsAlreadyClaimedJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParams("test.claim.once"))
	require.NoError(t, err)

	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a RUNNING job with an unexpired lease must not be claimable again")
}

// TestCompleteSuccess_TransitionsRunningToSucceeded is SF-001's completion
// step.
func TestCompleteSuccess_TransitionsRunningToSucceeded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParams("test.success"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result := json.RawMessage(`{"ok":true}`)
	completed, err := s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, result)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
	require.NotNil(t, completed.TerminalAt)
	require.Nil(t, completed.LeaseOwner)
	require.Nil(t, completed.LeaseExpiresAt)
	require.JSONEq(t, string(result), string(completed.ResultMetadata))
}

// TestCompleteFailure_TransitionsRunningToDeadLettered is the Phase 1
// failure path: docs/roadmap.md specifies "a failure goes straight to
// DEAD_LETTERED" — no RETRY_WAIT branch exists yet.
func TestCompleteFailure_TransitionsRunningToDeadLettered(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParams("test.failure"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	failed, err := s.CompleteFailure(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, failed.State)
	require.NotNil(t, failed.TerminalAt)
	require.NotNil(t, failed.LastError)
	require.Equal(t, "boom", *failed.LastError)
	require.NotNil(t, failed.LastErrorClass)
	require.Equal(t, job.ErrorClassPermanent, *failed.LastErrorClass)
}

// TestCompleteSuccess_RejectsWrongLeaseGeneration is the canonical
// fencing test described in docs/worker-protocol.md ("The Fencing
// Guarantee, Stated Precisely") and proves TF-INV-003's mechanism: a
// completion call carrying a stale lease_generation affects zero rows and
// is rejected, leaving the job's actual state untouched.
//
// This test demonstrates the fencing GUARD REJECTS a mismatched token
// correctly. It is not, by itself, a proof of TF-INV-003/014 under real
// concurrent multi-worker execution (that requires two independent
// workers racing, which is Phase 2/SF-008) — it proves the single-worker
// mechanism behaves correctly when handed a stale credential, which is as
// much of TF-INV-003 as Phase 1's single-worker design can exercise.
func TestCompleteSuccess_RejectsWrongLeaseGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParams("test.fencing"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	const wrongGeneration = int64(999)
	_, err = s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, wrongGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	// The job must be untouched: still RUNNING under the real generation.
	current, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, current.State)
	require.Equal(t, claimed.LeaseGeneration, current.LeaseGeneration)
}

// TestCompleteSuccess_RejectsWrongLeaseOwner is the same fencing mechanism
// keyed on lease_owner instead of lease_generation.
func TestCompleteSuccess_RejectsWrongLeaseOwner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParams("test.fencing.owner"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteSuccess(ctx, claimed.ID, "some-other-worker", claimed.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	current, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, current.State)
}

// TestTerminalStates_RejectFurtherTransitions is a database-backed
// analogue of scenario SF-015: once a job reaches SUCCEEDED or
// DEAD_LETTERED, every further attempted transition against it — a second
// completion call, or a claim attempt — must be rejected, and the row's
// state/terminal_at must never change (TF-INV-005).
func TestTerminalStates_RejectFurtherTransitions(t *testing.T) {
	t.Run("SUCCEEDED", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.Insert(ctx, newJobParams("test.terminal.succeeded"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "worker-1")
		require.NoError(t, err)
		require.True(t, ok)

		succeeded, err := s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, nil)
		require.NoError(t, err)
		terminalAtBefore := *succeeded.TerminalAt

		// A second completion call with the same (now-stale, since the
		// state is no longer RUNNING) lease must be rejected.
		_, err = s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, nil)
		require.ErrorIs(t, err, store.ErrStaleTransition)

		_, err = s.CompleteFailure(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, "late failure", job.ErrorClassPermanent)
		require.ErrorIs(t, err, store.ErrStaleTransition)

		// A terminal job must never be claimable again.
		reclaimed, ok, err := s.Claim(ctx, "worker-2")
		require.NoError(t, err)
		require.False(t, ok)
		require.Nil(t, reclaimed)

		final, err := s.GetByID(ctx, claimed.ID)
		require.NoError(t, err)
		require.Equal(t, jobstate.Succeeded, final.State)
		require.Equal(t, terminalAtBefore, *final.TerminalAt, "terminal_at must never change once set")
	})

	t.Run("DEAD_LETTERED", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.Insert(ctx, newJobParams("test.terminal.deadlettered"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "worker-1")
		require.NoError(t, err)
		require.True(t, ok)

		failed, err := s.CompleteFailure(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
		require.NoError(t, err)
		terminalAtBefore := *failed.TerminalAt

		_, err = s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, nil)
		require.ErrorIs(t, err, store.ErrStaleTransition)

		reclaimed, ok, err := s.Claim(ctx, "worker-2")
		require.NoError(t, err)
		require.False(t, ok)
		require.Nil(t, reclaimed)

		final, err := s.GetByID(ctx, claimed.ID)
		require.NoError(t, err)
		require.Equal(t, jobstate.DeadLettered, final.State)
		require.Equal(t, terminalAtBefore, *final.TerminalAt)
	})
}

// TestSchema_StateCheckConstraintRejectsInvalidState proves TF-INV-005's
// database-level backstop is real, not just enforced in Go: an attempt to
// write an undocumented state value directly is rejected by the
// jobs_state_check CHECK constraint, independent of any application code.
func TestSchema_StateCheckConstraintRejectsInvalidState(t *testing.T) {
	db := testutil.DB(t)
	_, err := db.Exec(`
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
		VALUES ($1, 'test.invalid', '{}', 'NOT_A_REAL_STATE', 30)`,
		uuid.New())
	require.Error(t, err)
}

// TestSchema_IdempotencyKeyUniqueConstraint proves the database-level
// mechanism behind TF-INV-008/TF-INV-016 exists in the schema now, even
// though Phase 1's API does not yet expose Idempotency-Key support
// (docs/roadmap.md). This is a schema test, not a claim that Phase 1
// enforces submission idempotency end-to-end.
func TestSchema_IdempotencyKeyUniqueConstraint(t *testing.T) {
	db := testutil.DB(t)
	key := "dup-key"

	_, err := db.Exec(`
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
		VALUES ($1, 'test.idem', '{}', 'QUEUED', 30, $2)`,
		uuid.New(), key)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
		VALUES ($1, 'test.idem', '{}', 'QUEUED', 30, $2)`,
		uuid.New(), key)
	require.Error(t, err, "duplicate (job_type, idempotency_key) must violate the unique constraint")
}

// TestFaultInjection_RollbackLeavesRowUnchanged proves TF-INV-013
// directly: forcing a transaction to fail partway through (via a
// deliberate constraint violation in a multi-statement transaction) must
// leave the job row byte-for-byte identical to its pre-transaction value,
// not partially applied.
func TestFaultInjection_RollbackLeavesRowUnchanged(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.rollback"))
	require.NoError(t, err)
	before, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'RUNNING', lease_owner = 'w1' WHERE id = $1`, created.ID)
	require.NoError(t, err)

	// Force the transaction to fail before commit: violate the state
	// CHECK constraint in the same transaction.
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'NOT_A_REAL_STATE' WHERE id = $1`, created.ID)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	after, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, before.State, after.State)
	require.Equal(t, before.Version, after.Version)
	require.Nil(t, after.LeaseOwner)
}

// TestRestart_RunningJobSurvivesFreshStoreInstance is a minimal version of
// SF-018 / the roadmap's restart quality gate: "no in-memory state holds
// anything correctness-relevant (verifiable by restarting the process
// mid-test and asserting state is unaffected)". A brand-new *store.Store
// (standing in for a restarted process, since Store holds no state of its
// own beyond a *sql.DB handle) must observe exactly the same durable state
// a "pre-restart" instance left behind, and recovery must not depend on
// any in-memory ownership state surviving the restart.
//
// The still-RUNNING job's lease has NOT expired here (default
// execution_timeout_seconds), so the restarted instance's Claim() must
// not touch it — same "unexpired lease is not claimable" property as
// TestClaim_SkipsAlreadyClaimedJob, now proven across a simulated
// restart. See TestReclaim_SurvivesFreshStoreInstance below for the
// restart-with-an-EXPIRED-lease case (the actual crash-recovery path,
// TF-INV-004).
func TestRestart_RunningJobSurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	before := store.New(db)
	ctx := context.Background()

	_, err := before.Insert(ctx, newJobParams("test.restart"))
	require.NoError(t, err)
	claimed, ok, err := before.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate a process restart: a fresh Store sharing only the durable
	// database, with no in-memory knowledge of the job above.
	after := store.New(db)

	seen, err := after.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, seen.State)
	require.Equal(t, claimed.LeaseGeneration, seen.LeaseGeneration)
	require.Equal(t, *claimed.LeaseOwner, *seen.LeaseOwner)

	// The lease is still valid, so the restarted instance must not
	// reclaim it.
	_, ok, err = after.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a RUNNING job with an unexpired lease is not claimable after a restart")

	// The restarted instance CAN still complete it, using the lease
	// credentials read back from durable state (proving nothing
	// correctness-relevant was lost by not being in worker-2's memory).
	completed, err := after.CompleteSuccess(ctx, seen.ID, *seen.LeaseOwner, seen.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
}

// TestReclaim_SurvivesFreshStoreInstance is SF-018's actual crash-recovery
// case: a RUNNING job whose lease expired while no process was running at
// all is reclaimed correctly by a store instance that never held any
// in-memory reference to it, proving recovery does not depend on
// in-memory ownership state surviving a restart (TF-INV-004).
func TestReclaim_SurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	before := store.New(db)
	ctx := context.Background()

	created, err := before.Insert(ctx, newJobParams("test.restart.reclaim"))
	require.NoError(t, err)
	_, ok, err := before.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)

	// A fresh Store, standing in for a fully restarted fleet (API +
	// workers), with no in-memory knowledge of worker-1's claim.
	after := store.New(db)

	reclaimed, ok, err := after.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok, "an expired lease must be reclaimable even by a process that never saw the original claim")
	require.Equal(t, jobstate.Running, reclaimed.State)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)
	require.Equal(t, "worker-2", *reclaimed.LeaseOwner)

	completed, err := after.CompleteSuccess(ctx, reclaimed.ID, "worker-2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
}

// TestConcurrentInserts_AllPersistIndependently is a light-weight
// sanity check that concurrent submissions each durably persist their own
// row without interfering with each other (each Insert is an independent
// statement/transaction).
func TestConcurrentInserts_AllPersistIndependently(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 10

	ids := make(chan uuid.UUID, n)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			j, err := s.Insert(ctx, newJobParams("test.concurrent"))
			if err != nil {
				errCh <- err
				ids <- uuid.Nil
				return
			}
			errCh <- nil
			ids <- j.ID
		}()
	}

	seen := make(map[uuid.UUID]bool, n)
	for i := 0; i < n; i++ {
		require.NoError(t, <-errCh)
		id := <-ids
		require.False(t, seen[id], "each concurrent insert must produce a distinct job id")
		seen[id] = true
	}
	require.Len(t, seen, n)
}
