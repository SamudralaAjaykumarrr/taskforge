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

// TestCheckAll_DetectsAttemptAfterTerminal proves the TF-INV-005 proxy
// check: a job_attempts row forged with started_at after the job's own
// terminal_at -- direct evidence a terminal job was somehow reclaimed.
func TestCheckAll_DetectsAttemptAfterTerminal(t *testing.T) {
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

	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, 2, 2, 'forged-reopener', now() + interval '1 hour')`,
		uuid.New(), created.ID)
	require.NoError(t, err)

	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	v := findViolation(t, violations, "TF-INV-005")
	require.Equal(t, "job:"+created.ID.String(), v.Subject)
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
