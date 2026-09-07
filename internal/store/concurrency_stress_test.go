// Phase 5 ("Concurrency Hardening", docs/roadmap.md): extended/stress
// variants of SF-006, SF-007, and SF-008 at higher worker/job counts and
// under sustained, repeated contention, per that phase's explicit scope
// ("Realistic multi-worker load: tens of concurrent workers against a
// shared job pool", "Contention behavior under SKIP LOCKED at higher
// concurrency", "Worker recovery drills under randomized crash
// injection"). This file re-verifies TF-INV-002, TF-INV-003, TF-INV-004,
// and TF-INV-014 — the exact invariants docs/roadmap.md's Phase 5 entry
// names — under load, not just the crafted two/three-worker scenarios
// internal/store/lease_test.go already proves at small scale.
//
// Every test here uses explicit synchronization (start barriers, wait
// groups, DB-time manipulation via forceExpireLease/forceSetEligibleAt) —
// never a sleep-based race — per docs/testing-strategy.md's determinism
// requirement, so a failure here is a real defect, not scheduler luck.
package store_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/retry"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// ---------------------------------------------------------------------
// SF-006 extended: many workers, many jobs, simultaneous initial claims
// (adversarial cases #2/#3: "20 workers race for 100 jobs", "all workers
// claim at the same instant").
// ---------------------------------------------------------------------

// TestStress_SF006_ManyWorkersManyJobs_NoDoubleClaimNoGenerationReuse is
// SF-006 at Phase 5 scale: tens of workers, hundreds of jobs, all workers
// released from a single barrier so their first claim attempts genuinely
// overlap. TF-INV-002 requires every job claimed exactly once and no
// lease_generation issued twice for the same job.
func TestStress_SF006_ManyWorkersManyJobs_NoDoubleClaimNoGenerationReuse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const numJobs = 300
	const numWorkers = 25

	jobIDs := make(map[string]bool, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.sf006", 5))
		require.NoError(t, err)
		jobIDs[created.ID.String()] = true
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	claimsCh := make(chan *job.Job, numJobs*2)
	errsCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, fmt.Sprintf("stress-worker-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
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
	close(errsCh)

	for err := range errsCh {
		require.NoError(t, err)
	}

	seenGeneration := make(map[string]map[int64]bool, numJobs)
	claimedCount := 0
	for j := range claimsCh {
		claimedCount++
		id := j.ID.String()
		require.True(t, jobIDs[id], "claimed an unexpected job id")
		if seenGeneration[id] == nil {
			seenGeneration[id] = make(map[int64]bool)
		}
		require.False(t, seenGeneration[id][j.LeaseGeneration],
			"TF-INV-002 violated: lease_generation %d issued twice for job %s", j.LeaseGeneration, id)
		seenGeneration[id][j.LeaseGeneration] = true
	}

	require.Equal(t, numJobs, claimedCount, "every job must be claimed exactly once, no job stranded")
	require.Len(t, seenGeneration, numJobs)
}

// TestStress_ManyWorkersRaceForOneJob is adversarial case #1: 20 workers
// simultaneously race for a single job. Exactly one must win; the other 19
// must observe ok=false (SKIP LOCKED semantics), never an error.
func TestStress_ManyWorkersRaceForOneJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("stress.one.job", 5))
	require.NoError(t, err)

	const numWorkers = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	var wins int64
	errsCh := make(chan error, numWorkers)
	winnersCh := make(chan *job.Job, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			j, ok, err := s.Claim(ctx, fmt.Sprintf("racer-%03d", workerID))
			if err != nil {
				errsCh <- err
				return
			}
			if ok {
				atomic.AddInt64(&wins, 1)
				winnersCh <- j
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errsCh)
	close(winnersCh)

	for err := range errsCh {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), wins, "exactly one of 20 concurrent racers must win the single job")

	winner := <-winnersCh
	require.Equal(t, created.ID, winner.ID)
	require.Equal(t, int64(1), winner.LeaseGeneration)
}

// ---------------------------------------------------------------------
// Claim contention shapes explicitly required by Phase 5: fewer jobs than
// workers, and more jobs than workers.
// ---------------------------------------------------------------------

