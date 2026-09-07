// Phase 3 retry/backoff/DLQ tests, against a real PostgreSQL instance
// (internal/testutil), per docs/testing-strategy.md. Lease-expiry and
// retry-eligibility timing use forceExpireLease/forceSetEligibleAt
// (deterministic DB-time manipulation), never a sleep.
package store_test

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// ---------------------------------------------------------------------
// Basic transition behavior (TF-INV-006, TF-INV-007, TF-INV-009)
// ---------------------------------------------------------------------

// TestCompleteRetryableFailure_TransitionsRunningToRetryWait proves the
// non-exhaustion branch: a retryable failure with attempts remaining moves
// the job to RETRY_WAIT, clears lease ownership, and does NOT set
// terminal_at (RETRY_WAIT is not terminal).
func TestCompleteRetryableFailure_TransitionsRunningToRetryWait(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.transition", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "transient error", 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
	require.Nil(t, result.LeaseOwner)
	require.Nil(t, result.LeaseExpiresAt)
	require.Nil(t, result.TerminalAt, "RETRY_WAIT must not set terminal_at")
	require.NotNil(t, result.LastError)
	require.Equal(t, "transient error", *result.LastError)
	require.NotNil(t, result.LastErrorClass)
	require.Equal(t, "RETRYABLE", *result.LastErrorClass)
	require.Equal(t, 1, result.AttemptCount, "attempt_count must not change on a retry -- only claim increments it")
}

// TestCompleteRetryableFailure_PersistsEligibleAtFromDelay proves the
// backoff delay is durably applied as eligible_at = (PostgreSQL's) now() +
// delay, within a tolerance that accounts for real wall-clock time
// elapsing during the round trip (not the caller's clock -- see
// docs/failure-model.md Clock Model).
func TestCompleteRetryableFailure_PersistsEligibleAtFromDelay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.eligible_at", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	before := time.Now()
	const delay = 10 * time.Second
	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", delay)
	require.NoError(t, err)

	wantEarliest := before.Add(delay - time.Second)
	wantLatest := time.Now().Add(delay + time.Second)
	require.True(t, result.EligibleAt.After(wantEarliest), "eligible_at %s too early (expected around %s)", result.EligibleAt, before.Add(delay))
	require.True(t, result.EligibleAt.Before(wantLatest), "eligible_at %s too late (expected around %s)", result.EligibleAt, before.Add(delay))
}

// TestCompleteRetryableFailure_SF010_ExhaustionTransitionsToDeadLettered is
// SF-010: all attempts (max_attempts=3) report FAILED_RETRYABLE; the job
// dead-letters after the 3rd, with last_error matching that attempt and
// all 3 job_attempts rows present and queryable (TF-INV-006, TF-INV-009).
func TestCompleteRetryableFailure_SF010_ExhaustionTransitionsToDeadLettered(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.sf010", 3))
	require.NoError(t, err)

	for attempt := 1; attempt <= 3; attempt++ {
		claimed, ok, err := s.Claim(ctx, "worker-1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, attempt, claimed.AttemptCount)

		errMsg := "failure on attempt " + string(rune('0'+attempt))
		result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, errMsg, time.Millisecond)
		require.NoError(t, err)

		if attempt < 3 {
			require.Equal(t, jobstate.RetryWait, result.State)
			forceSetEligibleAt(t, db, created.ID, -time.Second) // fast-forward past backoff
		} else {
			require.Equal(t, jobstate.DeadLettered, result.State)
			require.NotNil(t, result.TerminalAt)
			require.NotNil(t, result.LastError)
			require.Equal(t, errMsg, *result.LastError)
		}
	}

	// No further claim should ever succeed -- the job is terminal.
	_, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "an exhausted, dead-lettered job must never be claimable again")

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 3, "all 3 attempts must remain queryable after dead-lettering")
	for i, a := range attempts {
		require.Equal(t, i+1, a.AttemptNumber)
		require.NotNil(t, a.FinishedAt)
		require.NotNil(t, a.Outcome)
		require.Equal(t, "FAILED_RETRYABLE", *a.Outcome)
	}
}

