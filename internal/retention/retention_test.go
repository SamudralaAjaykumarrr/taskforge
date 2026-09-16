// Phase 13 (checkpoint 6): retention sweeper proofs against a real
// PostgreSQL instance. Covers SF-043 through SF-045 and SF-050.
package retention_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/retention"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func newJobParams(jobType string) job.NewParams {
	return job.NewParams{
		PrincipalID:             principal.SystemPrincipalID,
		JobType:                 jobType,
		Payload:                 []byte(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		QueueName:               "default",
	}
}

// backdateTerminalAt sets jobID's terminal_at directly (PostgreSQL's own
// clock arithmetic, no sleep) to simulate a job that terminalized `age`
// ago.
func backdateTerminalAt(t *testing.T, db *sql.DB, jobID uuid.UUID, age time.Duration) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE jobs SET terminal_at = now() - make_interval(secs => $2::double precision) WHERE id = $1`,
		jobID, age.Seconds())
	require.NoError(t, err)
}

func backdateWorkflowTerminalAt(t *testing.T, db *sql.DB, workflowID uuid.UUID, age time.Duration) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE workflow_instances SET terminal_at = now() - make_interval(secs => $2::double precision) WHERE id = $1`,
		workflowID, age.Seconds())
	require.NoError(t, err)
}

func baseConfig() retention.Config {
	return retention.Config{
		Enabled:        true,
		TerminalJobTTL: time.Hour,
		WorkflowTTL:    time.Hour,
		BatchSize:      100,
	}
}

// ---------------------------------------------------------------------
// Config validation (SF-050's validation half).
// ---------------------------------------------------------------------

func TestConfig_Validate_RejectsJobAttemptsTTLExceedingTerminalJobTTL(t *testing.T) {
	cfg := retention.Config{
		Enabled:        true,
		TerminalJobTTL: time.Hour,
		WorkflowTTL:    time.Hour,
		JobAttemptsTTL: 2 * time.Hour,
		BatchSize:      100,
	}
	err := cfg.Validate()
	require.ErrorIs(t, err, retention.ErrJobAttemptsTTLExceedsTerminalJobTTL)
}

func TestConfig_JobAttemptsTTL_DefaultsToTerminalJobTTL(t *testing.T) {
	cfg := retention.Config{TerminalJobTTL: 90 * time.Minute}
	require.Equal(t, 90*time.Minute, cfg.ResolveJobAttemptsTTL())

	cfg.JobAttemptsTTL = 10 * time.Minute
	require.Equal(t, 10*time.Minute, cfg.ResolveJobAttemptsTTL())
}

func TestSweep_Disabled_IsANoOp(t *testing.T) {
	db := testutil.DB(t)
	sweeper := retention.New(db)
	ctx := context.Background()

	s := store.New(db)
	created, err := s.Insert(ctx, newJobParams("test.retention.disabled"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	backdateTerminalAt(t, db, created.ID, 2*time.Hour)

	result, err := sweeper.Sweep(ctx, retention.Config{Enabled: false, TerminalJobTTL: time.Hour, WorkflowTTL: time.Hour, BatchSize: 100})
	require.NoError(t, err)
	require.Equal(t, int64(0), result.Total())

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, created.ID).Scan(&count))
	require.Equal(t, 1, count, "disabled retention must delete nothing")
}

// ---------------------------------------------------------------------
// SF-043 (roadmap-named): terminal-only pruning, batched, never touches
// non-terminal state.
// ---------------------------------------------------------------------

func TestSweep_NeverPrunesNonTerminalJobsOrAttempts(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	// A RUNNING job (claimed, never completed) and a QUEUED job -- both
	// non-terminal, must survive regardless of TTL.
	runningCreated, err := s.Insert(ctx, newJobParams("test.retention.running"))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)

	queuedCreated, err := s.Insert(ctx, newJobParams("test.retention.queued"))
	require.NoError(t, err)

	cfg := baseConfig()
	cfg.TerminalJobTTL = time.Nanosecond
	cfg.JobAttemptsTTL = time.Nanosecond
	_, err = sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id IN ($1, $2)`, runningCreated.ID, queuedCreated.ID).Scan(&count))
	require.Equal(t, 2, count, "non-terminal jobs must never be pruned regardless of how short the TTL is")

	var attemptCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE job_id = $1`, runningCreated.ID).Scan(&attemptCount))
	require.Equal(t, 1, attemptCount, "a non-terminal job's attempt history must never be pruned")
}

