// Phase 8: metric-assertion tests attached to existing scenarios, per
// docs/roadmap.md's Phase 8 entry ("Required tests: Metric-assertion
// tests attached to existing scenarios rather than a new scenario set")
// and its quality gate ("Every metric in docs/observability.md is
// emitted and independently verified against at least one scenario").
// No new SF-* scenario IDs are introduced here -- every test below
// attaches metric assertions to a scenario already named in
// docs/scenario-corpus.md.
package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// newStoreWithMetrics is this file's variant of store_test.go's newStore,
// returning an isolated *metrics.Metrics alongside the Store so each test
// asserts against its own private collector set (per
// internal/metrics.New's doc comment) rather than a shared/global
// registry that repeated or parallel test runs could pollute.
func newStoreWithMetrics(t *testing.T) (*store.Store, *metrics.Metrics, *sql.DB) {
	t.Helper()
	db := testutil.DB(t)
	m := metrics.New()
	return store.New(db, store.WithMetrics(m)), m, db
}

func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var d dto.Metric
	require.NoError(t, c.Write(&d))
	return d.GetCounter().GetValue()
}

// histogramSampleCount accepts anything implementing dto.Metric's Write
// method -- prometheus.Histogram directly, or the prometheus.Observer
// returned by HistogramVec.WithLabelValues, whose concrete type also
// implements it even though the Observer interface itself does not
// declare Write.
func histogramSampleCount(t *testing.T, h any) uint64 {
	t.Helper()
	w, ok := h.(interface{ Write(*dto.Metric) error })
	require.True(t, ok, "%T does not implement Write(*dto.Metric) error", h)
	var d dto.Metric
	require.NoError(t, w.Write(&d))
	return d.GetHistogram().GetSampleCount()
}

// --- SF-001: normal success -------------------------------------------

// TestMetrics_SF001_NormalSuccess attaches metric assertions to SF-001
// (submit, claim, handler succeeds): taskforge_jobs_submitted_total,
// taskforge_claim_latency_seconds/taskforge_queue_age_seconds,
// taskforge_jobs_completed_total{outcome="succeeded"}, and
// taskforge_retry_count all reflect exactly this one attempt.
func TestMetrics_SF001_NormalSuccess(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParams("test.metrics.sf001"))
	require.NoError(t, err)
	require.Equal(t, float64(1), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.metrics.sf001")))

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)
	require.Equal(t, uint64(1), histogramSampleCount(t, m.ClaimLatencySeconds))
	require.Equal(t, uint64(1), histogramSampleCount(t, m.QueueAgeSeconds))

	_, err = s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	require.Equal(t, float64(1),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf001", metrics.OutcomeSucceeded)))
	require.Equal(t, uint64(1),
		histogramSampleCount(t, m.ExecutionDurationSeconds.WithLabelValues("test.metrics.sf001", metrics.OutcomeSucceeded)))
	require.Equal(t, uint64(1), histogramSampleCount(t, m.RetryCount.WithLabelValues("test.metrics.sf001")))
	require.Equal(t, float64(0), counterValue(t, m.JobsDeadLetteredTotal.WithLabelValues("test.metrics.sf001")))
	require.Equal(t, float64(0), counterValue(t, m.StaleCompletionRejectionsTotal))
}

// --- SF-005: idempotent duplicate submission ---------------------------

// TestMetrics_SF005_IdempotentDuplicateSubmission attaches metric
// assertions to SF-005: taskforge_jobs_submitted_total increments on
// EVERY InsertIdempotent call (fresh or duplicate), while
// taskforge_idempotent_submission_hits_total increments only on the
// duplicate.
func TestMetrics_SF005_IdempotentDuplicateSubmission(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	key := "sf005-key"
	first, created, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.metrics.sf005", key))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, float64(1), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.metrics.sf005")))
	require.Equal(t, float64(0), counterValue(t, m.IdempotentSubmissionHitsTotal))

	second, created, err := s.InsertIdempotent(ctx, newJobParamsWithKey("test.metrics.sf005", key))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, float64(2), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.metrics.sf005")))
	require.Equal(t, float64(1), counterValue(t, m.IdempotentSubmissionHitsTotal))
}

