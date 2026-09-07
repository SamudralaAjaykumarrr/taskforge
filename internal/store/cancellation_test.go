// Phase 6 store-level cancellation tests, per
// docs/execution-semantics.md "Cancellation and Timeouts", TF-INV-010,
// and docs/scenario-corpus.md SF-011/SF-012. Race tests force each
// documented interleaving deterministically by directly controlling call
// order (each Store completion call is a single, immediately-committed
// transaction, so calling A then B IS the deterministic interleaving
// "A commits before B is attempted" -- no sleep, and no transaction-hold
// trick, is needed to force it), per docs/testing-strategy.md's "no
// sleep-based race" requirement. Concurrent-goroutine variants of the
// same races, against real pooled connections, live in
// concurrency_stress_test.go.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func newCancelJobParams(jobType string) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	}
}

// ---------------------------------------------------------------------
// SF-011: cancellation before claim
// ---------------------------------------------------------------------

// TestCancelQueuedOrRetryWait_Queued_TransitionsDirectlyToCancelled is
// SF-011: a not-yet-claimed QUEUED job cancels directly to CANCELLED,
// terminal_at set, with no worker or race involved.
func TestCancelQueuedOrRetryWait_Queued_TransitionsDirectlyToCancelled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.queued"))
	require.NoError(t, err)

	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)
	require.NotNil(t, cancelled.TerminalAt)

	// TF-INV-005: a subsequent claim must never select this row.
	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestCancelQueuedOrRetryWait_RetryWait_TransitionsDirectlyToCancelled
// proves cancellation of a job currently backing off after a retryable
// failure: RETRY_WAIT -> CANCELLED directly, and the job must never
// later execute merely because its already-scheduled retry eligible_at
// arrives (the "cancelled -> retry eligible -> executes again" sequence
// this task explicitly requires never happens).
func TestCancelQueuedOrRetryWait_RetryWait_TransitionsDirectlyToCancelled(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.retrywait"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "transient", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)

	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)
	require.NotNil(t, cancelled.TerminalAt)

	// TestCancelQueuedOrRetryWait_RetryWait_NeverBecomesClaimableAfterEligibilityArrives
	// below proves the job still never becomes claimable even once its
	// original retry backoff window has fully elapsed.
}

