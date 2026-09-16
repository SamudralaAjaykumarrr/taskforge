// Phase 13 correction pass (post-ADR-0009 independent review, finding
// H1): pickClaimCandidateQuery's per-queue LATERAL used to collapse a
// queue's fresh and reclaim candidates into a single winner (by priority
// DESC, eligible_at ASC) BEFORE capacity-eligibility was evaluated. If
// that collapsed winner was fresh-shaped and the queue had zero free
// slots, the ENTIRE queue was filtered out of the roster for that pick --
// even when a different, lower-ranked candidate in the same queue was a
// reclaim (expired-lease RUNNING), which ADR-0009 defines as
// unconditionally capacity-eligible ("it already holds its slot"). The
// reclaim candidate was never surfaced, never reclaimed, and since only a
// claim/reclaim advances attempt_count, it could never reach max_attempts
// either -- permanently stranding both the job and its slot, a TF-INV-004
// violation.
//
// Since priority is not settable via any public API (internal/job.NewParams
// has no Priority field -- see job.go's own doc comment), every job's
// priority is the schema default (0), so the only tie-break is
// eligible_at ASC -- meaning any later-submitted fresh job whose
// eligible_at (via ScheduledAt) sorts ahead of an already-reclaim-eligible
// job in the same full-capacity queue was sufficient to trigger this, not
// a contrived edge case.
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// newJobParamsScheduled is newJobParamsQueueN plus an explicit
// ScheduledAt, so a fresh candidate's eligible_at can be placed ahead of
// an already-reclaim-eligible candidate's -- the exact condition that
// used to collapse the LATERAL onto the wrong branch.
func newJobParamsScheduled(jobType, queueName string, maxAttempts int, scheduledAt time.Time) job.NewParams {
	p := newJobParamsQueueN(jobType, queueName, maxAttempts)
	p.ScheduledAt = &scheduledAt
	return p
}

// TestClaimSelection_H1_ReclaimNotStarvedByHigherRankedFreshCandidateAtCapacity
// is the direct regression test for finding H1. It fails against the
// pre-fix pickClaimCandidateQuery (the queue is silently excluded from
// the roster and Claim reports "nothing eligible" even though the
// reclaim-eligible job is both pending and capacity-eligible) and passes
// once capacity-eligibility is evaluated per branch, before the
// per-queue winner is collapsed.
func TestClaimSelection_H1_ReclaimNotStarvedByHigherRankedFreshCandidateAtCapacity(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const queueName = "starve"
	provisionQueueSlots(t, db, queueName, 1)

	// Job A: claims the queue's only slot, then its lease expires with
	// attempt budget remaining -- an unconditionally capacity-eligible
	// reclaim candidate (ADR-0009 "Reclaim/slot-ownership semantics").
	stuck, err := s.Insert(ctx, newJobParamsQueueN("test.p13.h1.reclaim", queueName, 5))
	require.NoError(t, err)
	claimedStuck, ok, err := s.Claim(ctx, "worker-doomed")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stuck.ID, claimedStuck.ID)
	forceExpireLease(t, db, stuck.ID)

	// Job B: a FRESH candidate in the SAME queue, backdated via
	// ScheduledAt so its eligible_at sorts strictly ahead of job A's
	// (priority is tied at 0 for both, per internal/job.NewParams having
	// no Priority field at all) -- the exact condition that used to make
	// the LATERAL's single per-queue winner be the capacity-ineligible
	// fresh candidate, hiding the capacity-eligible reclaim candidate
	// behind it.
	fresh, err := s.Insert(ctx, newJobParamsScheduled(
		"test.p13.h1.fresh", queueName, 5, time.Now().Add(-1*time.Hour)))
	require.NoError(t, err)

	// Sanity: confirm the ranking this test depends on -- B's eligible_at
	// really is earlier than A's, so B is the LATERAL's per-queue winner
	// by (priority DESC, eligible_at ASC) if capacity is not considered
	// first.
	var aEligible, bEligible time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT eligible_at FROM jobs WHERE id = $1`, stuck.ID).Scan(&aEligible))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT eligible_at FROM jobs WHERE id = $1`, fresh.ID).Scan(&bEligible))
	require.True(t, bEligible.Before(aEligible), "test setup: fresh candidate must outrank the reclaim candidate on (priority, eligible_at) alone")

	// Prove the reclaim candidate is genuinely PENDING (state=RUNNING,
	// lease_expires_at < now(), the two-branch definition
	// docs/adr/0009-phase-13-concurrency-and-fairness.md's "TF-INV-019:
	// exact definition" uses verbatim) and CAPACITY-ELIGIBLE (reclaim is
	// unconditional per that same section) BEFORE calling Claim, so a
	// failure below is unambiguously about admission selection, not
	// about whether the fixture is well-formed.
	var state string
	var leaseExpired bool
	var attemptCount, maxAttempts int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state, lease_expires_at < now(), attempt_count, max_attempts FROM jobs WHERE id = $1`,
		stuck.ID).Scan(&state, &leaseExpired, &attemptCount, &maxAttempts))
	require.Equal(t, "RUNNING", state)
	require.True(t, leaseExpired, "job A's lease must be expired -- pending, per the reclaim branch's own definition")
	require.Less(t, attemptCount, maxAttempts, "job A must still have retry budget -- pending, not sweep-eligible")
	require.Equal(t, 1, heldSlotCount(t, db, queueName), "the queue's one slot is legitimately held by job A throughout -- job A's own reclaim needs no free slot (unconditional capacity-eligibility)")

	// The fix under test: this queue must not be silently excluded from
	// the roster. Job A (the reclaim) is the only candidate that can
	// actually be admitted (job B is fresh and the queue has zero free
	// slots), so it must be the one claimed.
	claimed, ok, err := s.Claim(ctx, "worker-rescuer")
	require.NoError(t, err)
	require.True(t, ok, "H1 regression: a pending, capacity-eligible reclaim candidate must not be silently excluded from the roster merely because a higher-ranked, capacity-ineligible fresh candidate shares its queue")
	require.Equal(t, stuck.ID, claimed.ID, "the reclaim-eligible job must be the one served -- the fresh candidate remains correctly blocked (zero free slots)")
	require.Equal(t, int64(2), claimed.LeaseGeneration, "a genuine reclaim, not a fresh admission")

	// Concurrency exactness (requirement 5): still never more than the
	// configured limit concurrently RUNNING, and the reclaim retained
	// (never re-acquired) its original slot -- Option A, unaffected by
	// this fix.
	require.Equal(t, 1, runningCount(t, db, queueName))
	require.Equal(t, 1, heldSlotCount(t, db, queueName))
	assertPerRowSlotConsistency(t, db, queueName)

	// Job B (still fresh, still QUEUED, still correctly blocked by the
	// exhausted capacity) must remain un-admitted.
	var freshState string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = $1`, fresh.ID).Scan(&freshState))
	require.Equal(t, "QUEUED", freshState, "the fresh candidate must remain correctly blocked by the exhausted capacity -- this fix must not over-admit")
}

