// Phase 12 store-layer proofs (docs/phase-12-plan.md §4a): the ownership
// predicate lives in SQL, on the same statement that reads or mutates the
// row, and is therefore not bypassable by calling internal/store directly
// rather than going through an HTTP handler.
//
// This matters independently of internal/api's own isolation tests: those
// prove the HTTP boundary behaves correctly, which would still be true of
// a design that filtered in the handler. These prove the guarantee is in
// the data layer, where there is no window between check and mutation.
package store_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// TestGetByID_IsPrincipalScoped proves the read predicate is in the
// statement: B cannot read A's job, and the failure is ErrNotFound --
// exactly what a nonexistent id produces, through the same branch.
func TestGetByID_IsPrincipalScoped(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "store scope A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "store scope B")
	accessA := principal.AccessContext{PrincipalID: a}
	accessB := principal.AccessContext{PrincipalID: b}

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID: a, JobType: "test.p12.store.read", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	got, err := s.GetByID(ctx, created.ID, accessA)
	require.NoError(t, err, "the owner must be able to read its own job")
	require.Equal(t, a, got.PrincipalID)

	_, err = s.GetByID(ctx, created.ID, accessB)
	require.ErrorIs(t, err, store.ErrNotFound,
		"another principal's job must be reported exactly as a nonexistent one")

	_, nonexistentErr := s.GetByID(ctx, uuid.New(), accessB)
	require.ErrorIs(t, nonexistentErr, store.ErrNotFound)
	require.Equal(t, nonexistentErr.Error(), err.Error(),
		"\"not yours\" and \"does not exist\" must be the same error, not merely the same status")

	// The admin exception.
	adminGot, err := s.GetByID(ctx, created.ID, principal.AccessContext{PrincipalID: b, IsAdmin: true})
	require.NoError(t, err, "an admin principal may read any principal's job")
	require.Equal(t, created.ID, adminGot.ID)
}

// TestCancelQueuedOrRetryWait_IsPrincipalScoped_MutatesZeroRows is §4a's
// core claim at the layer that actually enforces it: the unauthorized
// cancel does not merely report a failure, it changes nothing.
func TestCancelQueuedOrRetryWait_IsPrincipalScoped_MutatesZeroRows(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "cancel scope A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "cancel scope B")

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID: a, JobType: "test.p12.store.cancel", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	beforeState, beforeVersion, beforeUpdated := rawJobState(t, db, created.ID)

	_, err = s.CancelQueuedOrRetryWait(ctx, created.ID, principal.AccessContext{PrincipalID: b})
	require.ErrorIs(t, err, store.ErrStaleTransition,
		"an unauthorized cancel must fail exactly like a wrong-state one -- no distinguishable error")

	afterState, afterVersion, afterUpdated := rawJobState(t, db, created.ID)
	require.Equal(t, beforeState, afterState)
	require.Equal(t, beforeVersion, afterVersion,
		"version must be untouched: any UPDATE that actually matched would have incremented it")
	require.Equal(t, beforeUpdated, afterUpdated)

	// The owner's cancel does work, and does bump version -- so the guard
	// is scoping, not disabling.
	cancelled, err := s.CancelQueuedOrRetryWait(ctx, created.ID, principal.AccessContext{PrincipalID: a})
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cancelled.State)
	_, ownerVersion, _ := rawJobState(t, db, created.ID)
	require.Greater(t, ownerVersion, beforeVersion)
}

// TestRequestCancellation_IsPrincipalScoped_MutatesZeroRows covers the
// statement §4a specifically identified as the blocker: a RUNNING job's
// cancel_requested flag, which is the first thing an unauthorized caller
// would otherwise have been able to flip.
func TestRequestCancellation_IsPrincipalScoped_MutatesZeroRows(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "running cancel A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "running cancel B")

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID: a, JobType: "test.p12.store.cancel.running", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)

	_, beforeVersion, _ := rawJobState(t, db, created.ID)
	require.False(t, rawCancelRequested(t, db, created.ID))

	_, err = s.RequestCancellation(ctx, created.ID, principal.AccessContext{PrincipalID: b})
	require.ErrorIs(t, err, store.ErrStaleTransition)

	require.False(t, rawCancelRequested(t, db, created.ID),
		"an unauthorized caller must never set cancel_requested on another principal's RUNNING job")
	_, afterVersion, _ := rawJobState(t, db, created.ID)
	require.Equal(t, beforeVersion, afterVersion, "zero rows mutated")

	_, err = s.RequestCancellation(ctx, created.ID, principal.AccessContext{PrincipalID: a})
	require.NoError(t, err)
	require.True(t, rawCancelRequested(t, db, created.ID))
}

