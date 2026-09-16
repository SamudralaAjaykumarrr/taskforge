// Phase 13 (ADR-0009, checkpoint 4): last_claimed_at fairness proofs
// (SF-054 through SF-057) and TF-INV-019's E-1 bound, against a real
// PostgreSQL instance. Adversarial hot-queue/sparse-queue scenarios per
// the checkpoint's own instructions.
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// ---------------------------------------------------------------------
// TF-INV-019 / SF-054: a flooded queue cannot starve a continuously
// pending, capacity-eligible sparse queue beyond E-1 other queues'
// service opportunities. Proved by direct trace of the served-queue
// order across many continuously-backlogged queues, not inferred from
// wall-clock wait.
// ---------------------------------------------------------------------

func TestFairness_TFINV019_NoOtherQueueServedTwiceBetweenTurns(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numQueues = 5
	const backlogPerQueue = 60
	const totalClaims = numQueues * backlogPerQueue

	queueNames := make([]string, numQueues)
	for i := 0; i < numQueues; i++ {
		q := fmt.Sprintf("hot-%d", i)
		queueNames[i] = q
		for j := 0; j < backlogPerQueue; j++ {
			_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.hot", q, 5))
			require.NoError(t, err)
		}
	}

	served := make([]string, 0, totalClaims)
	for i := 0; i < totalClaims; i++ {
		j, ok, err := s.Claim(ctx, fmt.Sprintf("fair-worker-%d", i))
		require.NoError(t, err)
		require.True(t, ok, "claim %d: every queue is continuously backlogged for the whole run", i)
		served = append(served, j.QueueName)
	}

	// TF-INV-019's E-1 proof: for every queue, look at each gap between
	// two consecutive appearances in the served sequence; no OTHER queue
	// may appear more than once within that gap (E = numQueues here,
	// since every queue is continuously pending and capacity-eligible for
	// the whole run -- the bound is E-1 = numQueues-1 OTHER service
	// opportunities, and this asserts the stronger, exact round-robin
	// property this ADR's mechanism actually provides: no other queue's
	// SECOND turn can happen before this queue's next one).
	lastSeenAt := map[string]int{}
	for i, q := range served {
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
}

// ---------------------------------------------------------------------
// SF-054 (concrete, named scenario): a sparse queue with exactly one
// pending job, entered into the roster while a hot queue has already
// advanced its own last_claimed_at, must be served ahead of the hot
// queue -- it is strictly the most-stale roster member the instant it
// becomes pending.
// ---------------------------------------------------------------------

func TestFairness_SF054_SparseQueueNotStarvedByHotQueue(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf054", "hot", 5))
		require.NoError(t, err)
	}
	// Advance "hot"'s last_claimed_at ahead of "sparse"'s eventual
	// (still-default -infinity) value.
	j, ok, err := s.Claim(ctx, "w-hot-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "hot", j.QueueName)

	_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf054", "sparse", 5))
	require.NoError(t, err)

	next, ok, err := s.Claim(ctx, "w-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "sparse", next.QueueName,
		"a queue that has never been served (last_claimed_at = -infinity) must be served ahead of a queue already advanced by a real service event")
}

// ---------------------------------------------------------------------
// SF-055: a queue that temporarily loses and regains capacity
// eligibility does not have its last_claimed_at position disturbed while
// ineligible, and other queues' service during that window does not
// permanently disadvantage it once it becomes eligible again.
// ---------------------------------------------------------------------

func TestFairness_SF055_TemporaryCapacityIneligibility_PositionPreserved(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "throttled", 1)

	holder, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf055", "throttled", 5))
	require.NoError(t, err)
	claimedHolder, ok, err := s.Claim(ctx, "w-holder")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, holder.ID, claimedHolder.ID)

	var lastClaimedAfterFirst time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_claimed_at FROM queue_state WHERE queue_name = $1`, "throttled").Scan(&lastClaimedAfterFirst))

	// A second job queues up behind the held slot -- "throttled" is
	// pending but NOT capacity-eligible (zero free slots) for as long as
	// the holder stays RUNNING.
	_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf055", "throttled", 5))
	require.NoError(t, err)

	// Meanwhile an unrelated queue is serviced repeatedly.
	for i := 0; i < 10; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf055", "other", 5))
		require.NoError(t, err)
	}
	for i := 0; i < 10; i++ {
		j, ok, err := s.Claim(ctx, fmt.Sprintf("w-other-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "other", j.QueueName, "'throttled' must not be claimable while ineligible")
	}

	// "throttled"'s last_claimed_at must be exactly unchanged -- only a
	// real service event ever writes it.
	var lastClaimedNow time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_claimed_at FROM queue_state WHERE queue_name = $1`, "throttled").Scan(&lastClaimedNow))
	require.True(t, lastClaimedAfterFirst.Equal(lastClaimedNow),
		"an ineligible queue's last_claimed_at must not change while nothing serves it")

	// Free the slot -- "throttled" becomes eligible again, and must be
	// served next despite "other"'s ten intervening services.
	_, err = s.CompleteSuccess(ctx, claimedHolder.ID, "w-holder", claimedHolder.LeaseGeneration, nil)
	require.NoError(t, err)

	next, ok, err := s.Claim(ctx, "w-final")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "throttled", next.QueueName,
		"once eligible again, the queue's stale last_claimed_at must win over a queue serviced many times in between")
}

// ---------------------------------------------------------------------
// SF-056: a newly-configured or never-before-served queue is served on
// its first opportunity without waiting behind any existing queue's
// backlog.
// ---------------------------------------------------------------------

func TestFairness_SF056_NewlyEligibleQueue_ServedFirstDespiteExistingBacklog(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	for i := 0; i < 200; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf056", "established", 5))
		require.NoError(t, err)
	}
	// Give "established" a real (non -infinity) last_claimed_at, far in
	// the past relative to nothing -- just a genuine service event.
	j, ok, err := s.Claim(ctx, "w-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "established", j.QueueName)

	_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf056", "brand-new", 5))
	require.NoError(t, err)

	next, ok, err := s.Claim(ctx, "w-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "brand-new", next.QueueName,
		"a queue with no prior last_claimed_at defaults to -infinity and is served on its very first opportunity")
}

// ---------------------------------------------------------------------
// SF-057: reclaim participates in last_claimed_at accounting identically
// to a fresh claim.
// ---------------------------------------------------------------------

func TestFairness_SF057_ReclaimAdvancesLastClaimedAtIdenticallyToFreshClaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf057", "reclaim-fair", 5))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "w-crash")
	require.NoError(t, err)
	require.True(t, ok)

	var afterFreshClaim time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_claimed_at FROM queue_state WHERE queue_name = $1`, "reclaim-fair").Scan(&afterFreshClaim))
	require.False(t, afterFreshClaim.IsZero())

	// Advance a different queue's last_claimed_at so it is now "newer"
	// than reclaim-fair's.
	_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.fairness.sf057", "other", 5))
	require.NoError(t, err)
	other, ok, err := s.Claim(ctx, "w-other")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "other", other.QueueName)

	forceExpireLease(t, db, created.ID)

	reclaimed, ok, err := s.Claim(ctx, "w-reclaimer")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, reclaimed.ID, "the reclaim must be the only eligible candidate at this point")

	var afterReclaim time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_claimed_at FROM queue_state WHERE queue_name = $1`, "reclaim-fair").Scan(&afterReclaim))
	require.True(t, afterReclaim.After(afterFreshClaim),
		"a reclaim must advance its queue's last_claimed_at exactly like a fresh claim, not leave it at the original claim's timestamp")
}
