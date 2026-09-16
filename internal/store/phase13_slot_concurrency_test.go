// Phase 13 (ADR-0009, checkpoint 3): slot-table concurrency admission and
// lifecycle proofs, against a real PostgreSQL instance. Covers SF-051
// through SF-059 from ADR-0009's "Required implementation proof
// obligations" table.
package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// provisionQueueSlots is this test file's stand-in for the
// cmd/taskforge-admin `set-queue-limit` DML (checkpoint 5): it inserts a
// queue_limits row and exactly `capacity` queue_slots rows for queueName,
// mirroring exactly what that tool will do, without depending on it --
// checkpoint 3's slot-table proofs must not depend on checkpoint 5's CLI.
func provisionQueueSlots(t *testing.T, db *sql.DB, queueName string, capacity int) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO queue_limits (id, queue_name, concurrency_limit) VALUES ($1, $2, $3)`,
		uuid.New(), queueName, capacity)
	require.NoError(t, err)
	for i := 0; i < capacity; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO queue_slots (queue_name, slot_index) VALUES ($1, $2)`, queueName, i)
		require.NoError(t, err)
	}
}

func newJobParamsQueueN(jobType, queueName string, maxAttempts int) job.NewParams {
	return job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{"k":"v"}`),
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: 30,
		QueueName:               queueName,
	}
}

// runningCount returns the current count(*) of RUNNING jobs for queueName.
func runningCount(t *testing.T, db *sql.DB, queueName string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE queue_name = $1 AND state = 'RUNNING'`, queueName).Scan(&n))
	return n
}

