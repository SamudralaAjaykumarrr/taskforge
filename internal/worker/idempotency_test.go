// Phase 4: execution-side idempotency identity, and the exactly-once
// LOGICAL EFFECT demonstration required by docs/roadmap.md's Phase 4
// scope ("building a reference example job handler demonstrating the
// pattern") and docs/scenario-corpus.md SF-004. These tests deliberately
// reconstruct SF-004's exact crash point ("worker crashes after side
// effect, before acknowledgement") using direct internal/store calls
// (mirroring internal/store/lease_test.go's fencing tests), because the
// point is to control precisely when the simulated side effect happens
// relative to the completion call -- something worker.RunOnce's own loop
// (which always completes immediately after the handler returns) cannot
// isolate.
//
// The pair below is the concrete, executable form of ADR-0003 and
// ADR-0004: TaskForge provides at-least-once EXECUTION (the handler may
// run twice) and a stable identity (job_id) a handler can use to make its
// own EFFECT idempotent -- these are two different guarantees, and
// neither test claims TaskForge solves arbitrary side-effect
// exactly-once-ness.
package worker_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice is
// docs/scenario-corpus.md SF-004's first half, and adversarial case #15
// ("Handler is intentionally non-idempotent to prove TaskForge still has
// at-least-once execution rather than magical exactly-once behavior"):
// Worker A performs a (simulated) external side effect, then crashes
// before ever calling a completion endpoint. TaskForge cannot distinguish
// this from "crashed before the side effect" (docs/retry-semantics.md's
// crash-timing table), so the job is reclaimed and retried by Worker B,
// which performs the side effect again. This is NOT a bug: it is the
// documented boundary of at-least-once execution (ADR-0003), proved here
// as an executable fact rather than only asserted in prose.
func TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.sf004.noidem",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	var sideEffectCount int32 // a non-idempotent external side effect double

	// Attempt 1 (Worker A): claim, perform the side effect, then crash --
	// no completion call of any kind is ever made for this attempt.
	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	atomic.AddInt32(&sideEffectCount, 1)
	// (Worker A crashes here.)

	forceExpireLeaseWT(t, db, claimedA.ID)

	// Attempt 2 (Worker B): reclaims the SAME logical job (TF-INV-004) and
	// re-invokes the handler, which performs the side effect again.
	claimedB, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, claimedA.ID, claimedB.ID, "a reclaim is the same logical job, not a new one")
	require.Equal(t, claimedA.LeaseGeneration+1, claimedB.LeaseGeneration)
	atomic.AddInt32(&sideEffectCount, 1)

	_, err = s.CompleteSuccess(ctx, claimedB.ID, *claimedB.LeaseOwner, claimedB.LeaseGeneration, nil)
	require.NoError(t, err)

	require.Equal(t, int32(2), atomic.LoadInt32(&sideEffectCount),
		"documented at-least-once limitation (ADR-0003, F3): a non-idempotent side effect runs twice "+
			"because TaskForge cannot see 'the handler already succeeded' before a durable acknowledgement arrives")
}

// TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect is
// SF-004's companion test, and the "exactly-once LOGICAL EFFECT
// demonstration" required by docs/roadmap.md's Phase 4 scope: the exact
// same crash sequence as the test above, except the handler's side effect
// goes through a handler-owned deduplication token table keyed on job_id
// -- TaskForge's stable, execution-side idempotency identity
// (docs/idempotency.md: "job_id -- stable across all retries of the same
// logical job"). Despite the handler code running twice (at-least-once
// execution is unchanged and still proven true below), the dedup table
// ends up with exactly one row: at-least-once execution + application-level
// idempotency = exactly-once LOGICAL effect for this side effect
// specifically -- NOT a claim that TaskForge achieves exactly-once
// execution in general.
func TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	// A handler-owned dedup token table, exactly the pattern documented in
	// docs/idempotency.md ("Deduplication token table"). TaskForge does
	// not create, own, or manage this table -- per ADR-0004, that document
	// explicitly rejects building generic dedup infrastructure into
	// TaskForge itself; this table exists only for this test/demo, built
	// on the same PostgreSQL instance the way a real handler author might
	// choose to.
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS test_handler_side_effects (
			job_id       UUID PRIMARY KEY,
			performed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS test_handler_side_effects`)
	})

	_, err = s.Insert(ctx, job.NewParams{
		JobType:                 "test.sf004.idem",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	performIdempotentEffect := func(jobID uuid.UUID) {
		// job_id is the SAME value on every attempt/generation of this
		// logical job (see the identity-stability tests below), so
		// ON CONFLICT DO NOTHING makes the EFFECT idempotent even though
		// the HANDLER CODE runs twice.
		_, err := db.ExecContext(ctx, `
			INSERT INTO test_handler_side_effects (job_id) VALUES ($1)
			ON CONFLICT (job_id) DO NOTHING`, jobID)
		require.NoError(t, err)
	}

	var handlerInvocations int32

	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	atomic.AddInt32(&handlerInvocations, 1)
	performIdempotentEffect(claimedA.ID) // attempt 1's effect
	// (Worker A crashes -- no completion call, exactly like the test above.)

	forceExpireLeaseWT(t, db, claimedA.ID)

	claimedB, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, claimedA.ID, claimedB.ID, "job_id is stable across the reclaim -- this is what makes it usable as a token")
	atomic.AddInt32(&handlerInvocations, 1)
	performIdempotentEffect(claimedB.ID) // attempt 2's effect -- SAME job_id

	_, err = s.CompleteSuccess(ctx, claimedB.ID, *claimedB.LeaseOwner, claimedB.LeaseGeneration, nil)
	require.NoError(t, err)

	require.Equal(t, int32(2), atomic.LoadInt32(&handlerInvocations),
		"at-least-once execution is UNCHANGED: the handler still ran twice")

	var effectRows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM test_handler_side_effects WHERE job_id = $1`, claimedA.ID).
		Scan(&effectRows))
	require.Equal(t, 1, effectRows,
		"exactly-once LOGICAL effect: despite two handler invocations, the job_id-keyed dedup table has exactly one row -- "+
			"this is application-level idempotency built on TaskForge's stable identity, NOT a TaskForge guarantee "+
			"about arbitrary side effects (see docs/idempotency.md, ADR-0004)")
}

// TestIdempotencyIdentity_JobIDStableAcrossReclaim proves the documented
// execution-side idempotency identity (docs/idempotency.md: "job_id --
// stable across all retries of the same logical job") survives a lease
// reclaim (adversarial case #8): the job_id a handler would see on
// generation 2 is byte-identical to what it saw on generation 1, even
// though lease_generation and attempt_count both advanced.
func TestIdempotencyIdentity_JobIDStableAcrossReclaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.identity.reclaim",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	gen1, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLeaseWT(t, db, gen1.ID)

	gen2, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)

	require.Equal(t, created.ID, gen1.ID)
	require.Equal(t, created.ID, gen2.ID, "job_id must survive a reclaim/new lease_generation unchanged")
	require.NotEqual(t, gen1.LeaseGeneration, gen2.LeaseGeneration)
	require.Equal(t, gen1.AttemptCount+1, gen2.AttemptCount)
}

// TestIdempotencyIdentity_JobIDStableAcrossRetry is the same proof for the
// RUNNING -> RETRY_WAIT -> RUNNING cycle (adversarial case #7), driven
// through the actual worker.RunOnce loop (not direct store calls) with a
// Recorder so the exact value a real Handler.Execute call would observe is
// asserted, not just the job row's own id column.
func TestIdempotencyIdentity_JobIDStableAcrossRetry(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.identity.retry",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	rec := testdoubles.NewRecorder(testdoubles.NewFlakyThenSucceed(1, nil))
	registry := handler.NewRegistry()
	registry.Register("test.identity.retry", rec)
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	mid, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, mid.State)
	forceSetEligibleAtWT(t, db, created.ID, -time.Second)

	claimed, err = w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)

	execs := rec.Executions()
	require.Len(t, execs, 2)
	require.Equal(t, created.ID.String(), execs[0].JobID)
	require.Equal(t, created.ID.String(), execs[1].JobID,
		"job_id -- the documented execution-side idempotency identity -- must be identical across a retry, "+
			"even though attempt_count advanced")
	require.Equal(t, 1, execs[0].AttemptCount)
	require.Equal(t, 2, execs[1].AttemptCount)
}
