// Integration tests against a real PostgreSQL instance (internal/testutil),
// per docs/testing-strategy.md.
package governance_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/governance"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func TestSetConcurrencyLimit_ProvisionsAndDeprovisionsSlots(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	five := 5
	require.NoError(t, g.SetConcurrencyLimit(ctx, "q1", &five))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = 'q1'`).Scan(&count))
	require.Equal(t, 5, count)

	three := 3
	require.NoError(t, g.SetConcurrencyLimit(ctx, "q1", &three))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = 'q1'`).Scan(&count))
	require.Equal(t, 3, count, "shrinking must remove the excess free slots")

	seven := 7
	require.NoError(t, g.SetConcurrencyLimit(ctx, "q1", &seven))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = 'q1'`).Scan(&count))
	require.Equal(t, 7, count, "growing must add the missing slots")

	require.NoError(t, g.SetConcurrencyLimit(ctx, "q1", nil))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = 'q1'`).Scan(&count))
	require.Equal(t, 0, count, "unlimited must deprovision every free slot")

	limit, err := g.GetQueueLimit(ctx, "q1")
	require.NoError(t, err)
	require.Nil(t, limit.ConcurrencyLimit)
}

func TestSetConcurrencyLimit_IsIdempotent(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	five := 5
	require.NoError(t, g.SetConcurrencyLimit(ctx, "idempotent", &five))
	require.NoError(t, g.SetConcurrencyLimit(ctx, "idempotent", &five))
	require.NoError(t, g.SetConcurrencyLimit(ctx, "idempotent", &five))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = 'idempotent'`).Scan(&count))
	require.Equal(t, 5, count, "re-running the same limit must converge, never accumulate or duplicate slots")

	var limitCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_limits WHERE queue_name = 'idempotent'`).Scan(&limitCount))
	require.Equal(t, 1, limitCount, "must not accumulate duplicate queue_limits rows either")
}

func TestSetConcurrencyLimit_DoesNotEvictAHeldSlot(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	five := 5
	require.NoError(t, g.SetConcurrencyLimit(ctx, "shrink-held", &five))

	// A real job row, since queue_slots.held_by_job_id has a foreign key
	// into jobs(id).
	jobID := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, queue_name)
		VALUES ($1, $2, 'test.governance.held', '{}', 'RUNNING', 30, 'shrink-held')`,
		jobID, principal.SystemPrincipalID)
	require.NoError(t, err)

	// Simulate slot 4 being held by that running job.
	_, err = db.ExecContext(ctx, `UPDATE queue_slots SET held_by_job_id = $2 WHERE queue_name = $1 AND slot_index = 4`, "shrink-held", jobID)
	require.NoError(t, err)

	one := 1
	require.NoError(t, g.SetConcurrencyLimit(ctx, "shrink-held", &one))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_slots WHERE queue_name = $1`, "shrink-held").Scan(&count))
	require.Equal(t, 2, count, "slot 0 (in range) plus the still-held slot 4 (out of range but not evicted) must both remain")

	var stillHeld bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT held_by_job_id IS NOT NULL FROM queue_slots WHERE queue_name = $1 AND slot_index = 4`, "shrink-held").Scan(&stillHeld))
	require.True(t, stillHeld, "a held slot must never be evicted by a concurrency-limit shrink")
}

func TestSetConcurrencyLimit_RejectsZero(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	zero := 0
	err := g.SetConcurrencyLimit(ctx, "zero-test", &zero)
	require.ErrorIs(t, err, governance.ErrZeroConcurrencyLimit)

	// Must fail closed, not silently create an unlimited or a zero-slot
	// row.
	limit, err := g.GetQueueLimit(ctx, "zero-test")
	require.NoError(t, err)
	require.Nil(t, limit, "a rejected configuration attempt must leave no queue_limits row behind")
}

func TestSetRateLimit_SeedsBucketAndPersists(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	require.NoError(t, g.SetRateLimit(ctx, "rated", 10, 5))

	limit, err := g.GetQueueLimit(ctx, "rated")
	require.NoError(t, err)
	require.NotNil(t, limit.RateLimitPerSec)
	require.Equal(t, 10.0, *limit.RateLimitPerSec)
	require.NotNil(t, limit.RateLimitBurst)
	require.Equal(t, 5, *limit.RateLimitBurst)

	var tokens float64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT tokens FROM rate_limit_buckets WHERE scope_key = $1`, governance.RateLimitScopeKey("rated")).Scan(&tokens))
	require.Equal(t, 5.0, tokens)
}

