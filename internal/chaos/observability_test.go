// Observability-under-failure validation (Phase 9 adversarial campaign
// item 20, docs/roadmap.md "Observability During Failure"): Phase 8's
// metrics/logging guarantees must hold while chaos is actively happening
// -- metrics stay race-safe and cardinality-bounded, specific counters
// increment for reclaim/retry/dead-letter/cancellation/timeout/stale-
// rejection/workflow outcomes, and telemetry never becomes authoritative
// (a scrape failure or a metrics-recording bug must never be able to
// change a job's durable outcome -- this is true by construction, per
// internal/metrics's package doc comment: "Recording a metric here can
// never fail or block").
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// gatherFamilies scrapes m's registry into a name-indexed map, mirroring
// internal/metrics's own test helpers' shape, for assertions below.
func gatherFamilies(t *testing.T, m *metrics.Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

func counterValue(f *dto.MetricFamily) float64 {
	if f == nil {
		return 0
	}
	var total float64
	for _, m := range f.GetMetric() {
		if m.GetCounter() != nil {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// TestChaos_ObservabilityDuringFailure_MetricsReflectEveryFaultClass runs
// one job through every fault-driven code path Phase 9 exercises
// elsewhere in this package -- a genuine lease-expiry reclaim, a
// retryable failure, dead-lettering via exhaustion, a stale completion
// rejection, a cancellation, and a timeout -- all against ONE shared
// *metrics.Metrics instance, and asserts each corresponding counter in
// docs/observability.md's table incremented at least once. This is what
// docs/roadmap.md's Phase 9 "Observability During Failure" quality gate
// means concretely: metrics corresponding to reclaim, retry, dead-letter,
// cancellation, timeout, and stale-mutation rejection.
func TestChaos_ObservabilityDuringFailure_MetricsReflectEveryFaultClass(t *testing.T) {
	db := testutil.DB(t)
	m := metrics.New()
	s := store.New(db, store.WithMetrics(m), store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	// Reclaim: claim, force-expire, reclaim.
	reclaimJob, err := s.Insert(ctx, newChaosParams("chaos.obs.reclaim", 5))
	require.NoError(t, err)
	claimed, ok, err := s.Claim(ctx, "obs-w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, chaos.ForceExpireLease(ctx, db, claimed.ID))
	reclaimed, ok, err := s.Claim(ctx, "obs-w2")
	require.NoError(t, err)
	require.True(t, ok)

	// Stale rejection: the original (superseded) generation tries to
	// heartbeat and complete after the reclaim above.
	_, err = s.Heartbeat(ctx, reclaimJob.ID, "obs-w1", claimed.LeaseGeneration, time.Second)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	_, err = s.CompleteSuccess(ctx, reclaimJob.ID, "obs-w1", claimed.LeaseGeneration, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition)
	_, err = s.CompleteSuccess(ctx, reclaimed.ID, "obs-w2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// Retry then exhaustion -> dead-letter.
	dlqJob, err := s.Insert(ctx, newChaosParams("chaos.obs.dlq", 2))
	require.NoError(t, err)
	c1, ok, err := s.Claim(ctx, "obs-dlq-w1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteRetryableFailure(ctx, c1.ID, "obs-dlq-w1", c1.LeaseGeneration, "transient", 0)
	require.NoError(t, err)
	require.NoError(t, chaos.ForceSetEligibleAt(ctx, db, dlqJob.ID, 0))
	c2, ok, err := s.Claim(ctx, "obs-dlq-w2")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteRetryableFailure(ctx, c2.ID, "obs-dlq-w2", c2.LeaseGeneration, "transient again", 0)
	require.NoError(t, err) // attempt_count == max_attempts -> DEAD_LETTERED

	// Cancellation.
	cancelJob, err := s.Insert(ctx, newChaosParams("chaos.obs.cancel", 3))
	require.NoError(t, err)
	cc, ok, err := s.Claim(ctx, "obs-cancel-w")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.RequestCancellation(ctx, cancelJob.ID)
	require.NoError(t, err)
	_, err = s.CompleteCancelled(ctx, cc.ID, "obs-cancel-w", cc.LeaseGeneration)
	require.NoError(t, err)

	// Timeout.
	_, err = s.Insert(ctx, newChaosParams("chaos.obs.timeout", 3))
	require.NoError(t, err)
	tc, ok, err := s.Claim(ctx, "obs-timeout-w")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteTimeout(ctx, tc.ID, "obs-timeout-w", tc.LeaseGeneration, 0)
	require.NoError(t, err)

	fams := gatherFamilies(t, m)
	require.Positive(t, counterValue(fams["taskforge_lease_expirations_total"]), "reclaim must increment taskforge_lease_expirations_total")
	require.Positive(t, counterValue(fams["taskforge_stale_completion_rejections_total"]), "stale writes must increment taskforge_stale_completion_rejections_total")
	require.Positive(t, counterValue(fams["taskforge_jobs_dead_lettered_total"]), "exhaustion must increment taskforge_jobs_dead_lettered_total")
	require.GreaterOrEqual(t, familyLabelSum(fams["taskforge_jobs_completed_total"], "outcome", metrics.OutcomeFailedRetryable), 2.0, "retry attempts must be counted")
	require.Positive(t, familyLabelSum(fams["taskforge_jobs_completed_total"], "outcome", metrics.OutcomeCancelled), "cancellation must increment taskforge_jobs_completed_total{outcome=cancelled}")
	require.Positive(t, familyLabelSum(fams["taskforge_jobs_completed_total"], "outcome", metrics.OutcomeTimedOut), "timeout must increment taskforge_jobs_completed_total{outcome=timed_out}")

	checkNoViolations(t, ctx, checker, -1)
}

// familyLabelSum sums every metric in f whose labelName equals labelValue
// -- a small local helper since dto's label representation is a flat
// slice of {Name, Value} pairs, not a map.
func familyLabelSum(f *dto.MetricFamily, labelName, labelValue string) float64 {
	if f == nil {
		return 0
	}
	var total float64
	for _, m := range f.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == labelName && lp.GetValue() == labelValue {
				if m.GetCounter() != nil {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// TestChaos_ObservabilityDuringFailure_CardinalityBoundedUnderManyJobs
// proves docs/observability.md's cardinality policy holds under chaos
// volume, not just in isolation: many distinct job IDs (and several
// distinct job_types) driven through claim/reclaim/complete concurrently
// must never create a new metric series per job -- only per (job_type,
// outcome), a small, bounded vocabulary regardless of how many jobs ran.
func TestChaos_ObservabilityDuringFailure_CardinalityBoundedUnderManyJobs(t *testing.T) {
	db := testutil.DB(t)
	m := metrics.New()
	s := store.New(db, store.WithMetrics(m), store.WithLogger(discardLogger()))
	ctx := context.Background()

	const numJobs = 200
	const numJobTypes = 4
	for i := 0; i < numJobs; i++ {
		jobType := fmt.Sprintf("chaos.obs.cardinality.%d", i%numJobTypes)
		created, err := s.Insert(ctx, newChaosParams(jobType, 3))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("obs-card-worker-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		_, err = s.CompleteSuccess(ctx, created.ID, fmt.Sprintf("obs-card-worker-%d", i), claimed.LeaseGeneration, nil)
		require.NoError(t, err)
	}

	fams := gatherFamilies(t, m)
	completed := fams["taskforge_jobs_completed_total"]
	require.NotNil(t, completed)
	// At most numJobTypes distinct job_type values, each with a "succeeded"
	// outcome -- never one series per job, per docs/observability.md's
	// cardinality policy (no metric label is ever a job_id).
	require.LessOrEqual(t, len(completed.GetMetric()), numJobTypes,
		"taskforge_jobs_completed_total must have at most one series per (job_type, outcome), never per job")

	seenLabelSets := make(map[string]bool)
	for _, mm := range completed.GetMetric() {
		key := ""
		for _, lp := range mm.GetLabel() {
			key += lp.GetName() + "=" + lp.GetValue() + ";"
			require.NotEqual(t, "job_id", lp.GetName(), "no metric label may be job_id")
			require.NotEqual(t, "idempotency_key", lp.GetName(), "no metric label may be idempotency_key")
		}
		require.False(t, seenLabelSets[key], "duplicate label set %s registered as a separate series", key)
		seenLabelSets[key] = true
	}
}

// TestChaos_ObservabilityDuringFailure_ConcurrentRecordingRaceSafe drives
// real concurrent claim/reclaim/complete/cancel activity against one
// shared *metrics.Metrics instance while concurrently scraping its
// registry, proving metric recording is race-safe under chaos load (run
// under `go test -race`, per docs/roadmap.md's "Concurrency / Race"
// requirement: "Run chaos code under Go race detection").
func TestChaos_ObservabilityDuringFailure_ConcurrentRecordingRaceSafe(t *testing.T) {
	db := testutil.DB(t)
	m := metrics.New()
	s := store.New(db, store.WithMetrics(m), store.WithLogger(discardLogger()))
	ctx := context.Background()

	const numJobs = 80
	const numWorkers = 12
	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newChaosParams("chaos.obs.race", 3))
		require.NoError(t, err)
	}

	scrapeDone := make(chan struct{})
	var scrapeWg sync.WaitGroup
	scrapeWg.Add(1)
	go func() {
		defer scrapeWg.Done()
		for {
			select {
			case <-scrapeDone:
				return
			default:
				_, _ = m.Registry.Gather()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, fmt.Sprintf("obs-race-worker-%d", workerID))
				require.NoError(t, err)
				if !ok {
					return
				}
				owner := *j.LeaseOwner
				switch workerID % 3 {
				case 0:
					_, err = s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
				case 1:
					_, err = s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos", 0)
				case 2:
					_, err = s.Heartbeat(ctx, j.ID, owner, j.LeaseGeneration, time.Second)
					if err == nil {
						_, err = s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
					}
				}
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(scrapeDone)
	scrapeWg.Wait()

	// Drain any RETRY_WAIT jobs left by outcome 1 above so the invariant
	// checker sees a fully converged, terminal-consistent database.
	_, err := chaos.ForceAllEligibleNow(ctx, db, "RETRY_WAIT")
	require.NoError(t, err)
	for {
		j, ok, err := s.Claim(ctx, "obs-race-drain")
		require.NoError(t, err)
		if !ok {
			break
		}
		_, err = s.CompleteSuccess(ctx, j.ID, "obs-race-drain", j.LeaseGeneration, nil)
		require.NoError(t, err)
	}

	checker := invariant.New(db)
	checkNoViolations(t, ctx, checker, -1)
}