// --- SF-007: lease expires and job is reclaimed ------------------------

// TestMetrics_SF007_LeaseExpiresAndJobIsReclaimed attaches metric
// assertions to SF-007: taskforge_lease_expirations_total increments
// exactly once, and the superseded attempt is recorded as
// taskforge_jobs_completed_total{outcome="lease_expired"} -- but only
// for the genuine reclaim. See
// TestMetrics_ReclaimVsRetry_LeaseExpirationsNotConflatedWithBackoffRetry
// below for the companion regression test proving an ordinary
// RETRY_WAIT claim after backoff does NOT also increment this counter.
func TestMetrics_SF007_LeaseExpiresAndJobIsReclaimed(t *testing.T) {
	s, m, db := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf007", 5))
	require.NoError(t, err)

	first, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, float64(0), counterValue(t, m.LeaseExpirationsTotal.WithLabelValues("test.metrics.sf007")))

	forceExpireLease(t, db, created.ID)

	second, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), second.LeaseGeneration)

	require.Equal(t, float64(1), counterValue(t, m.LeaseExpirationsTotal.WithLabelValues("test.metrics.sf007")))
	require.Equal(t, float64(1),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf007", metrics.OutcomeLeaseExpired)))
	require.Equal(t, uint64(2), histogramSampleCount(t, m.ClaimLatencySeconds), "both the original claim and the reclaim are claims")
	_ = first
}

// --- SF-008: stale worker attempts completion after losing its lease --

// TestMetrics_SF008_StaleCompletionRejected attaches metric assertions
// to SF-008: taskforge_stale_completion_rejections_total increments when
// a stale (superseded) generation's completion call is rejected by the
// fencing check, and the job's genuine completion is unaffected.
func TestMetrics_SF008_StaleCompletionRejected(t *testing.T) {
	s, m, db := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf008", 5))
	require.NoError(t, err)

	firstClaim, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	forceExpireLease(t, db, created.ID)

	secondClaim, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, secondClaim.ID, "worker-B", secondClaim.LeaseGeneration, nil)
	require.NoError(t, err)

	require.Equal(t, float64(0), counterValue(t, m.StaleCompletionRejectionsTotal))

	// Worker A's stale (generation-1) completion call arrives late.
	_, err = s.CompleteSuccess(ctx, firstClaim.ID, "worker-A", firstClaim.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	require.Equal(t, float64(1), counterValue(t, m.StaleCompletionRejectionsTotal))

	// Heartbeat and every other fenced completion path are covered too --
	// a stale heartbeat call must also be counted (docs/observability.md
	// scopes this metric to "completion/heartbeat calls").
	_, err = s.Heartbeat(ctx, firstClaim.ID, "worker-A", firstClaim.LeaseGeneration, 30*time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	require.Equal(t, float64(2), counterValue(t, m.StaleCompletionRejectionsTotal))
	require.Equal(t, float64(1), counterValue(t, m.HeartbeatsTotal), "HeartbeatsTotal counts every call, including this rejected one")
}

// --- SF-009 / SF-010: retries and dead-lettering -----------------------

// TestMetrics_SF009_RetryableFailureEventuallySucceeds attaches metric
// assertions to SF-009: each retryable failure increments
// taskforge_jobs_completed_total{outcome="failed_retryable"}, and the
// eventual success increments {outcome="succeeded"} with
// taskforge_retry_count observing the final attempt_count.
func TestMetrics_SF009_RetryableFailureEventuallySucceeds(t *testing.T) {
	s, m, db := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf009", 5))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		claimed, ok, err := s.Claim(ctx, "worker-A")
		require.NoError(t, err)
		require.True(t, ok)
		_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, "transient", time.Millisecond)
		require.NoError(t, err)
		forceSetEligibleAt(t, db, created.ID, 0)
	}
	require.Equal(t, float64(2),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf009", metrics.OutcomeFailedRetryable)))
	require.Equal(t, float64(0), counterValue(t, m.JobsDeadLetteredTotal.WithLabelValues("test.metrics.sf009")))

	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 3, claimed.AttemptCount)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	require.Equal(t, float64(1),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf009", metrics.OutcomeSucceeded)))
	require.Equal(t, uint64(1), histogramSampleCount(t, m.RetryCount.WithLabelValues("test.metrics.sf009")),
		"RetryCount observes only at terminal state -- once, at the eventual success")
}