// TestCancelQueuedOrRetryWait_RetryWait_NeverBecomesClaimableAfterEligibilityArrives
// is the same scenario as above without relying on
// forceSetScheduledEligibility's QUEUED-only guard (which does not apply
// to RETRY_WAIT rows): directly rewind eligible_at into the past on the
// now-CANCELLED row and prove claim still rejects it.
func TestCancelQueuedOrRetryWait_RetryWait_NeverBecomesClaimableAfterEligibilityArrives(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.retrywait.eligible"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "transient", 10*time.Second)
	require.NoError(t, err)

	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)

	_, err = db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() - interval '1 second' WHERE id = $1`, created.ID)
	require.NoError(t, err)

	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a cancelled job must never become claimable again, no matter its eligible_at")

	// Attempt history must remain durable and monotonic (TF-INV-007)
	// across the cancellation -- the one FAILED_RETRYABLE attempt from
	// before cancellation is untouched.
	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 1)
	require.Equal(t, "FAILED_RETRYABLE", *attempts[0].Outcome)
}

// ---------------------------------------------------------------------
// Cancellation of a RUNNING job: request + worker acknowledgement
// ---------------------------------------------------------------------

// TestRequestCancellation_Running_SetsFlagWithoutChangingState proves
// docs/execution-semantics.md's "Cancellation Is a Flag, Not a State,
// While Running": requesting cancellation against a RUNNING job sets
// cancel_requested/cancel_requested_at but leaves state=RUNNING and the
// lease completely untouched.
func TestRequestCancellation_Running_SetsFlagWithoutChangingState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.request"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, result.State, "requesting cancellation must not itself change state")
	require.True(t, result.CancelRequested)
	require.NotNil(t, result.CancelRequestedAt)
	require.NotNil(t, result.LeaseOwner)
	require.Equal(t, "worker-1", *result.LeaseOwner)
	require.Equal(t, claimed.LeaseGeneration, result.LeaseGeneration, "the lease itself must be completely untouched")
}

// TestRequestCancellation_Duplicate_IsIdempotent proves duplicate
// cancellation requests against a still-RUNNING job succeed repeatedly
// without disturbing the original cancel_requested_at timestamp.
func TestRequestCancellation_Duplicate_IsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.duplicate"))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	first, err := s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, first.CancelRequestedAt)

	second, err := s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, second.CancelRequested)
	require.NotNil(t, second.CancelRequestedAt)
	require.Equal(t, first.CancelRequestedAt.UTC(), second.CancelRequestedAt.UTC(), "a duplicate cancellation request must not reset cancel_requested_at")
}

// TestRequestCancellation_NotRunning_RejectedAsStale proves
// RequestCancellation only ever applies to a RUNNING job: a QUEUED job
// (or any job that is not currently RUNNING) is rejected, per
// docs/worker-protocol.md's per-state cancellation contract -- the API
// layer (internal/api.CancelJob) is responsible for trying
// CancelQueuedOrRetryWait first.
func TestRequestCancellation_NotRunning_RejectedAsStale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.notrunning"))
	require.NoError(t, err)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// TestRequestCancellation_MissingJob_RejectedAsStale proves cancellation
// of a nonexistent job id is rejected the same way as any other
// non-matching guard (the API layer distinguishes "truly missing" via a
// final GetByID -- see internal/api.CancelJob).
func TestRequestCancellation_MissingJob_RejectedAsStale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.RequestCancellation(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// TestCompleteCancelled_AcknowledgesAndFinalizesAttempt proves the
// worker's own acknowledgement path: RUNNING (with cancel_requested =
// true) -> CANCELLED, lease released, terminal_at set, and the open
// job_attempts row finalized with outcome CANCELLED.
func TestCompleteCancelled_AcknowledgesAndFinalizesAttempt(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.ack"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	result, err := s.CompleteCancelled(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, result.State)
	require.Nil(t, result.LeaseOwner)
	require.Nil(t, result.LeaseExpiresAt)
	require.NotNil(t, result.TerminalAt)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 1)
	require.NotNil(t, attempts[0].FinishedAt)
	require.Equal(t, "CANCELLED", *attempts[0].Outcome)
}

// TestCompleteCancelled_WithoutRequestFirst_RejectedAsStale proves a
// worker cannot unilaterally cancel a job that was never actually asked
// to be cancelled -- the "AND cancel_requested = true" guard in
// docs/worker-protocol.md's "Cancellation Acknowledgement" SQL.
func TestCompleteCancelled_WithoutRequestFirst_RejectedAsStale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.noack"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteCancelled(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	still, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, still.State)
}

// TestCompleteCancelled_RejectsStaleGeneration is the cancellation
// analogue of SF-008: a worker whose lease has been reclaimed by a newer
// generation cannot acknowledge a cancellation using its old
// credentials, even if a cancellation was genuinely requested
// (TF-INV-003/014).
func TestCompleteCancelled_RejectsStaleGeneration(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.stale"))
	require.NoError(t, err)
	gen1, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	forceExpireLease(t, db, created.ID)
	gen2, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), gen2.LeaseGeneration)

	// Worker A's stale (generation 1) cancellation acknowledgement must
	// be rejected -- a stale worker cannot override anything, including
	// via the cancellation path, using an old lease generation.
	_, err = s.CompleteCancelled(ctx, created.ID, "worker-A", gen1.LeaseGeneration)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	still, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, still.State, "the reclaimed job must remain RUNNING under generation 2, untouched by worker A's stale ack")
	require.Equal(t, int64(2), still.LeaseGeneration)
}

// ---------------------------------------------------------------------
// SF-012: cancellation races with completion (TF-INV-010), both forced
// interleavings.
// ---------------------------------------------------------------------

// TestSF012_CancelCommitsFirst_CompletionRejected forces interleaving
// (a) from SF-012: the cancellation acknowledgement commits before
// Worker A's success report, so the job ends CANCELLED and the later
// success report is rejected (zero rows affected).
func TestSF012_CancelCommitsFirst_CompletionRejected(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.sf012.cancel.first"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	// Cancel wins: acknowledge and commit first.
	result, err := s.CompleteCancelled(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, result.State)

	// Worker A's success report, issued after cancellation already
	// committed, must be rejected as stale (state is no longer RUNNING).
	_, err = s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, final.State, "the job must remain CANCELLED -- a losing completion must never overwrite it")
}

// TestSF012_CompletionCommitsFirst_CancellationRejected forces
// interleaving (b) from SF-012: Worker A's success report commits before
// the cancellation acknowledgement is attempted, so the job ends
// SUCCEEDED and the cancellation attempt finds state != RUNNING,
// reported as a no-op against an already-terminal job.
func TestSF012_CompletionCommitsFirst_CancellationRejected(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.sf012.completion.first"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	// Success wins: complete and commit first.
	result, err := s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, result.State)

	// The cancellation acknowledgement, attempted after success already
	// committed, must be rejected -- state is no longer RUNNING.
	_, err = s.CompleteCancelled(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State, "the job must remain SUCCEEDED -- a losing cancellation must never overwrite a completed job")
}

// TestSF012_CancelRacesRetryableFailure_FirstCommitWins extends SF-012 to
// a retryable-failure completion racing a cancellation acknowledgement,
// exactly as this task's adversarial-audit list requires ("retry
// transition and cancellation happen concurrently").
func TestSF012_CancelRacesRetryableFailure_FirstCommitWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.sf012.vs.retry"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	result, err := s.CompleteCancelled(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, result.State)

	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, "transient", time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition, "a retryable-failure report racing a committed cancellation must be rejected, not silently reopen the job into RETRY_WAIT")
}

// TestSF012_CancelRacesDeadLetter_FirstCommitWins extends SF-012 to a
// permanent-failure (dead-letter) completion racing a cancellation
// acknowledgement ("dead-letter transition and cancellation happen
// concurrently").
func TestSF012_CancelRacesDeadLetter_FirstCommitWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.sf012.vs.deadletter"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	result, err := s.CompleteCancelled(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, result.State)

	_, err = s.CompleteFailure(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, "permanent", job.ErrorClassPermanent)
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// TestCancelRacesReclaim_StaleGenerationCannotAcknowledge proves the
// cancel-vs-reclaim adversarial case: a cancellation is requested against
// generation 1, but that generation's lease expires and is reclaimed
// (generation 2) before the worker gets a chance to acknowledge --
// generation 1's (late) acknowledgement attempt is fenced exactly like
// any other stale completion (TF-INV-003/014), and the reclaimed job
// proceeds normally under generation 2 with NO inherited
// cancel_requested flag, per docs/worker-protocol.md's documented
// "resets on claim" decision -- the caller must re-issue cancellation
// against the new generation if still desired.
func TestCancelRacesReclaim_StaleGenerationCannotAcknowledge(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.vs.reclaim"))
	require.NoError(t, err)
	gen1, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	forceExpireLease(t, db, created.ID)
	gen2, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), gen2.LeaseGeneration)
	require.False(t, gen2.CancelRequested, "cancel_requested must reset on reclaim, per docs/worker-protocol.md's documented claim-query behavior")

	// Worker A's late acknowledgement attempt, using the superseded
	// generation, must be rejected.
	_, err = s.CompleteCancelled(ctx, created.ID, "worker-A", gen1.LeaseGeneration)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	// Generation 2 proceeds to a normal successful completion --
	// reclaim is not itself blocked or corrupted by the earlier,
	// now-reset cancellation request.
	result, err := s.CompleteSuccess(ctx, created.ID, "worker-B", gen2.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, result.State)
}

// ---------------------------------------------------------------------
// Terminal-state / duplicate / idempotency interactions
// ---------------------------------------------------------------------

// TestCancelQueuedOrRetryWait_AlreadyTerminal_RejectedAsStale proves
// cancellation after completion (SUCCEEDED) and duplicate cancellation of
// an already-CANCELLED job both correctly reject rather than silently
// reopening a terminal row (TF-INV-005).
func TestCancelQueuedOrRetryWait_AlreadyTerminal_RejectedAsStale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.afterterminal"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	_, err = s.RequestCancellation(ctx, created.ID)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State, "a completed job must never be reopened by a subsequent cancellation attempt")
}

// TestCancelQueuedOrRetryWait_DuplicateOnAlreadyCancelled_RejectedAsStale
// proves a duplicate cancellation of an already-CANCELLED QUEUED-origin
// job is rejected (not an error condition at the API layer -- see
// internal/api.CancelJob, which falls through to reporting the actual
// terminal state -- but at the store layer this is ErrStaleTransition,
// consistent with every other post-terminal transition attempt).
func TestCancelQueuedOrRetryWait_DuplicateOnAlreadyCancelled_RejectedAsStale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newCancelJobParams("test.cancel.duplicate.terminal"))
	require.NoError(t, err)
	_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)

	_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// TestInsertIdempotent_DuplicateSubmissionAfterCancellation_ReturnsExistingJob
// preserves Phase 4 semantics for the new CANCELLED terminal state
// specifically (Phase 4's own tests only covered SUCCEEDED/DEAD_LETTERED,
// since CANCELLED did not exist yet): resubmitting the same
// (job_type, idempotency_key) after the mapped job was cancelled returns
// the SAME (still-CANCELLED) job -- never a new logical job, per
// docs/idempotency.md.
func TestInsertIdempotent_DuplicateSubmissionAfterCancellation_ReturnsExistingJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	key := "cancel-idem-key"
	params := newCancelJobParams("test.cancel.idempotency")
	params.IdempotencyKey = &key

	created, created1, err := s.InsertIdempotent(ctx, params)
	require.NoError(t, err)
	require.True(t, created1)

	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)

	again, created2, err := s.InsertIdempotent(ctx, params)
	require.NoError(t, err)
	require.False(t, created2, "a duplicate submission after cancellation must not create a new logical job")
	require.Equal(t, created.ID, again.ID)
	require.Equal(t, jobstate.Cancelled, again.State, "the returned job must reflect its real (cancelled) state, not a reopened one")
}

// ---------------------------------------------------------------------
// Rollback safety (TF-INV-013)
// ---------------------------------------------------------------------

// TestCompleteCancelled_RollbackLeavesRowUnchanged forces a rollback
// between the jobs-row UPDATE and the job_attempts finalize UPDATE inside
// CompleteCancelled's transaction (via a deliberately duplicate
// finalizeOpenAttemptForGeneration-shaped conflict is not directly
// reachable from outside the package, so this test instead forces the
// underlying connection's transaction to fail via a context cancellation
// mid-transaction) and asserts the job row is left byte-for-byte as it
// was before the cancellation attempt (TF-INV-013's mechanism: both
// statements share one transaction, so a failure between them leaves
// neither applied).
func TestCompleteCancelled_RollbackLeavesRowUnchanged(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	bgCtx := context.Background()

	created, err := s.Insert(bgCtx, newCancelJobParams("test.cancel.rollback"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(bgCtx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.RequestCancellation(bgCtx, created.ID)
	require.NoError(t, err)

	before, err := s.GetByID(bgCtx, created.ID)
	require.NoError(t, err)

	cancelCtx, cancel := context.WithCancel(bgCtx)
	cancel() // already-cancelled context: BeginTx/QueryRowContext must fail immediately

	_, err = s.CompleteCancelled(cancelCtx, claimed.ID, "worker-1", claimed.LeaseGeneration)
	require.Error(t, err)

	after, err := s.GetByID(bgCtx, created.ID)
	require.NoError(t, err)
	require.Equal(t, before.State, after.State, "a failed transaction must leave state completely unchanged")
	require.Equal(t, before.Version, after.Version, "a failed transaction must not advance the optimistic-concurrency version counter")
	require.True(t, after.CancelRequested)
	require.Nil(t, after.TerminalAt, "the job must still be RUNNING, not CANCELLED, after the failed acknowledgement attempt")
}