func TestSweep_PrunesOnlyTerminalJobsPastTTL(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	// Terminal, but still inside the TTL window.
	fresh, err := s.Insert(ctx, newJobParams("test.retention.fresh"))
	require.NoError(t, err)
	claimedFresh, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimedFresh.ID, "w1", claimedFresh.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, fresh.ID, claimedFresh.ID)

	// Terminal AND past the TTL.
	old, err := s.Insert(ctx, newJobParams("test.retention.old"))
	require.NoError(t, err)
	claimedOld, ok, err := s.Claim(ctx, "w2")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimedOld.ID, "w2", claimedOld.LeaseGeneration, nil)
	require.NoError(t, err)
	backdateTerminalAt(t, db, old.ID, 2*time.Hour)

	result, err := sweeper.Sweep(ctx, baseConfig())
	require.NoError(t, err)
	require.Equal(t, int64(1), result.JobsDeleted)
	require.GreaterOrEqual(t, result.JobAttemptsDeleted, int64(1))

	var freshCount, oldCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, fresh.ID).Scan(&freshCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, old.ID).Scan(&oldCount))
	require.Equal(t, 1, freshCount, "a terminal job still inside its TTL must survive")
	require.Equal(t, 0, oldCount, "a terminal job past its TTL must be deleted")
}