// TestMetrics_SF010_RetriesExhaustedDeadLetters attaches metric
// assertions to SF-010: every attempt (including the exhausting one)
// counts as taskforge_jobs_completed_total{outcome="failed_retryable"},
// but taskforge_jobs_dead_lettered_total and taskforge_retry_count fire
// exactly once, at exhaustion.
func TestMetrics_SF010_RetriesExhaustedDeadLetters(t *testing.T) {
	s, m, db := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf010", 3))
	require.NoError(t, err)

	var last *job.Job
	for i := 0; i < 3; i++ {
		claimed, ok, err := s.Claim(ctx, "worker-A")
		require.NoError(t, err)
		require.True(t, ok)
		last, err = s.CompleteRetryableFailure(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, "still failing", time.Millisecond)
		require.NoError(t, err)
		if i < 2 {
			forceSetEligibleAt(t, db, created.ID, 0)
		}
	}
	require.NotNil(t, last)

	require.Equal(t, float64(3),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf010", metrics.OutcomeFailedRetryable)))
	require.Equal(t, float64(1), counterValue(t, m.JobsDeadLetteredTotal.WithLabelValues("test.metrics.sf010")))
	require.Equal(t, uint64(1), histogramSampleCount(t, m.RetryCount.WithLabelValues("test.metrics.sf010")))
}

// --- SF-011 / SF-012: cancellation --------------------------------------

// TestMetrics_SF011_CancellationBeforeClaim attaches metric assertions to
// SF-011: a pre-claim cancellation has no job_attempts row, so it
// contributes to taskforge_retry_count (attempt_count == 0) but not to
// taskforge_jobs_completed_total (which is scoped to attempt-level
// outcomes -- docs/observability.md).
func TestMetrics_SF011_CancellationBeforeClaim(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf011", 5))
	require.NoError(t, err)

	_, err = s.CancelQueuedOrRetryWait(ctx, created.ID)
	require.NoError(t, err)

	require.Equal(t, uint64(1), histogramSampleCount(t, m.RetryCount.WithLabelValues("test.metrics.sf011")))
	require.Equal(t, float64(0),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf011", metrics.OutcomeCancelled)))
}

// TestMetrics_SF012_CancelCommitsFirst_CompletionRejected attaches metric
// assertions to SF-012's cancel-wins interleaving: the worker's later
// success report is rejected as stale (fencing, not merely "already
// cancelled" -- CompleteCancelled/CompleteSuccess share the same
// WHERE state='RUNNING' guard) and counted accordingly.
func TestMetrics_SF012_CancelCommitsFirst_CompletionRejected(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.sf012", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)
	_, err = s.CompleteCancelled(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration)
	require.NoError(t, err)

	require.Equal(t, float64(1),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf012", metrics.OutcomeCancelled)))

	_, err = s.CompleteSuccess(ctx, claimed.ID, "worker-A", claimed.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	require.Equal(t, float64(1), counterValue(t, m.StaleCompletionRejectionsTotal))
	require.Equal(t, float64(0),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.sf012", metrics.OutcomeSucceeded)),
		"the rejected stale completion must never be recorded as a genuine success")
}

// --- Defect fix: reclaim vs. ordinary retry conflation ------------------

// TestMetrics_ReclaimVsRetry_LeaseExpirationsNotConflatedWithBackoffRetry
// is a regression test for a defect found during Phase 8 implementation:
// prior to this phase, internal/worker logged (and would have metered,
// had metrics existed then) every claim with lease_generation > 1 as a
// "reclaim after lease expiration" -- but lease_generation and
// attempt_count are incremented together on EVERY claim, fresh or
// reclaimed, so that heuristic also fired on an ordinary RETRY_WAIT claim
// after backoff (no crash, no lease loss at all). This test drives a job
// through one genuine retryable-failure-then-backoff-claim cycle (no
// lease expiry involved) and asserts taskforge_lease_expirations_total
// stays at zero throughout, while a companion claim that genuinely
// reclaims an expired lease (SF-007, above) does increment it -- proving
// the two are now distinguished correctly.
func TestMetrics_ReclaimVsRetry_LeaseExpirationsNotConflatedWithBackoffRetry(t *testing.T) {
	s, m, db := newStoreWithMetrics(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsN("test.metrics.reclaimvsretry", 5))
	require.NoError(t, err)

	first, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), first.LeaseGeneration)

	_, err = s.CompleteRetryableFailure(ctx, first.ID, "worker-A", first.LeaseGeneration, "transient", time.Millisecond)
	require.NoError(t, err)
	forceSetEligibleAt(t, db, created.ID, 0)

	// This claim's lease_generation will be 2 -- identical to what a
	// genuine reclaim would look like from the caller's perspective --
	// but no lease ever expired here; the job cycled through RETRY_WAIT
	// on schedule. No worker crashed, so this must NOT be counted as a
	// lease expiration.
	second, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), second.LeaseGeneration, "sanity: lease_generation advanced exactly like a reclaim would")

	require.Equal(t, float64(0), counterValue(t, m.LeaseExpirationsTotal.WithLabelValues("test.metrics.reclaimvsretry")),
		"an ordinary post-backoff RETRY_WAIT claim must never be counted as a lease expiration")
	require.Equal(t, float64(0),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.reclaimvsretry", metrics.OutcomeLeaseExpired)))
}

// --- taskforge_jobs_by_state / taskforge_active_workers -----------------

// TestJobStateCounts proves taskforge_jobs_by_state's underlying query
// reflects durable state exactly, queried fresh (docs/roadmap.md's
// "STATE METRICS" requirement) -- not an in-memory count that could
// drift.
func TestJobStateCounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := s.Insert(ctx, newJobParams(fmt.Sprintf("test.statecounts.%d", i)))
		require.NoError(t, err)
	}
	counts, err := s.JobStateCounts(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), counts["QUEUED"])
	require.Equal(t, int64(0), counts["RUNNING"])
}

