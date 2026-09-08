// Worker-crash and lease/fencing chaos campaigns (Phase 9 adversarial
// campaign items 1-5, docs/roadmap.md "Worker Crash Campaigns" and
// "Lease/Fencing Chaos"): crash point varied around claim, execution,
// pre-completion, and heartbeat; a reclaimed job's stale first owner must
// never be able to authoritatively complete it (TF-INV-003, TF-INV-014);
// a heartbeat racing a reclaim must resolve deterministically
// (TF-INV-002, TF-INV-015).
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
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// crashPoint enumerates where, relative to claim/execution/completion, a
// worker "disappears" -- adversarial campaign items 1 ("immediately after
// claim"), 2 ("during execution"), and item covering "after external
// logical effect but before completion" (F3, ADR-0003).
type crashPoint int

const (
	crashAfterClaim crashPoint = iota
	crashDuringExecution
	crashAfterSideEffectBeforeCompletion
	crashDuringRetryableFailureBeforeRetryCommit
)

// TestChaos_WorkerCrashCampaign_SeededVariedCrashPoints is campaign items
// 1, 2, 3, and 4: for each of several fixed seeds, a batch of jobs is each
// claimed and then "crashed" at a seeded-random point before ever
// reporting an outcome; every crashed job's lease is then force-expired
// (item 3: "worker pause causes lease expiration") and reclaimed by a
// second wave; the FIRST wave's original credentials must never be able
// to authoritatively complete the job afterward (item 4: "stale worker
// attempts completion after reclaim"), and every job must eventually
// reach a terminal state with all durable invariants intact.
func TestChaos_WorkerCrashCampaign_SeededVariedCrashPoints(t *testing.T) {
	for _, seed := range []int64{101, 202, 303} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numJobs = 40
			type crashed struct {
				jobID       string
				staleOwner  string
				staleGen    int64
				point       crashPoint
				sideEffects *int
			}
			var batch []crashed

			for i := 0; i < numJobs; i++ {
				created, err := s.Insert(ctx, newChaosParams("chaos.crash", 5))
				require.NoError(t, err)

				owner := fmt.Sprintf("crash-worker-%03d-%d", i, seed)
				claimed, ok, err := s.Claim(ctx, owner)
				require.NoError(t, err)
				require.True(t, ok)

				point := crashPoint(rng.Intn(4))
				sideEffects := 0
				if point == crashAfterSideEffectBeforeCompletion {
					// Simulate a non-idempotent external side effect
					// performed before the worker vanished -- the
					// documented at-least-once duplication risk (F3,
					// ADR-0003), made concrete: this counter increments
					// again when the reclaiming worker retries.
					sideEffects++
				}
				if point == crashDuringRetryableFailureBeforeRetryCommit {
					// The handler decided the outcome (retryable failure)
					// but the worker vanishes before CompleteRetryableFailure
					// ever reaches PostgreSQL -- indistinguishable, from the
					// store's point of view, from any other mid-attempt
					// crash: the lease simply expires.
				}
				batch = append(batch, crashed{
					jobID: created.ID.String(), staleOwner: owner, staleGen: claimed.LeaseGeneration,
					point: point, sideEffects: &sideEffects,
				})
			}

			// Every crashed job's lease expires at once (item 3).
			n, err := chaos.ForceExpireAllRunningLeases(ctx, db)
			require.NoError(t, err)
			require.EqualValues(t, numJobs, n)

			// Second wave reclaims and completes every job concurrently.
			// Claim() returns WHICHEVER eligible job is next in the shared
			// pool, not a caller-chosen one -- so each goroutine resolves
			// its own claimed job's identity via byJobID AFTER claiming,
			// rather than assuming a 1:1 correspondence with the batch
			// entry it happened to be spawned for.
			byJobID := make(map[string]*crashed, len(batch))
			for i := range batch {
				byJobID[batch[i].jobID] = &batch[i]
			}
			var wg sync.WaitGroup
			for i := 0; i < numJobs; i++ {
				wg.Add(1)
				workerIdx := i
				go func() {
					defer wg.Done()
					reclaimOwner := fmt.Sprintf("reclaimer-%d-%d", seed, workerIdx)
					reclaimed, ok, err := s.Claim(ctx, reclaimOwner)
					require.NoError(t, err)
					require.True(t, ok)

					c := byJobID[reclaimed.ID.String()]
					require.NotNil(t, c, "reclaimed job %s was not part of this campaign's batch", reclaimed.ID)
					require.Equal(t, c.staleGen+1, reclaimed.LeaseGeneration, "TF-INV-002: reclaim must strictly advance the generation")

					if c.point == crashAfterSideEffectBeforeCompletion {
						*c.sideEffects++ // the reclaiming attempt re-runs the same non-idempotent effect
					}

					_, err = s.CompleteSuccess(ctx, reclaimed.ID, reclaimOwner, reclaimed.LeaseGeneration, nil)
					require.NoError(t, err)
				}()
			}
			wg.Wait()

			// Item 4: every stale first-wave owner's completion attempt,
			// arriving arbitrarily late after reclaim, must be rejected --
			// TF-INV-003/TF-INV-014, regardless of which crash point
			// produced it.
			for _, c := range batch {
				id := mustParseUUID(t, c.jobID)
				_, err := s.CompleteSuccess(ctx, id, c.staleOwner, c.staleGen, nil)
				require.ErrorIs(t, err, store.ErrStaleTransition,
					"stale generation %d (crash point %v) must never authoritatively complete job %s after reclaim", c.staleGen, c.point, c.jobID)

				got, err := s.GetByID(ctx, id)
				require.NoError(t, err)
				require.Equal(t, jobstate.Succeeded, got.State)
				require.Equal(t, c.staleGen+1, got.LeaseGeneration, "the job's authoritative generation must remain the reclaiming worker's, untouched by the stale call")

				if c.point == crashAfterSideEffectBeforeCompletion {
					require.Equal(t, 2, *c.sideEffects,
						"documents ADR-0003: a non-idempotent side effect performed before the crash runs again on reclaim -- at-least-once, not exactly-once")
				}
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_HeartbeatRacesReclaim_Seeded is campaign item 5: for each
// seed, a batch of long-running (Gated) jobs are claimed by a first-wave
// worker with a short execution_timeout_seconds (so heartbeats fire on a
// fast, real cadence). For each job, a seeded coin flip decides whether
// the job's owner is given a chance to heartbeat successfully before any
// reclaim attempt (heartbeat wins) or whether its lease is force-expired
// and reclaimed first (reclaim wins) -- both orderings must resolve
// deterministically per TF-INV-002/TF-INV-015: a job can never end up
// with two simultaneously "valid" owners, and a heartbeat can never
// resurrect a lease reclaim has already superseded.
func TestChaos_HeartbeatRacesReclaim_Seeded(t *testing.T) {
	for _, seed := range []int64{11, 22, 33} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numJobs = 15
			registry := handler.NewRegistry()

			type inFlight struct {
				jobID        string
				jobType      string
				started      chan struct{}
				proceed      chan struct{}
				done         chan struct{}
				heartbeatWon bool
			}
			var flights []*inFlight

			for i := 0; i < numJobs; i++ {
				jobType := fmt.Sprintf("chaos.hbrace.%d.%d", seed, i)
				created, err := s.Insert(ctx, job.NewParams{
					JobType:                 jobType,
					Payload:                 []byte(`{}`),
					MaxAttempts:             5,
					ExecutionTimeoutSeconds: 1, // ~333ms heartbeat interval
				})
				require.NoError(t, err)

				f := &inFlight{
					jobID: created.ID.String(), jobType: jobType,
					started: make(chan struct{}, 1), proceed: make(chan struct{}), done: make(chan struct{}),
					heartbeatWon: rng.Bool(0.5),
				}
				registry.Register(jobType, testdoubles.Gated{Started: f.started, Proceed: f.proceed})
				flights = append(flights, f)
			}

			w1 := newWorker("hbrace-w1", s, registry)
			var wg sync.WaitGroup
			for _, f := range flights {
				wg.Add(1)
				go func(f *inFlight) {
					defer wg.Done()
					defer close(f.done)
					_, _ = w1.RunOnce(ctx)
				}(f)
			}
			for _, f := range flights {
				<-f.started
			}

			var reclaimWg sync.WaitGroup
			for _, f := range flights {
				f := f
				if f.heartbeatWon {
					// Let the natural heartbeat ticker do its job -- no
					// force-expiry, so the very first heartbeat cycle
					// renews the lease well before this test's own
					// timeout. The job stays legitimately owned by w1.
					continue
				}
				// Reclaim wins: force-expire immediately (before any
				// heartbeat can land) and reclaim from a second worker.
				reclaimWg.Add(1)
				go func() {
					defer reclaimWg.Done()
					require.NoError(t, chaos.ForceExpireLease(ctx, db, mustParseUUID(t, f.jobID)))
					reclaimed, ok, err := s.Claim(ctx, "hbrace-reclaimer-"+f.jobID)
					require.NoError(t, err)
					require.True(t, ok)
					require.Equal(t, int64(2), reclaimed.LeaseGeneration)
					_, err = s.CompleteSuccess(ctx, reclaimed.ID, "hbrace-reclaimer-"+f.jobID, reclaimed.LeaseGeneration, nil)
					require.NoError(t, err)
				}()
			}
			reclaimWg.Wait()

			// Give heartbeat-winning jobs time for at least one real
			// heartbeat tick, then release every handler so w1.RunOnce can
			// finish (successfully for the ones it still legitimately
			// owns; its completion call will be rejected as stale for the
			// ones that were reclaimed instead).
			time.Sleep(500 * time.Millisecond)
			for _, f := range flights {
				close(f.proceed)
			}
			for _, f := range flights {
				<-f.done
			}
			wg.Wait()

			for _, f := range flights {
				var state string
				var leaseGen int64
				row := db.QueryRowContext(ctx, `SELECT state, lease_generation FROM jobs WHERE id = $1`, f.jobID)
				require.NoError(t, row.Scan(&state, &leaseGen))
				require.Equal(t, "SUCCEEDED", state, "job %s must reach SUCCEEDED via exactly one authoritative path", f.jobID)
				if f.heartbeatWon {
					require.Equal(t, int64(1), leaseGen, "job %s: heartbeat should have kept generation 1 authoritative", f.jobID)
				} else {
					require.Equal(t, int64(2), leaseGen, "job %s: reclaim should have made generation 2 authoritative", f.jobID)
				}
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}