// TestClaimSelection_H1_ConcurrentContention_NeverExceedsLimitAndNeverStrandsReclaim
// is H1's concurrent-stress complement: many workers race the same
// fresh-outranks-reclaim-at-capacity setup. Proves both that the
// reclaim-eligible job is (eventually, within the run) served, and that
// the concurrency cap is never exceeded while this contention is live --
// requirement 5, under real concurrency rather than a single serialized
// call.
func TestClaimSelection_H1_ConcurrentContention_NeverExceedsLimitAndNeverStrandsReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const queueName = "starve-concurrent"
	const capacity = 3
	provisionQueueSlots(t, db, queueName, capacity)

	// Insert and claim all `capacity` distinct jobs FIRST, while none is
	// yet expired, THEN force-expire them together -- claiming them one
	// at a time interleaved with expiring each immediately (the naive
	// order) would make the already-expired job from iteration i keep
	// winning the pick step's tie-break for iteration i+1 too (its
	// eligible_at, fixed at its own earlier insert time, always sorts
	// ahead of a not-yet-inserted job's), so only one distinct job would
	// ever actually get claimed. Mirrors
	// TestSlotTable_SF051_MixedFreshAndReclaimLoad_NeverExceedsLimit's
	// own documented fix for the identical pitfall.
	stuckIDs := make(map[string]bool, capacity)
	stuckJobIDs := make([]string, capacity)
	for i := 0; i < capacity; i++ {
		stuck, err := s.Insert(ctx, newJobParamsQueueN("test.p13.h1.reclaim.concurrent", queueName, 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("worker-doomed-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, stuck.ID, claimed.ID)
		stuckIDs[stuck.ID.String()] = true
		stuckJobIDs[i] = stuck.ID.String()
	}
	for _, id := range stuckJobIDs {
		forceExpireLease(t, db, uuid.MustParse(id))
	}
	require.Equal(t, capacity, heldSlotCount(t, db, queueName))

	// A wave of fresh candidates, every one backdated ahead of the
	// reclaim-eligible jobs, so every LATERAL winner (pre-fix) would have
	// been fresh-shaped and capacity-ineligible.
	const numFresh = 15
	for i := 0; i < numFresh; i++ {
		_, err := s.Insert(ctx, newJobParamsScheduled(
			"test.p13.h1.fresh.concurrent", queueName, 5, time.Now().Add(-2*time.Hour)))
		require.NoError(t, err)
	}

	const numWorkers = 20
	start := make(chan struct{})
	claimedCh := make(chan string, numWorkers)
	errsCh := make(chan error, numWorkers)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for w := 0; w < numWorkers; w++ {
			<-start
			j, ok, err := s.Claim(ctx, fmt.Sprintf("worker-concurrent-%d", w))
			if err != nil {
				errsCh <- err
				continue
			}
			if ok {
				claimedCh <- j.ID.String()
			}
		}
	}()
	close(start)
	<-done
	close(claimedCh)
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}

	claimedReclaims := 0
	for id := range claimedCh {
		if stuckIDs[id] {
			claimedReclaims++
		}
	}
	require.Equal(t, capacity, claimedReclaims, "every reclaim-eligible job must be served exactly once -- none may be silently excluded by a competing fresh candidate at capacity")
	require.LessOrEqual(t, runningCount(t, db, queueName), capacity, "concurrency cap must never be exceeded")
	require.Equal(t, capacity, heldSlotCount(t, db, queueName))
	assertPerRowSlotConsistency(t, db, queueName)
}