func TestStress_ClaimContention_JobToWorkerRatios(t *testing.T) {
	cases := []struct {
		name       string
		numJobs    int
		numWorkers int
	}{
		{"fewer_jobs_than_workers", 5, 20},
		{"more_jobs_than_workers", 100, 10},
		{"roughly_equal", 40, 40},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()

			for i := 0; i < tc.numJobs; i++ {
				_, err := s.Insert(ctx, newJobParamsN("stress.ratio."+tc.name, 5))
				require.NoError(t, err)
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			var totalClaims int64
			errsCh := make(chan error, tc.numWorkers)

			for w := 0; w < tc.numWorkers; w++ {
				wg.Add(1)
				workerID := w
				go func() {
					defer wg.Done()
					<-start
					for {
						_, ok, err := s.Claim(ctx, fmt.Sprintf("ratio-worker-%03d", workerID))
						if err != nil {
							errsCh <- err
							return
						}
						if !ok {
							return
						}
						atomic.AddInt64(&totalClaims, 1)
					}
				}()
			}

			close(start)
			wg.Wait()
			close(errsCh)
			for err := range errsCh {
				require.NoError(t, err)
			}

			require.Equal(t, int64(tc.numJobs), totalClaims,
				"every job must be claimed exactly once regardless of the job-to-worker ratio")
		})
	}
}

// ---------------------------------------------------------------------
// SF-007 extended: sustained concurrent reclaim of many simultaneously
// expired leases (adversarial case: "simultaneous expired-lease reclaim
// attempts").
// ---------------------------------------------------------------------

// TestStress_SF007_SustainedConcurrentReclaimOfManyExpiredLeases claims a
// large pool of jobs (simulating many workers that then "crash" — never
// completing), force-expires every one of their leases, and then unleashes
// many concurrent workers to reclaim from that pool at once. TF-INV-004
// requires every job to become claimable again; TF-INV-002 requires each
// to be reclaimed exactly once, under a strictly incremented generation.
func TestStress_SF007_SustainedConcurrentReclaimOfManyExpiredLeases(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 120
	const numReclaimers = 20

	jobIDs := make([]string, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.sf007", 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("crashed-worker-%03d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, int64(1), claimed.LeaseGeneration)
		jobIDs = append(jobIDs, created.ID.String())
	}

	// Every job's owning "worker" now silently disappears (never calls
	// CompleteSuccess/Failure) — simulate the crash by force-expiring every
	// lease at once via PostgreSQL's own clock, no sleep required.
	_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
	require.NoError(t, err)

	start := make(chan struct{})
	var wg sync.WaitGroup
	reclaimsCh := make(chan *job.Job, numJobs*2)
	errsCh := make(chan error, numReclaimers)

	for w := 0; w < numReclaimers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, fmt.Sprintf("reclaimer-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
				if !ok {
					return
				}
				reclaimsCh <- j
			}
		}()
	}

	close(start)
	wg.Wait()
	close(reclaimsCh)
	close(errsCh)

	for err := range errsCh {
		require.NoError(t, err)
	}

	seen := make(map[string]bool, numJobs)
	count := 0
	for j := range reclaimsCh {
		count++
		require.Equal(t, int64(2), j.LeaseGeneration, "a reclaim must strictly advance the generation exactly once")
		require.Equal(t, 2, j.AttemptCount)
		require.False(t, seen[j.ID.String()], "job %s reclaimed more than once", j.ID)
		seen[j.ID.String()] = true
	}
	require.Equal(t, numJobs, count, "every crashed job must be reclaimed exactly once, none stranded (TF-INV-004)")
	for _, id := range jobIDs {
		require.True(t, seen[id], "job %s was never reclaimed", id)
	}
}

// ---------------------------------------------------------------------
// SF-008 extended: fencing holds under sustained concurrent completion
// attempts across many jobs and many stale generations at once.
// ---------------------------------------------------------------------

// TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts
// advances many jobs through several generations each (simulating repeated
// crashes), then fires every generation's completion call for every job
// concurrently, all at once. TF-INV-003/TF-INV-014 require that, for each
// job, exactly one completion (the current generation) succeeds and every
// other (stale) generation's call is rejected — regardless of arrival
// order under real concurrent pressure, not just a hand-sequenced test.
func TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts(t *testing.T) {
	db := testutil.DB(t)
	// Bound concurrent connection usage: this test fires ~160 completion
	// calls at once, which would otherwise ask PostgreSQL for more
	// simultaneous connections than its default max_connections comfortably
	// allows. Excess goroutines simply queue for a pooled connection rather
	// than erroring — itself a small demonstration that claim/complete's
	// short-transaction design degrades to queueing, not failure, under a
	// bounded pool (see the dedicated connection-pool test below for the
	// focused version of that property).
	db.SetMaxOpenConns(50)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 40
	const generationsPerJob = 4 // 1 legitimate final generation + 3 stale ones

	type staleCall struct {
		jobID      string
		owner      string
		generation int64
	}
	type finalCall struct {
		jobID      string
		owner      string
		generation int64
	}

	var stale []staleCall
	var final []finalCall

	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.sf008", 10))
		require.NoError(t, err)

		var lastOwner string
		var lastGen int64
		for g := 0; g < generationsPerJob; g++ {
			owner := fmt.Sprintf("job-%03d-gen-%d", i, g)
			claimed, ok, err := s.Claim(ctx, owner)
			require.NoError(t, err)
			require.True(t, ok)
			if g < generationsPerJob-1 {
				// This generation "crashes" — force-expire so the next
				// generation can be claimed, and remember it as a stale
				// completion attempt to fire later.
				stale = append(stale, staleCall{jobID: created.ID.String(), owner: owner, generation: claimed.LeaseGeneration})
				_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, created.ID)
				require.NoError(t, err)
			} else {
				lastOwner = owner
				lastGen = claimed.LeaseGeneration
			}
		}
		final = append(final, finalCall{jobID: created.ID.String(), owner: lastOwner, generation: lastGen})
	}

	// Now fire every stale completion call AND every legitimate final
	// completion call concurrently, all released from one barrier, so the
	// legitimate write and its stale predecessors genuinely race.
	start := make(chan struct{})
	var wg sync.WaitGroup

	staleResults := make(chan error, len(stale))
	for _, c := range stale {
		wg.Add(1)
		c := c
		go func() {
			defer wg.Done()
			<-start
			id := mustParseUUID(t, c.jobID)
			_, err := s.CompleteSuccess(ctx, id, c.owner, c.generation, nil)
			staleResults <- err
		}()
	}

	finalResults := make(chan error, len(final))
	for _, c := range final {
		wg.Add(1)
		c := c
		go func() {
			defer wg.Done()
			<-start
			id := mustParseUUID(t, c.jobID)
			_, err := s.CompleteSuccess(ctx, id, c.owner, c.generation, nil)
			finalResults <- err
		}()
	}

	close(start)
	wg.Wait()
	close(staleResults)
	close(finalResults)

	for err := range staleResults {
		require.ErrorIs(t, err, store.ErrStaleTransition, "every stale generation's completion must be rejected, even under concurrent pressure")
	}
	for err := range finalResults {
		require.NoError(t, err, "the current generation's completion must succeed exactly once")
	}

	for _, c := range final {
		got, err := s.GetByID(ctx, mustParseUUID(t, c.jobID))
		require.NoError(t, err)
		require.Equal(t, jobstate.Succeeded, got.State)
		require.Equal(t, c.generation, got.LeaseGeneration, "the job's final generation must be the legitimate one, untouched by any stale writer")
	}
}

// ---------------------------------------------------------------------
// Retry eligibility under concurrency at Phase 5 scale.
// ---------------------------------------------------------------------