// TestCompleteRetryableFailure_SF009_EventuallySucceeds is SF-009 at the
// store level: attempts 1 and 2 report FAILED_RETRYABLE, attempt 3
// succeeds. Final state SUCCEEDED, attempt_count = 3, three job_attempts
// rows in the documented order.
func TestCompleteRetryableFailure_SF009_EventuallySucceeds(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.sf009", 5))
	require.NoError(t, err)

	for attempt := 1; attempt <= 2; attempt++ {
		claimed, ok, err := s.Claim(ctx, "worker-1")
		require.NoError(t, err)
		require.True(t, ok)

		result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "transient", time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, jobstate.RetryWait, result.State)
		forceSetEligibleAt(t, db, created.ID, -time.Second)
	}

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 3, claimed.AttemptCount)

	succeeded, err := s.CompleteSuccess(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, succeeded.State)
	require.Equal(t, 3, succeeded.AttemptCount)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 3)
	require.Equal(t, "FAILED_RETRYABLE", *attempts[0].Outcome)
	require.Equal(t, "FAILED_RETRYABLE", *attempts[1].Outcome)
	require.Equal(t, "SUCCEEDED", *attempts[2].Outcome)
}

// TestCompleteRetryableFailure_ExhaustionExactlyAtMaxAttemptsBoundary is
// adversarial cases #5/#11: with max_attempts=1, the FIRST failure must
// already be exhaustion (attempt_count(1) >= max_attempts(1)) -- straight
// to DEAD_LETTERED, never RETRY_WAIT even once.
func TestCompleteRetryableFailure_ExhaustionExactlyAtMaxAttemptsBoundary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.boundary.one", 1))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, claimed.AttemptCount)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, result.State, "max_attempts=1 must dead-letter on the very first failure")
	require.NotNil(t, result.TerminalAt)
}

// TestCompleteRetryableFailure_AttemptOutcomeAndErrorClassRecorded proves
// the exact ledger/job-row fields docs/worker-protocol.md's "Retryable
// Failure" SQL specifies: last_error_class is 'RETRYABLE' on the JOB row
// even when exhaustion sends it to DEAD_LETTERED (distinct from
// CompleteFailure's 'PERMANENT'), and the job_attempts row's outcome is
// FAILED_RETRYABLE with error_class/error_message populated.
func TestCompleteRetryableFailure_AttemptOutcomeAndErrorClassRecorded(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.ledger", 1))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "downstream 503", time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, result.State)
	require.Equal(t, "RETRYABLE", *result.LastErrorClass, "job-level last_error_class records the attempt's own classification even though exhaustion, not permanence, caused dead-lettering")

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 1)
	require.Equal(t, "FAILED_RETRYABLE", *attempts[0].Outcome)
	require.Equal(t, "RETRYABLE", *attempts[0].ErrorClass)
	require.Equal(t, "downstream 503", *attempts[0].ErrorMessage)
}

// ---------------------------------------------------------------------
// Fencing (TF-INV-003, TF-INV-014) -- adversarial cases #3, #4, #14, #15
// ---------------------------------------------------------------------

// TestCompleteRetryableFailure_RejectsStaleGeneration and
// TestCompleteRetryableFailure_RejectsWrongOwner mirror the
// CompleteSuccess fencing tests exactly, for the new retry path.
func TestCompleteRetryableFailure_RejectsStaleGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.fencing.gen", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", 999, "boom", time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	current, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, current.State, "a rejected retry report must leave the job untouched")
}

func TestCompleteRetryableFailure_RejectsWrongOwner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.fencing.owner", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "some-other-worker", claimed.LeaseGeneration, "boom", time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// TestCompleteRetryableFailure_RejectedAfterReclaim is adversarial case #3
// ("stale generation reports retryable failure after another worker
// reclaimed"): Worker A's lease expires, Worker B reclaims (generation 2)
// and succeeds; Worker A's late retryable-failure report (generation 1)
// must be rejected and must NOT alter the job Worker B already resolved.
func TestCompleteRetryableFailure_RejectedAfterReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.fencing.reclaim", 5))
	require.NoError(t, err)

	a, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	b, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), b.LeaseGeneration)

	succeeded, err := s.CompleteSuccess(ctx, b.ID, "worker-B", b.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, succeeded.State)

	// Worker A, unaware it lost the lease, reports a retryable failure
	// with its stale generation-1 credential -- must be rejected, and must
	// NOT resurrect the job into RETRY_WAIT.
	_, err = s.CompleteRetryableFailure(ctx, a.ID, "worker-A", a.LeaseGeneration, "late retryable report", time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State, "worker B's success must be untouched by worker A's stale retry report")
}

