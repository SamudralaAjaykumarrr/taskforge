// Phase 4 store-level tests: submission idempotency, per
// docs/idempotency.md, docs/invariants.md TF-INV-008/TF-INV-016, and
// docs/scenario-corpus.md SF-005. Concurrency tests use real goroutines
// against real pooled PostgreSQL connections (never a mock), per
// docs/testing-strategy.md.
package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func newJobParamsWithKey(jobType, key string) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{"k":"v"}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		IdempotencyKey:          &key,
	}
}

// TestInsertIdempotent_NoKey_AlwaysCreatesNewJob proves docs/idempotency.md's
// "No Idempotency-Key supplied: every POST /jobs call creates a new job" --
// even with byte-identical job_type and payload, two calls with a nil key
// each get their own job row.
func TestInsertIdempotent_NoKey_AlwaysCreatesNewJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first, created1, err := s.InsertIdempotent(ctx, newJobParams("test.idem.nokey"))
	require.NoError(t, err)
	require.True(t, created1)

	second, created2, err := s.InsertIdempotent(ctx, newJobParams("test.idem.nokey"))
	require.NoError(t, err)
	require.True(t, created2)

	require.NotEqual(t, first.ID, second.ID)
}

// TestInsertIdempotent_FirstSubmission_CreatesJob is the baseline: a fresh
// key creates a job, created=true, and the row is durably readable back by
// (job_type, idempotency_key) (TF-INV-008's mechanism).
func TestInsertIdempotent_FirstSubmission_CreatesJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j, created, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.first", "key-1"))
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, j.IdempotencyKey)
	require.Equal(t, "key-1", *j.IdempotencyKey)

	found, err := s.GetByIdempotencyKey(ctx, "test.idem.first", "key-1")
	require.NoError(t, err)
	require.Equal(t, j.ID, found.ID)
}

// TestInsertIdempotent_SequentialDuplicate_ReturnsExistingJob is the
// documented "Acknowledgement / Response Loss" scenario at the store
// level: a client that submits, then (believing the response was lost)
// submits again with the same key, gets back the SAME job, and no second
// job row is ever created.
func TestInsertIdempotent_SequentialDuplicate_ReturnsExistingJob(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	first, created1, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.seq", "retry-key"))
	require.NoError(t, err)
	require.True(t, created1)

	second, created2, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.seq", "retry-key"))
	require.NoError(t, err)
	require.False(t, created2, "a retried submission with the same key must not create a new job")
	require.Equal(t, first.ID, second.ID)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`,
		"test.idem.seq", "retry-key").Scan(&count))
	require.Equal(t, 1, count, "TF-INV-008: exactly one job row for this (job_type, idempotency_key) pair")
}

// TestInsertIdempotent_DifferentJobTypeSameKey_CreatesSeparateJobs proves
// docs/idempotency.md's documented scope: "(job_type, idempotency_key) --
// the key is scoped per job type." The identical key value under two
// different job_types is not a collision at all.
func TestInsertIdempotent_DifferentJobTypeSameKey_CreatesSeparateJobs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a, created1, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.scope.a", "shared-key"))
	require.NoError(t, err)
	require.True(t, created1)

	b, created2, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.scope.b", "shared-key"))
	require.NoError(t, err)
	require.True(t, created2, "same key value under a DIFFERENT job_type is not a duplicate")

	require.NotEqual(t, a.ID, b.ID)
}

// TestInsertIdempotent_ConflictingPayloadSameKey_FirstWriteWins proves
// docs/idempotency.md's explicit, deliberate v1 decision (see "Open
// Questions"): a retried submission under the same key with a DIFFERENT
// payload is not rejected, not flagged as a conflict, and does not update
// the existing row -- the first submission's payload wins unconditionally.
func TestInsertIdempotent_ConflictingPayloadSameKey_FirstWriteWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	key := "conflict-key"
	first, created1, err := s.InsertIdempotent(ctx, job.NewParams{
		JobType:                 "test.idem.conflict",
		Payload:                 json.RawMessage(`{"version":"original"}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		IdempotencyKey:          &key,
	})
	require.NoError(t, err)
	require.True(t, created1)

	second, created2, err := s.InsertIdempotent(ctx, job.NewParams{
		JobType:                 "test.idem.conflict",
		Payload:                 json.RawMessage(`{"version":"DIFFERENT"}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		IdempotencyKey:          &key,
	})
	require.NoError(t, err, "TaskForge does not reject a differing payload under a reused key -- see docs/idempotency.md Open Questions")
	require.False(t, created2)
	require.Equal(t, first.ID, second.ID)
	require.JSONEq(t, `{"version":"original"}`, string(second.Payload),
		"the ORIGINAL submission's payload must win -- the second call's payload is silently ignored, not merged or compared")
}

// TestInsertIdempotent_DuplicateSubmissionAfterDeadLetter_ReturnsExistingJob
// is adversarial case #10: a dead-lettered (terminal) job receiving a
// duplicate submission under its original key must still return that same
// job's current (terminal) representation -- never create a fresh job,
// and never attempt to reopen the terminal row (TF-INV-005 is untouched by
// idempotency logic entirely; InsertIdempotent never mutates an existing
// row).
func TestInsertIdempotent_DuplicateSubmissionAfterDeadLetter_ReturnsExistingJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	key := "dlq-key"
	created, createdFlag, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.dlq", key))
	require.NoError(t, err)
	require.True(t, createdFlag)

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.CompleteFailure(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, "fatal", job.ErrorClassPermanent)
	require.NoError(t, err)

	again, createdAgain, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.dlq", key))
	require.NoError(t, err)
	require.False(t, createdAgain)
	require.Equal(t, created.ID, again.ID)
	require.Equal(t, jobstate.DeadLettered, again.State, "the existing DEAD_LETTERED job's current state is returned, not reopened")
}

// TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005 is
// docs/roadmap.md's Phase 4 quality gate ("Concurrency test with 50+
// simultaneous duplicate-key submissions consistently yields exactly one
// job row") and docs/scenario-corpus.md SF-005: N real goroutines, real
// pooled PostgreSQL connections, racing to submit the identical
// (job_type, idempotency_key) pair. Exactly one job row must exist
// afterward, and every caller must observe the SAME job_id (TF-INV-008,
// proved under true concurrency, not sequential retries -- TF-INV-016's
// mechanism, the database constraint with no preceding check-then-act
// read, is what makes this hold).
func TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const n = 60
	key := "concurrent-key"

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	createdFlags := make([]bool, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, created, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.concurrent", key))
			errs[i] = err
			if err == nil {
				ids[i] = j.ID
				createdFlags[i] = created
			}
		}(i)
	}
	wg.Wait()

	firstCreated := 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i], "every caller must observe the same job_id")
		if createdFlags[i] {
			firstCreated++
		}
	}
	require.Equal(t, 1, firstCreated, "exactly one caller's INSERT must have won the race")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`,
		"test.idem.concurrent", key).Scan(&count))
	require.Equal(t, 1, count, "TF-INV-008 under true concurrency: exactly one job row, not %d", n)
}

