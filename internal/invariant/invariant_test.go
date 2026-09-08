// Direct unit tests for internal/invariant, per docs/testing-strategy.md's
// "no mock" requirement: every test here runs against a real, disposable
// PostgreSQL instance (internal/testutil), exactly like every other
// integration test in this repository.
//
// internal/chaos's own seeded campaigns exercise this package extensively
// against organically-produced durable state (the state real claim/
// complete/cancel/retry/workflow calls actually produce), which is the
// primary evidence this package's checks are correct in the cases that
// matter. This file complements that with direct, deliberately
// constructed violations -- states the product code itself can never
// legitimately produce (several are only reachable by writing SQL that
// bypasses internal/store's fenced API entirely) -- so each check's
// detection logic is proven in isolation, not only ever exercised
// incidentally by a passing chaos run.
package invariant_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func newParams(jobType string) job.NewParams {
	return job.NewParams{
		JobType: jobType, Payload: []byte(`{}`), MaxAttempts: 5, ExecutionTimeoutSeconds: 30,
	}
}

// findViolation returns the first violation in vs whose InvariantID
// matches id, or fails the test if none does.
func findViolation(t *testing.T, vs []invariant.Violation, id string) invariant.Violation {
	t.Helper()
	for _, v := range vs {
		if v.InvariantID == id {
			return v
		}
	}
	t.Fatalf("no violation with InvariantID %q found among %d violation(s): %v", id, len(vs), vs)
	return invariant.Violation{}
}

// findViolationForSubject returns the violation in vs matching both id and
// subject, or fails the test if none does. Unlike findViolation, this is
// safe to use from a subtest that shares its parent's *testutil.DB (and
// therefore CheckAll's full, whole-database result set) with sibling
// subtests that deliberately leave their own TF-INV-005 violations behind
// -- findViolation alone would silently match an earlier sibling's
// violation instead of this subtest's own job.
func findViolationForSubject(t *testing.T, vs []invariant.Violation, id, subject string) invariant.Violation {
	t.Helper()
	for _, v := range vs {
		if v.InvariantID == id && v.Subject == subject {
			return v
		}
	}
	t.Fatalf("no violation with InvariantID %q and Subject %q found among %d violation(s): %v", id, subject, len(vs), vs)
	return invariant.Violation{}
}