// TestCompleteFailure_PermanentRejectedAfterReclaim is adversarial cases
// #4/#15 ("old generation tries to dead-letter after new generation
// succeeded" / "permanent failure races with lease reclaim"): the same
// shape as above, but with a PERMANENT failure report from the stale
// generation.
func TestCompleteFailure_PermanentRejectedAfterReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.permanent.fencing.reclaim", 5))
	require.NoError(t, err)

	a, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	b, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)

	succeeded, err := s.CompleteSuccess(ctx, b.ID, "worker-B", b.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, succeeded.State)

	_, err = s.CompleteFailure(ctx, a.ID, "worker-A", a.LeaseGeneration, "late permanent report", job.ErrorClassPermanent)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
}

// TestCompleteRetryableFailure_AcceptedAfterExpiryButBeforeReclaim is
// adversarial case #14 ("lease expires just before failure result is
// persisted"): a retryable-failure report arriving after
// lease_expires_at has passed, but before any other worker has actually
// reclaimed the job, must still be accepted -- expiry alone does not fence
// a worker, only a new generation does (same documented boundary as
// TestCompleteSuccess_AcceptedAfterExpiryButBeforeReclaim in lease_test.go).
func TestCompleteRetryableFailure_AcceptedAfterExpiryButBeforeReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.late.not.superseded", 5))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	// Deliberately no reclaim here.

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Second)
	require.NoError(t, err, "a late-but-not-yet-superseded retry report must be accepted")
	require.Equal(t, jobstate.RetryWait, result.State)
}

// ---------------------------------------------------------------------
// Retry eligibility timing and claim interaction (TF-INV-011's mechanism
// applied to RETRY_WAIT; adversarial cases #1, #2, #10)
// ---------------------------------------------------------------------

// TestClaim_RetryWaitNotClaimableBeforeEligibility proves a RETRY_WAIT job
// is not claimable while its computed backoff has not yet elapsed.
func TestClaim_RetryWaitNotClaimableBeforeEligibility(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.not.yet.eligible", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	// A long delay -- eligible_at is far in the future.
	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)

	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a RETRY_WAIT job must not be claimable before its persisted eligible_at")
}

// TestClaim_RetryWaitClaimableAfterEligibility proves the positive case,
// using deterministic DB-time manipulation (never a real sleep) to
// simulate the backoff having elapsed.
func TestClaim_RetryWaitClaimableAfterEligibility(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.now.eligible", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)

	forceSetEligibleAt(t, db, created.ID, -time.Second)

	reclaimed, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok, "a RETRY_WAIT job must be claimable once eligible_at has passed")
	require.Equal(t, jobstate.Running, reclaimed.State)
	require.Equal(t, 2, reclaimed.AttemptCount, "claim from RETRY_WAIT must increment attempt_count")
	require.Equal(t, int64(2), reclaimed.LeaseGeneration, "claim from RETRY_WAIT must advance lease_generation like any other claim")
}

// TestClaim_ConcurrentWorkersRaceForEligibleRetryWaitJob is adversarial
// case #1 ("two workers race when RETRY_WAIT becomes eligible") --
// SF-006's exact concurrency proof, replayed starting from RETRY_WAIT
// instead of QUEUED, since docs/worker-protocol.md's claim query treats
// both identically.
func TestClaim_ConcurrentWorkersRaceForEligibleRetryWaitJob(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.race", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-0")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-0", claimed.LeaseGeneration, "boom", time.Hour)
	require.NoError(t, err)
	forceSetEligibleAt(t, db, created.ID, -time.Second)

	const numWorkers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *job.Job, numWorkers)
	errs := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			j, ok, err := s.Claim(ctx, workerIDName(workerID))
			if err != nil {
				errs <- err
				return
			}
			if ok {
				results <- j
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	var winners []*job.Job
	for j := range results {
		winners = append(winners, j)
	}
	require.Len(t, winners, 1, "exactly one worker must win the RETRY_WAIT claim race")
	require.Equal(t, int64(2), winners[0].LeaseGeneration)
	require.Equal(t, 2, winners[0].AttemptCount)
}