// TestGetWorkflow_IsPrincipalScoped is the workflow read equivalent.
func TestGetWorkflow_IsPrincipalScoped(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "wf scope A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "wf scope B")

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{
		PrincipalID: a,
		Nodes: []workflow.NodeSpec{
			{NodeKey: "A", JobType: "test.p12.store.wf.a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
		},
	})
	require.NoError(t, err)
	require.Equal(t, a, inst.PrincipalID)

	_, err = s.GetWorkflow(ctx, inst.ID, principal.AccessContext{PrincipalID: a})
	require.NoError(t, err)

	_, err = s.GetWorkflow(ctx, inst.ID, principal.AccessContext{PrincipalID: b})
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.GetWorkflow(ctx, inst.ID, principal.AccessContext{PrincipalID: b, IsAdmin: true})
	require.NoError(t, err, "an admin principal may read any principal's workflow")
}

// TestCreateWorkflow_AttributesInstanceAndEveryNodeJobToOnePrincipal
// proves a workflow and its nodes can never end up owned by different
// principals -- which is what makes the workflow-level ownership check
// sufficient to protect the node jobs.
func TestCreateWorkflow_AttributesInstanceAndEveryNodeJobToOnePrincipal(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "wf attribution")

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{
		PrincipalID: a,
		Nodes: []workflow.NodeSpec{
			{NodeKey: "root", JobType: "test.p12.wf.attr.root", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
			{NodeKey: "leaf", JobType: "test.p12.wf.attr.leaf", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"root"}},
		},
	})
	require.NoError(t, err)

	var instOwner uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT principal_id FROM workflow_instances WHERE id = $1`, inst.ID).Scan(&instOwner))
	require.Equal(t, a, instOwner)

	require.Len(t, inst.Nodes, 2)
	for _, n := range inst.Nodes {
		var jobOwner uuid.UUID
		require.NoError(t, db.QueryRow(`SELECT principal_id FROM jobs WHERE id = $1`, n.JobID).Scan(&jobOwner))
		require.Equal(t, a, jobOwner, "node %q's job must belong to the workflow's principal", n.NodeKey)
	}
}

// TestCancelWorkflow_IsPrincipalScoped_TouchesNoNodeJob proves the
// workflow-level check gates before the per-node cancellation loop
// (docs/phase-12-plan.md §7) -- so a non-owned workflow's node jobs are
// never touched, not merely its top-level row.
func TestCancelWorkflow_IsPrincipalScoped_TouchesNoNodeJob(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "wf cancel A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "wf cancel B")

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{
		PrincipalID: a,
		Nodes: []workflow.NodeSpec{
			{NodeKey: "root", JobType: "test.p12.wf.cancel.root", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
			{NodeKey: "leaf", JobType: "test.p12.wf.cancel.leaf", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"root"}},
		},
	})
	require.NoError(t, err)

	type snap struct {
		state   string
		version int64
	}
	before := map[uuid.UUID]snap{}
	for _, n := range inst.Nodes {
		st, v, _ := rawJobState(t, db, n.JobID)
		before[n.JobID] = snap{st, v}
	}
	var wfCancelRequested bool
	require.NoError(t, db.QueryRow(`SELECT cancel_requested FROM workflow_instances WHERE id = $1`, inst.ID).Scan(&wfCancelRequested))
	require.False(t, wfCancelRequested)

	_, err = s.CancelWorkflow(ctx, inst.ID, principal.AccessContext{PrincipalID: b})
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, db.QueryRow(`SELECT cancel_requested FROM workflow_instances WHERE id = $1`, inst.ID).Scan(&wfCancelRequested))
	require.False(t, wfCancelRequested, "the workflow_instances row must be untouched")
	for _, n := range inst.Nodes {
		st, v, _ := rawJobState(t, db, n.JobID)
		require.Equal(t, before[n.JobID], snap{st, v},
			"node job %s must be untouched: the workflow check gates before the per-node loop", n.JobID)
	}

	// The owner's cancel reaches every node.
	cancelled, err := s.CancelWorkflow(ctx, inst.ID, principal.AccessContext{PrincipalID: a})
	require.NoError(t, err)
	require.True(t, cancelled.CancelRequested)
	for _, n := range cancelled.Nodes {
		require.Equal(t, jobstate.Cancelled, n.JobState, "the owner's cancel must reach node %q", n.NodeKey)
	}
}

// TestGetByIdempotencyKey_IsPrincipalScoped proves the conflict-recovery
// read cannot cross tenants. This is load-bearing, not cosmetic: a
// globally-scoped read here would let principal A's INSERT
// conflict-recover onto principal B's row and hand B's job back to A.
func TestGetByIdempotencyKey_IsPrincipalScoped(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "idem scope A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "idem scope B")

	const jobType = "test.p12.store.idem"
	key := "shared-key"

	aJob, _, err := s.InsertIdempotent(ctx, job.NewParams{
		PrincipalID: a, JobType: jobType, Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
	})
	require.NoError(t, err)

	_, err = s.GetByIdempotencyKey(ctx, b, jobType, key)
	require.ErrorIs(t, err, store.ErrNotFound,
		"one principal's idempotency key must be invisible to another")

	// B's own submission with the same key creates a SECOND, independent
	// job -- not a false duplicate hit on A's.
	bJob, created, err := s.InsertIdempotent(ctx, job.NewParams{
		PrincipalID: b, JobType: jobType, Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
	})
	require.NoError(t, err)
	require.True(t, created, "B's submission must create its own row, not resolve to A's")
	require.NotEqual(t, aJob.ID, bJob.ID)

	// And each principal's own repeat still deduplicates to its own row.
	aAgain, createdAgain, err := s.InsertIdempotent(ctx, job.NewParams{
		PrincipalID: a, JobType: jobType, Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
	})
	require.NoError(t, err)
	require.False(t, createdAgain)
	require.Equal(t, aJob.ID, aAgain.ID,
		"the conflict-recovery re-read must return the CALLER's row, never the other tenant's")
}

// TestInsertIdempotent_ConflictRecoveryStillWorksAfterIndexRename is the
// regression guard for the single hardcoded string
// docs/phase-12-plan.md §5 flagged: internal/store's
// idempotencyKeyIndexName must match migration 0009's actual index name,
// or conflict recovery silently degrades to a generic insert failure
// instead of returning the existing row.
//
// created=false is the observable that proves the recovery path ran: if
// the constant had drifted, this second call would have returned an error
// rather than the existing job.
func TestInsertIdempotent_ConflictRecoveryStillWorksAfterIndexRename(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	p := testutil.NewPrincipal(t, db, principal.KindCaller, "index name coupling")
	key := "conflict-recovery-key"

	first, created, err := s.InsertIdempotent(ctx, job.NewParams{
		PrincipalID: p, JobType: "test.p12.indexname", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
	})
	require.NoError(t, err)
	require.True(t, created)

	second, created, err := s.InsertIdempotent(ctx, job.NewParams{
		PrincipalID: p, JobType: "test.p12.indexname", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
	})
	require.NoError(t, err,
		"a duplicate submission must be recovered, not reported as an insert failure -- "+
			"if this errors, idempotencyKeyIndexName has drifted from migration 0009's index name")
	require.False(t, created)
	require.Equal(t, first.ID, second.ID)
}

// TestInsert_ZeroPrincipalIDIsRejectedByTheDatabase proves there is no
// silent default anywhere on the store's insert path: an unattributed
// submission fails loudly at the foreign key rather than being quietly
// assigned to the system principal.
func TestInsert_ZeroPrincipalIDIsRejectedByTheDatabase(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)

	_, err := s.Insert(context.Background(), job.NewParams{
		JobType: "test.p12.noprincipal", Payload: []byte(`{}`),
		MaxAttempts: 3, ExecutionTimeoutSeconds: 30,
	})
	require.Error(t, err, "a job with no principal must not be insertable")

	var systemRows int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE principal_id = $1`, principal.SystemPrincipalID).Scan(&systemRows))
	require.Zero(t, systemRows, "and it must certainly not have been attributed to the system principal")
}

