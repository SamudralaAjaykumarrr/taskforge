// Phase 2 lease/claim/reclaim/heartbeat/fencing tests, against a real
// PostgreSQL instance (internal/testutil), per docs/testing-strategy.md.
// Every lease-expiry scenario here uses forceExpireLease (DB-time
// manipulation) instead of a sleep, per that document's explicit
// requirement for deterministic time control.
package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func newJobParamsN(jobType string, maxAttempts int) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{"k":"v"}`),
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: 30,
	}
}

// ---------------------------------------------------------------------
// Reclaim (TF-INV-004, SF-002/003/007)
// ---------------------------------------------------------------------

// TestClaim_ReclaimsExpiredLease is SF-007: a RUNNING job whose lease has
// expired is reclaimed by the same claim query under a new, strictly
// greater lease_generation, and the superseded attempt's job_attempts row
// is finalized (LEASE_EXPIRED) in the same transaction as the reclaim.
func TestClaim_ReclaimsExpiredLease(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.reclaim", 5))
	require.NoError(t, err)

	first, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), first.LeaseGeneration)
	require.Equal(t, 1, first.AttemptCount)

	forceExpireLease(t, db, created.ID)

	second, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok, "an expired lease must be reclaimable")
	require.Equal(t, created.ID, second.ID)
	require.Equal(t, jobstate.Running, second.State)
	require.Equal(t, int64(2), second.LeaseGeneration, "reclaim must strictly advance the fencing generation")
	require.Equal(t, 2, second.AttemptCount)
	require.Equal(t, "worker-B", *second.LeaseOwner)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 2)

	require.Equal(t, 1, attempts[0].AttemptNumber)
	require.Equal(t, int64(1), attempts[0].LeaseGeneration)
	require.Equal(t, "worker-A", attempts[0].WorkerID)
	require.NotNil(t, attempts[0].FinishedAt, "the superseded attempt must be finalized by reclaim")
	require.NotNil(t, attempts[0].Outcome)
	require.Equal(t, "LEASE_EXPIRED", *attempts[0].Outcome)

	require.Equal(t, 2, attempts[1].AttemptNumber)
	require.Equal(t, int64(2), attempts[1].LeaseGeneration)
	require.Equal(t, "worker-B", attempts[1].WorkerID)
	require.Nil(t, attempts[1].FinishedAt, "the new attempt must still be open")
}

// TestClaim_UnexpiredLeaseIsNeverReclaimed is the negative counterpart:
// an expired-looking lease is never touched before it actually expires,
// and a lease that has NOT expired is never stolen regardless of how many
// times Claim is called against it.
func TestClaim_UnexpiredLeaseIsNeverReclaimed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.reclaim.unexpired", 5))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	for i := 0; i < 3; i++ {
		_, ok, err := s.Claim(ctx, "worker-B")
		require.NoError(t, err)
		require.False(t, ok, "an unexpired lease must never be reclaimed")
	}

	current, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), current.LeaseGeneration)
	require.Equal(t, "worker-A", *current.LeaseOwner)
}

// TestClaim_TerminalJobNeverReclaimed proves adversarial case #10 ("a
// terminal job is presented to the reclaim query"): even a row whose
// lease_expires_at is deep in the past is never reclaimed once it has
// reached a terminal state (TF-INV-005), because the claim query's
// candidate predicate requires state = 'RUNNING'.
func TestClaim_TerminalJobNeverReclaimed(t *testing.T) {
	for _, terminal := range []jobstate.State{jobstate.Succeeded, jobstate.DeadLettered} {
		t.Run(string(terminal), func(t *testing.T) {
			db := testutil.DB(t)
			s := store.New(db)
			ctx := context.Background()

			created, err := s.Insert(ctx, newJobParamsN("test.terminal.reclaim", 5))
			require.NoError(t, err)
			claimed, ok, err := s.Claim(ctx, "worker-A")
			require.NoError(t, err)
			require.True(t, ok)

			var completeErr error
			switch terminal {
			case jobstate.Succeeded:
				_, completeErr = s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
			case jobstate.DeadLettered:
				_, completeErr = s.CompleteFailure(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
			}
			require.NoError(t, completeErr)

			// Simulate a row that somehow still carries a long-expired
			// lease_expires_at timestamp from before completion cleared
			// it, bypassing the Store to construct the adversarial case
			// directly (completion normally clears these fields, so this
			// is deliberately an out-of-band adversarial write).
			_, err = db.ExecContext(ctx, `
				UPDATE jobs SET lease_expires_at = now() - interval '1 hour' WHERE id = $1`, created.ID)
			require.NoError(t, err)

			_, ok, err = s.Claim(ctx, "worker-B")
			require.NoError(t, err)
			require.False(t, ok, "a terminal job must never be presented to the reclaim query")

			final, err := s.GetByID(ctx, created.ID)
			require.NoError(t, err)
			require.Equal(t, terminal, final.State)
		})
	}
}

// TestClaim_SweepDeadLettersAttemptExhaustedExpiredLease proves TF-INV-006
// holds on the reclaim path: a job whose lease expired AND whose
// attempt_count already equals max_attempts is dead-lettered by the Lazy
// Dead-Letter Sweep, never handed back out as a fresh RUNNING attempt.
func TestClaim_SweepDeadLettersAttemptExhaustedExpiredLease(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.sweep.exhausted", 1))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, claimed.AttemptCount) // == max_attempts already

	forceExpireLease(t, db, created.ID)

	_, ok, err = s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.False(t, ok, "an attempt-exhausted expired lease must be swept to DEAD_LETTERED, not reclaimed")

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State)
	require.NotNil(t, final.TerminalAt)
	require.NotNil(t, final.LastErrorClass)
	require.Equal(t, "LEASE_EXPIRED", *final.LastErrorClass)
	require.Nil(t, final.LeaseOwner)
	require.Nil(t, final.LeaseExpiresAt)

	attempts := attemptsForJob(t, db, created.ID)
	require.Len(t, attempts, 1)
	require.NotNil(t, attempts[0].FinishedAt)
	require.Equal(t, "LEASE_EXPIRED", *attempts[0].Outcome)
}

// TestClaim_SweepDoesNotAffectBudgetRemaining is the sweep's negative
// case: attempt_count < max_attempts must be reclaimed normally, not
// swept, even though its lease has also expired.
func TestClaim_SweepDoesNotAffectBudgetRemaining(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.sweep.budget", 3))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, claimed.AttemptCount)

	forceExpireLease(t, db, created.ID)

	reclaimed, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok, "a job with attempt budget remaining must be reclaimed, not swept")
	require.Equal(t, jobstate.Running, reclaimed.State)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)
}

// ---------------------------------------------------------------------
// Fencing / stale-worker rejection (TF-INV-003, TF-INV-014, SF-008)
// ---------------------------------------------------------------------

// TestFencing_StaleWorkerCompletionRejectedAfterReclaim is the canonical
// SF-008 scenario from docs/worker-protocol.md: Worker A holds generation
// 1, its lease expires, Worker B reclaims (generation 2) and completes the
// job. Worker A's later completion call, still carrying generation 1, must
// be rejected — and the job's actual state (from Worker B) must be
// untouched by Worker A's stale call.
func TestFencing_StaleWorkerCompletionRejectedAfterReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.fencing.sf008", 5))
	require.NoError(t, err)

	a, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), a.LeaseGeneration)

	forceExpireLease(t, db, created.ID)

	b, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), b.LeaseGeneration)

	completed, err := s.CompleteSuccess(ctx, b.ID, "worker-B", b.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)

	// Worker A, unaware it lost the lease, now tries to complete with its
	// stale generation 1 credential.
	_, err = s.CompleteSuccess(ctx, a.ID, "worker-A", a.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	// Adversarial case #16: the OLD generation tries to mark FAILURE
	// after the NEW generation already succeeded — must also be rejected.
	_, err = s.CompleteFailure(ctx, a.ID, "worker-A", a.LeaseGeneration, "late failure", job.ErrorClassPermanent)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State, "worker B's result must be untouched by worker A's stale calls")
}

// TestFencing_StaleCompletionRejectedBeforeNewOwnerCompletes covers the
// exact ordering from the adversarial audit list: Worker A pauses just
// before completion; A's lease expires; Worker B reclaims; A attempts
// completion (must be rejected) BEFORE B itself gets around to
// completing — proving rejection depends only on the generation no
// longer matching, not on whether the new owner has finished yet.
func TestFencing_StaleCompletionRejectedBeforeNewOwnerCompletes(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.fencing.order", 5))
	require.NoError(t, err)

	a, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	b, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)

	// A attempts completion right after B's reclaim, before B has done
	// anything else — must already be rejected.
	_, err = s.CompleteSuccess(ctx, a.ID, "worker-A", a.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	// The job must still be exactly as B's claim left it: RUNNING under
	// generation 2, not disturbed by A's rejected attempt.
	mid, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, mid.State)
	require.Equal(t, int64(2), mid.LeaseGeneration)

	// Now B completes normally.
	completed, err := s.CompleteSuccess(ctx, b.ID, "worker-B", b.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
}

// TestCompleteSuccess_AcceptedAfterExpiryButBeforeReclaim proves the
// specific documented boundary in docs/execution-semantics.md ("Cancel/
// Timeout Interaction Cases"): a completion call arriving after
// lease_expires_at has passed, but BEFORE any other worker has actually
// reclaimed the job, still succeeds — fencing rejects a superseded
// generation, not merely a late one. Lease expiry alone does not fence a
// worker; only a new generation being issued does.
func TestCompleteSuccess_AcceptedAfterExpiryButBeforeReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.late.not.superseded", 5))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	// Deliberately no reclaim here — nobody else has claimed it yet.

	completed, err := s.CompleteSuccess(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, nil)
	require.NoError(t, err, "a late-but-not-yet-superseded completion must be accepted, per docs/execution-semantics.md")
	require.Equal(t, jobstate.Succeeded, completed.State)
}

// TestFencing_ArbitrarilyLateArrivalAcrossManyGenerations is TF-INV-014's
// specific claim: fencing holds no matter how many generations have
// advanced or how late the stale write arrives — not just for the
// immediately-next generation (TestFencing_StaleWorkerCompletionRejectedAfterReclaim
// above), but arbitrarily far behind.
func TestFencing_ArbitrarilyLateArrivalAcrossManyGenerations(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.fencing.multigen", 10))
	require.NoError(t, err)

	gen1, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Advance through several generations of reclaim before anyone
	// completes the job.
	const generations = 4
	var last *job.Job = gen1
	for i := 0; i < generations-1; i++ {
		forceExpireLease(t, db, created.ID)
		next, ok, err := s.Claim(ctx, "worker-next")
		require.NoError(t, err)
		require.True(t, ok)
		last = next
	}
	require.Equal(t, int64(generations), last.LeaseGeneration)

	completed, err := s.CompleteSuccess(ctx, last.ID, "worker-next", last.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)

	// Generation 1's very first worker, arriving arbitrarily late (after
	// 3 further reclaims AND the eventual success), must still be
	// rejected.
	_, err = s.CompleteSuccess(ctx, gen1.ID, "worker-1", gen1.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, int64(generations), final.LeaseGeneration)
}

// ---------------------------------------------------------------------
// Heartbeat (TF-INV-015, SF-016/017)
// ---------------------------------------------------------------------

func TestHeartbeat_ExtendsValidLease(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.heartbeat.extend", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	before := *claimed.LeaseExpiresAt

	renewed, err := s.Heartbeat(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, 30*time.Second)
	require.NoError(t, err)
	require.True(t, renewed.LeaseExpiresAt.After(before) || renewed.LeaseExpiresAt.Equal(before),
		"heartbeat must never move lease_expires_at backward")
	require.Equal(t, jobstate.Running, renewed.State)
	require.Equal(t, claimed.LeaseGeneration, renewed.LeaseGeneration, "heartbeat must never change lease_generation")
	require.Equal(t, "worker-1", *renewed.LeaseOwner, "heartbeat must never change lease_owner")
}

func TestHeartbeat_MonotonicAcrossRapidRenewals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.heartbeat.rapid", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	prev := *claimed.LeaseExpiresAt
	for i := 0; i < 5; i++ {
		renewed, err := s.Heartbeat(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, 30*time.Second)
		require.NoError(t, err)
		require.False(t, renewed.LeaseExpiresAt.Before(prev), "lease_expires_at must be non-decreasing across successive heartbeats")
		prev = *renewed.LeaseExpiresAt
	}
}

func TestHeartbeat_RejectsStaleGeneration(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.heartbeat.stale.gen", 5))
	require.NoError(t, err)
	a, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)
	b, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), b.LeaseGeneration)

	// Worker A's heartbeat, still carrying generation 1, races with (and
	// arrives after) Worker B's reclaim — must be rejected.
	_, err = s.Heartbeat(ctx, a.ID, "worker-A", a.LeaseGeneration, 30*time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	// Worker B's own heartbeat, using the current generation, must
	// succeed — proving the rejection above was about A's stale
	// credential, not some blanket failure.
	_, err = s.Heartbeat(ctx, b.ID, "worker-B", b.LeaseGeneration, 30*time.Second)
	require.NoError(t, err)
}

func TestHeartbeat_RejectsWrongOwner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.heartbeat.wrong.owner", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.Heartbeat(ctx, claimed.ID, "some-other-worker", claimed.LeaseGeneration, 30*time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	current, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, claimed.LeaseGeneration, current.LeaseGeneration)
}

// TestHeartbeat_NeverResurrectsOrReopensTerminalJob proves heartbeat
// cannot "not silently ignore lost ownership": once a job is terminal, a
// heartbeat from its last legitimate owner is rejected and the job stays
// exactly as it was (TF-INV-005 applied to heartbeat, not just
// completion).
func TestHeartbeat_NeverResurrectsOrReopensTerminalJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.heartbeat.terminal", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	completed, err := s.CompleteSuccess(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	terminalAtBefore := *completed.TerminalAt

	_, err = s.Heartbeat(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, 30*time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)

	final, err := s.GetByID(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, terminalAtBefore, *final.TerminalAt)
}

// TestHeartbeat_ThenReclaim_ValidLeaseIsNotStolen and
// TestReclaim_ThenHeartbeat_StaleHeartbeatRejected together prove
// adversarial cases #7/#8 ("A heartbeats after B owns it" /
// "heartbeat races with reclaim") deterministically, for both possible
// arrival orderings, per docs/testing-strategy.md's requirement for
// explicit forced interleavings rather than repeated-run luck.
func TestHeartbeat_ThenReclaim_ValidLeaseIsNotStolen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsN("test.heartbeat.vs.reclaim.a", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Heartbeat arrives first, extending the lease.
	_, err = s.Heartbeat(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, 30*time.Second)
	require.NoError(t, err)

	// A reclaim attempt immediately after must find the lease still
	// valid and do nothing.
	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.False(t, ok, "a lease just extended by heartbeat must not be reclaimable")
}

func TestReclaim_ThenHeartbeat_StaleHeartbeatRejected(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.heartbeat.vs.reclaim.b", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Reclaim arrives first (lease expired).
	forceExpireLease(t, db, created.ID)
	_, ok, err = s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok)

	// Worker 1's heartbeat, arriving after the reclaim, must be rejected.
	_, err = s.Heartbeat(ctx, claimed.ID, "worker-1", claimed.LeaseGeneration, 30*time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)
}

// ---------------------------------------------------------------------
// Concurrency (TF-INV-002, SF-006, and the reclaim analogue)
// ---------------------------------------------------------------------

// TestClaim_ConcurrentWorkersRaceForSameJob is SF-006: N real goroutines,
// each with their own pooled connection issuing a real concurrent
// transaction, race to claim from a shared pool of jobs. Exactly one
// worker must win each job; no lease_generation may be issued twice for
// the same job (TF-INV-002).
func TestClaim_ConcurrentWorkersRaceForSameJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const numJobs = 20
	const numWorkers = 8

	jobIDs := make(map[string]bool, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParamsN("test.race", 5))
		require.NoError(t, err)
		jobIDs[created.ID.String()] = true
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	claimsCh := make(chan *job.Job, numJobs*2)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, workerIDName(workerID))
				require.NoError(t, err)
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

	seenGeneration := make(map[string]map[int64]bool)
	claimedCount := 0
	for j := range claimsCh {
		claimedCount++
		id := j.ID.String()
		require.True(t, jobIDs[id], "claimed an unexpected job id")
		if seenGeneration[id] == nil {
			seenGeneration[id] = make(map[int64]bool)
		}
		require.False(t, seenGeneration[id][j.LeaseGeneration], "lease_generation %d issued twice for job %s", j.LeaseGeneration, id)
		seenGeneration[id][j.LeaseGeneration] = true
	}

	require.Equal(t, numJobs, claimedCount, "every job must be claimed exactly once")
	require.Len(t, seenGeneration, numJobs)
}

// TestClaim_ConcurrentReclaimRace is the reclaim-branch analogue of
// SF-006: multiple workers concurrently attempt to reclaim the SAME
// expired-lease job. Exactly one must win, with generation 2; the losers
// must see ok=false, not an error (SKIP LOCKED semantics, not lock
// contention errors).
func TestClaim_ConcurrentReclaimRace(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.reclaim.race", 5))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "worker-0")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)

	const numWorkers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *job.Job, numWorkers)
	errs := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			j, ok, err := s.Claim(ctx, workerIDName(workerID))
			if err != nil {
				errs <- err
				return
			}
			if ok {
				results <- j
			}
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var winners []*job.Job
	for j := range results {
		winners = append(winners, j)
	}
	require.Len(t, winners, 1, "exactly one worker must win the reclaim race")
	require.Equal(t, int64(2), winners[0].LeaseGeneration)
}

func workerIDName(i int) string {
	return "worker-" + string(rune('A'+i))
}

// ---------------------------------------------------------------------
// Transaction atomicity for the new multi-statement claim path
// (TF-INV-013, adversarial case #11: "DB transaction aborts halfway
// through claim")
// ---------------------------------------------------------------------

// TestClaim_RollbackOnAttemptConflictLeavesJobRowUnchanged forces the
// job_attempts INSERT inside Claim's transaction to fail (by seeding a
// conflicting row out of band beforehand) and asserts the entire claim is
// rolled back: the jobs row must be byte-for-byte as it was before Claim
// was ever called, not partially transitioned to RUNNING with the
// job_attempts write silently missing.
func TestClaim_RollbackOnAttemptConflictLeavesJobRowUnchanged(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.claim.rollback", 5))
	require.NoError(t, err)

	// Seed a conflicting job_attempts row out of band: Claim will compute
	// attempt_number=1 for this job's first claim, which collides with
	// this pre-existing row on UNIQUE(job_id, attempt_number).
	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, 1, 0, 'seed', now())`, uuid.New(), created.ID)
	require.NoError(t, err)

	_, ok, err := s.Claim(ctx, "worker-1")
	require.Error(t, err, "a job_attempts conflict inside the claim transaction must surface as an error")
	require.False(t, ok)

	after, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Queued, after.State, "a rolled-back claim must leave the job exactly as it was")
	require.Equal(t, int64(0), after.LeaseGeneration)
	require.Equal(t, 0, after.AttemptCount)
	require.Nil(t, after.LeaseOwner)

	// The row is still genuinely claimable afterward (this failure is not
	// permanently poisoning it), once the conflicting seed row is gone.
	_, err = db.ExecContext(ctx, `DELETE FROM job_attempts WHERE worker_id = 'seed'`)
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), claimed.LeaseGeneration)
}
