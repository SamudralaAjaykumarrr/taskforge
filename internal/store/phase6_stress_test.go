// Phase 6 concurrency-hardening tests, mirroring Phase 5's stress-test
// shape (concurrency_stress_test.go) but for the new scheduling/
// cancellation/timeout machinery specifically: this task's explicit
// requirements to prove, under real concurrent PostgreSQL sessions, that
// many workers cannot claim a future job early, that cancellation cannot
// be overwritten by a stale generation even under contention, and that
// terminal cancelled jobs remain invisible to the claim query under
// pressure.
package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestStress_CancellationSurvivesConcurrentCompletionAttemptsAcrossManyGenerations
// extends SF-008's fencing proof to cancellation specifically: a job is
// driven through several reclaim generations (simulating repeated
// crash/reclaim cycles), a cancellation is requested and acknowledged
// under the CURRENT (final) generation, and then every one of the
// job's PAST (now-stale) generations fires a concurrent completion
// attempt (success, permanent failure, retryable failure, and a second
// cancellation acknowledgement) all at once. Every stale attempt must be
// rejected; the job must remain CANCELLED throughout, under sustained
// concurrent pressure, not just in a hand-crafted two-call sequence.
func TestStress_CancellationSurvivesConcurrentCompletionAttemptsAcrossManyGenerations(t *testing.T) {
	db := testutil.DB(t)
	// Bound concurrent connection usage, mirroring
	// TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts:
	// this test fires 260 completion calls at once, which would otherwise
	// ask PostgreSQL for more simultaneous connections than its default
	// max_connections comfortably allows. Excess goroutines simply queue
	// for a pooled connection rather than erroring.
	db.SetMaxOpenConns(50)
	s := store.New(db)
	ctx := context.Background()

	const jobCount = 20
	const staleGenerationsPerJob = 3 // generations 1..3 are stale; generation 4 is current

	type jobCase struct {
		id                uuid.UUID
		staleGenerations  []int64
		currentGeneration int64
		currentLeaseOwner string
	}

	cases := make([]jobCase, jobCount)
	for i := 0; i < jobCount; i++ {
		created, err := s.Insert(ctx, newCancelJobParams("test.stress.cancel.fencing"))
		require.NoError(t, err)

		var staleGens []int64
		owner := "worker-0"
		for g := 0; g < staleGenerationsPerJob; g++ {
			claimed, ok, err := s.Claim(ctx, owner)
			require.NoError(t, err)
			require.True(t, ok)
			staleGens = append(staleGens, claimed.LeaseGeneration)
			forceExpireLease(t, db, created.ID)
			owner = "worker-" + string(rune('1'+g))
		}
		final, ok, err := s.Claim(ctx, owner)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, int64(staleGenerationsPerJob+1), final.LeaseGeneration)

		_, err = s.RequestCancellation(ctx, created.ID)
		require.NoError(t, err)
		result, err := s.CompleteCancelled(ctx, created.ID, owner, final.LeaseGeneration)
		require.NoError(t, err)
		require.Equal(t, jobstate.Cancelled, result.State)

		cases[i] = jobCase{
			id:                created.ID,
			staleGenerations:  staleGens,
			currentGeneration: final.LeaseGeneration,
			currentLeaseOwner: owner,
		}
	}

	var wg sync.WaitGroup
	var rejectedCount int64
	start := make(chan struct{})

	for _, c := range cases {
		for gi, gen := range c.staleGenerations {
			gen := gen
			owner := "worker-" + string(rune('0'+gi))
			id := c.id
			wg.Add(4)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CompleteSuccess(ctx, id, owner, gen, nil)
				require.ErrorIs(t, err, store.ErrStaleTransition)
				atomic.AddInt64(&rejectedCount, 1)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CompleteFailure(ctx, id, owner, gen, "boom", "PERMANENT")
				require.ErrorIs(t, err, store.ErrStaleTransition)
				atomic.AddInt64(&rejectedCount, 1)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CompleteRetryableFailure(ctx, id, owner, gen, "boom", time.Millisecond)
				require.ErrorIs(t, err, store.ErrStaleTransition)
				atomic.AddInt64(&rejectedCount, 1)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CompleteCancelled(ctx, id, owner, gen)
				require.ErrorIs(t, err, store.ErrStaleTransition)
				atomic.AddInt64(&rejectedCount, 1)
			}()
		}
		// Also race a duplicate cancellation acknowledgement attempt
		// under the CURRENT generation -- it must be rejected too, since
		// the job is already CANCELLED (not RUNNING).
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CompleteCancelled(ctx, c.id, c.currentLeaseOwner, c.currentGeneration)
			require.ErrorIs(t, err, store.ErrStaleTransition)
			atomic.AddInt64(&rejectedCount, 1)
		}()
	}

	close(start)
	wg.Wait()

	require.Equal(t, int64(jobCount*(staleGenerationsPerJob*4+1)), rejectedCount)

	for _, c := range cases {
		final, err := s.GetByID(ctx, c.id)
		require.NoError(t, err)
		require.Equal(t, jobstate.Cancelled, final.State, "job %s must remain CANCELLED after sustained concurrent stale-generation pressure", c.id)
	}
}

// TestStress_ManyEligibleScheduledJobsClaimedExactlyOnceUnderPressure
// extends the scheduling race proof to many simultaneously-eligible
// scheduled jobs claimed by a worker pool, mixed with already-terminal
// (cancelled) scheduled jobs present in the same pool -- proving
// TF-INV-005 and TF-INV-002 both hold for scheduling specifically under
// contention, not just in isolation (mirrors Phase 5's
// TestStress_ConcurrentClaimPressureWithTerminalJobsPresent shape).
func TestStress_ManyEligibleScheduledJobsClaimedExactlyOnceUnderPressure(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const eligibleCount = 40
	const cancelledCount = 40
	const workers = 15

	eligibleIDs := make(map[uuid.UUID]bool, eligibleCount)
	for i := 0; i < eligibleCount; i++ {
		future := time.Now().Add(1 * time.Hour)
		created, err := s.Insert(ctx, newScheduledJobParams("test.stress.sched.eligible", &future))
		require.NoError(t, err)
		forceSetScheduledEligibility(t, db, created.ID, -time.Second)
		eligibleIDs[created.ID] = true
	}
	for i := 0; i < cancelledCount; i++ {
		future := time.Now().Add(1 * time.Hour)
		created, err := s.Insert(ctx, newScheduledJobParams("test.stress.sched.cancelled", &future))
		require.NoError(t, err)
		_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
		require.NoError(t, err)
	}

	var mu sync.Mutex
	claimedIDs := make(map[uuid.UUID]int)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerNum int) {
			defer wg.Done()
			<-start
			owner := "worker-" + uuid.New().String()
			for {
				claimed, ok, err := s.Claim(ctx, owner)
				require.NoError(t, err)
				if !ok {
					return
				}
				mu.Lock()
				claimedIDs[claimed.ID]++
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()

	require.Len(t, claimedIDs, eligibleCount, "exactly the eligible scheduled jobs must be claimed, never the cancelled ones")
	for id, count := range claimedIDs {
		require.True(t, eligibleIDs[id], "claimed job %s must be one of the eligible scheduled jobs, never a cancelled one", id)
		require.Equal(t, 1, count, "job %s claimed more than once", id)
	}
}
