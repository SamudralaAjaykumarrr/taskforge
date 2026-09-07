// Phase 6 ("Scheduling, Cancellation, Timeouts", docs/roadmap.md)
// scheduling tests: scheduled_at/eligible_at semantics per
// docs/scheduling.md, TF-INV-011, SF-013. Time-dependent assertions use
// deterministic DB-time manipulation (forceSetScheduledEligibility,
// mirroring Phase 3's forceSetEligibleAt) rather than sleeps, per
// docs/testing-strategy.md.
package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// forceSetScheduledEligibility rewinds or fast-forwards jobID's
// eligible_at to exactly delta relative to PostgreSQL's own clock,
// simulating a scheduled job's eligibility timestamp arriving without a
// real sleep -- the scheduling analogue of testhelpers_test.go's
// forceSetEligibleAt, which is scoped to RETRY_WAIT only. The row must
// currently be QUEUED, or this fails loudly.
func forceSetScheduledEligibility(t *testing.T, db *sql.DB, jobID uuid.UUID, delta time.Duration) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE jobs SET eligible_at = now() + $2 * interval '1 second'
		WHERE id = $1 AND state = 'QUEUED'`, jobID, delta.Seconds())
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forceSetScheduledEligibility: job %s was not QUEUED", jobID)
}

func newScheduledJobParams(jobType string, scheduledAt *time.Time) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		ScheduledAt:             scheduledAt,
	}
}

// TestInsertIdempotent_NoScheduledAt_ImmediatelyEligible proves the
// unchanged Phase 1-5 default: a submission with no ScheduledAt is
// immediately claimable (eligible_at defaults to insert-time now(), not
// left in the future).
func TestInsertIdempotent_NoScheduledAt_ImmediatelyEligible(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.none", nil))
	require.NoError(t, err)
	require.Nil(t, created.ScheduledAt)
	require.False(t, created.EligibleAt.After(time.Now().Add(time.Second)), "eligible_at must not be in the future")

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok, "an immediately-eligible job must be claimable right away")
	require.Equal(t, created.ID, claimed.ID)
}

// TestInsertIdempotent_FutureScheduledAt_DurablyRecordedAndNotClaimable
// proves the core scheduling contract (TF-INV-011): a job submitted with
// scheduled_at in the future has BOTH scheduled_at (audit) and
// eligible_at (the live gate) durably set to that timestamp, and the
// exact same claim query every other job uses simply does not select it
// -- no separate scheduler code path, per docs/scheduling.md.
func TestInsertIdempotent_FutureScheduledAt_DurablyRecordedAndNotClaimable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour).Truncate(time.Millisecond)
	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.future", &future))
	require.NoError(t, err)

	require.NotNil(t, created.ScheduledAt)
	require.WithinDuration(t, future, *created.ScheduledAt, time.Second)
	require.WithinDuration(t, future, created.EligibleAt, time.Second)
	require.Equal(t, jobstate.Queued, created.State, "a future-scheduled job is QUEUED, not a distinct state -- docs/execution-semantics.md")

	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok, "a job scheduled an hour in the future must not be claimable now")

	// Re-read to prove the row itself (not just the claim query) shows no
	// side effect from the rejected claim attempt.
	reread, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Queued, reread.State)
	require.Equal(t, int64(0), reread.LeaseGeneration)
}

// TestInsertIdempotent_ScheduledJob_ClaimableAfterEligibilityArrives
// proves the other half of TF-INV-011: once PostgreSQL's own clock
// reaches eligible_at, the job becomes claimable via the unmodified claim
// query, with no catch-up or special-case logic. Eligibility is forced
// via DB-time manipulation (never a real sleep), per
// docs/testing-strategy.md.
func TestInsertIdempotent_ScheduledJob_ClaimableAfterEligibilityArrives(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour)
	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.arrives", &future))
	require.NoError(t, err)

	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok)

	forceSetScheduledEligibility(t, db, created.ID, -time.Second)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok, "the job must be claimable the instant eligible_at <= now(), regardless of the original scheduled_at")
	require.Equal(t, created.ID, claimed.ID)
	require.Equal(t, jobstate.Running, claimed.State)
	// scheduled_at (audit/caller-intent) must survive the claim
	// unmodified -- only eligible_at is retry/schedule-mutable.
	require.NotNil(t, claimed.ScheduledAt)
	require.WithinDuration(t, future, *claimed.ScheduledAt, time.Second)
}

// TestInsertIdempotent_ScheduledJob_SurvivesFreshStoreInstance is SF-013:
// a job scheduled for the future is submitted, "the API server and all
// workers" (here: this *store.Store, standing in for the whole process,
// per this codebase's established restart-simulation pattern) are
// discarded and a brand new *store.Store is constructed sharing only the
// database, and the job resumes exactly where it was left --
// unclaimable before eligibility, claimable after -- entirely from
// durable state, with no in-memory scheduler of any kind (TF-INV-011,
// TF-INV-001).
func TestInsertIdempotent_ScheduledJob_SurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	s1 := store.New(db)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour)
	created, err := s1.Insert(ctx, newScheduledJobParams("test.sched.restart", &future))
	require.NoError(t, err)

	// Simulate total process restart: nothing from s1 is reused below.
	s2 := store.New(db)

	_, ok, err := s2.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok, "a fresh process must still refuse to claim before eligibility -- no in-memory schedule state exists to lose or recover")

	forceSetScheduledEligibility(t, db, created.ID, -time.Second)

	claimed, ok, err := s2.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok, "a fresh process must claim correctly once eligible, with no catch-up logic needed")
	require.Equal(t, created.ID, claimed.ID)
}

// TestInsertIdempotent_ScheduledJob_CancelledBeforeEligibility_NeverExecutes
// proves cancellation of a not-yet-eligible scheduled job: it transitions
// directly to CANCELLED (docs/execution-semantics.md: QUEUED ->
// CANCELLED, no worker involved), and remains permanently unclaimable
// even once its original eligible_at arrives and passes -- TF-INV-005
// (terminal states never reopen) applies regardless of how the job got
// to QUEUED in the first place.
func TestInsertIdempotent_ScheduledJob_CancelledBeforeEligibility_NeverExecutes(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour)
	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.cancel", &future))
	require.NoError(t, err)

	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)
	require.NotNil(t, cancelled.TerminalAt)

	// Fast-forward past the original eligible_at -- a cancelled row must
	// never become claimable, no matter what its (now-irrelevant)
	// eligible_at says.
	_, err = db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() - interval '1 second' WHERE id = $1`, created.ID)
	require.NoError(t, err)

	_, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok, "a cancelled job must never be claimed, even past its original eligible_at")

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, final.State)
}