// heldSlotCount returns the current count of held (non-NULL
// held_by_job_id) queue_slots rows for queueName.
func heldSlotCount(t *testing.T, db *sql.DB, queueName string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM queue_slots WHERE queue_name = $1 AND held_by_job_id IS NOT NULL`, queueName).Scan(&n))
	return n
}

// assertPerRowSlotConsistency is SF-053, strengthened: every RUNNING job's
// id appears in exactly one held queue_slots row, and every held
// queue_slots row's held_by_job_id names exactly one currently-RUNNING job
// -- checked per row, not merely as the aggregate count(RUNNING) ==
// count(held slots), which cannot distinguish a correct state from two
// offsetting errors (one job wrongly holding two slots, another wrongly
// holding none).
func assertPerRowSlotConsistency(t *testing.T, db *sql.DB, queueName string) {
	t.Helper()
	ctx := context.Background()

	// Every RUNNING job in this queue holds EXACTLY one slot.
	rows, err := db.QueryContext(ctx, `
		SELECT j.id, count(qs.slot_index)
		FROM jobs j
		LEFT JOIN queue_slots qs ON qs.queue_name = j.queue_name AND qs.held_by_job_id = j.id
		WHERE j.queue_name = $1 AND j.state = 'RUNNING'
		GROUP BY j.id`, queueName)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int
		require.NoError(t, rows.Scan(&id, &n))
		require.Equal(t, 1, n, "RUNNING job %s must hold exactly one slot, held %d", id, n)
	}
	require.NoError(t, rows.Err())

	// Every held slot names exactly one currently-RUNNING job.
	rows2, err := db.QueryContext(ctx, `
		SELECT qs.slot_index, count(j.id)
		FROM queue_slots qs
		LEFT JOIN jobs j ON j.id = qs.held_by_job_id AND j.state = 'RUNNING'
		WHERE qs.queue_name = $1 AND qs.held_by_job_id IS NOT NULL
		GROUP BY qs.slot_index`, queueName)
	require.NoError(t, err)
	defer rows2.Close()
	for rows2.Next() {
		var idx, n int
		require.NoError(t, rows2.Scan(&idx, &n))
		require.Equal(t, 1, n, "held slot %d must name exactly one currently-RUNNING job", idx)
	}
	require.NoError(t, rows2.Err())
}

// ---------------------------------------------------------------------
// SF-051: exact concurrency-limit enforcement under concurrent claim
// attempts, mixed fresh/reclaim load.
// ---------------------------------------------------------------------

func TestSlotTable_SF051_ExactConcurrencyLimitUnderConcurrentClaims(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const capacity = 5
	const numJobs = 50
	const numWorkers = 50
	provisionQueueSlots(t, db, "limited", capacity)

	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.exact", "limited", 5))
		require.NoError(t, err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var claims int64
	errsCh := make(chan error, numWorkers)
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := s.Claim(ctx, fmt.Sprintf("slot-worker-%03d", workerID))
			if err != nil {
				errsCh <- err
				return
			}
			if ok {
				atomic.AddInt64(&claims, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}

	require.Equal(t, int64(capacity), claims,
		"exactly %d of %d concurrent claim attempts must succeed -- the configured capacity, never more, never fewer while capacity-eligible work remains", capacity, numWorkers)
	require.Equal(t, capacity, runningCount(t, db, "limited"), "never more than the configured limit concurrently RUNNING")
	require.Equal(t, capacity, heldSlotCount(t, db, "limited"))
	assertPerRowSlotConsistency(t, db, "limited")
}

// TestSlotTable_SF051_MixedFreshAndReclaimLoad_NeverExceedsLimit exercises
// the "lease expiry/reclaim active" combination requirement 6 of
// ADR-0009 names: some jobs are mid-reclaim (expired lease, attempt
// budget remaining, already counted against capacity) while fresh jobs
// simultaneously compete for the SAME queue's remaining capacity.
func TestSlotTable_SF051_MixedFreshAndReclaimLoad_NeverExceedsLimit(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const capacity = 4
	provisionQueueSlots(t, db, "mixed", capacity)

	// Fill capacity with DISTINCT jobs first (claim all `capacity` of them
	// before expiring any), THEN crash their "workers" together -- lease
	// force-expired, never completed. Expiring each one immediately
	// inside the same loop that inserts the next would make the
	// already-expired job dominate every later iteration's pick (its
	// eligible_at, fixed at its own insert time, is always earlier than a
	// not-yet-inserted job's), so the loop would keep reclaiming the SAME
	// one job instead of claiming `capacity` distinct ones.
	reclaimIDs := make([]uuid.UUID, capacity)
	for i := 0; i < capacity; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.mixed", "mixed", 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("crashed-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		reclaimIDs[i] = claimed.ID
	}
	for _, id := range reclaimIDs {
		forceExpireLease(t, db, id)
	}
	require.Equal(t, capacity, heldSlotCount(t, db, "mixed"), "capacity is fully committed to the about-to-be-reclaimed jobs")

	// Now add fresh jobs on the SAME queue -- they must find zero free
	// slots (every slot is legitimately held by a reclaimable job) and
	// therefore never get admitted ahead of, or alongside, the reclaims.
	const numFresh = 20
	for i := 0; i < numFresh; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.mixed", "mixed", 5))
		require.NoError(t, err)
	}

	const numWorkers = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	errsCh := make(chan error, numWorkers)
	claimedIDs := make(chan uuid.UUID, numWorkers)
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			j, ok, err := s.Claim(ctx, fmt.Sprintf("mixed-worker-%03d", workerID))
			if err != nil {
				errsCh <- err
				return
			}
			if ok {
				claimedIDs <- j.ID
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errsCh)
	close(claimedIDs)
	for err := range errsCh {
		require.NoError(t, err)
	}

	claimedSet := map[uuid.UUID]bool{}
	for id := range claimedIDs {
		claimedSet[id] = true
	}
	// Exactly the `capacity` reclaimable jobs were claimed (reclaimed) --
	// no fresh job could have been admitted, since every slot was already
	// legitimately held.
	require.Len(t, claimedSet, capacity)
	for _, id := range reclaimIDs {
		require.True(t, claimedSet[id], "job %s should have been reclaimed", id)
	}
	require.Equal(t, capacity, runningCount(t, db, "mixed"))
	require.Equal(t, capacity, heldSlotCount(t, db, "mixed"))
	assertPerRowSlotConsistency(t, db, "mixed")
}

// TestSlotTable_UnlimitedQueue_NeverGatedBySlots proves the compatibility
// path: a queue with no provisioned queue_slots rows at all admits every
// concurrently eligible job -- the slot mechanism is never consulted.
func TestSlotTable_UnlimitedQueue_NeverGatedBySlots(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 30
	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.unlimited", "default", 5))
		require.NoError(t, err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var claims int64
	errsCh := make(chan error, numJobs)
	for w := 0; w < numJobs; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := s.Claim(ctx, fmt.Sprintf("unlimited-worker-%03d", workerID))
			if err != nil {
				errsCh <- err
				return
			}
			if ok {
				atomic.AddInt64(&claims, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	require.Equal(t, int64(numJobs), claims, "an unconfigured (unlimited) queue must never be gated by the slot mechanism")
}

// ---------------------------------------------------------------------
// SF-052: reclaim never writes queue_slots.
// ---------------------------------------------------------------------

func TestSlotTable_SF052_ReclaimNeverWritesQueueSlots(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "reclaim-no-write", 3)
	created, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.reclaim_no_write", "reclaim-no-write", 5))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	var heldSlotIndex int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT slot_index FROM queue_slots WHERE queue_name = $1 AND held_by_job_id = $2`,
		"reclaim-no-write", claimed.ID).Scan(&heldSlotIndex))

	// Snapshot the ENTIRE queue_slots table for this queue before reclaim.
	type slotRow struct {
		idx   int
		held  uuid.UUID
		isNil bool
	}
	snapshot := func() []slotRow {
		rows, err := db.QueryContext(ctx, `SELECT slot_index, held_by_job_id FROM queue_slots WHERE queue_name = $1 ORDER BY slot_index`, "reclaim-no-write")
		require.NoError(t, err)
		defer rows.Close()
		var out []slotRow
		for rows.Next() {
			var idx int
			var held sql.NullString
			require.NoError(t, rows.Scan(&idx, &held))
			r := slotRow{idx: idx, isNil: !held.Valid}
			if held.Valid {
				r.held = uuid.MustParse(held.String)
			}
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := snapshot()

	forceExpireLease(t, db, created.ID)
	reclaimed, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, reclaimed.ID)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)

	after := snapshot()
	require.Equal(t, before, after, "reclaim must not write queue_slots at all -- the held slot's binding is byte-identical before and after")

	var stillHeldIndex int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT slot_index FROM queue_slots WHERE queue_name = $1 AND held_by_job_id = $2`,
		"reclaim-no-write", reclaimed.ID).Scan(&stillHeldIndex))
	require.Equal(t, heldSlotIndex, stillHeldIndex, "the reclaimed job must retain and reuse its original slot binding")
}

// TestSlotTable_ReclaimDoesNotAcquireASecondSlot proves the reclaim path
// never increases the held-slot count for its queue.
func TestSlotTable_ReclaimDoesNotAcquireASecondSlot(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "no-double-slot", 1)
	_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.no_double", "no-double-slot", 5))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, heldSlotCount(t, db, "no-double-slot"))

	forceExpireLease(t, db, claimed.ID)
	_, ok, err = s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, heldSlotCount(t, db, "no-double-slot"), "reclaim must never bring the held-slot count above the pre-reclaim value")
}

// ---------------------------------------------------------------------
// SF-053: per-row slot/job consistency, checked across every
// terminalization path.
// ---------------------------------------------------------------------

// TestSlotTable_SF053_TerminalCompletionReleasesExactlyItsSlot covers
// CompleteSuccess, CompleteFailure, CompleteCancelled,
// CompleteRetryableFailure (both RETRY_WAIT and DEAD_LETTERED
// destinations), and CompleteTimeout (both destinations) -- the six
// terminalization call sites ADR-0009 names (the Lazy Dead-Letter Sweep
// is covered separately, SF-058 below), proving each releases the
// completing job's slot durably, and that a subsequent claim can reuse
// the freed slot.
func TestSlotTable_SF053_TerminalCompletionReleasesExactlyItsSlot(t *testing.T) {
	cases := []struct {
		name     string
		complete func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State
	}{
		{"CompleteSuccess", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			res, err := s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
			require.NoError(t, err)
			return res.State
		}},
		{"CompleteFailure", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			res, err := s.CompleteFailure(ctx, j.ID, owner, j.LeaseGeneration, "boom", job.ErrorClassPermanent)
			require.NoError(t, err)
			return res.State
		}},
		{"CompleteCancelled", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			_, err := s.RequestCancellation(ctx, j.ID, testAccess)
			require.NoError(t, err)
			res, err := s.CompleteCancelled(ctx, j.ID, owner, j.LeaseGeneration)
			require.NoError(t, err)
			return res.State
		}},
		{"CompleteRetryableFailure_RetryWait", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			res, err := s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "transient", time.Minute)
			require.NoError(t, err)
			require.Equal(t, jobstate.RetryWait, res.State)
			return res.State
		}},
		{"CompleteRetryableFailure_Exhausted", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			res, err := s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "transient", 0)
			require.NoError(t, err)
			require.Equal(t, jobstate.DeadLettered, res.State)
			return res.State
		}},
		{"CompleteTimeout_RetryWait", func(t *testing.T, s *store.Store, ctx context.Context, j *job.Job, owner string) jobstate.State {
			res, err := s.CompleteTimeout(ctx, j.ID, owner, j.LeaseGeneration, time.Minute)
			require.NoError(t, err)
			require.Equal(t, jobstate.RetryWait, res.State)
			return res.State
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.DB(t)
			s := store.New(db)
			ctx := context.Background()

			queueName := "slot-release-" + tc.name
			provisionQueueSlots(t, db, queueName, 1)

			maxAttempts := 5
			if tc.name == "CompleteRetryableFailure_Exhausted" {
				maxAttempts = 1
			}
			created, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.release", queueName, maxAttempts))
			require.NoError(t, err)

			claimed, ok, err := s.Claim(ctx, "worker")
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, 1, heldSlotCount(t, db, queueName))

			tc.complete(t, s, ctx, claimed, "worker")

			require.Equal(t, 0, heldSlotCount(t, db, queueName),
				"%s must release the slot in the same transaction as the transition", tc.name)
			assertPerRowSlotConsistency(t, db, queueName)

			// The freed slot must be immediately reusable.
			_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.slot.release.next", queueName, 5))
			require.NoError(t, err)
			next, ok, err := s.Claim(ctx, "worker-2")
			require.NoError(t, err)
			require.True(t, ok, "the freed slot must be claimable again")
			require.Equal(t, 1, heldSlotCount(t, db, queueName))
			require.NotEqual(t, created.ID, next.ID)
		})
	}
}

// TestSlotTable_SF053_PerRowConsistencyUnderSustainedMixedLoad drives a
// larger, sustained mix of claims, completions, failures, and reclaims
// against one capacity-limited queue and asserts the per-row invariant
// holds at the end -- a broader adversarial complement to the
// single-transition cases above.
func TestSlotTable_SF053_PerRowConsistencyUnderSustainedMixedLoad(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const capacity = 6
	provisionQueueSlots(t, db, "sustained", capacity)

	const numJobs = 60
	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newJobParamsQueueN("test.p13.slot.sustained", "sustained", 3))
		require.NoError(t, err)
	}

	rounds := 0
	for rounds = 0; rounds < 40; rounds++ {
		var wg sync.WaitGroup
		claimsCh := make(chan *job.Job, capacity*2)
		for w := 0; w < capacity*2; w++ {
			wg.Add(1)
			workerID := w
			go func() {
				defer wg.Done()
				j, ok, err := s.Claim(ctx, fmt.Sprintf("sustained-r%d-w%d", rounds, workerID))
				require.NoError(t, err)
				if ok {
					claimsCh <- j
				}
			}()
		}
		wg.Wait()
		close(claimsCh)

		claimedAny := false
		for j := range claimsCh {
			claimedAny = true
			owner := *j.LeaseOwner
			switch rounds % 3 {
			case 0:
				_, err := s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
				require.NoError(t, err)
			case 1:
				_, err := s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "transient", 0)
				require.NoError(t, err)
			case 2:
				// Simulated crash: leave it RUNNING; force-expire so the
				// next round can reclaim it.
				forceExpireLease(t, db, j.ID)
			}
		}
		assertPerRowSlotConsistency(t, db, "sustained")
		require.LessOrEqual(t, runningCount(t, db, "sustained"), capacity)
		if !claimedAny {
			break
		}
	}
	require.Less(t, rounds, 40, "sustained load did not converge within the round budget")
}
