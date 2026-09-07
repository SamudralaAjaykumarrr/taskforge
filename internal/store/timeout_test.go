// Phase 6 store-level execution-timeout tests: CompleteTimeout, per
// docs/execution-semantics.md "Timeout Semantics" and
// docs/retry-semantics.md ("A TIMED_OUT ... outcome is treated as
// retryable by default").
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func newTimeoutJobParams(jobType string, maxAttempts int) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: 30,
	}
}

// TestCompleteTimeout_TransitionsRunningToRetryWait proves the
// non-exhaustion branch: a timeout with attempts remaining moves the job
// to RETRY_WAIT, exactly like an ordinary retryable failure, but with
// last_error_class = TIMEOUT (distinguishable from RETRYABLE/PERMANENT).
func TestCompleteTimeout_TransitionsRunningToRetryWait(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newTimeoutJobParams("test.timeout.retry", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteTimeout(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
	require.Nil(t, result.LeaseOwner)
	require.Nil(t, result.TerminalAt)
	require.NotNil(t, result.LastErrorClass)
	require.Equal(t, "TIMEOUT", *result.LastErrorClass)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 1)
	require.Equal(t, "TIMED_OUT", *attempts[0].Outcome)
}

// TestCompleteTimeout_ExhaustionTransitionsToDeadLettered proves
// TF-INV-006 holds identically for the timeout path: exhausting
// max_attempts via repeated timeouts dead-letters the job with full
// attempt history preserved (TF-INV-009), never exceeding max_attempts.
func TestCompleteTimeout_ExhaustionTransitionsToDeadLettered(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newTimeoutJobParams("test.timeout.exhaust", 2))
	require.NoError(t, err)

	for attempt := 1; attempt <= 2; attempt++ {
		claimed, ok, err := s.Claim(ctx, "worker-1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, attempt, claimed.AttemptCount)

		result, err := s.CompleteTimeout(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, time.Millisecond)
		require.NoError(t, err)
		if attempt < 2 {
			require.Equal(t, jobstate.RetryWait, result.State)
			forceSetEligibleAt(t, db, created.ID, -time.Second)
		} else {
			require.Equal(t, jobstate.DeadLettered, result.State)
			require.NotNil(t, result.TerminalAt)
			require.Equal(t, "TIMEOUT", *result.LastErrorClass)
		}
	}

	_, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "an exhausted, dead-lettered job must never be claimable again")

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 2)
	for _, a := range attempts {
		require.Equal(t, "TIMED_OUT", *a.Outcome)
	}
}

// TestCompleteTimeout_RejectsStaleGeneration is the timeout analogue of
// SF-008: a worker whose lease has already been superseded cannot
// authoritatively report a timeout outcome (TF-INV-003/014) -- a stale
// generation is no more authoritative for this completion path than any
// other.
func TestCompleteTimeout_RejectsStaleGeneration(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newTimeoutJobParams("test.timeout.stale", 5))
	require.NoError(t, err)
	gen1, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	gen2, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteTimeout(ctx, created.ID, "worker-A", gen1.LeaseGeneration, time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	result, err := s.CompleteSuccess(ctx, created.ID, "worker-B", gen2.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, result.State)
}

// TestCompleteTimeout_RacesCancellation_FirstCommitWins extends SF-012's
// race rule (TF-INV-010) to a timeout completion racing a cancellation
// acknowledgement -- "timeout fires just as cancellation occurs", from
// this task's adversarial-audit list.
func TestCompleteTimeout_RacesCancellation_FirstCommitWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newTimeoutJobParams("test.timeout.vs.cancel", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	// Cancellation acknowledgement commits first.
	result, err := s.CompleteCancelled(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, result.State)

	// The timeout report, arriving after, must be rejected -- a timeout
	// cannot reopen an already-cancelled terminal job.
	_, err = s.CompleteTimeout(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, final.State)
}

// TestCompleteTimeout_RollbackLeavesRowUnchanged proves TF-INV-013 holds
// for the new completion path: a failed transaction (forced via an
// already-cancelled context) leaves the job row completely untouched.
func TestCompleteTimeout_RollbackLeavesRowUnchanged(t *testing.T) {
	s := newStore(t)
	bgCtx := context.Background()

	created, err := s.Insert(bgCtx, newTimeoutJobParams("test.timeout.rollback", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(bgCtx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	before, err := s.GetByID(bgCtx, created.ID)
	require.NoError(t, err)

	cancelledCtx, cancel := context.WithCancel(bgCtx)
	cancel()

	_, err = s.CompleteTimeout(cancelledCtx, claimed.ID, "worker-1", claimed.LeaseGeneration, time.Second)
	require.Error(t, err)

	after, err := s.GetByID(bgCtx, created.ID)
	require.NoError(t, err)
	require.Equal(t, before.State, after.State)
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, before.AttemptCount, after.AttemptCount)
}

// TestSchema_TimedOutOutcomeAlreadySupportedByCheckConstraint proves --
// rather than merely asserting in prose -- that no migration was needed
// for Phase 6's new job_attempts.outcome value: migration 0002's CHECK
// constraint already permitted 'TIMED_OUT' and 'CANCELLED' (see that
// migration's own comment), specifically so a later phase would not need
// a constraint migration to start using them.
func TestSchema_TimedOutOutcomeAlreadySupportedByCheckConstraint(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, newTimeoutJobParams("test.schema.timedout", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteTimeout(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, time.Second)
	require.NoError(t, err, "job_attempts_outcome_check must already permit TIMED_OUT without a schema migration")
}
