// Retry storm and max-attempt exhaustion campaigns (Phase 9 adversarial
// campaign items 7, 8, docs/roadmap.md "Retry Storm"): many jobs failing
// retryably under multiple concurrent workers, verifying max_attempts is
// never exceeded, attempt numbers stay monotonic, backoff eligibility is
// enforced, exhausted jobs dead-letter, and no duplicate authoritative
// transition occurs under contention.
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestChaos_RetryStorm_SeededManyJobsUnderContention is campaign items 7
// and 8: a large pool of jobs, each with a seeded-random max_attempts and
// a seeded-random number of retryable failures before eventual success or
// exhaustion, driven through repeated rounds of concurrent claim +
// resolve by many workers at once (no round is allowed to "get ahead" of
// backoff eligibility -- each round fast-forwards eligible_at explicitly,
// never sleeps). Every job must converge to exactly the terminal state
// its own script implies, with attempt_count never exceeding max_attempts
// at any point along the way.
func TestChaos_RetryStorm_SeededManyJobsUnderContention(t *testing.T) {
	for _, seed := range []int64{51, 52} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numJobs = 100
			const numWorkersPerRound = 15
			const maxRounds = 40

			type script struct {
				jobID          string
				maxAttempts    int
				totalFailures  int // the ORIGINAL scripted failure count -- immutable, used only for the final-outcome prediction below
				failuresBudget int // mutable countdown consumed round by round
			}
			scripts := make(map[string]*script, numJobs)

			for i := 0; i < numJobs; i++ {
				maxAttempts := 2 + rng.Intn(5) // 2..6
				// Deliberately allow failuresBudget to sometimes reach or
				// exceed maxAttempts, so a fraction of jobs are scripted to
				// exhaust their retry budget and dead-letter (item 8): if
				// totalFailures >= maxAttempts, every one of the job's
				// maxAttempts attempts is a scripted failure and the store
				// itself dead-letters on the last one (attempt_count >=
				// max_attempts) -- the exact same CASE decision
				// completeRetryableOutcome makes in production, not a
				// prediction this test invents independently.
				failuresBudget := rng.Intn(maxAttempts + 2)
				created, err := s.Insert(ctx, newChaosParams("chaos.retrystorm", maxAttempts))
				require.NoError(t, err)
				scripts[created.ID.String()] = &script{
					jobID: created.ID.String(), maxAttempts: maxAttempts,
					totalFailures: failuresBudget, failuresBudget: failuresBudget,
				}
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
							j, ok, err := s.Claim(ctx, fmt.Sprintf("storm-r%d-w%03d-s%d", round, workerID, seed))
							if err != nil {
								errsCh <- err
								return
							}
							if !ok {
								return
							}
							atomic.AddInt64(&claimedThisRound, 1)

							sc := scripts[j.ID.String()]
							owner := *j.LeaseOwner
							if sc.failuresBudget > 0 {
								sc.failuresBudget--
								_, err := s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: retry storm", 0)
								require.NoError(t, err)
							} else {
								_, err := s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
								require.NoError(t, err)
							}
						}
					}()
				}

				close(start)
				wg.Wait()
				close(errsCh)
				for err := range errsCh {
					require.NoError(t, err)
				}

				_, ffErr := chaos.ForceAllEligibleNow(ctx, db, "RETRY_WAIT")
				require.NoError(t, ffErr)

				if claimedThisRound == 0 {
					break
				}
			}
			require.Less(t, round, maxRounds, "retry storm did not converge within the round budget")

			for _, sc := range scripts {
				final, err := s.GetByID(ctx, mustParseUUID(t, sc.jobID))
				require.NoError(t, err)
				require.True(t, final.IsTerminal(), "job %s did not reach a terminal state", sc.jobID)
				require.LessOrEqual(t, final.AttemptCount, sc.maxAttempts, "TF-INV-006: attempt_count must never exceed max_attempts")

				if sc.totalFailures >= sc.maxAttempts {
					// Every one of this job's maxAttempts attempts was
					// scripted as a failure -- the store's own CASE
					// decision dead-letters on the last one.
					require.Equal(t, jobstate.DeadLettered, final.State, "job %s should have exhausted its retry budget", sc.jobID)
					require.Equal(t, sc.maxAttempts, final.AttemptCount)
				} else {
					require.Equal(t, jobstate.Succeeded, final.State, "job %s should have eventually succeeded", sc.jobID)
					require.Equal(t, sc.totalFailures+1, final.AttemptCount)
				}
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}