// TestCheckAll_CleanOrganicState_NoViolations proves the checker reports
// nothing on durable state produced entirely through internal/store's own
// fenced API -- a normal success, a normal dead-letter, and a normal
// two-node workflow -- establishing the "no false positives" baseline
// every other test in this file assumes.
func TestCheckAll_CleanOrganicState_NoViolations(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	succeeded, err := s.Insert(ctx, newParams("invtest.clean.succeed"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, succeeded.ID, claimed.ID)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	dlq, err := s.Insert(ctx, newParams("invtest.clean.dlq"))
	require.NoError(t, err)
	claimed2, ok, err := s.Claim(ctx, "w2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, dlq.ID, claimed2.ID)
	_, err = s.CompleteFailure(ctx, claimed2.ID, "w2", claimed2.LeaseGeneration, "boom", job.ErrorClassPermanent)
	require.NoError(t, err)

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		{NodeKey: "A", JobType: "invtest.clean.wf.a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
		{NodeKey: "B", JobType: "invtest.clean.wf.b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
	}})
	require.NoError(t, err)
	var aJobID uuid.UUID
	for _, n := range inst.Nodes {
		if n.NodeKey == "A" {
			aJobID = n.JobID
		}
	}
	claimedA, ok, err := s.Claim(ctx, "wf-w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, aJobID, claimedA.ID)
	_, err = s.CompleteSuccess(ctx, claimedA.ID, "wf-w1", claimedA.LeaseGeneration, nil)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	require.Empty(t, violations, "clean, store-produced durable state must never report a violation")
}

// TestCheckAll_DetectsAttemptCountExceedsMaxAttempts proves TF-INV-006
// detection. This state cannot be forged for a non-terminal job at all:
// migration 0001's own `jobs_attempt_count_check` CHECK constraint
// (`attempt_count <= max_attempts OR state = 'DEAD_LETTERED'`) is a
// second, schema-level enforcement layer for this exact invariant and
// rejects any UPDATE that would violate it outside DEAD_LETTERED --
// discovered directly by this test (an earlier version tried to corrupt
// a QUEUED job's attempt_count and got a real SQLSTATE 23514 back, not a
// silent success). The one state the schema deliberately still allows
// past the limit is a DEAD_LETTERED job, so that is what this test
// forges to exercise the checker's own query.
func TestCheckAll_DetectsAttemptCountExceedsMaxAttempts(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.overattempt"))
	require.NoError(t, err)

	// Confirm the schema-level defense-in-depth layer directly: outside
	// DEAD_LETTERED, PostgreSQL itself refuses this state.
	_, err = db.ExecContext(ctx, `UPDATE jobs SET attempt_count = max_attempts + 3 WHERE id = $1`, created.ID)
	require.Error(t, err, "jobs_attempt_count_check must reject attempt_count > max_attempts for a non-DEAD_LETTERED job")

	// The one state the CHECK constraint still permits past the limit.
	_, err = db.ExecContext(ctx, `
		UPDATE jobs SET state = 'DEAD_LETTERED', attempt_count = max_attempts + 3,
			terminal_at = now(), last_error = 'forged', last_error_class = 'PERMANENT',
			lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-006")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsDuplicateLeaseGeneration proves TF-INV-002
// detection: a second job_attempts row bearing an already-used
// lease_generation is seeded directly (this is precisely the shape a
// fencing bug would produce -- two attempts believing they hold the same
// generation).
func TestCheckAll_DetectsDuplicateLeaseGeneration(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.dupgen"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), claimed.LeaseGeneration)

	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at, finished_at, outcome)
		VALUES ($1, $2, 2, 1, 'forged-duplicate', now(), now(), 'SUCCEEDED')`,
		uuid.New(), created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-002")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsNonMonotonicLeaseGeneration proves TF-INV-014
// detection: two DISTINCT generations recorded out of increasing order
// (a later attempt_number carrying a lower lease_generation than an
// earlier one) -- the shape a stale-write-not-actually-rejected bug would
// produce.
func TestCheckAll_DetectsNonMonotonicLeaseGeneration(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.nonmonotonic"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), claimed.LeaseGeneration)
	_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "w1", claimed.LeaseGeneration, "transient", 0)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE jobs SET eligible_at = now() WHERE id = $1`, created.ID)
	require.NoError(t, err)
	claimed2, ok, err := s.Claim(ctx, "w2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), claimed2.LeaseGeneration)

	// Forge attempt 2's recorded generation to be LOWER than attempt 1's --
	// impossible via the real claim query, which always increments.
	_, err = db.ExecContext(ctx, `UPDATE job_attempts SET lease_generation = 0 WHERE job_id = $1 AND attempt_number = 2`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-014")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsAttemptHistoryGap proves TF-INV-007 detection:
// attempt_count is advanced without a corresponding job_attempts row,
// producing a gap between the durable attempt ledger and the job's own
// counter.
func TestCheckAll_DetectsAttemptHistoryGap(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.gap"))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = db.ExecContext(ctx, `UPDATE jobs SET attempt_count = 2 WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-007")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsDeadLetteredWithoutReason proves TF-INV-009
// detection: a job forced into DEAD_LETTERED without a last_error, the
// exact "undebuggable dead-letter" scenario that invariant documents.
func TestCheckAll_DetectsDeadLetteredWithoutReason(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.silentdlq"))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = db.ExecContext(ctx, `
		UPDATE jobs SET state = 'DEAD_LETTERED', terminal_at = now(), lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-009")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsTerminalStateReopened proves TF-INV-005 detection:
// a terminal job's state forced back to RUNNING while terminal_at is left
// stuck at its original value -- exactly the durable artifact any real
// Claim-level bug would leave behind (claim.go's UPDATE always writes
// state = 'RUNNING' in the same statement as any job_attempts insert, but
// never touches terminal_at), and the only state this checker's rewritten
// query can actually observe (see checkNoAttemptAfterTerminal's doc
// comment for why comparing state to terminal_at replaced the earlier
// cross-transaction timestamp comparison).
func TestCheckAll_DetectsTerminalStateReopened(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.reopened"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// Mirror exactly what claim.go's claimQuery UPDATE writes on a
	// (buggy) reclaim: state -> RUNNING, a new job_attempts row inserted,
	// terminal_at left untouched from the original completion above.
	_, err = db.ExecContext(ctx, `UPDATE jobs SET state = 'RUNNING', lease_owner = 'forged-reopener', lease_generation = 2 WHERE id = $1`, created.ID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, 2, 2, 'forged-reopener', now())`,
		uuid.New(), created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-005")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
}

// TestCheckAll_DetectsTerminalStateReopenedThenReterminalized is the
// regression proof for the gap TestCheckAll_DetectsTerminalStateReopened
// alone cannot cover: a job that reached a terminal state, was
// illegitimately reclaimed (reopening it), and then reached a terminal
// state a SECOND time -- so by the time CheckAll runs, jobs.state reads
// terminal again and jobs.terminal_at has been overwritten with the
// second terminalization's own timestamp. A checker that only compares
// jobs.state against jobs.terminal_at (case 1 in
// checkNoAttemptAfterTerminal's doc comment) sees nothing wrong here: both
// columns agree, and the entire first terminalization -- and the illegal
// reopening in between -- has been erased from jobs.terminal_at. Only
// terminal_attempt_count (case 2, frozen at the FIRST terminalization via
// COALESCE and never written again) still proves it happened.
//
// This is run for all three terminal states -- SUCCEEDED, CANCELLED, and
// DEAD_LETTERED -- as required by TF-INV-005's scope, each as its own
// subtest so a regression in any single terminal state's coverage is
// individually visible.
//
// Every write here mirrors the EXACT durable effects the corresponding
// real internal/store code path performs (job_attempts insert with
// attempt_number = old attempt_count + 1, matching UPDATE ... SET
// state = 'RUNNING', attempt_count = attempt_count + 1,
// lease_generation = lease_generation + 1 for the illegal reclaim --
// terminal_at/terminal_attempt_count deliberately left untouched, exactly
// as claim.go's real claimQuery leaves them; then the same
// finalize-attempt + terminal_at/terminal_attempt_count write shape the
// real Complete*/sweep paths use for the second terminalization), so this
// is a faithful stand-in for what a hypothetical claimQuery WHERE-clause
// bug (or equivalent direct-SQL corruption) would leave behind -- not a
// state internal/store's own fenced API can currently produce, since
// claimQuery's WHERE clause (state IN ('QUEUED','RETRY_WAIT') OR
// (state='RUNNING' AND lease_expires_at < now())) already excludes every
// terminal state by construction. That exclusion is exactly the
// application-level guarantee this checker exists to independently, and
// durably, verify never silently breaks.
func TestCheckAll_DetectsTerminalStateReopenedThenReterminalized(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	t.Run("SUCCEEDED", func(t *testing.T) {
		created, err := s.Insert(ctx, newParams("invtest.reopened.reterm.succeeded"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, created.ID, claimed.ID)
		_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
		require.NoError(t, err)

		// Prove the CURRENT rewritten checker (case 1 alone) would in fact
		// miss this once the forged reopening below is re-terminalized --
		// this is the "prove it fails first" step the task requires.
		reopenThenReterminalize(t, ctx, db, created.ID, "forged-reopener-succeeded", "SUCCEEDED")

		violations, err := checker.CheckAll(ctx)
		require.NoError(t, err)
		findViolationForSubject(t, violations, "TF-INV-005", "job:"+created.ID.String())
	})

	t.Run("CANCELLED", func(t *testing.T) {
		created, err := s.Insert(ctx, newParams("invtest.reopened.reterm.cancelled"))
		require.NoError(t, err)
		// CancelQueuedOrRetryWait: reaches CANCELLED with ZERO job_attempts
		// rows -- the case terminal_attempt_count exists specifically to
		// still cover (see checkNoAttemptAfterTerminal's doc comment).
		_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
		require.NoError(t, err)

		reopenThenReterminalize(t, ctx, db, created.ID, "forged-reopener-cancelled", "CANCELLED")

		violations, err := checker.CheckAll(ctx)
		require.NoError(t, err)
		findViolationForSubject(t, violations, "TF-INV-005", "job:"+created.ID.String())
	})

	t.Run("DEAD_LETTERED", func(t *testing.T) {
		created, err := s.Insert(ctx, newParams("invtest.reopened.reterm.deadlettered"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, created.ID, claimed.ID)
		_, err = s.CompleteFailure(ctx, claimed.ID, "w1", claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
		require.NoError(t, err)

		reopenThenReterminalize(t, ctx, db, created.ID, "forged-reopener-deadlettered", "DEAD_LETTERED")

		violations, err := checker.CheckAll(ctx)
		require.NoError(t, err)
		findViolationForSubject(t, violations, "TF-INV-005", "job:"+created.ID.String())
	})
}

// reopenThenReterminalize forges, via raw SQL, the exact durable effects
// of (a) an illegitimate reclaim of jobID after it already reached a
// terminal state (mirroring claim.go's claimQuery UPDATE + job_attempts
// INSERT verbatim, minus the WHERE-clause guard that makes this
// unreachable through the real fenced API) and then (b) a second,
// legitimate-shaped terminalization back to finalState (mirroring the
// finalize-attempt + terminal_at/terminal_attempt_count write shape every
// real Complete*/sweep path uses). It asserts, as a precondition, that
// checkNoAttemptAfterTerminal's case-1-only predecessor (current state
// vs. terminal_at) would NOT catch this once step (b) has run --
// confirming the gap this test exists to close is real before relying on
// terminal_attempt_count to close it.
func reopenThenReterminalize(t *testing.T, ctx context.Context, db *sql.DB, jobID uuid.UUID, forgedOwner, finalState string) {
	t.Helper()

	var attemptCount int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT attempt_count FROM jobs WHERE id = $1`, jobID).Scan(&attemptCount))
	newAttemptNumber := attemptCount + 1

	// (a) illegal reclaim: exactly claimQuery's SET list, minus the WHERE
	// guard that would normally reject a terminal row.
	res, err := db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'RUNNING',
			lease_owner = $2,
			lease_generation = lease_generation + 1,
			lease_expires_at = now() + interval '30 seconds',
			heartbeat_at = now(),
			attempt_count = attempt_count + 1,
			updated_at = now(),
			cancel_requested = false,
			cancel_requested_at = NULL,
			version = version + 1
		WHERE id = $1`, jobID, forgedOwner)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forged reclaim must affect exactly the target row")

	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, $3, (SELECT lease_generation FROM jobs WHERE id = $2), $4, now())`,
		uuid.New(), jobID, newAttemptNumber, forgedOwner)
	require.NoError(t, err)

	// Precondition: at this point (reopened, not yet re-terminalized), the
	// PRE-fix checker's exact case-1 predicate (state vs terminal_at) DOES
	// see this -- confirming the forged reopen is durably a case-1 shape
	// before we mask it below.
	var stillFlaggedByCase1 bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs
			WHERE id = $1 AND terminal_at IS NOT NULL AND state NOT IN ('SUCCEEDED', 'CANCELLED', 'DEAD_LETTERED')
		)`, jobID).Scan(&stillFlaggedByCase1))
	require.True(t, stillFlaggedByCase1, "test setup: the forged reopen must be case-1-visible before re-terminalization masks it")

	// (b) re-terminalize: finalize the forged attempt and set
	// terminal_at/terminal_attempt_count exactly like a real completion
	// path would (terminal_attempt_count via the same
	// COALESCE(jobs.terminal_attempt_count, jobs.attempt_count) every real
	// site uses -- here a no-op since it is already non-NULL from the
	// job's real, original terminalization, which is exactly the point).
	var errMsg, errClass any
	if finalState == "DEAD_LETTERED" {
		errMsg, errClass = "forged re-terminalization", "PERMANENT"
	}
	_, err = db.ExecContext(ctx, `
		UPDATE jobs
		SET state = $2,
			lease_owner = NULL,
			lease_expires_at = NULL,
			last_error = COALESCE($3, last_error),
			last_error_class = COALESCE($4, last_error_class),
			terminal_at = now(),
			terminal_attempt_count = COALESCE(jobs.terminal_attempt_count, jobs.attempt_count),
			updated_at = now(),
			version = version + 1
		WHERE id = $1`, jobID, finalState, errMsg, errClass)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		UPDATE job_attempts
		SET finished_at = now(), outcome = $2
		WHERE job_id = $1 AND finished_at IS NULL`, jobID, attemptOutcomeForFinalState(finalState))
	require.NoError(t, err)

	// Confirm the precondition this whole test is proving: case 1 alone
	// (current state vs. terminal_at) is now BLIND to the reopening --
	// state and terminal_at agree again, exactly as they would after any
	// ordinary, legitimate single terminalization.
	var stillFlaggedByCase1AfterReterm bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs
			WHERE id = $1 AND terminal_at IS NOT NULL AND state NOT IN ('SUCCEEDED', 'CANCELLED', 'DEAD_LETTERED')
		)`, jobID).Scan(&stillFlaggedByCase1AfterReterm))
	require.False(t, stillFlaggedByCase1AfterReterm, "test setup: re-terminalization must mask case 1 -- that is the exact gap this test proves case 2 closes")
}

func attemptOutcomeForFinalState(finalState string) string {
	switch finalState {
	case "SUCCEEDED":
		return "SUCCEEDED"
	case "CANCELLED":
		return "CANCELLED"
	default:
		return "FAILED_PERMANENT"
	}
}

// TestCheckAll_NoFalsePositiveOnClockSkewedAttemptTimestamp is the
// regression proof for the bug checkNoAttemptAfterTerminal's rewrite
// fixes: a job_attempts row whose started_at wall-clock value ended up
// AFTER the job's own terminal_at (simulating exactly the kind of
// backward NTP/hypervisor wall-clock correction observed landing between
// two causally-ordered transactions under `go test -race`), while the
// job's CURRENT state correctly remains SUCCEEDED the whole time -- i.e.
// state was never actually reopened, only the wall-clock timestamps on
// two different rows/transactions ended up out of order. The old
// started_at-vs-terminal_at comparison flagged this as a TF-INV-005
// violation; it must not.
func TestCheckAll_NoFalsePositiveOnClockSkewedAttemptTimestamp(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.clockskew"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// The job's own (real) attempt 1 row now has finished_at/outcome set
	// by CompleteSuccess above; forge ITS started_at backward past the
	// job's terminal_at -- a legitimate row, an illegitimate (skewed)
	// wall-clock value, and critically: jobs.state was never touched, so
	// it is still, correctly, SUCCEEDED.
	res, err := db.ExecContext(ctx, `UPDATE job_attempts SET started_at = now() + interval '1 hour' WHERE job_id = $1 AND attempt_number = 1`, created.ID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	for _, v := range violations {
		require.NotEqual(t, "TF-INV-005", v.InvariantID, "a clock-skewed but otherwise-consistent attempt timestamp must not be reported as a reopened terminal state: %v", v)
	}
}

// TestCheckAll_DetectsMissingTerminalAttemptCountMarker proves
// checkNoAttemptAfterTerminal's case A: a row that reached a terminal
// state (terminal_at set) but whose terminal_attempt_count was never
// captured. This is exactly the shape a future terminalization code path
// that writes terminal_at/state but forgets terminal_attempt_count would
// leave behind -- unreachable via every CURRENT internal/store call site
// (each sets both columns in the same UPDATE), so it is forged directly
// via SQL, mirroring this file's established pattern for states the real
// fenced API cannot produce.
func TestCheckAll_DetectsMissingTerminalAttemptCountMarker(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.missingmarker"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE jobs SET terminal_attempt_count = NULL WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-005")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
	require.Contains(t, v.Detail, "terminal_attempt_count is NULL")
}

// TestCheckAll_DetectsNegativeTerminalAttemptCount proves
// checkNoAttemptAfterTerminal's case B: terminal_attempt_count < 0 can
// never be a legitimate snapshot of attempt_count (which never goes
// negative), so it is reported regardless of anything else on the row.
func TestCheckAll_DetectsNegativeTerminalAttemptCount(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.negativemarker"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE jobs SET terminal_attempt_count = -1 WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-005")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
	require.Contains(t, v.Detail, "is negative")
}

// TestCheckAll_DetectsTerminalAttemptCountExceedsAttemptCount proves
// checkNoAttemptAfterTerminal's case C: terminal_attempt_count can never
// durably exceed the job's current attempt_count, since the marker is
// defined as a snapshot of attempt_count taken at (and never after) first
// terminalization, and attempt_count only ever increases afterward.
func TestCheckAll_DetectsTerminalAttemptCountExceedsAttemptCount(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.excessmarker"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE jobs SET terminal_attempt_count = attempt_count + 5 WHERE id = $1`, created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-005")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
	require.Contains(t, v.Detail, "exceeds current attempt_count")
}

