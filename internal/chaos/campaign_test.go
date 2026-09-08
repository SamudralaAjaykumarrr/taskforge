// The flagship Phase 9 adversarial campaign (docs/roadmap.md "Adversarial
// Campaign" and "Required invariants: All of them, simultaneously, under
// combined stress"): campaign item 19 ("multiple workers/process-like
// instances restart") plus a single seeded, property-style campaign that
// combines every fault class this package exercises individually
// elsewhere -- worker crashes, lease expiry/reclaim, retries, dead-
// lettering, cancellation, scheduling, duplicate idempotent submissions,
// and workflow dependency propagation -- against one shared database,
// with continuous (not just end-of-run) durable invariant checking.
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// TestChaos_MultipleWorkerInstancesRestart_Seeded is campaign item 19: a
// pool of real *worker.Worker instances (standing in for separate
// process instances, per SF-018/SF-029's established "fresh Store, no
// shared in-memory state" convention) is processing a shared job pool
// when the entire pool's context is cancelled mid-flight (several jobs
// still claimed and RUNNING) -- a full simulated fleet restart. A
// brand-new pool of workers, with fresh identities and a fresh
// *store.Store sharing only the database, then finishes the work. No job
// may be lost, stranded, or double-processed across the restart.
func TestChaos_MultipleWorkerInstancesRestart_Seeded(t *testing.T) {
	for _, seed := range []int64{131, 232} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			ctx := context.Background()
			rng := chaos.NewRand(seed)

			const numJobs = 50
			gen1Store := store.New(db, store.WithLogger(discardLogger()))
			registry := handler.NewRegistry()
			registry.Register("chaos.fleetrestart", testdoubles.AlwaysSucceed{})
			for i := 0; i < numJobs; i++ {
				_, err := gen1Store.Insert(ctx, newChaosParams("chaos.fleetrestart", 5))
				require.NoError(t, err)
			}

			const numWorkersGen1 = 8
			gen1Ctx, gen1Cancel := context.WithCancel(ctx)
			workersGen1 := make([]*worker.Worker, numWorkersGen1)
			for i := range workersGen1 {
				workersGen1[i] = worker.New(fmt.Sprintf("fleet-gen1-%d-%d", seed, i), gen1Store, registry, time.Millisecond, discardLogger())
			}
			var wg1 sync.WaitGroup
			for _, w := range workersGen1 {
				wg1.Add(1)
				go func(w *worker.Worker) {
					defer wg1.Done()
					_ = w.Run(gen1Ctx)
				}(w)
			}

			// Let generation 1 make some, but not necessarily complete,
			// progress -- a seeded short window standing in for
			// "the fleet was killed at some non-deterministic point."
			time.Sleep(time.Duration(5+rng.Intn(20)) * time.Millisecond)
			gen1Cancel()
			wg1.Wait()

			// Anything generation 1 left RUNNING is exactly the
			// "in-flight when the fleet died" case -- force-expire so
			// generation 2 can reclaim it, precisely mirroring
			// SF-002/SF-007's mechanism at fleet scale.
			_, err := chaos.ForceExpireAllRunningLeases(ctx, db)
			require.NoError(t, err)

			gen2Store := store.New(db, store.WithLogger(discardLogger()))
			const numWorkersGen2 = 10
			gen2Ctx, gen2Cancel := context.WithTimeout(ctx, 20*time.Second)
			defer gen2Cancel()
			workersGen2 := make([]*worker.Worker, numWorkersGen2)
			for i := range workersGen2 {
				workersGen2[i] = worker.New(fmt.Sprintf("fleet-gen2-%d-%d", seed, i), gen2Store, registry, time.Millisecond, discardLogger())
			}
			var wg2 sync.WaitGroup
			for _, w := range workersGen2 {
				wg2.Add(1)
				go func(w *worker.Worker) {
					defer wg2.Done()
					_ = w.Run(gen2Ctx)
				}(w)
			}

			require.Eventually(t, func() bool {
				var n int
				row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE job_type = 'chaos.fleetrestart' AND state = 'SUCCEEDED'`)
				require.NoError(t, row.Scan(&n))
				return n == numJobs
			}, 15*time.Second, 20*time.Millisecond, "every job must eventually succeed despite a full simulated fleet restart mid-flight")

			gen2Cancel()
			wg2.Wait()

			checker := invariant.New(db)
			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_CombinedCampaign_AllInvariantsSimultaneously_Seeded is the
// flagship Phase 9 campaign: for each of a small set of fixed seeds, a
// single mixed workload (plain jobs, scheduled jobs, duplicate idempotent
// submissions, and diamond workflows) is driven through several rounds of
// concurrent claim + a seeded-random resolution (success, retryable
// failure, permanent failure, crash/abandon, cancellation, or timeout),
// interleaved with lease expiry, backoff/schedule fast-forwarding, and a
// simulated mid-campaign fleet restart -- with the full durable invariant
// suite checked after EVERY round, not just at the end, per
// docs/roadmap.md's "continuous invariant assertion (not just
// end-of-run checks)."
func TestChaos_CombinedCampaign_AllInvariantsSimultaneously_Seeded(t *testing.T) {
	for _, seed := range []int64{9001, 9002} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			var submittedIDs []uuid.UUID

			// Plain jobs, a mix of immediate and future-scheduled.
			const numPlainJobs = 40
			for i := 0; i < numPlainJobs; i++ {
				var scheduledAt *time.Time
				if rng.Bool(0.2) {
					st := time.Now().Add(time.Duration(rng.Intn(500)) * time.Millisecond)
					scheduledAt = &st
				}
				created, err := s.Insert(ctx, job.NewParams{
					JobType: "chaos.combined.plain", Payload: []byte(`{}`),
					MaxAttempts: 2 + rng.Intn(4), ExecutionTimeoutSeconds: 5, ScheduledAt: scheduledAt,
				})
				require.NoError(t, err)
				submittedIDs = append(submittedIDs, created.ID)
			}

			// Duplicate idempotent submissions: several logical requests,
			// each submitted 2-4 times concurrently.
			const numIdemGroups = 8
			var idemWG sync.WaitGroup
			var idemMu sync.Mutex
			for i := 0; i < numIdemGroups; i++ {
				key := fmt.Sprintf("chaos-combined-key-%d-%d", seed, i)
				dupes := 2 + rng.Intn(3)
				barrier := make(chan struct{})
				var innerWG sync.WaitGroup
				for d := 0; d < dupes; d++ {
					innerWG.Add(1)
					idemWG.Add(1)
					go func() {
						defer innerWG.Done()
						defer idemWG.Done()
						<-barrier
						k := key
						created, _, err := s.InsertIdempotent(ctx, job.NewParams{
							JobType: "chaos.combined.idem", Payload: []byte(`{}`),
							MaxAttempts: 3, ExecutionTimeoutSeconds: 5, IdempotencyKey: &k,
						})
						require.NoError(t, err)
						idemMu.Lock()
						submittedIDs = append(submittedIDs, created.ID)
						idemMu.Unlock()
					}()
				}
				close(barrier)
				innerWG.Wait()
			}
			idemWG.Wait()

			// Workflows: a handful of diamond DAGs.
			const numWorkflows = 5
			for i := 0; i < numWorkflows; i++ {
				prefix := fmt.Sprintf("chaos.combined.wf.%d.%d", seed, i)
				inst, err := s.CreateWorkflow(ctx, diamondSpec(prefix))
				require.NoError(t, err)
				for _, n := range inst.Nodes {
					submittedIDs = append(submittedIDs, n.JobID)
				}
			}

			checkNoViolations(t, ctx, checker, seed) // invariants must already hold on the freshly submitted, not-yet-executed workload

			const numWorkersPerRound = 10
			const maxRounds = 60
			restartRound := 5 + rng.Intn(10) // exactly one simulated fleet restart during the campaign

			round := 0
			for ; round < maxRounds; round++ {
				if round == restartRound {
					// Simulate item 19 mid-campaign: swap in a fresh Store
					// sharing only the database, standing in for a full
					// process restart with no in-memory state carried
					// forward.
					s = store.New(db, store.WithLogger(discardLogger()))
				}

				start := make(chan struct{})
				var wg sync.WaitGroup
				var claimedThisRound int64Counter
				errsCh := make(chan error, numWorkersPerRound)

				for w := 0; w < numWorkersPerRound; w++ {
					wg.Add(1)
					workerID := w
					go func() {
						defer wg.Done()
						<-start
						for {
							j, ok, err := s.Claim(ctx, fmt.Sprintf("combined-r%d-w%03d-s%d", round, workerID, seed))
							if err != nil {
								errsCh <- err
								return
							}
							if !ok {
								return
							}
							claimedThisRound.add(1)
							resolveCombinedClaim(t, s, ctx, j, rng.Intn(6))
						}
					}()
				}

				close(start)
				wg.Wait()
				close(errsCh)
				for err := range errsCh {
					require.NoError(t, err)
				}

				// Time advances for the next round: crashed (still-RUNNING)
				// jobs' leases expire, RETRY_WAIT jobs and future-scheduled
				// jobs become eligible. The upper bound on eligible_at
				// (1 hour out) is deliberate: it fast-forwards genuine
				// backoff/schedule windows (both far under an hour here)
				// without ever touching a dependency-blocked workflow
				// node's eligible_at, which is pinned at internal/store's
				// blockedEligibleAt sentinel (year 9999) specifically so
				// nothing but real predecessor-completion propagation may
				// ever move it -- an unbounded UPDATE here would silently
				// defeat TF-INV-012's gating instead of merely simulating
				// time passing.
				_, err := chaos.ForceExpireAllRunningLeases(ctx, db)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `
					UPDATE jobs SET eligible_at = now()
					WHERE state IN ('RETRY_WAIT', 'QUEUED') AND eligible_at > now() AND eligible_at < now() + interval '1 hour'`)
				require.NoError(t, err)

				// Continuous invariant assertion -- every round, not just
				// at the end.
				checkNoViolations(t, ctx, checker, seed)

				if claimedThisRound.value() == 0 {
					break
				}
			}
			require.Less(t, round, maxRounds, "combined campaign did not converge within the round budget")

			// TF-INV-001: every job this campaign ever successfully
			// submitted must still have a durable row.
			violations, err := checker.CheckJobsExist(ctx, submittedIDs)
			require.NoError(t, err)
			require.Empty(t, violations, "seed=%d: %v", seed, violations)

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// int64Counter is a tiny mutex-guarded counter -- avoids importing
// sync/atomic purely for one local round-progress tally in the test
// above.
type int64Counter struct {
	mu sync.Mutex
	n  int64
}

func (c *int64Counter) add(delta int64) {
	c.mu.Lock()
	c.n += delta
	c.mu.Unlock()
}

func (c *int64Counter) value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// resolveCombinedClaim resolves one claimed job per a 0-5 seeded
// decision, covering every fault class the combined campaign exercises:
//
//	0 -> success
//	1 -> retryable failure (store decides RETRY_WAIT vs DEAD_LETTERED)
//	2 -> permanent failure (dead-letters immediately, may cascade-cancel
//	     workflow dependents)
//	3 -> crash: abandon, leaving the job RUNNING for next round's
//	     force-expire-then-reclaim cycle
//	4 -> cancellation: request then acknowledge
//	5 -> timeout: report CompleteTimeout directly (simulating the
//	     worker-level execution-timeout path without needing a real
//	     handler hang)
func resolveCombinedClaim(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, decision int) {
	t.Helper()
	owner := *j.LeaseOwner
	var err error
	switch decision {
	case 0:
		_, err = s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
	case 1:
		_, err = s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: combined transient", 0)
	case 2:
		_, err = s.CompleteFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: combined permanent", "PERMANENT")
	case 3:
		// crash: do nothing
		return
	case 4:
		if _, rerr := s.RequestCancellation(ctx, j.ID); rerr != nil {
			require.ErrorIs(t, rerr, store.ErrStaleTransition)
			return
		}
		_, err = s.CompleteCancelled(ctx, j.ID, owner, j.LeaseGeneration)
	case 5:
		_, err = s.CompleteTimeout(ctx, j.ID, owner, j.LeaseGeneration, 0)
	}
	require.NoError(t, err)
}