func TestSweep_BatchesAcrossMultiplePasses(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	const numJobs = 25
	ids := make([]uuid.UUID, numJobs)
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newJobParams("test.retention.batch"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
		require.NoError(t, err)
		backdateTerminalAt(t, db, created.ID, 2*time.Hour)
		ids[i] = created.ID
	}

	cfg := baseConfig()
	cfg.BatchSize = 7 // deliberately not a divisor of numJobs
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(numJobs), result.JobsDeleted, "batching must eventually cover the whole eligible backlog in one Sweep call")

	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE job_type = 'test.retention.batch'`).Scan(&remaining))
	require.Equal(t, 0, remaining)
}

// ---------------------------------------------------------------------
// SF-043 (measured, not assumed): a retention sweep over a large backlog
// does not starve concurrent claim-query traffic on unrelated,
// non-terminal jobs.
// ---------------------------------------------------------------------

func TestSweep_SF043_DoesNotStarveConcurrentClaimTraffic(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	const numTerminal = 500
	for i := 0; i < numTerminal; i++ {
		created, err := s.Insert(ctx, newJobParams("test.retention.load"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, "loader")
		require.NoError(t, err)
		require.True(t, ok)
		_, err = s.CompleteSuccess(ctx, claimed.ID, "loader", claimed.LeaseGeneration, nil)
		require.NoError(t, err)
		backdateTerminalAt(t, db, created.ID, 2*time.Hour)
	}

	// A fresh, unrelated claimable job, inserted AFTER the backlog.
	freshJob, err := s.Insert(ctx, newJobParams("test.retention.fresh_during_sweep"))
	require.NoError(t, err)

	cfg := baseConfig()
	cfg.BatchSize = 50

	sweepDone := make(chan error, 1)
	go func() { _, err := sweeper.Sweep(ctx, cfg); sweepDone <- err }()

	// The claim must complete promptly (well under a generous bound),
	// proving the sweep does not block the claim path.
	claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	claimed, ok, err := s.Claim(claimCtx, "concurrent-claimer")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, freshJob.ID, claimed.ID)
	require.NoError(t, claimCtx.Err(), "the claim must not have been starved by the concurrent sweep")

	require.NoError(t, <-sweepDone)
}

// ---------------------------------------------------------------------
// SF-044: idempotency dedup still fires inside the retention window;
// after legitimate deletion, a resubmission creates a fresh row rather
// than erroring or colliding.
// ---------------------------------------------------------------------

func TestSweep_SF044_IdempotencyWindow_NoSecondClock(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	key := "dedup-key-1"
	params := newJobParams("test.retention.idempotency")
	params.IdempotencyKey = &key

	first, created, err := s.InsertIdempotent(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.ID, claimed.ID)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// terminal_at is old, but still inside the TTL -- dedup must still
	// fire against the existing row.
	backdateTerminalAt(t, db, first.ID, 30*time.Minute)
	cfg := baseConfig() // TerminalJobTTL = 1 hour
	dup, created, err := s.InsertIdempotent(ctx, params)
	require.NoError(t, err)
	require.False(t, created, "a duplicate submission inside the retention/idempotency window must resolve to the existing row")
	require.Equal(t, first.ID, dup.ID)

	// Run the sweep BEFORE the row is actually past TTL -- it must
	// survive.
	_, err = sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	var stillThere int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, first.ID).Scan(&stillThere))
	require.Equal(t, 1, stillThere)

	// Now push it past the TTL and sweep for real.
	backdateTerminalAt(t, db, first.ID, 2*time.Hour)
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.JobsDeleted)

	// A resubmission with the SAME key must now create a genuinely NEW
	// row, not error and not collide with the deleted one.
	fresh, created, err := s.InsertIdempotent(ctx, params)
	require.NoError(t, err)
	require.True(t, created, "resubmission after legitimate deletion must create a fresh row, not error")
	require.NotEqual(t, first.ID, fresh.ID)
}

// ---------------------------------------------------------------------
// SF-045: pruning a terminal job's job_attempts to zero does not disable
// TF-INV-005's historical-reopening check.
// ---------------------------------------------------------------------

func TestSweep_SF045_PruningDoesNotDisableTFINV005Check(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.retention.sf045"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// Prune this job's attempts to zero (independently paced, shorter
	// than the job's own TTL) while the job row itself survives.
	backdateTerminalAt(t, db, created.ID, 2*time.Hour)
	cfg := retention.Config{
		Enabled:        true,
		TerminalJobTTL: 100 * time.Hour, // job row must NOT be deleted
		JobAttemptsTTL: time.Hour,       // attempts ARE past this TTL
		WorkflowTTL:    time.Hour,
		BatchSize:      100,
	}
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.JobsDeleted, "test setup: the job row itself must survive")
	require.GreaterOrEqual(t, result.JobAttemptsDeleted, int64(1))

	var attemptCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&attemptCount))
	require.Equal(t, 0, attemptCount, "test setup: this job's attempts must now be pruned to zero")

	// Simulate an illegitimate reopening bypassing normal code paths:
	// durably insert one anomalous job_attempts row above
	// terminal_attempt_count directly (the same technique
	// internal/invariant's own TF-INV-005 tests use).
	var terminalAttemptCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT terminal_attempt_count FROM jobs WHERE id = $1`, created.ID).Scan(&terminalAttemptCount))
	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at, finished_at, outcome)
		VALUES ($1, $2, $3, 999, 'anomalous-worker', now(), now(), 'SUCCEEDED')`,
		uuid.New(), created.ID, terminalAttemptCount+1)
	require.NoError(t, err)

	checker := invariant.New(db)
	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)

	found := false
	wantSubject := "job:" + created.ID.String()
	for _, v := range violations {
		if v.InvariantID == "TF-INV-005" && v.Subject == wantSubject {
			found = true
		}
	}
	require.True(t, found, "TF-INV-005's historical-reopening check must still detect the anomalous attempt even after this job's legitimate attempts were pruned to zero")
}

// ---------------------------------------------------------------------
// SF-050: attempt-history retention is independently paced; job_attempts
// pruned while the job row still exists.
// ---------------------------------------------------------------------

func TestSweep_SF050_JobAttemptsIndependentlyPacedShorterThanJobTTL(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.retention.sf050"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	backdateTerminalAt(t, db, created.ID, 2*time.Hour)

	cfg := retention.Config{
		Enabled:        true,
		TerminalJobTTL: 100 * time.Hour,
		JobAttemptsTTL: time.Hour,
		WorkflowTTL:    time.Hour,
		BatchSize:      100,
	}
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.JobsDeleted)
	require.Equal(t, int64(1), result.JobAttemptsDeleted)

	var jobCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, created.ID).Scan(&jobCount))
	require.Equal(t, 1, jobCount, "the job row must survive its own, longer TTL")
}

// ---------------------------------------------------------------------
// Workflow retention.
// ---------------------------------------------------------------------

func TestSweep_WorkflowRetention_DeletesNodesThenInstance_ThenUnblocksJobDeletion(t *testing.T) {
	db := testutil.DB(t)
	sweeper := retention.New(db)
	ctx := context.Background()

	// Minimal single-node workflow, driven directly against the schema
	// (internal/store/workflow.go's own shape) so this test does not
	// depend on internal/workflow's package for a one-node graph.
	workflowID := uuid.New()
	_, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, principal_id, state, terminal_at) VALUES ($1, $2, 'SUCCEEDED', now())`,
		workflowID, principal.SystemPrincipalID)
	require.NoError(t, err)
	jobID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, terminal_at, terminal_attempt_count)
		VALUES ($1, $2, 'test.retention.workflow_node', '{}', 'SUCCEEDED', 30, now(), 1)`,
		jobID, principal.SystemPrincipalID)
	require.NoError(t, err)
	nodeID := uuid.New()
	_, err = db.ExecContext(ctx, `INSERT INTO workflow_nodes (id, workflow_instance_id, node_key, job_id) VALUES ($1, $2, 'root', $3)`,
		nodeID, workflowID, jobID)
	require.NoError(t, err)

	backdateWorkflowTerminalAt(t, db, workflowID, 2*time.Hour)
	backdateTerminalAt(t, db, jobID, 2*time.Hour)

	cfg := baseConfig()
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.WorkflowNodesDeleted)
	require.Equal(t, int64(1), result.WorkflowInstancesDeleted)
	require.Equal(t, int64(1), result.JobsDeleted, "once the workflow_nodes reference is gone, the underlying job is eligible for deletion in the SAME sweep pass")

	for _, q := range []struct {
		table string
		id    uuid.UUID
	}{
		{"workflow_nodes", nodeID}, {"workflow_instances", workflowID}, {"jobs", jobID},
	} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+q.table+` WHERE id = $1`, q.id).Scan(&count))
		require.Equal(t, 0, count, "%s row must be deleted", q.table)
	}
}

func TestSweep_JobStillReferencedByLiveWorkflowNode_IsNotDeleted(t *testing.T) {
	db := testutil.DB(t)
	sweeper := retention.New(db)
	ctx := context.Background()

	// Workflow is terminal but still INSIDE its own TTL -- its node's job
	// (independently old enough by the job's own TTL) must NOT be deleted
	// while the workflow_nodes row still references it.
	workflowID := uuid.New()
	_, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, principal_id, state, terminal_at) VALUES ($1, $2, 'SUCCEEDED', now())`,
		workflowID, principal.SystemPrincipalID)
	require.NoError(t, err)
	jobID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, terminal_at, terminal_attempt_count)
		VALUES ($1, $2, 'test.retention.workflow_node_live', '{}', 'SUCCEEDED', 30, now(), 1)`,
		jobID, principal.SystemPrincipalID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO workflow_nodes (id, workflow_instance_id, node_key, job_id) VALUES ($1, $2, 'root', $3)`,
		uuid.New(), workflowID, jobID)
	require.NoError(t, err)

	backdateTerminalAt(t, db, jobID, 2*time.Hour) // job itself is old enough
	// workflow terminal_at left at "now" -- still inside its own TTL.

	cfg := baseConfig()
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, int64(0), result.JobsDeleted, "a job still referenced by a live (not-yet-aged-out) workflow_nodes row must not be deleted")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, jobID).Scan(&count))
	require.Equal(t, 1, count)
}