// TestTerminalAttemptCount_CapturedOnFirstTerminalizationByFamily proves
// item 2's requirement directly: every current terminalization family
// leaves terminal_attempt_count == attempt_count at the moment it FIRST
// makes a job terminal -- including the zero-attempt cancellation case,
// which never opens a job_attempts row at all. Each row of the table
// drives one job through the real internal/store fenced API for that
// specific family and asserts the marker directly (not merely "the
// checker reports no violation" -- a marker that failed to write at all
// would still, on its own, pass CheckAll if nothing else touched the row,
// so this asserts terminal_attempt_count itself, not just the absence of
// a TF-INV-005 finding).
func TestTerminalAttemptCount_CapturedOnFirstTerminalizationByFamily(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	type tc struct {
		name      string
		terminate func(t *testing.T, jobID uuid.UUID) string // returns expected terminal state
	}

	cases := []tc{
		{
			name: "CompleteSuccess",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				claimed, ok, err := s.Claim(ctx, "w-success")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, jobID, claimed.ID)
				_, err = s.CompleteSuccess(ctx, claimed.ID, "w-success", claimed.LeaseGeneration, nil)
				require.NoError(t, err)
				return "SUCCEEDED"
			},
		},
		{
			name: "CompleteFailure",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				claimed, ok, err := s.Claim(ctx, "w-failure")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, jobID, claimed.ID)
				_, err = s.CompleteFailure(ctx, claimed.ID, "w-failure", claimed.LeaseGeneration, "boom", job.ErrorClassPermanent)
				require.NoError(t, err)
				return "DEAD_LETTERED"
			},
		},
		{
			name: "CancelQueuedOrRetryWait_ZeroAttempts",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				// Never claimed -- attempt_count is 0 the whole time, and
				// no job_attempts row is ever opened. Exactly the "zero
				// attempt cancellation case" item 2 calls out.
				_, err := s.CancelQueuedOrRetryWait(ctx, jobID)
				require.NoError(t, err)
				return "CANCELLED"
			},
		},
		{
			name: "CompleteCancelled",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				claimed, ok, err := s.Claim(ctx, "w-cancel")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, jobID, claimed.ID)
				_, err = s.RequestCancellation(ctx, claimed.ID)
				require.NoError(t, err)
				_, err = s.CompleteCancelled(ctx, claimed.ID, "w-cancel", claimed.LeaseGeneration)
				require.NoError(t, err)
				return "CANCELLED"
			},
		},
		{
			name: "RetryExhaustion_DeadLettered",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				// maxAttempts=1: the first retryable failure already
				// exhausts the budget, landing straight on DEAD_LETTERED.
				claimed, ok, err := s.Claim(ctx, "w-exhaust")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, jobID, claimed.ID)
				j, err := s.CompleteRetryableFailure(ctx, claimed.ID, "w-exhaust", claimed.LeaseGeneration, "transient", 0)
				require.NoError(t, err)
				require.Equal(t, "DEAD_LETTERED", string(j.State), "test setup: maxAttempts=1 must exhaust on the first retryable failure")
				return "DEAD_LETTERED"
			},
		},
		{
			name: "ExpiredLeaseExhaustion_LazySweep",
			terminate: func(t *testing.T, jobID uuid.UUID) string {
				// maxAttempts=1: claim it, then force its lease to expire
				// (mirroring a crashed worker) so the next Claim call's
				// Lazy Dead-Letter Sweep dead-letters it directly, never
				// via any Complete* call.
				claimed, ok, err := s.Claim(ctx, "w-sweep-victim")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, jobID, claimed.ID)
				_, err = db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID)
				require.NoError(t, err)

				// The Lazy Dead-Letter Sweep runs unconditionally at the
				// top of every Claim, regardless of what (if anything)
				// that Claim call itself goes on to find -- no decoy job
				// is needed for it to catch jobID.
				_, _, err = s.Claim(ctx, "w-sweep-trigger")
				require.NoError(t, err)

				return "DEAD_LETTERED"
			},
		},
		{
			// jobID for this family is node B's job (see the special-cased
			// setup below): cancelling predecessor node A's job via
			// CompleteFailure cascades B to CANCELLED through
			// resolveDependent -- with zero job_attempts rows for B, the
			// same zero-attempt shape as CancelQueuedOrRetryWait above,
			// but reached through the workflow cascade path instead. Its
			// setup differs enough (two jobs, not one) that it is driven
			// directly in the loop below rather than through the
			// terminate func every other family shares.
			name:      "WorkflowCascadeCancellation",
			terminate: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var jobID uuid.UUID
			var wantState string

			if c.name == "WorkflowCascadeCancellation" {
				prefix := "invtest.family.cascade." + c.name
				inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{
					{NodeKey: "A", JobType: prefix + ".a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
					{NodeKey: "B", JobType: prefix + ".b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
				}})
				require.NoError(t, err)
				var aJobID, bJobID uuid.UUID
				for _, n := range inst.Nodes {
					if n.NodeKey == "A" {
						aJobID = n.JobID
					}
					if n.NodeKey == "B" {
						bJobID = n.JobID
					}
				}
				claimedA, ok, err := s.Claim(ctx, "w-cascade")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, aJobID, claimedA.ID)
				_, err = s.CompleteFailure(ctx, claimedA.ID, "w-cascade", claimedA.LeaseGeneration, "boom", job.ErrorClassPermanent)
				require.NoError(t, err)
				jobID, wantState = bJobID, "CANCELLED"
			} else {
				maxAttempts := 5
				if c.name == "RetryExhaustion_DeadLettered" || c.name == "ExpiredLeaseExhaustion_LazySweep" {
					maxAttempts = 1
				}
				created, err := s.Insert(ctx, job.NewParams{
					JobType: "invtest.family." + c.name, Payload: []byte(`{}`), MaxAttempts: maxAttempts, ExecutionTimeoutSeconds: 30,
				})
				require.NoError(t, err)
				jobID = created.ID
				wantState = c.terminate(t, jobID)
			}

			var state string
			var attemptCount int
			var terminalAttemptCount sql.NullInt64
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT state, attempt_count, terminal_attempt_count FROM jobs WHERE id = $1`, jobID,
			).Scan(&state, &attemptCount, &terminalAttemptCount))

			require.Equal(t, wantState, state, "test setup: %s must land on the expected terminal state", c.name)
			require.True(t, terminalAttemptCount.Valid, "%s: terminal_attempt_count must be captured on first terminalization", c.name)
			require.Equal(t, int64(attemptCount), terminalAttemptCount.Int64,
				"%s: terminal_attempt_count must equal attempt_count at first terminalization", c.name)
		})
	}
}

// TestCheckAll_IdempotencyCheck_NoFalsePositiveOnManyNullKeys proves
// checkIdempotencyKeyUniqueness does not mistake many jobs that simply
// never supplied an Idempotency-Key (idempotency_key IS NULL) for
// duplicates of each other -- the WHERE clause must exclude NULLs, per
// TF-INV-008's own scope ("a given idempotency key"). Constructing an
// actual TF-INV-008 violation is not possible even via raw SQL, since
// idx_jobs_idempotency_key is a real, enforced database constraint
// (TF-INV-016) -- there is no state to forge around it.
func TestCheckAll_IdempotencyCheck_NoFalsePositiveOnManyNullKeys(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	for i := 0; i < 10; i++ {
		_, err := s.Insert(ctx, newParams("invtest.idem.nullkey"))
		require.NoError(t, err)
	}

	key := "invtest-shared-key"
	_, _, err := s.InsertIdempotent(ctx, job.NewParams{
		JobType: "invtest.idem.keyed", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30,
		IdempotencyKey: &key,
	})
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	require.Empty(t, violations)
}

// TestCheckAll_DetectsWorkflowDependencyGatingViolation proves the
// TF-INV-012 gating check: a dependent node is forced to look claimed
// (attempt_count > 0, RUNNING) while its sole predecessor has not
// succeeded -- unreachable via the real claim query (a blocked node's
// eligible_at can only be moved by genuine predecessor-completion
// propagation), so this is constructed directly via SQL.
func TestCheckAll_DetectsWorkflowDependencyGatingViolation(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		{NodeKey: "A", JobType: "invtest.gating.a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
		{NodeKey: "B", JobType: "invtest.gating.b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
	}})
	require.NoError(t, err)

	var bJobID, bNodeID uuid.UUID
	for _, n := range inst.Nodes {
		if n.NodeKey == "B" {
			bJobID, bNodeID = n.JobID, n.ID
		}
	}
	require.NotEqual(t, uuid.Nil, bJobID)

	// A is still QUEUED (never claimed, never succeeded); force B's job
	// row to look like it was already claimed anyway.
	_, err = db.ExecContext(ctx, `
		UPDATE jobs SET state = 'RUNNING', attempt_count = 1, lease_owner = 'forged', lease_generation = 1,
			lease_expires_at = now() + interval '1 hour', eligible_at = now()
		WHERE id = $1`, bJobID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-012")
	require.Equal(t, "workflow_node:"+bNodeID.String(), v.Subject)
}

// TestCheckAll_DetectsStuckWorkflowTerminality proves the workflow-level
// TF-INV-012 check: every node forced to a terminal job state directly,
// without going through propagateWorkflowTransition (the only real path
// that also finalizes workflow_instances), leaving the workflow itself
// incorrectly stuck at RUNNING.
func TestCheckAll_DetectsStuckWorkflowTerminality(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		{NodeKey: "A", JobType: "invtest.stuck.a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
	}})
	require.NoError(t, err)
	require.Len(t, inst.Nodes, 1)

	// Force the sole node's job straight to SUCCEEDED via raw SQL,
	// bypassing CompleteSuccess entirely -- so
	// finalizeWorkflowIfComplete never runs, and workflow_instances is
	// left stuck at RUNNING even though its only node is done.
	_, err = db.ExecContext(ctx, `
		UPDATE jobs SET state = 'SUCCEEDED', terminal_at = now(), lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1`, inst.Nodes[0].JobID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-012")
	require.Equal(t, "workflow:"+inst.ID.String(), v.Subject)
}

// TestCheckJobsExist proves TF-INV-001's harness-tracked check: a real,
// submitted job is never flagged, a never-existing (random) ID is always
// flagged, and an empty ID list is a no-op (no error, no violation).
func TestCheckJobsExist(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	checker := invariant.New(db)

	created, err := s.Insert(ctx, newParams("invtest.exists"))
	require.NoError(t, err)
	ghost := uuid.New()

	violations, err := checker.CheckJobsExist(ctx, []uuid.UUID{created.ID, ghost})
	require.NoError(t, err)
	require.Len(t, violations, 1)
	require.Equal(t, "TF-INV-001", violations[0].InvariantID)
	require.Equal(t, "job:"+ghost.String(), violations[0].Subject)

	empty, err := checker.CheckJobsExist(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestViolation_String proves the Violation formatting a failing chaos
// seed relies on to print a self-contained reproduction line actually
// contains all three fields.
func TestViolation_String(t *testing.T) {
	v := invariant.Violation{InvariantID: "TF-INV-006", Subject: "job:abc", Detail: "attempt_count too high"}
	s := v.String()
	require.Contains(t, s, "TF-INV-006")
	require.Contains(t, s, "job:abc")
	require.Contains(t, s, "attempt_count too high")
}