// TestClaim_MultipleWorkersOnlyOneWinsOnceEligible extends the above with
// several independent RETRY_WAIT jobs becoming eligible at once, proving
// no job is claimed twice and no job is left behind, across a small pool
// (the "multiple workers when retry becomes eligible" required case).
func TestClaim_MultipleWorkersOnlyOneWinsOnceEligible(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	// Seed every job into RETRY_WAIT with a FAR-future eligible_at first
	// (so none is claimable yet, and each seeding claim below deterministically
	// picks up its own freshly-inserted QUEUED job rather than racing an
	// already-eligible RETRY_WAIT row left over from a prior iteration),
	// then flip them all to eligible (past) at once.
	const numJobs = 10
	ids := make([]uuid.UUID, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("test.retry.multi", 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "seed-worker")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, created.ID, claimed.ID, "seeding claim must pick up its own freshly-inserted job")
		_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "seed-worker", claimed.LeaseGeneration, "boom", time.Hour)
		require.NoError(t, err)
		ids = append(ids, created.ID)
	}
	for _, id := range ids {
		forceSetEligibleAt(t, db, id, -time.Second)
	}

	const numWorkers = 5
	start := make(chan struct{})
	var wg sync.WaitGroup
	claimsCh := make(chan *job.Job, numJobs*2)
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, workerIDName(workerID))
				require.NoError(t, err)
				if !ok {
					return
				}
				claimsCh <- j
			}
		}()
	}
	close(start)
	wg.Wait()
	close(claimsCh)

	seen := make(map[uuid.UUID]bool, numJobs)
	for j := range claimsCh {
		require.False(t, seen[j.ID], "job %s claimed more than once", j.ID)
		seen[j.ID] = true
	}
	require.Len(t, seen, numJobs, "every eligible retry-wait job must be claimed exactly once")
}

// TestClaim_DeadLetteredViaExhaustionNeverClaimed is adversarial case #10
// ("dead-letter job appears to claim query"), specifically for a job
// dead-lettered via retry exhaustion (as opposed to CompleteFailure's
// unconditional path, already covered by TestClaim_TerminalJobNeverReclaimed).
func TestClaim_DeadLetteredViaExhaustionNeverClaimed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.dlq.never.claimed", 1))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, result.State)

	for i := 0; i < 3; i++ {
		_, ok, err := s.Claim(ctx, "worker-2")
		require.NoError(t, err)
		require.False(t, ok, "a job dead-lettered via retry exhaustion must never be claimable again")
	}
}

// ---------------------------------------------------------------------
// Durability across restart (adversarial case #9)
// ---------------------------------------------------------------------

// TestRetryWait_SurvivesFreshStoreInstance proves a RETRY_WAIT job's
// backoff timer is durable, not an in-memory timer: a fresh *store.Store
// (standing in for a full process restart) with no in-memory knowledge of
// the prior failure correctly refuses to claim it before eligible_at and
// correctly claims it after, exactly as the original process would have.
func TestRetryWait_SurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	before := store.New(db)
	ctx := context.Background()

	created, err := before.Insert(ctx, newJobParamsN("test.retry.restart", 5))
	require.NoError(t, err)
	claimed, ok, err := before.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := before.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "boom", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)

	// Simulate a full process restart: a fresh Store, no in-memory timer.
	after := store.New(db)

	_, ok, err = after.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "restart must not forget the still-pending backoff window")

	forceSetEligibleAt(t, db, created.ID, -time.Second)

	reclaimed, ok, err := after.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok, "restart must not prevent claiming once eligible_at has genuinely passed")
	require.Equal(t, jobstate.Running, reclaimed.State)
}

// ---------------------------------------------------------------------
// Interaction with Phase 2 reclaim (adversarial case #8: worker crashes
// after handler failure but before retry state commit)
// ---------------------------------------------------------------------