// TestJobStateCounts_ReflectsRestartAcrossFreshStoreInstance proves the
// gauge cannot drift across a process restart: a fresh *store.Store
// sharing only the database sees the same counts as the instance that
// wrote them, with no in-memory state to lose or recover (mirrors
// SF-018's "no in-memory state holds anything correctness-relevant"
// shape, applied to this diagnostic gauge).
func TestJobStateCounts_ReflectsRestartAcrossFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	s1 := store.New(db)
	ctx := context.Background()

	_, err := s1.Insert(ctx, newJobParams("test.statecounts.restart"))
	require.NoError(t, err)

	s2 := store.New(db)
	counts, err := s2.JobStateCounts(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), counts["QUEUED"])
}

// TestActiveWorkerCount proves taskforge_active_workers counts distinct
// lease_owner values with a recent heartbeat, computed fresh from
// durable state at call time.
func TestActiveWorkerCount(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	count, err := s.ActiveWorkerCount(ctx, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)

	_, err = s.Insert(ctx, newJobParams("test.activeworkers"))
	require.NoError(t, err)
	_, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)

	count, err = s.ActiveWorkerCount(ctx, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	// Outside the window, the same worker no longer counts as active --
	// proves this is a live, windowed read, not a sticky "ever seen"
	// count.
	count, err = s.ActiveWorkerCount(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)
}

// --- Cardinality audit --------------------------------------------------