// TestInsertIdempotent_ScheduledJob_OrderingAgainstImmediatelyEligibleJob
// proves scheduling and ordinary immediate jobs interact correctly
// through the identical claim query (docs/scheduling.md: "the exact same
// claim query workers already run ... is the only mechanism that ever
// makes a scheduled job eligible"): an immediately-eligible job is
// claimed before a job scheduled for the future, purely because the
// scheduled job does not satisfy eligible_at <= now() yet -- no special
// priority/scheduling logic is involved.
func TestInsertIdempotent_ScheduledJob_OrderingAgainstImmediatelyEligibleJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour)
	_, err := s.Insert(ctx, newScheduledJobParams("test.sched.order.future", &future))
	require.NoError(t, err)
	immediate, err := s.Insert(ctx, newScheduledJobParams("test.sched.order.now", nil))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, immediate.ID, claimed.ID, "only the immediately-eligible job should be claimable while the other's eligible_at is still in the future")

	_, ok, err = s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.False(t, ok, "nothing else should be claimable yet")
}

// TestInsertIdempotent_ScheduledJob_ManyWorkersRaceAtEligibility is the
// scheduling analogue of SF-006: once a scheduled job's eligible_at
// arrives, many concurrent workers race for it exactly as they would for
// any other eligible row -- exactly one claim succeeds (TF-INV-002),
// proving Phase 6 scheduling introduces no new claim code path that
// could weaken existing fencing guarantees.
func TestInsertIdempotent_ScheduledJob_ManyWorkersRaceAtEligibility(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	future := time.Now().Add(1 * time.Hour)
	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.race", &future))
	require.NoError(t, err)
	forceSetScheduledEligibility(t, db, created.ID, -time.Second)

	const workers = 20
	var wg sync.WaitGroup
	var successCount int64
	generations := make([]int64, workers)

	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			claimed, ok, err := s.Claim(ctx, uuid.New().String())
			require.NoError(t, err)
			if ok && claimed.ID == created.ID {
				atomic.AddInt64(&successCount, 1)
				generations[idx] = claimed.LeaseGeneration
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.Equal(t, int64(1), successCount, "exactly one worker must win the claim race for the newly-eligible scheduled job")
}

// TestInsertIdempotent_RetryWaitEligibility_NotBypassedByScheduling proves
// the required separation from docs/scheduling.md "Relationship to Retry
// Backoff" and this task's explicit requirement: a RETRY_WAIT job's
// backoff-computed eligible_at governs its own reclaim, completely
// independent of scheduled_at (which stays NULL for a job that was never
// user-scheduled in the first place) -- retry backoff cannot be bypassed
// by, or confused with, user-requested scheduling.
func TestInsertIdempotent_RetryWaitEligibility_NotBypassedByScheduling(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newScheduledJobParams("test.sched.vs.retry", nil))
	require.NoError(t, err)
	require.Nil(t, created.ScheduledAt, "an ordinary (non-scheduled) job must never acquire a scheduled_at value merely by retrying")

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, "transient", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
	require.Nil(t, result.ScheduledAt, "retrying must never populate scheduled_at -- that field is exclusively caller-supplied submission intent")

	// The retry's own backoff window governs eligibility -- not yet
	// claimable immediately after the failure report.
	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a RETRY_WAIT job must not be claimable before its own computed backoff eligible_at, regardless of scheduling semantics")
}
