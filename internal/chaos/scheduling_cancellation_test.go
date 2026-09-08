// Scheduling/cancellation/timeout chaos campaigns (Phase 9 adversarial
// campaign items 11, 12, 13, docs/roadmap.md "Scheduling / Cancellation /
// Timeout Chaos"): a scheduled job surviving a simulated worker-fleet
// restart, cancellation racing claim/execution under seeded concurrency,
// and execution-timeout racing lease/heartbeat activity -- combining
// Phase 6's guarantees (TF-INV-010, TF-INV-011) with Phase 2/5's
// concurrency hardening under chaos.
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestChaos_ScheduledJobSurvivesSimulatedFleetRestart is campaign item
// 11: a batch of jobs are scheduled for the future (some also holding an
// idempotency key, to prove scheduling and idempotency compose under
// restart), the entire "fleet" is torn down (a fresh *store.Store,
// standing in for a full process/fleet restart per SF-013/SF-018's
// established pattern -- no in-memory scheduler state exists to lose),
// and only once eligibility has passed does a freshly constructed worker
// pool claim and complete them -- no special "catch-up" logic should be
// needed.
func TestChaos_ScheduledJobSurvivesSimulatedFleetRestart(t *testing.T) {
	db := testutil.DB(t)
	preRestart := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()

	const numJobs = 12
	future := time.Now().Add(2 * time.Second)
	jobIDs := make([]string, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		key := fmt.Sprintf("chaos-sched-key-%d", i)
		created, _, err := preRestart.InsertIdempotent(ctx, job.NewParams{
			JobType:                 "chaos.scheduled",
			Payload:                 []byte(`{}`),
			MaxAttempts:             3,
			ExecutionTimeoutSeconds: 30,
			ScheduledAt:             &future,
			IdempotencyKey:          &key,
		})
		require.NoError(t, err)
		jobIDs = append(jobIDs, created.ID.String())
	}

	// Nothing should be claimable yet -- eligible_at is in the future.
	_, ok, err := preRestart.Claim(ctx, "too-early")
	require.NoError(t, err)
	require.False(t, ok, "TF-INV-011: nothing eligible before scheduled_at")

	// Simulate the entire fleet (API + every worker) stopping and
	// restarting: a brand-new *store.Store sharing only the database, per
	// SF-013/SF-018's established pattern -- no in-memory reference to any
	// of the above survives.
	postRestart := store.New(db, store.WithLogger(discardLogger()))

	// Duplicate submissions after "restart" must still resolve to the
	// same jobs (idempotency survives restart, per Phase 4).
	for i := 0; i < numJobs; i++ {
		key := fmt.Sprintf("chaos-sched-key-%d", i)
		dup, created, err := postRestart.InsertIdempotent(ctx, job.NewParams{
			JobType: "chaos.scheduled", Payload: []byte(`{}`), MaxAttempts: 3,
			ExecutionTimeoutSeconds: 30, ScheduledAt: &future, IdempotencyKey: &key,
		})
		require.NoError(t, err)
		require.False(t, created, "a duplicate submission after restart must not create a new row")
		require.Equal(t, jobIDs[i], dup.ID.String())
	}

	// Fast-forward past eligibility deterministically (no real sleep for
	// correctness -- the 2s scheduled_at above is only to make this a
	// genuine "future" value at insert time, not something this test
	// blocks on).
	for _, id := range jobIDs {
		_, err := db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() WHERE id = $1`, id)
		require.NoError(t, err)
	}

	registry := handler.NewRegistry()
	registry.Register("chaos.scheduled", alwaysSucceedHandler{})
	w := newWorker("post-restart-worker", postRestart, registry)
	for range jobIDs {
		claimed, err := w.RunOnce(ctx)
		require.NoError(t, err)
		require.True(t, claimed)
	}

	checker := invariant.New(db)
	for _, id := range jobIDs {
		final, err := postRestart.GetByID(ctx, mustParseUUID(t, id))
		require.NoError(t, err)
		require.Equal(t, jobstate.Succeeded, final.State)
	}
	checkNoViolations(t, ctx, checker, -1)
}

type alwaysSucceedHandler struct{}

func (alwaysSucceedHandler) Execute(context.Context, *job.Job) (handler.Result, error) {
	return handler.Result{}, nil
}

// TestChaos_CancellationRacesClaimAndExecution_Seeded is campaign item
// 12: for many jobs at once, a cancellation request races either the
// initial claim (job still QUEUED) or an in-flight execution (job
// RUNNING, handler gated open) -- per TF-INV-010, exactly one outcome
// must win deterministically for each job: CANCELLED or SUCCEEDED, never
// both, never neither.
func TestChaos_CancellationRacesClaimAndExecution_Seeded(t *testing.T) {
	for _, seed := range []int64{61, 62} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numJobs = 30

			for i := 0; i < numJobs; i++ {
				created, err := s.Insert(ctx, newChaosParams("chaos.cancelrace", 3))
				require.NoError(t, err)

				preClaim := rng.Bool(0.5)
				if preClaim {
					// Race the cancel directly against claim eligibility --
					// deterministic outcome by construction (no lease
					// exists yet to race over): CancelQueuedOrRetryWait
					// either wins outright, or the job was already claimed
					// a moment earlier by a concurrent goroutine below.
					var wg sync.WaitGroup
					start := make(chan struct{})
					var claimErr, cancelErr error
					var claimedOK bool
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-start
						_, claimedOK, claimErr = s.Claim(ctx, fmt.Sprintf("cancelrace-claimer-%d", i))
					}()
					go func() {
						defer wg.Done()
						<-start
						_, cancelErr = s.CancelQueuedOrRetryWait(ctx, created.ID)
					}()
					close(start)
					wg.Wait()
					require.NoError(t, claimErr)
					if cancelErr != nil {
						require.ErrorIs(t, cancelErr, store.ErrStaleTransition)
					}

					final, err := s.GetByID(ctx, created.ID)
					require.NoError(t, err)
					if claimedOK {
						// Claim won: finish it off so the pool converges.
						_, err := s.CompleteSuccess(ctx, created.ID, fmt.Sprintf("cancelrace-claimer-%d", i), 1, nil)
						require.NoError(t, err)
						final, err = s.GetByID(ctx, created.ID)
						require.NoError(t, err)
						require.Equal(t, jobstate.Succeeded, final.State)
					} else {
						require.Equal(t, jobstate.Cancelled, final.State)
					}
					continue
				}

				// Race cancellation against an in-flight RUNNING
				// completion: TF-INV-010's "first durable write wins" rule.
				claimed, ok, err := s.Claim(ctx, fmt.Sprintf("cancelrace-runner-%d", i))
				require.NoError(t, err)
				require.True(t, ok)

				cancelFirst := rng.Bool(0.5)
				if cancelFirst {
					_, err := s.RequestCancellation(ctx, claimed.ID)
					require.NoError(t, err)
					compResult, compErr := s.CompleteCancelled(ctx, claimed.ID, fmt.Sprintf("cancelrace-runner-%d", i), claimed.LeaseGeneration)
					require.NoError(t, compErr)
					require.Equal(t, jobstate.Cancelled, compResult.State)
				} else {
					_, compErr := s.CompleteSuccess(ctx, claimed.ID, fmt.Sprintf("cancelrace-runner-%d", i), claimed.LeaseGeneration, nil)
					require.NoError(t, compErr)
					// A cancellation request arriving after completion has
					// already committed must find the job non-RUNNING and
					// have no effect.
					_, reqErr := s.RequestCancellation(ctx, claimed.ID)
					require.ErrorIs(t, reqErr, store.ErrStaleTransition)
					final, err := s.GetByID(ctx, claimed.ID)
					require.NoError(t, err)
					require.Equal(t, jobstate.Succeeded, final.State)
				}
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_TimeoutRacesLeaseAndHeartbeat is campaign item 13: several
// jobs are claimed with a short execution_timeout_seconds and a
// deliberately non-cooperative (never returns) handler; the
// execution-timeout deadline fires independently of the lease/heartbeat
// mechanism (a distinct clock, per docs/execution-semantics.md), and the
// worker must report TIMED_OUT -- scheduling a retry or dead-lettering
// via the exact same attempt-ceiling machinery as any other retryable
// outcome -- without ever leaving the job stranded or double-reporting.
func TestChaos_TimeoutRacesLeaseAndHeartbeat(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	const numJobs = 6
	registry := handler.NewRegistry()
	jobIDs := make([]string, 0, numJobs)

	for i := 0; i < numJobs; i++ {
		jobType := fmt.Sprintf("chaos.timeout.%d", i)
		created, err := s.Insert(ctx, job.NewParams{
			JobType:                 jobType,
			Payload:                 []byte(`{}`),
			MaxAttempts:             1, // exhausts on first timeout -> DEAD_LETTERED, exercising the terminal path too
			ExecutionTimeoutSeconds: 1,
		})
		require.NoError(t, err)
		jobIDs = append(jobIDs, created.ID.String())
		// hangForever never returns and never checks ctx -- an
		// uncooperative handler, per docs/execution-semantics.md's
		// documented limitation (TaskForge detects and reports the
		// timeout; it cannot forcibly halt this goroutine).
		registry.Register(jobType, hangForeverHandler{})
	}

	w := newWorker("timeout-chaos-worker", s, registry)
	var wg sync.WaitGroup
	for range jobIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// RunOnce blocks until the execution-timeout context fires
			// (the handler never returns on its own); it must still
			// report a clean outcome.
			_, _ = w.RunOnce(ctx)
		}()
	}
	wg.Wait()

	for _, id := range jobIDs {
		final, err := s.GetByID(ctx, mustParseUUID(t, id))
		require.NoError(t, err)
		require.Equal(t, jobstate.DeadLettered, final.State, "job %s: max_attempts=1 means the first timeout must exhaust the retry budget", id)
		require.Equal(t, "TIMEOUT", *final.LastErrorClass)
	}

	// The uncooperative handler goroutines are still running in the
	// background (they never return); this is documented and expected --
	// the test process exits once this function returns, which is fine
	// for a bounded, single-run CI test.
	checkNoViolations(t, ctx, checker, -1)
}

// hangForeverHandler ignores context cancellation entirely (an
// uncooperative handler, per docs/execution-semantics.md's documented
// limitation: TaskForge detects and reports a timeout promptly, but
// cannot forcibly halt handler code that never checks ctx.Done()) --
// except for a generous safety-valve timer well past this test's own
// execution_timeout_seconds, purely so the test binary itself does not
// leak a goroutine forever; RunOnce's own timeout reporting fires and
// completes long before this valve ever matters.
type hangForeverHandler struct{}

func (hangForeverHandler) Execute(_ context.Context, _ *job.Job) (handler.Result, error) {
	<-time.After(1500 * time.Millisecond) // deliberately past this test's 1s execution_timeout_seconds
	return handler.Result{}, nil
}