// TestCardinality_ManyUniqueJobsDoNotCreateNewMetricSeries is this
// phase's adversarial-audit item #2: 100 jobs with unique IDs must not
// grow the metric series count -- every counter/histogram here is
// labeled only by job_type (a single fixed value in this test) and/or
// outcome (a small fixed enum), never by job_id.
func TestCardinality_ManyUniqueJobsDoNotCreateNewMetricSeries(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	const n = 100
	for i := 0; i < n; i++ {
		created, err := s.Insert(ctx, newJobParams("test.cardinality.fixed_type"))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("worker-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, created.ID, claimed.ID)
		_, err = s.CompleteSuccess(ctx, claimed.ID, fmt.Sprintf("worker-%d", i), claimed.LeaseGeneration, nil)
		require.NoError(t, err)
	}

	require.Equal(t, float64(n), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.cardinality.fixed_type")))
	require.Equal(t, float64(n),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.cardinality.fixed_type", metrics.OutcomeSucceeded)))

	// Exactly one series per (metric name, label combination) -- not one
	// per job_id or per worker_id, despite 100 unique job IDs and 100
	// unique worker IDs having flowed through the system above.
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() == "taskforge_jobs_submitted_total" || fam.GetName() == "taskforge_jobs_completed_total" {
			require.LessOrEqual(t, len(fam.GetMetric()), 6,
				"metric %s must not grow a series per job_id/worker_id", fam.GetName())
		}
	}
}

// TestCardinality_NoMetricLabelIsIdempotencyKeyOrJobID audits every
// registered metric family's label names against the documented
// cardinality policy: job_id, workflow_id, idempotency_key, and raw
// error text must never appear as a label name anywhere.
func TestCardinality_NoMetricLabelIsIdempotencyKeyOrJobID(t *testing.T) {
	m := metrics.New()
	families, err := m.Registry.Gather()
	require.NoError(t, err)

	forbidden := map[string]bool{
		"job_id": true, "workflow_id": true, "node_id": true,
		"idempotency_key": true, "worker_id": true, "request_id": true,
		"error_message": true, "error": true,
	}
	// Force every metric to have at least a zero-value series registered
	// so label names are discoverable even before any Inc()/Observe()
	// call -- CounterVec/HistogramVec with no observations yet report no
	// series at all via Gather, so exercise each one with a bounded,
	// fixed label set first.
	m.JobsSubmittedTotal.WithLabelValues("t").Inc()
	m.JobsCompletedTotal.WithLabelValues("t", "succeeded").Inc()
	m.JobsDeadLetteredTotal.WithLabelValues("t").Inc()
	m.ExecutionDurationSeconds.WithLabelValues("t", "succeeded").Observe(0)
	m.LeaseExpirationsTotal.WithLabelValues("t").Inc()
	m.RetryCount.WithLabelValues("t").Observe(1)

	families, err = m.Registry.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				require.False(t, forbidden[lp.GetName()],
					"metric %s must not carry label %q (docs/observability.md cardinality policy)", fam.GetName(), lp.GetName())
			}
		}
	}
}

// --- Concurrency / race safety ------------------------------------------

// TestMetrics_ConcurrentRecording_RaceSafe drives many goroutines through
// real Claim/CompleteSuccess calls concurrently against a shared
// *metrics.Metrics, proving the metrics recording added in this phase
// introduces no data race (run this test under -race, per
// docs/testing-strategy.md) and that final counter totals are exactly
// consistent with the number of completions performed -- no lost
// updates.
func TestMetrics_ConcurrentRecording_RaceSafe(t *testing.T) {
	s, m, _ := newStoreWithMetrics(t)
	ctx := context.Background()

	const n = 50
	for i := 0; i < n; i++ {
		_, err := s.Insert(ctx, newJobParams("test.metrics.concurrent"))
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, ok, err := s.Claim(ctx, fmt.Sprintf("race-worker-%03d", i))
			require.NoError(t, err)
			if !ok {
				return
			}
			_, err = s.CompleteSuccess(ctx, claimed.ID, fmt.Sprintf("race-worker-%03d", i), claimed.LeaseGeneration, nil)
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()

	require.Equal(t, float64(n), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.metrics.concurrent")))
	require.Equal(t, float64(n),
		counterValue(t, m.JobsCompletedTotal.WithLabelValues("test.metrics.concurrent", metrics.OutcomeSucceeded)))
	require.Equal(t, uint64(n), histogramSampleCount(t, m.ClaimLatencySeconds))
}