func TestCheckAndConsumeRateLimit_AllowsUpToBurstThenRejects(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	scopeKey := governance.RateLimitScopeKey("burst-test")
	for i := 0; i < 3; i++ {
		allowed, _, err := g.CheckAndConsumeRateLimit(ctx, scopeKey, 1, 3)
		require.NoError(t, err)
		require.True(t, allowed, "request %d within burst must be allowed", i)
	}

	allowed, retryAfter, err := g.CheckAndConsumeRateLimit(ctx, scopeKey, 1, 3)
	require.NoError(t, err)
	require.False(t, allowed, "the 4th request beyond burst must be rejected")
	require.Greater(t, retryAfter, time.Duration(0))
	require.LessOrEqual(t, retryAfter, 2*time.Second)
}

func TestCheckAndConsumeRateLimit_RefillsOverTime(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	scopeKey := governance.RateLimitScopeKey("refill-test")
	allowed, _, err := g.CheckAndConsumeRateLimit(ctx, scopeKey, 100, 1)
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, _, err = g.CheckAndConsumeRateLimit(ctx, scopeKey, 100, 1)
	require.NoError(t, err)
	require.False(t, allowed, "burst of 1 is exhausted immediately after the first request")

	// Force last_refill_at back in time via PostgreSQL's own clock (no
	// sleep), simulating enough elapsed time for a full refill at 100/s.
	_, err = db.ExecContext(ctx, `UPDATE rate_limit_buckets SET last_refill_at = now() - interval '1 second' WHERE scope_key = $1`, scopeKey)
	require.NoError(t, err)

	allowed, _, err = g.CheckAndConsumeRateLimit(ctx, scopeKey, 100, 1)
	require.NoError(t, err)
	require.True(t, allowed, "a full second at 100/s must refill well past 1 token")
}

// TestCheckAndConsumeRateLimit_ToleratesBackwardClockJump guards against a
// regression where a non-monotonic wall clock (an NTP or VM-host clock
// correction -- PostgreSQL's now() is wall-clock time, not monotonic, per
// docs/failure-model.md's Clock Model) causes now() - last_refill_at to go
// briefly negative. Before the fix, that negative elapsed time was
// multiplied by ratePerSec and added directly into the refill computation
// -- i.e. it SUBTRACTED tokens -- so a backward jump could spuriously deny
// a request the bucket already held a token for. This is the suspected
// root cause of TestCheckAndConsumeRateLimit_RefillsOverTime's single
// observed flake: that test's own backward-in-time UPDATE only pushes
// last_refill_at 1 second into the past, and a sufficiently large backward
// clock jump between that UPDATE and the following check would reproduce
// exactly this failure without any product code being wrong in the
// ordinary (monotonic-clock) case.
func TestCheckAndConsumeRateLimit_ToleratesBackwardClockJump(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	scopeKey := governance.RateLimitScopeKey("backward-clock-test")

	// Seed the bucket directly with exactly 1 of 10 tokens and
	// last_refill_at = now(): a state that already qualifies for one more
	// request with zero additional elapsed time.
	_, err := db.ExecContext(ctx, `INSERT INTO rate_limit_buckets (scope_key, tokens, last_refill_at) VALUES ($1, 1, now())`, scopeKey)
	require.NoError(t, err)

	// Simulate the wall clock having gone backward by 100ms since that
	// write by moving last_refill_at 100ms into the future relative to
	// now() -- this produces exactly the same now() - last_refill_at < 0
	// condition a real backward jump would, deterministically and without
	// a sleep.
	_, err = db.ExecContext(ctx, `UPDATE rate_limit_buckets SET last_refill_at = last_refill_at + interval '100 milliseconds' WHERE scope_key = $1`, scopeKey)
	require.NoError(t, err)

	// Real elapsed time is ~0 and the bucket already held exactly 1
	// token, so this must be allowed regardless of the simulated backward
	// clock jump: negative elapsed time must never subtract tokens.
	allowed, _, err := g.CheckAndConsumeRateLimit(ctx, scopeKey, 1, 10)
	require.NoError(t, err)
	require.True(t, allowed, "a backward clock jump must not subtract tokens from a bucket that already qualified")
}

