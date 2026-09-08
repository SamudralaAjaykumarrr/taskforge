// Idempotency-under-failure campaigns (Phase 9 adversarial campaign items
// 9, 10, docs/roadmap.md "Idempotency Under Failure"): a response-loss
// duplicate submission racing concurrently with the original, and a
// duplicate logical effect after reclaim mitigated by a job_id-keyed
// idempotency token -- combining Phase 4's guarantees (TF-INV-008,
// TF-INV-016) with Phase 2's reclaim mechanism under chaos, exactly as
// SF-004/SF-005 individually prove, now driven by seeded concurrency.
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestChaos_ResponseLossDuplicateSubmission_SeededConcurrentRetries is
// campaign item 9: many distinct logical submissions, each retried a
// seeded-random number of times concurrently (simulating a client that
// believes its request was lost and retries), racing each other -- every
// group must resolve to exactly one durable job row and one job_id,
// regardless of how many duplicate concurrent requests arrived.
func TestChaos_ResponseLossDuplicateSubmission_SeededConcurrentRetries(t *testing.T) {
	for _, seed := range []int64{9, 19} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numLogicalSubmissions = 25
			var wg sync.WaitGroup
			resultsMu := sync.Mutex{}
			results := make(map[string][]string) // key -> observed job IDs

			for i := 0; i < numLogicalSubmissions; i++ {
				key := fmt.Sprintf("chaos-idem-key-%d-%d", seed, i)
				duplicates := 2 + rng.Intn(8) // 2..9 concurrent "retries" of the same logical request

				start := make(chan struct{})
				var innerWg sync.WaitGroup
				for d := 0; d < duplicates; d++ {
					innerWg.Add(1)
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer innerWg.Done()
						<-start
						k := key
						created, _, err := s.InsertIdempotent(ctx, job.NewParams{
							JobType:                 "chaos.idempotency",
							Payload:                 []byte(`{}`),
							MaxAttempts:             3,
							ExecutionTimeoutSeconds: 30,
							IdempotencyKey:          &k,
						})
						require.NoError(t, err)
						resultsMu.Lock()
						results[key] = append(results[key], created.ID.String())
						resultsMu.Unlock()
					}()
				}
				close(start)
				innerWg.Wait()
			}
			wg.Wait()

			for key, ids := range results {
				first := ids[0]
				for _, id := range ids {
					require.Equal(t, first, id, "key %s: every concurrent duplicate submission must resolve to the same job_id (TF-INV-008)", key)
				}
			}

			var duplicateJobTypeCount int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE job_type = 'chaos.idempotency'`).Scan(&duplicateJobTypeCount))
			require.Equal(t, numLogicalSubmissions, duplicateJobTypeCount, "exactly one durable row per logical submission, no matter how many duplicates raced")

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// dedupEffect is an in-memory stand-in for an external, durable
// idempotency-token table (docs/idempotency.md's "Deduplication token
// table" pattern) keyed by job_id -- the handler-side mechanism that
// turns TaskForge's at-least-once execution into an exactly-once
// *logical* effect, per ADR-0004. Safe for concurrent use.
type dedupEffect struct {
	mu      sync.Mutex
	applied map[string]bool
}

func newDedupEffect() *dedupEffect { return &dedupEffect{applied: make(map[string]bool)} }

// Apply records jobID's effect at most once, returning whether this call
// was the one that actually recorded it (false means some earlier call --
// possibly from a different, reclaiming attempt of the same job_id --
// already had).
func (d *dedupEffect) Apply(jobID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.applied[jobID] {
		return false
	}
	d.applied[jobID] = true
	return true
}

// TestChaos_DuplicateLogicalEffectAfterReclaim_IdempotentDownstreamDedupes
// is campaign item 10: SF-004's exact crash sequence (a handler performs
// a side effect, then the worker vanishes before ever reporting an
// outcome, and the job is reclaimed and re-executed), replayed across
// many seeded jobs concurrently. The RAW side effect (a plain counter)
// runs twice per job -- proving TaskForge's documented at-least-once
// limitation is real under chaos, not just in the single hand-crafted
// SF-004 test -- while the job_id-keyed dedupEffect ends with exactly one
// recorded logical effect per job, despite the same doubled physical
// execution.
func TestChaos_DuplicateLogicalEffectAfterReclaim_IdempotentDownstreamDedupes(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	const numJobs = 30
	rawSideEffects := make(map[string]int)
	dedup := newDedupEffect()

	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newChaosParams("chaos.dupeffect", 5))
		require.NoError(t, err)
		id := created.ID.String()

		firstOwner := fmt.Sprintf("dupeffect-first-%d", i)
		claimed, ok, err := claimUntilResolved(t, ctx, s, firstOwner)
		require.NoError(t, err)
		require.True(t, ok)

		// The handler "performs its side effect" -- both the raw,
		// non-idempotent one and the job_id-keyed idempotent one -- then
		// the worker vanishes before ever calling CompleteSuccess.
		rawSideEffects[id]++
		dedup.Apply(id)

		require.NoError(t, chaos.ForceExpireLease(ctx, db, claimed.ID))

		secondOwner := fmt.Sprintf("dupeffect-second-%d", i)
		reclaimed, ok, err := s.Claim(ctx, secondOwner)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, int64(2), reclaimed.LeaseGeneration)

		// The reclaiming attempt re-runs the handler from scratch,
		// including its side effect -- job_id is stable across the
		// reclaim (ADR-0004), so the dedup table correctly recognizes
		// this as the same logical unit of work.
		rawSideEffects[id]++
		dedup.Apply(id)

		_, err = s.CompleteSuccess(ctx, reclaimed.ID, secondOwner, reclaimed.LeaseGeneration, nil)
		require.NoError(t, err)
	}

	for id, count := range rawSideEffects {
		require.Equal(t, 2, count, "job %s: the raw side effect must run twice -- documents ADR-0003's at-least-once limitation under chaos", id)
	}
	require.Len(t, dedup.applied, numJobs, "the job_id-keyed dedup table must end with exactly one entry per job, despite double physical execution")

	checkNoViolations(t, ctx, checker, -1)
}
