// Phase 13 (ADR-0009, checkpoint 3): the Lazy Dead-Letter Sweep's slot
// release -- SF-058 (durable, idempotent) and SF-059 (fault-injection
// atomicity), against a real PostgreSQL instance. This is the specific
// call site ADR-0009's own drafting found completely untested by every
// prior evidence pass, and its own benchmark database was found holding
// 22 permanently-stranded-slot jobs as a direct result.
package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestSweep_SF058_ReleasesSlotDurablyOnAttemptBudgetExhaustion constructs
// a job whose lease has expired and whose attempt_count has reached
// max_attempts while RUNNING; confirms the Lazy Dead-Letter Sweep
// transitions it to DEAD_LETTERED and releases its slot in the same
// transaction, and that a subsequent claim attempt for that queue can
// successfully use the freed slot.
func TestSweep_SF058_ReleasesSlotDurablyOnAttemptBudgetExhaustion(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "sweep-durable", 1)
	created, err := s.Insert(ctx, newJobParamsQueueN("test.p13.sweep.durable", "sweep-durable", 1))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "worker-exhausting")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, claimed.AttemptCount) // == max_attempts already
	require.Equal(t, 1, heldSlotCount(t, db, "sweep-durable"))

	forceExpireLease(t, db, created.ID)

	// The sweep runs as a side effect of ANY Claim call, before that
	// call's own pick step. Nothing else is claimable on this queue (the
	// only job just got swept, not reclaimed), so this call itself
	// reports ok=false -- the sweep's effect is what this test asserts,
	// not this call's own return value.
	_, ok, err = s.Claim(ctx, "worker-triggering-sweep")
	require.NoError(t, err)
	require.False(t, ok)

	swept, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, swept.State)
	require.NotNil(t, swept.TerminalAt)
	require.Equal(t, 0, heldSlotCount(t, db, "sweep-durable"),
		"the sweep must release the job's slot in the SAME transaction as its DEAD_LETTERED transition")

	// The freed slot is immediately usable by a fresh claim.
	_, err = s.Insert(ctx, newJobParamsQueueN("test.p13.sweep.durable.next", "sweep-durable", 5))
	require.NoError(t, err)
	next, ok, err := s.Claim(ctx, "worker-next")
	require.NoError(t, err)
	require.True(t, ok, "the slot freed by the sweep must be claimable again")
	require.Equal(t, 1, heldSlotCount(t, db, "sweep-durable"))
	require.NotEqual(t, created.ID, next.ID)
}

// TestSweep_SF058_SecondSweepPassIsIdempotent_DoesNotDisturbANewHolder
// confirms a second, later sweep pass against the same already-
// DEAD_LETTERED job is a no-op that does not re-release (or otherwise
// disturb) a slot that has since been legitimately reclaimed by a
// different job -- releaseSlot/releaseSlotsForJobs match by
// held_by_job_id, not by slot_index, so this is checked directly against
// that mechanism rather than merely relying on the sweep's own WHERE
// state='RUNNING' clause never matching a DEAD_LETTERED row a second time
// (which would make the idempotency claim true only by coincidence of the
// state machine, not by the release step's own design).
func TestSweep_SF058_SecondSweepPassIsIdempotent_DoesNotDisturbANewHolder(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "sweep-idempotent", 1)
	swept, err := s.Insert(ctx, newJobParamsQueueN("test.p13.sweep.idempotent", "sweep-idempotent", 1))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, swept.ID, claimed.ID)
	forceExpireLease(t, db, swept.ID)

	// Trigger the sweep.
	_, ok, err = s.Claim(ctx, "worker-b")
	require.NoError(t, err)
	require.False(t, ok)
	afterSweep, err := s.GetByID(ctx, swept.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, afterSweep.State)
	require.Equal(t, 0, heldSlotCount(t, db, "sweep-idempotent"))

	// A different job legitimately claims -- and now holds -- the freed
	// slot.
	newHolder, err := s.Insert(ctx, newJobParamsQueueN("test.p13.sweep.idempotent.holder", "sweep-idempotent", 5))
	require.NoError(t, err)
	claimedNewHolder, ok, err := s.Claim(ctx, "worker-c")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, newHolder.ID, claimedNewHolder.ID)

	var slotIndexBefore int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT slot_index FROM queue_slots WHERE queue_name = $1 AND held_by_job_id = $2`,
		"sweep-idempotent", newHolder.ID).Scan(&slotIndexBefore))

	// Simulate a second sweep pass's release step running again against
	// the ORIGINAL, already-swept job (e.g. after a crash and restart
	// re-runs the sweep) -- the exact statement shape
	// releaseSlotsForJobs/releaseSlot use, matching by held_by_job_id.
	_, err = db.ExecContext(ctx,
		`UPDATE queue_slots SET held_by_job_id = NULL WHERE held_by_job_id = $1`, swept.ID)
	require.NoError(t, err)

	// The new, legitimate holder's slot must be completely undisturbed.
	var slotIndexAfter int
	var stillHeld bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT slot_index, true FROM queue_slots WHERE queue_name = $1 AND held_by_job_id = $2`,
		"sweep-idempotent", newHolder.ID).Scan(&slotIndexAfter, &stillHeld))
	require.True(t, stillHeld, "the new holder's slot must not have been released by the repeated sweep pass")
	require.Equal(t, slotIndexBefore, slotIndexAfter)
}