// TestClaim_ReclaimStillWorksAfterPriorRetryCycle proves Phase 2's
// lease-expiry reclaim mechanism (unrelated to, and unaffected by, the new
// RETRY_WAIT path) still functions correctly on an attempt after the first:
// a worker crashes mid-execution on attempt 2 (never even calling
// CompleteRetryableFailure -- the "crash after handler failure but before
// retry state commit" case degrades to an ordinary crash, exactly like
// attempt 1's), and is reclaimed exactly like Phase 2's SF-007.
func TestClaim_ReclaimStillWorksAfterPriorRetryCycle(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.retry.then.reclaim", 5))
	require.NoError(t, err)

	first, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	result, err := s.CompleteRetryableFailure(ctx, first.ID, "worker-1", first.LeaseGeneration, "boom", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
	forceSetEligibleAt(t, db, created.ID, -time.Second)

	second, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, second.AttemptCount)

	// worker-2 crashes: never reports any outcome. Its lease simply
	// expires.
	forceExpireLease(t, db, created.ID)

	third, ok, err := s.Claim(ctx, "worker-3")
	require.NoError(t, err)
	require.True(t, ok, "reclaim must still work normally for a job that has already been through one retry cycle")
	require.Equal(t, jobstate.Running, third.State)
	require.Equal(t, 3, third.AttemptCount)
	require.Equal(t, int64(3), third.LeaseGeneration)

	completed, err := s.CompleteSuccess(ctx, third.ID, "worker-3", third.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 3)
	require.Equal(t, "FAILED_RETRYABLE", *attempts[0].Outcome)
	require.Equal(t, "LEASE_EXPIRED", *attempts[1].Outcome, "the crashed attempt-2 is finalized as LEASE_EXPIRED by reclaim, exactly like Phase 2")
	require.Equal(t, "SUCCEEDED", *attempts[2].Outcome)
}

// ---------------------------------------------------------------------
// Transaction atomicity (TF-INV-013) -- adversarial cases #6, #7
// ---------------------------------------------------------------------

// TestRetryTransition_RollbackLeavesJobAndAttemptConsistent proves
// TF-INV-013 for the new retry transition: performing the same two writes
// CompleteRetryableFailure performs (job UPDATE, then job_attempts
// finalize UPDATE) inside one transaction, then forcing the transaction to
// fail before commit, must leave BOTH rows completely unchanged -- never a
// job that says RETRY_WAIT with an attempt row that still looks open, and
// never the reverse.
func TestRetryTransition_RollbackLeavesJobAndAttemptConsistent(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.retry.rollback", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	beforeJob, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	beforeAttempts := attemptsForJob(t, db, claimed.ID)
	require.Len(t, beforeAttempts, 1)
	require.Nil(t, beforeAttempts[0].FinishedAt)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
		UPDATE jobs
		SET state = CASE WHEN attempt_count >= max_attempts THEN 'DEAD_LETTERED' ELSE 'RETRY_WAIT' END,
			lease_owner = NULL,
			lease_expires_at = NULL,
			eligible_at = now() + interval '5 seconds',
			last_error = 'boom',
			last_error_class = 'RETRYABLE',
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'`,
		claimed.ID, "worker-1", claimed.LeaseGeneration)
	require.NoError(t, err)

	// Force the transaction to fail before commit: an invalid outcome
	// value violates job_attempts_outcome_check.
	_, err = tx.ExecContext(ctx, `
		UPDATE job_attempts
		SET finished_at = now(), outcome = 'NOT_A_REAL_OUTCOME'
		WHERE job_id = $1 AND finished_at IS NULL`, claimed.ID)
	require.Error(t, err, "an invalid outcome value must violate the CHECK constraint")
	require.NoError(t, tx.Rollback())

	afterJob, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, beforeJob.State, afterJob.State, "rollback must leave the job row's state untouched")
	require.Equal(t, beforeJob.Version, afterJob.Version)
	require.Equal(t, beforeJob.EligibleAt, afterJob.EligibleAt)
	require.Nil(t, afterJob.LastError)

	afterAttempts := attemptsForJob(t, db, claimed.ID)
	require.Len(t, afterAttempts, 1)
	require.Nil(t, afterAttempts[0].FinishedAt, "rollback must leave the attempt row exactly as open as it was before")
	require.Nil(t, afterAttempts[0].Outcome)

	// The row must still be genuinely usable afterward -- this failure did
	// not permanently poison it.
	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "real failure", time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
}

// ---------------------------------------------------------------------
// Property test (roadmap.md Phase 3 quality gate): attempt_count never
// exceeds max_attempts across randomized failure sequences.
// ---------------------------------------------------------------------