func TestCheckAndConsumeRateLimit_SurvivesAcrossFreshStoreInstance(t *testing.T) {
	// Durability: rate_limit_buckets is a PostgreSQL table, never
	// in-memory-only (docs/phase-13-plan.md §7/§13).
	db := testutil.DB(t)
	ctx := context.Background()

	scopeKey := governance.RateLimitScopeKey("durable-test")
	g1 := governance.New(db)
	allowed, _, err := g1.CheckAndConsumeRateLimit(ctx, scopeKey, 1, 1)
	require.NoError(t, err)
	require.True(t, allowed)

	// A brand new Store instance (stand-in for a process restart) sees
	// the exhausted bucket.
	g2 := governance.New(db)
	allowed, _, err = g2.CheckAndConsumeRateLimit(ctx, scopeKey, 1, 1)
	require.NoError(t, err)
	require.False(t, allowed, "rate-limit state must survive across a fresh Store instance (process restart stand-in)")
}

func TestCheckAndConsumeRateLimit_ConcurrentRequestsNeverExceedBurst(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	const burst = 10
	const attempts = 50
	scopeKey := governance.RateLimitScopeKey("concurrent-test")

	start := make(chan struct{})
	results := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			<-start
			allowed, _, err := g.CheckAndConsumeRateLimit(ctx, scopeKey, 0.001, burst)
			require.NoError(t, err)
			results <- allowed
		}()
	}
	close(start)

	allowedCount := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			allowedCount++
		}
	}
	require.Equal(t, burst, allowedCount, "exactly `burst` concurrent requests must be allowed when the rate is negligible, never more")
}

func TestListQueueState_ReflectsConfigurationAndLiveState(t *testing.T) {
	db := testutil.DB(t)
	g := governance.New(db)
	ctx := context.Background()

	five := 5
	require.NoError(t, g.SetConcurrencyLimit(ctx, "snapshot-q", &five))
	require.NoError(t, g.SetRateLimit(ctx, "snapshot-q", 2, 4))

	for _, idx := range []int{0, 1} {
		jobID := uuid.New()
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, queue_name)
			VALUES ($1, $2, 'test.governance.snapshot', '{}', 'RUNNING', 30, 'snapshot-q')`,
			jobID, principal.SystemPrincipalID)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE queue_slots SET held_by_job_id = $1 WHERE queue_name = 'snapshot-q' AND slot_index = $2`, jobID, idx)
		require.NoError(t, err)
	}

	states, err := g.ListQueueState(ctx)
	require.NoError(t, err)
	var found *governance.QueueState
	for i := range states {
		if states[i].QueueName == "snapshot-q" {
			found = &states[i]
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.ConcurrencyLimit)
	require.Equal(t, 5, *found.ConcurrencyLimit)
	require.Equal(t, 5, found.SlotsTotal)
	require.Equal(t, 2, found.SlotsHeld)
	require.NotNil(t, found.RateLimitPerSec)
	require.Equal(t, 2.0, *found.RateLimitPerSec)
}