// ---------------------------------------------------------------------
// FK backstop: a misconfiguration that somehow bypasses Config.Validate
// still fails loudly (a foreign-key violation), never silently.
// ---------------------------------------------------------------------

func TestForeignKeyBackstop_CannotDeleteJobWhileAttemptsRemain(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.retention.fk_backstop"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `DELETE FROM jobs WHERE id = $1`, created.ID)
	require.Error(t, err, "PostgreSQL must refuse to delete a jobs row while its job_attempts rows still exist")
}

// ---------------------------------------------------------------------
// Correction pass finding M1: a job_attempts backlog too large for one
// sweep's iteration cap to fully drain must not abort the whole jobs
// batch via the foreign-key backstop above -- it is an ordinary,
// legitimate backlog (e.g. first adoption against an old deployment), not
// a misconfiguration, and pruneJobs must simply defer that job to a later
// sweep instead of erroring.
// ---------------------------------------------------------------------

func TestSweep_JobAttemptsBacklogExceedingIterationCap_DoesNotAbortJobsBatch(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	sweeper := retention.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.retention.backlog_cap"))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	backdateTerminalAt(t, db, created.ID, 2*time.Hour)

	// The claim/complete cycle above already left this job with one real
	// job_attempts row. Bulk-insert enough SYNTHETIC additional rows,
	// directly via SQL (test setup only, not a realistic execution history
	// -- a real max_attempts bounds a job's actual attempt count far below
	// this), so the total exceeds retention's own maxBatchIterationsPerSweep
	// (1000) at BatchSize=1 -- one Sweep call's pruneJobAttempts pass
	// cannot finish draining this one job's backlog.
	const extraAttempts = 1005
	_, err = db.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at, finished_at, outcome)
		SELECT gen_random_uuid(), $1, n + 1, 1, 'synthetic', now(), now(), 'SUCCEEDED'
		FROM generate_series(1, $2) AS n`, created.ID, extraAttempts)
	require.NoError(t, err)

	var totalAttempts int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&totalAttempts))
	require.Greater(t, totalAttempts, 1000, "test setup: backlog must exceed the 1000-batch-per-sweep iteration cap at BatchSize=1")

	cfg := baseConfig()
	cfg.BatchSize = 1

	// First sweep: pruneJobAttempts hits its 1000-batch iteration cap
	// before this job's backlog is fully drained. Before the fix,
	// pruneJobs's own WHERE clause did not exclude a job with remaining
	// job_attempts, so it would attempt this job's row anyway, hit the
	// foreign key, and abort the WHOLE batch DELETE statement -- failing
	// this entire Sweep call. After the fix, pruneJobs must skip this job.
	result, err := sweeper.Sweep(ctx, cfg)
	require.NoError(t, err, "a large-but-legitimate job_attempts backlog exceeding one sweep's iteration cap must not abort the whole sweep")
	require.Equal(t, int64(0), result.JobsDeleted, "the job must not be attempted for deletion while its attempts backlog is still being drained")
	require.Equal(t, int64(1000), result.JobAttemptsDeleted, "one sweep drains exactly its iteration cap's worth of attempts")

	var jobCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, created.ID).Scan(&jobCount))
	require.Equal(t, 1, jobCount, "the job row must survive this sweep -- its attempts are not yet fully pruned")

	// Repeated sweeps eventually drain the rest and then delete the job --
	// self-healing across sweep cycles, exactly like
	// TestSweep_BatchesAcrossMultiplePasses already proves for job_attempts
	// alone.
	for i := 0; i < 10; i++ {
		var remaining int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&remaining))
		if remaining == 0 {
			break
		}
		_, err := sweeper.Sweep(ctx, cfg)
		require.NoError(t, err)
	}

	var finalAttempts, finalJobs int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&finalAttempts))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, created.ID).Scan(&finalJobs))
	require.Equal(t, 0, finalAttempts)
	require.Equal(t, 0, finalJobs, "once its attempts backlog is fully drained, a later sweep must delete the job")
}