// TestProperty_AttemptCountNeverExceedsMaxAttempts runs many randomized
// scenarios -- a random max_attempts and a random number of retryable
// failures before an eventual success or exhaustion -- asserting
// TF-INV-006 holds in every one: attempt_count never exceeds max_attempts,
// and the job reaches DEAD_LETTERED if and only if it was exhausted before
// succeeding. Uses a fixed seed for reproducibility (a failing seed can be
// pinned and re-run deterministically), per docs/testing-strategy.md's
// "no flaky timing tests" (this test has no timing dependency at all --
// only DB-time manipulation).
func TestProperty_AttemptCountNeverExceedsMaxAttempts(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	rng := rand.New(rand.NewSource(42))
	const scenarios = 50

	for i := 0; i < scenarios; i++ {
		maxAttempts := 1 + rng.Intn(6) // [1, 6]
		// Failures before the job either succeeds or exhausts: allow this
		// to run past maxAttempts sometimes, proving exhaustion (not an
		// artificial stop) is what ends the loop.
		failuresBeforeStop := rng.Intn(maxAttempts + 3)
		succeedsAtEnd := rng.Intn(2) == 0

		created, err := s.Insert(ctx, newJobParamsN("test.property.retry", maxAttempts))
		require.NoError(t, err)

		attemptsSeen := 0
		for {
			claimed, ok, err := s.Claim(ctx, "worker-prop")
			require.NoError(t, err)
			if !ok {
				break // eligible_at not yet reached, or job terminal -- see below
			}
			attemptsSeen++
			require.LessOrEqualf(t, claimed.AttemptCount, maxAttempts,
				"scenario %d: attempt_count exceeded max_attempts (TF-INV-006 violated)", i)

			shouldSucceedNow := succeedsAtEnd && claimed.AttemptCount > failuresBeforeStop
			if shouldSucceedNow {
				result, err := s.CompleteSuccess(ctx, claimed.ID, "worker-prop", claimed.LeaseGeneration, nil)
				require.NoError(t, err)
				require.Equal(t, jobstate.Succeeded, result.State)
				break
			}

			result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-prop", claimed.LeaseGeneration, "boom", time.Millisecond)
			require.NoError(t, err)
			require.LessOrEqualf(t, result.AttemptCount, maxAttempts,
				"scenario %d: post-failure attempt_count exceeded max_attempts", i)

			if result.State == jobstate.DeadLettered {
				require.GreaterOrEqualf(t, result.AttemptCount, maxAttempts, "scenario %d: dead-lettered before exhausting max_attempts", i)
				break
			}
			require.Equal(t, jobstate.RetryWait, result.State)
			forceSetEligibleAt(t, db, created.ID, -time.Second)
		}

		final, err := s.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.LessOrEqualf(t, final.AttemptCount, maxAttempts, "scenario %d: final attempt_count exceeded max_attempts", i)
		require.Truef(t, final.State == jobstate.Succeeded || final.State == jobstate.DeadLettered,
			"scenario %d: job ended in non-terminal state %s", i, final.State)
		require.GreaterOrEqualf(t, attemptsSeen, 1, "scenario %d: no attempt was ever claimed", i)
	}
}

// ---------------------------------------------------------------------
// Schema readiness: proves Phase 3 required no migration -- the RETRY_WAIT
// state and FAILED_RETRYABLE outcome were already accepted by the
// migration 0001/0002 CHECK constraints (deliberately, per those
// migrations' own comments, exactly to avoid a Phase 2 -> Phase 3 schema
// migration).
// ---------------------------------------------------------------------

func TestSchema_RetryWaitStateAlreadySupportedByCheckConstraint(t *testing.T) {
	db := testutil.DB(t)
	_, err := db.Exec(`
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds, eligible_at)
		VALUES ($1, 'test.schema.retrywait', '{}', 'RETRY_WAIT', 30, now() + interval '1 minute')`,
		uuid.New())
	require.NoError(t, err, "RETRY_WAIT must already be a legal state value under migration 0001's CHECK constraint")
}

func TestSchema_FailedRetryableOutcomeAlreadySupportedByCheckConstraint(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.schema.failedretryable", 5))
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at, finished_at, outcome)
		VALUES ($1, $2, 1, 1, 'worker-x', now(), now(), 'FAILED_RETRYABLE')`,
		uuid.New(), created.ID)
	require.NoError(t, err, "FAILED_RETRYABLE must already be a legal outcome value under migration 0002's CHECK constraint")
}