// rawJobState reads a job's state, version and updated_at straight from
// PostgreSQL, bypassing every store method -- so a "nothing was mutated"
// assertion does not depend on the very layer under test.
//
// version is the decisive field: every fenced UPDATE in internal/store
// increments it, so an UPDATE whose WHERE clause actually matched cannot
// leave it unchanged. Comparing version before and after is therefore a
// direct proof that zero rows were affected, not an inference from the
// returned error.
func rawJobState(t *testing.T, db *sql.DB, id uuid.UUID) (state string, version int64, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT state, version, updated_at FROM jobs WHERE id = $1`, id).Scan(&state, &version, &updatedAt))
	return state, version, updatedAt
}

// rawCancelRequested reads the cancel_requested flag directly, for the
// RUNNING-job proof where that single column is the thing an unauthorized
// caller must never be able to flip.
func rawCancelRequested(t *testing.T, db *sql.DB, id uuid.UUID) bool {
	t.Helper()
	var flag bool
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT cancel_requested FROM jobs WHERE id = $1`, id).Scan(&flag))
	return flag
}

// TestConcurrentTwoPrincipalIdempotency_ExactlyOneRowPerPrincipal is
// docs/phase-12-plan.md §8's two-principal concurrency requirement, run
// against the pool-based path (the txenqueue package proves the same
// property for the caller-owned pgx.Tx path).
//
// It is the direct extension of Phase 4's TestConcurrent*Idempotency
// pattern to the re-scoped index: N concurrent submissions from principal
// A and N from principal B, all with the identical job_type and
// idempotency_key, must yield exactly one row per principal -- not one
// globally (which would be a false duplicate across tenants) and not 2N
// (which would mean the constraint stopped enforcing).
//
// Every goroutine's returned job id is checked too, so a submission that
// "succeeded" by resolving onto the OTHER tenant's row would be caught
// even though the row counts alone would look correct.
func TestConcurrentTwoPrincipalIdempotency_ExactlyOneRowPerPrincipal(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "concurrent tenant A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "concurrent tenant B")

	const jobType = "test.p12.concurrent.idem"
	key := "identical-key-both-tenants"
	const perPrincipal = 16

	type result struct {
		principalID uuid.UUID
		jobID       uuid.UUID
	}
	results := make(chan result, perPrincipal*2)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, p := range []uuid.UUID{a, b} {
		for i := 0; i < perPrincipal; i++ {
			wg.Add(1)
			go func(principalID uuid.UUID) {
				defer wg.Done()
				<-start
				j, _, err := s.InsertIdempotent(ctx, job.NewParams{
					PrincipalID: principalID, JobType: jobType, Payload: []byte(`{}`),
					MaxAttempts: 3, ExecutionTimeoutSeconds: 30, IdempotencyKey: &key,
				})
				if err != nil {
					t.Errorf("concurrent submission failed: %v", err)
					return
				}
				results <- result{principalID: principalID, jobID: j.ID}
			}(p)
		}
	}
	close(start)
	wg.Wait()
	close(results)

	idsByPrincipal := map[uuid.UUID]map[uuid.UUID]bool{a: {}, b: {}}
	total := 0
	for r := range results {
		idsByPrincipal[r.principalID][r.jobID] = true
		total++
	}
	require.Equal(t, perPrincipal*2, total, "every concurrent submission must have succeeded")

	for _, p := range []uuid.UUID{a, b} {
		require.Len(t, idsByPrincipal[p], 1,
			"all of one principal's concurrent submissions must resolve to a single job id")
	}

	var aID, bID uuid.UUID
	for id := range idsByPrincipal[a] {
		aID = id
	}
	for id := range idsByPrincipal[b] {
		bID = id
	}
	require.NotEqual(t, aID, bID,
		"the two tenants must have received DIFFERENT jobs -- the same id would mean one tenant "+
			"was handed the other's job by the conflict-recovery re-read")

	// And the durable rows agree with what the callers were told.
	var rowCount int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key).Scan(&rowCount))
	require.Equal(t, 2, rowCount, "exactly one durable row per principal")

	for id, want := range map[uuid.UUID]uuid.UUID{aID: a, bID: b} {
		var owner uuid.UUID
		require.NoError(t, db.QueryRow(`SELECT principal_id FROM jobs WHERE id = $1`, id).Scan(&owner))
		require.Equal(t, want, owner)
	}
}