// TestStress_ConcurrentWorkersRaceForManyEligibleRetryWaitJobs extends
// TestClaim_MultipleWorkersOnlyOneWinsOnceEligible to many simultaneously
// eligible RETRY_WAIT jobs and many workers: no job may be claimed twice
// the instant it becomes eligible, and no RETRY_WAIT job is claimed early.
func TestStress_ConcurrentWorkersRaceForManyEligibleRetryWaitJobs(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 60
	const numWorkers = 15

	jobIDs := make([]string, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.retrywait", 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("first-attempt-%03d", i))
		require.NoError(t, err)
		require.True(t, ok)
		_, err = s.CompleteRetryableFailure(ctx, claimed.ID, fmt.Sprintf("first-attempt-%03d", i), claimed.LeaseGeneration, "transient", 10*time.Second)
		require.NoError(t, err)
		jobIDs = append(jobIDs, created.ID.String())
	}

	// Nobody should be able to claim any of these yet: eligible_at is 10s
	// in the future.
	preemptive, ok, err := s.Claim(ctx, "too-early")
	require.NoError(t, err)
	require.False(t, ok, "TF-INV-011 (via eligible_at) must hold: nothing eligible yet")
	require.Nil(t, preemptive)

	// Fast-forward every job's eligibility to "now" at once (DB-time
	// manipulation, no sleep), then unleash concurrent claimers.
	for _, id := range jobIDs {
		forceSetEligibleAt(t, db, mustParseUUID(t, id), 0)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	claimsCh := make(chan *job.Job, numJobs*2)
	errsCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, fmt.Sprintf("retry-claimer-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
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
	close(errsCh)

	for err := range errsCh {
		require.NoError(t, err)
	}

	seen := make(map[string]bool, numJobs)
	count := 0
	for j := range claimsCh {
		count++
		require.Equal(t, jobstate.Running, j.State)
		require.Equal(t, int64(2), j.LeaseGeneration)
		require.False(t, seen[j.ID.String()], "RETRY_WAIT job %s claimed more than once at its eligibility instant", j.ID)
		seen[j.ID.String()] = true
	}
	require.Equal(t, numJobs, count)
}

// ---------------------------------------------------------------------
// Adversarial case #10 at scale: DEAD_LETTERED/SUCCEEDED jobs remain
// invisible to the claim query even under heavy concurrent claim pressure
// from a pool that also contains genuinely claimable work.
// ---------------------------------------------------------------------

func TestStress_ConcurrentClaimPressureWithTerminalJobsPresent(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numClaimable = 80
	const numTerminal = 80
	const numWorkers = 20

	for i := 0; i < numTerminal; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.terminal.pressure", 1))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "terminal-setup")
		require.NoError(t, err)
		require.True(t, ok)
		if i%2 == 0 {
			_, err = s.CompleteSuccess(ctx, claimed.ID, "terminal-setup", claimed.LeaseGeneration, nil)
		} else {
			_, err = s.CompleteFailure(ctx, claimed.ID, "terminal-setup", claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
		}
		require.NoError(t, err)
		// Adversarial: give the terminal row a long-expired lease timestamp
		// anyway, as if it somehow still carried one, to make sure the
		// claim query's state='RUNNING' guard (not lease timing) is what
		// protects it.
		_, err = db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 hour' WHERE id = $1`, created.ID)
		require.NoError(t, err)
	}

	claimableIDs := make(map[string]bool, numClaimable)
	for i := 0; i < numClaimable; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.terminal.pressure", 5))
		require.NoError(t, err)
		claimableIDs[created.ID.String()] = true
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	claimsCh := make(chan *job.Job, numClaimable*2)
	errsCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, fmt.Sprintf("pressure-worker-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
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
	close(errsCh)

	for err := range errsCh {
		require.NoError(t, err)
	}

	count := 0
	for j := range claimsCh {
		count++
		require.True(t, claimableIDs[j.ID.String()], "a terminal job was claimed under concurrent pressure: TF-INV-005 violated")
	}
	require.Equal(t, numClaimable, count, "all genuinely claimable jobs must still be claimed despite terminal jobs in the same pool")
}

// ---------------------------------------------------------------------
// Concurrent Lazy Dead-Letter Sweep of many exhausted, expired leases at
// once — the "lock convoy" concern docs/architecture.md and Phase 5's
// roadmap entry both call out (the sweep step is a plain UPDATE, not
// SKIP LOCKED, so concurrent claimers serialize briefly on overlapping
// rows). This proves that serialization is merely a latency
// consideration, not a correctness one: no double dead-lettering, no
// error, no deadlock.
// ---------------------------------------------------------------------

func TestStress_ConcurrentSweepOfManyExhaustedExpiredLeases_NoDoubleDeadLetter(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 60
	const numWorkers = 15

	jobIDs := make([]string, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("stress.sweep", 1)) // max_attempts=1
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("exhausting-worker-%03d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, 1, claimed.AttemptCount) // == max_attempts already
		jobIDs = append(jobIDs, created.ID.String())
	}

	_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
	require.NoError(t, err)

	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errsCh := make(chan error, numWorkers)
	var totalClaims int64

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-ctxTimeout.Done():
					errsCh <- ctxTimeout.Err()
					return
				default:
				}
				_, ok, err := s.Claim(ctxTimeout, fmt.Sprintf("sweep-worker-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
				if !ok {
					return
				}
				atomic.AddInt64(&totalClaims, 1)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err, "no deadlock or error expected while sweeping many exhausted leases concurrently")
	}

	require.Equal(t, int64(0), totalClaims, "an attempt-exhausted expired lease must never be reclaimed, only swept (TF-INV-006)")

	for _, id := range jobIDs {
		final, err := s.GetByID(ctx, mustParseUUID(t, id))
		require.NoError(t, err)
		require.Equal(t, jobstate.DeadLettered, final.State)
		require.NotNil(t, final.TerminalAt)
	}
}

// ---------------------------------------------------------------------
// Database connection pooling: worker concurrency must not require one
// permanently held connection per worker, since claim/heartbeat/complete
// are each single short transactions with no handler execution inside
// them. This deliberately constrains the pool BELOW the goroutine count to
// catch any accidental "hold a connection across an await" pattern, which
// would deadlock (every goroutine holding a connection while waiting for
// one that never frees up) rather than merely queueing.
// ---------------------------------------------------------------------

func TestStress_ClaimProgressesUnderConstrainedConnectionPool(t *testing.T) {
	db := testutil.DB(t)
	const maxConns = 3
	db.SetMaxOpenConns(maxConns)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 60
	const numWorkers = 15 // deliberately > maxConns

	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newJobParamsN("stress.pool", 5))
		require.NoError(t, err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var totalClaims int64
	errsCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-ctxTimeout.Done():
					return
				default:
				}
				_, ok, err := s.Claim(ctxTimeout, fmt.Sprintf("pool-worker-%03d", workerID))
				if err != nil {
					errsCh <- err
					return
				}
				if !ok {
					return
				}
				atomic.AddInt64(&totalClaims, 1)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errsCh)

	for err := range errsCh {
		require.NoError(t, err, "a connection pool smaller than the worker count must never deadlock claiming, only serialize it")
	}
	require.Equal(t, int64(numJobs), totalClaims, "all jobs must still be claimed despite a constrained pool")
	require.NoError(t, ctxTimeout.Err(), "the claim drain must finish well before the 30s bound, proving no pool deadlock occurred")
}