// TestInsertIdempotent_SurvivesFreshStoreInstance mirrors
// TestRestart_RunningJobSurvivesFreshStoreInstance: a brand-new
// *store.Store (standing in for a restarted API process, since Store
// holds no state beyond a *sql.DB handle -- no in-memory idempotency
// cache exists anywhere) must still deduplicate a submission against a
// key recorded before the "restart."
func TestInsertIdempotent_SurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	before := store.New(db)
	ctx := context.Background()

	created, createdFlag, err := before.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.restart", "restart-key"))
	require.NoError(t, err)
	require.True(t, createdFlag)

	// Simulate an API process restart: a fresh Store sharing only the
	// durable database, with no in-memory knowledge of the submission
	// above.
	after := store.New(db)

	again, createdAgain, err := after.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.restart", "restart-key"))
	require.NoError(t, err)
	require.False(t, createdAgain, "durability across restart: no in-memory cache is involved, only the database constraint")
	require.Equal(t, created.ID, again.ID)
}

// TestInsertIdempotent_RollbackLeavesNoPartialIdempotencyState is
// adversarial case #2 ("DB transaction aborts after job insert but before
// idempotency operation") made concrete: because idempotency_key is a
// column on the jobs row ITSELF, inserted in the exact same single
// statement/transaction as the job row (there is no separate "idempotency
// mapping" table that could be written independently and diverge --
// TF-INV-013), a rolled-back submission leaves neither a job row NOR an
// idempotency mapping. This is unlike a design with a separate mapping
// table, where "job created but mapping lost" (or vice versa) would be a
// real, distinct failure mode to test for; here it cannot occur by
// construction, and this test proves that construction, not just asserts
// it in prose.
func TestInsertIdempotent_RollbackLeavesNoPartialIdempotencyState(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()
	key := "rollback-key"

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, max_attempts, execution_timeout_seconds, idempotency_key)
		VALUES ($1, 'test.idem.rollback', '{}', 'QUEUED', 5, 30, $2)`,
		uuid.New(), key)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	_, err = s.GetByIdempotencyKey(ctx, "test.idem.rollback", key)
	require.ErrorIs(t, err, store.ErrNotFound,
		"a rolled-back submission must leave no durable job row and no durable idempotency mapping")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE job_type = $1`, "test.idem.rollback").Scan(&count))
	require.Equal(t, 0, count)

	// A fresh submission with the SAME key afterward must succeed cleanly
	// -- the rolled-back attempt must not have left the key "reserved" or
	// otherwise unusable.
	created, createdFlag, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.idem.rollback", key))
	require.NoError(t, err)
	require.True(t, createdFlag)
	require.NotNil(t, created)
}

// TestGetByIdempotencyKey_NotFound proves the plain-read path reports a
// clean ErrNotFound (not e.g. a scan panic or ambiguous error) for a key
// that was never submitted.
func TestGetByIdempotencyKey_NotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.GetByIdempotencyKey(ctx, "test.idem.missing", "never-submitted")
	require.ErrorIs(t, err, store.ErrNotFound)
}