// TestSweep_SF059_FaultInjection_TerminalTransitionAndSlotReleaseAreAtomic
// forces a rollback between the sweep's job-state UPDATE and its slot
// release, in a hand-constructed transaction that replicates both
// statements, and confirms PostgreSQL's own transaction atomicity leaves
// the row exactly as it was before -- never DEAD_LETTERED with its slot
// still held, and never slot-released with the job row still RUNNING.
// Mirrors TF-INV-013's own SF-014 precedent
// (TestFaultInjection_SuccessAndPropagationRollbackTogether in
// workflow_test.go), extended to the slot-table mechanism.
func TestSweep_SF059_FaultInjection_TerminalTransitionAndSlotReleaseAreAtomic(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	provisionQueueSlots(t, db, "sweep-atomic", 1)
	created, err := s.Insert(ctx, newJobParamsQueueN("test.p13.sweep.atomic", "sweep-atomic", 1))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)
	forceExpireLease(t, db, created.ID)

	beforeJob, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	var beforeHeld bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT held_by_job_id = $2 FROM queue_slots WHERE queue_name = $1`, "sweep-atomic", created.ID).Scan(&beforeHeld))
	require.True(t, beforeHeld, "test setup: the slot must still be held by the about-to-be-swept job")

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	// Replicate sweepExpiredExhaustedLeases's own transition for this one
	// row.
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'DEAD_LETTERED', lease_owner = NULL, lease_expires_at = NULL,
		    last_error = 'lease expired, retry budget exhausted', last_error_class = 'LEASE_EXPIRED',
		    terminal_at = now(), terminal_attempt_count = COALESCE(terminal_attempt_count, attempt_count),
		    updated_at = now(), version = version + 1
		WHERE id = $1 AND state = 'RUNNING' AND lease_expires_at < now() AND attempt_count >= max_attempts`,
		created.ID)
	require.NoError(t, err)

	// Replicate releaseSlotsForJobs's own release step, in the SAME
	// transaction.
	_, err = tx.ExecContext(ctx,
		`UPDATE queue_slots SET held_by_job_id = NULL WHERE held_by_job_id = $1`, created.ID)
	require.NoError(t, err)

	// Force the transaction to abort before commit (a stand-in for a
	// crashed connection or any other mid-transaction failure).
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'NOT_A_REAL_STATE' WHERE id = $1`, created.ID)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	afterJob, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, beforeJob.State, afterJob.State, "the DEAD_LETTERED transition must not survive a rolled-back transaction")
	require.Equal(t, beforeJob.Version, afterJob.Version)
	require.Nil(t, afterJob.TerminalAt, "must never be DEAD_LETTERED with a terminal_at set if the transaction rolled back")

	var afterHeld bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT held_by_job_id = $2 FROM queue_slots WHERE queue_name = $1`, "sweep-atomic", created.ID).Scan(&afterHeld))
	require.True(t, afterHeld, "the slot release must not survive a rolled-back transaction either -- never slot-released with the job row still RUNNING")

	// The job is still legitimately RUNNING-with-expired-lease-and-
	// exhausted-budget and can be swept for real afterward -- the
	// rollback did not corrupt or strand it.
	_, ok, err = s.Claim(ctx, "worker-b")
	require.NoError(t, err)
	require.False(t, ok)
	finalJob, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, finalJob.State)
	require.Equal(t, 0, heldSlotCount(t, db, "sweep-atomic"))
}