// ---------------------------------------------------------------------
// Randomized crash/retry/success injection, repeated across several fixed
// seeds, with deterministic per-round synchronization (never a sleep) so
// a failure here is reproducible. This is Phase 5's "worker recovery
// drills under randomized crash injection (not just the single-crash
// scenarios of Phase 2, but repeated/overlapping crashes)" requirement.
//
// The simulation proceeds in rounds: each round, a pool of worker
// goroutines races (via a shared barrier) to drain every currently
// claimable job, and each winner resolves its claim according to a
// seeded pseudo-random decision (succeed / retryable-fail / "crash" by
// abandoning it). After every round, any job left RUNNING (a simulated
// crash) has its lease force-expired via PostgreSQL's clock so the next
// round's claim query can reclaim it, and RETRY_WAIT jobs are
// fast-forwarded to eligible. The simulation ends when a round claims
// nothing, which is guaranteed within a bounded number of rounds because
// max_attempts is small and finite.
// ---------------------------------------------------------------------

func TestStress_RandomizedCrashRetrySucceed_RepeatedSeeds_InvariantsHold(t *testing.T) {
	const numJobs = 80
	const numWorkersPerRound = 12
	const maxAttempts = 4
	const maxRounds = 30

	for _, seed := range []int64{1, 2, 3, 4, 5} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			db := testutil.DB(t)
			s := store.New(db)
			ctx := context.Background()
			// retry.Rand (not math/rand.Rand directly) because many
			// worker goroutines below share this one source concurrently;
			// math/rand.Rand is not safe for concurrent use, and retry.Rand
			// is the mutex-guarded wrapper this codebase already uses for
			// exactly that reason (see internal/retry/backoff.go).
			rng := retry.NewRand(seed)

			jobIDs := make([]string, 0, numJobs)
			for i := 0; i < numJobs; i++ {
				created, err := s.Insert(ctx, newJobParamsN("stress.chaos", maxAttempts))
				require.NoError(t, err)
				jobIDs = append(jobIDs, created.ID.String())
			}

			round := 0
			for ; round < maxRounds; round++ {
				start := make(chan struct{})
				var wg sync.WaitGroup
				var claimedThisRound int64
				errsCh := make(chan error, numWorkersPerRound)

				for w := 0; w < numWorkersPerRound; w++ {
					wg.Add(1)
					workerID := w
					go func() {
						defer wg.Done()
						<-start
						for {
							j, ok, err := s.Claim(ctx, fmt.Sprintf("chaos-r%d-w%03d", round, workerID))
							if err != nil {
								errsCh <- err
								return
							}
							if !ok {
								return
							}
							atomic.AddInt64(&claimedThisRound, 1)
							resolveChaosClaim(t, s, ctx, j, int(rng.Int63n(3)))
						}
					}()
				}

				close(start)
				wg.Wait()
				close(errsCh)
				for err := range errsCh {
					require.NoError(t, err)
				}

				// Advance simulated time for the next round: force-expire
				// any "crashed" (still-RUNNING) job's lease, and make any
				// RETRY_WAIT job immediately eligible, so the next round's
				// claim query has something deterministic to work with
				// rather than waiting on real backoff timers.
				_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() WHERE state = 'RETRY_WAIT'`)
				require.NoError(t, err)

				if claimedThisRound == 0 {
					break
				}
			}
			require.Less(t, round, maxRounds, "simulation did not converge to all-terminal within the round budget")

			// Final invariant checks: every job reached a terminal state,
			// attempt history is gapless/monotonic/append-only-consistent,
			// and DEAD_LETTERED only happened at or above max_attempts.
			for _, idStr := range jobIDs {
				id := mustParseUUID(t, idStr)
				final, err := s.GetByID(ctx, id)
				require.NoError(t, err)
				require.True(t, final.IsTerminal(), "job %s did not reach a terminal state (TF-INV-004: nothing may be stranded)", idStr)
				require.LessOrEqual(t, final.AttemptCount, maxAttempts, "TF-INV-006: attempt_count must never exceed max_attempts")
				if final.State == jobstate.DeadLettered {
					require.NotNil(t, final.LastError, "TF-INV-009: a dead-lettered job must carry a failure reason")
				}

				attempts := attemptsForJob(t, db, id)
				require.NotEmpty(t, attempts)
				for i, a := range attempts {
					require.Equal(t, i+1, a.AttemptNumber, "TF-INV-007: attempt_number must be gapless and 1-based for job %s", idStr)
					require.NotNil(t, a.FinishedAt, "every attempt but possibly the very last must be finalized")
					require.NotNil(t, a.Outcome)
				}
				require.Equal(t, final.AttemptCount, len(attempts), "job_attempts row count must match attempt_count exactly")

				var prevGen int64 = -1
				for _, a := range attempts {
					require.Greater(t, a.LeaseGeneration, prevGen, "lease_generation must be strictly increasing across a job's own attempt history")
					prevGen = a.LeaseGeneration
				}
			}
		})
	}
}

// resolveChaosClaim resolves a claimed job per a 0/1/2 decision:
//
//	0 -> report success
//	1 -> report retryable failure (store decides RETRY_WAIT vs DEAD_LETTERED)
//	2 -> "crash": do nothing, leaving the job RUNNING for the next round's
//	     force-expire-then-reclaim cycle to pick up
func resolveChaosClaim(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, decision int) {
	t.Helper()
	owner := *j.LeaseOwner
	switch decision {
	case 0:
		_, err := s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
		require.NoError(t, err)
	case 1:
		_, err := s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: simulated transient failure", 0)
		require.NoError(t, err)
	case 2:
		// Simulated crash: intentionally do nothing.
	}
}

// ---------------------------------------------------------------------
// small local helpers
// ---------------------------------------------------------------------

// mustParseUUID parses a job ID string previously obtained from
// job.Job.ID.String(); every caller here controls the input (IDs this
// same test generated), so a parse failure indicates a test bug, not bad
// input to handle gracefully.
func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}