// TestClaimSelection_H1_TFINV019HoldsAcrossQueuesWithMixedFreshReclaimContention
// is requirement 4: TF-INV-019's E-1 bound (docs/adr/0009's proof, mirrored
// by TestFairness_TFINV019_NoOtherQueueServedTwiceBetweenTurns) must still
// hold once some roster queues exhibit the fresh-outranks-reclaim-at-capacity
// condition this fix addresses, not just the simple all-fresh case that
// test already covers.
func TestClaimSelection_H1_TFINV019HoldsAcrossQueuesWithMixedFreshReclaimContention(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numQueues = 4
	const backlogPerQueue = 25
	queueNames := make([]string, numQueues)

	for i := 0; i < numQueues; i++ {
		q := fmt.Sprintf("mixed-%d", i)
		queueNames[i] = q
		provisionQueueSlots(t, db, q, 1)

		// Every queue starts with one job claimed (fills its one slot),
		// then force-expired -- a reclaim-eligible candidate outranked by
		// a backdated fresh candidate in the SAME queue, exactly H1's
		// condition, reproduced on every roster queue at once. maxAttempts
		// is deliberately large (not the usual 5): this test repeatedly
		// reclaims and immediately re-expires the same job to keep every
		// queue continuously pending for the whole run (matching
		// TestFairness_TFINV019_NoOtherQueueServedTwiceBetweenTurns's own
		// "continuously backlogged" methodology) -- a small max_attempts
		// would let the Lazy Dead-Letter Sweep correctly exhaust and
		// remove it partway through (TF-INV-006, unrelated to H1), which
		// would just make the queue legitimately empty rather than
		// exercising the fairness property under test.
		const highMaxAttempts = 10_000
		stuck, err := s.Insert(ctx, newJobParamsQueueN("test.p13.h1.tfinv019.reclaim", q, highMaxAttempts))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("w-doomed-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, stuck.ID, claimed.ID)
		forceExpireLease(t, db, stuck.ID)

		_, err = s.Insert(ctx, newJobParamsScheduled(
			"test.p13.h1.tfinv019.fresh", q, highMaxAttempts, time.Now().Add(-1*time.Hour)))
		require.NoError(t, err)
	}

	// Every claim from here on must be a reclaim (each queue's slot stays
	// permanently held by whichever job currently occupies it -- the
	// reclaimed job never releases it, and the fresh candidate can never
	// be admitted at capacity 1). Force-expire each newly reclaimed job's
	// lease immediately so the SAME queue remains continuously pending
	// and capacity-eligible for the whole run, matching
	// TestFairness_TFINV019_NoOtherQueueServedTwiceBetweenTurns's own
	// "continuously backlogged" methodology.
	const totalClaims = numQueues * backlogPerQueue
	served := make([]string, 0, totalClaims)
	for i := 0; i < totalClaims; i++ {
		j, ok, err := s.Claim(ctx, fmt.Sprintf("w-mixed-%d", i))
		require.NoError(t, err)
		require.True(t, ok, "claim %d: every queue must remain servable via its reclaim candidate despite the competing fresh candidate", i)
		served = append(served, j.QueueName)
		forceExpireLease(t, db, j.ID)
	}

	lastSeenAt := map[string]int{}
	seenQueues := map[string]bool{}
	for i, q := range served {
		seenQueues[q] = true
		if prev, ok := lastSeenAt[q]; ok {
			gap := served[prev+1 : i]
			counts := map[string]int{}
			for _, other := range gap {
				counts[other]++
				require.LessOrEqual(t, counts[other], 1,
					"TF-INV-019 violated: queue %q was served twice between two consecutive turns of queue %q (positions %d..%d)",
					other, q, prev, i)
			}
			require.LessOrEqual(t, len(gap), numQueues-1,
				"queue %q waited behind more than E-1=%d other queues' service opportunities", q, numQueues-1)
		}
		lastSeenAt[q] = i
	}
	require.Len(t, seenQueues, numQueues, "every queue must actually appear in the served sequence -- none may be silently excluded from the roster")

	for _, q := range queueNames {
		require.Equal(t, 1, runningCount(t, db, q), "concurrency cap must remain exact for every queue throughout")
		require.Equal(t, 1, heldSlotCount(t, db, q))
		assertPerRowSlotConsistency(t, db, q)
	}
}
