package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
)

// TestNew_IsolatedPerInstance proves two independently constructed
// Metrics instances never share state -- the mechanism that lets each
// test in internal/store assert against its own private collector set
// without cross-test interference (see internal/metrics.New's doc
// comment).
func TestNew_IsolatedPerInstance(t *testing.T) {
	a := metrics.New()
	b := metrics.New()

	a.JobsSubmittedTotal.WithLabelValues("t").Inc()

	famA, err := a.Registry.Gather()
	require.NoError(t, err)
	famB, err := b.Registry.Gather()
	require.NoError(t, err)

	require.NotEmpty(t, famA)
	for _, fam := range famB {
		if fam.GetName() == "taskforge_jobs_submitted_total" {
			require.Empty(t, fam.GetMetric(), "b must not observe a's increment")
		}
	}
}

// TestNew_RegistersExactlyTheDocumentedMetrics asserts every metric name
// in docs/observability.md's table is registered, and nothing else --
// this package must not invent a second observability model.
func TestNew_RegistersExactlyTheDocumentedMetrics(t *testing.T) {
	m := metrics.New()

	// Force every metric to report at least one series so Gather
	// discovers its family (a *Vec with no observations yet reports no
	// series).
	m.JobsSubmittedTotal.WithLabelValues("t").Inc()
	m.JobsCompletedTotal.WithLabelValues("t", "succeeded").Inc()
	m.JobsDeadLetteredTotal.WithLabelValues("t").Inc()
	m.ClaimLatencySeconds.Observe(0)
	m.QueueAgeSeconds.Observe(0)
	m.ExecutionDurationSeconds.WithLabelValues("t", "succeeded").Observe(0)
	m.LeaseExpirationsTotal.WithLabelValues("t").Inc()
	m.StaleCompletionRejectionsTotal.Inc()
	m.RetryCount.WithLabelValues("t").Observe(1)
	m.HeartbeatsTotal.Inc()
	m.IdempotentSubmissionHitsTotal.Inc()

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	want := map[string]bool{
		"taskforge_jobs_submitted_total":              true,
		"taskforge_jobs_completed_total":              true,
		"taskforge_jobs_dead_lettered_total":          true,
		"taskforge_claim_latency_seconds":             true,
		"taskforge_queue_age_seconds":                 true,
		"taskforge_execution_duration_seconds":        true,
		"taskforge_lease_expirations_total":           true,
		"taskforge_stale_completion_rejections_total": true,
		"taskforge_retry_count":                       true,
		"taskforge_heartbeats_total":                  true,
		"taskforge_idempotent_submission_hits_total":  true,
	}
	got := make(map[string]bool, len(families))
	for _, fam := range families {
		got[fam.GetName()] = true
	}
	require.Equal(t, want, got)
}

// fakeQuerier is a deterministic, in-memory StateQuerier double, used to
// test stateCollector without a real database -- the metrics package
// itself has no PostgreSQL dependency (only internal/store does), so its
// own tests should not require one either.
type fakeQuerier struct {
	counts    map[string]int64
	countsErr error
	active    int64
	activeErr error
	sawWindow time.Duration
}

func (f *fakeQuerier) JobStateCounts(ctx context.Context) (map[string]int64, error) {
	return f.counts, f.countsErr
}

func (f *fakeQuerier) ActiveWorkerCount(ctx context.Context, window time.Duration) (int64, error) {
	f.sawWindow = window
	return f.active, f.activeErr
}

// TestStateCollector_ReportsFreshQueryEachScrape proves
// taskforge_jobs_by_state/taskforge_active_workers are computed fresh at
// every Collect call, per docs/roadmap.md's "STATE METRICS" requirement
// -- changing the querier's underlying values between two scrapes must
// change what is reported, unlike an in-memory counter that could drift.
func TestStateCollector_ReportsFreshQueryEachScrape(t *testing.T) {
	q := &fakeQuerier{counts: map[string]int64{"QUEUED": 3, "RUNNING": 1}, active: 2}
	m := metrics.New()
	m.Registry.MustRegister(metrics.NewStateCollector(q, 30*time.Second, nil))

	families, err := m.Registry.Gather()
	require.NoError(t, err)
	assertGauge(t, families, "taskforge_jobs_by_state", "QUEUED", 3)
	assertGauge(t, families, "taskforge_jobs_by_state", "RUNNING", 1)
	assertGauge(t, families, "taskforge_active_workers", "", 2)

	q.counts = map[string]int64{"QUEUED": 99}
	q.active = 7
	families, err = m.Registry.Gather()
	require.NoError(t, err)
	assertGauge(t, families, "taskforge_jobs_by_state", "QUEUED", 99)
	assertGauge(t, families, "taskforge_active_workers", "", 7)
}

// TestStateCollector_QueryFailureDoesNotPanicOrBlock is this phase's
// telemetry-failure-behavior requirement: a scrape-time query failure
// (e.g. the database briefly unreachable) must not panic, must not
// block, and must simply omit the affected series -- it never touches
// any correctness-critical code path (nothing here runs on a job's
// transition path at all).
func TestStateCollector_QueryFailureDoesNotPanicOrBlock(t *testing.T) {
	q := &fakeQuerier{countsErr: errors.New("db unreachable"), activeErr: errors.New("db unreachable")}
	m := metrics.New()
	m.Registry.MustRegister(metrics.NewStateCollector(q, 30*time.Second, nil))

	require.NotPanics(t, func() {
		families, err := m.Registry.Gather()
		require.NoError(t, err)
		for _, fam := range families {
			require.NotContains(t, []string{"taskforge_jobs_by_state", "taskforge_active_workers"}, fam.GetName())
		}
	})
}

func assertGauge(t *testing.T, families []*dto.MetricFamily, name, labelValue string, want float64) {
	t.Helper()
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, met := range fam.GetMetric() {
			if labelValue == "" {
				require.Equal(t, want, met.GetGauge().GetValue())
				return
			}
			for _, lp := range met.GetLabel() {
				if lp.GetValue() == labelValue {
					require.Equal(t, want, met.GetGauge().GetValue())
					return
				}
			}
		}
	}
	t.Fatalf("metric %s{%s} not found", name, labelValue)
}
