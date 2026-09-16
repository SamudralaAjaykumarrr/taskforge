// Phase 13 (checkpoint 7): taskforge_queue_depth/taskforge_queue_running/
// taskforge_queue_concurrency_limit collector proofs, using fake queriers
// rather than a real database -- mirroring stateCollector's own test
// pattern in metrics_test.go exactly.
package metrics_test

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
)

type fakeQueueDepthQuerier struct {
	counts map[string]map[string]int64
	err    error
}

func (f *fakeQueueDepthQuerier) QueueDepthAndRunningCounts(ctx context.Context) (map[string]map[string]int64, error) {
	return f.counts, f.err
}

type fakeQueueLimitQuerier struct {
	limits map[string]int
	err    error
}

func (f *fakeQueueLimitQuerier) ConcurrencyLimits(ctx context.Context) (map[string]int, error) {
	return f.limits, f.err
}

func TestQueueStateCollector_ReportsDepthRunningAndLimits(t *testing.T) {
	depth := &fakeQueueDepthQuerier{counts: map[string]map[string]int64{
		"reports": {"QUEUED": 5, "RUNNING": 2},
		"emails":  {"RUNNING": 1},
	}}
	limits := &fakeQueueLimitQuerier{limits: map[string]int{"reports": 3}}

	m := metrics.New()
	m.Registry.MustRegister(metrics.NewQueueStateCollector(depth, limits, nil))

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	assertQueueGauge(t, families, "taskforge_queue_depth", "reports", "QUEUED", 5)
	assertQueueGauge(t, families, "taskforge_queue_depth", "reports", "RUNNING", 2)
	assertQueueGauge(t, families, "taskforge_queue_depth", "emails", "RUNNING", 1)
	assertQueueRunning(t, families, "reports", 2)
	assertQueueRunning(t, families, "emails", 1)
	assertQueueLimit(t, families, "reports", 3)

	// "emails" has no configured limit -- it must have NO series at all
	// for taskforge_queue_concurrency_limit, never a fabricated
	// "unlimited" value.
	for _, fam := range families {
		if fam.GetName() != "taskforge_queue_concurrency_limit" {
			continue
		}
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if lp.GetName() == "queue_name" {
					require.NotEqual(t, "emails", lp.GetValue(), "an unconfigured queue must have no concurrency_limit series")
				}
			}
		}
	}
}

func TestQueueStateCollector_QueryFailureDoesNotPanicOrBlock(t *testing.T) {
	depth := &fakeQueueDepthQuerier{err: context.DeadlineExceeded}
	limits := &fakeQueueLimitQuerier{err: context.DeadlineExceeded}

	m := metrics.New()
	m.Registry.MustRegister(metrics.NewQueueStateCollector(depth, limits, nil))

	require.NotPanics(t, func() {
		_, err := m.Registry.Gather()
		require.NoError(t, err, "a scrape-time query failure must be swallowed (logged), never surfaced as a Gather error")
	})
}

func assertQueueGauge(t *testing.T, families []*dto.MetricFamily, name, queueName, state string, want float64) {
	t.Helper()
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, met := range fam.GetMetric() {
			var gotQueue, gotState string
			for _, lp := range met.GetLabel() {
				switch lp.GetName() {
				case "queue_name":
					gotQueue = lp.GetValue()
				case "state":
					gotState = lp.GetValue()
				}
			}
			if gotQueue == queueName && gotState == state {
				require.Equal(t, want, met.GetGauge().GetValue())
				return
			}
		}
	}
	t.Fatalf("metric %s{queue_name=%s,state=%s} not found", name, queueName, state)
}

func assertQueueRunning(t *testing.T, families []*dto.MetricFamily, queueName string, want float64) {
	t.Helper()
	for _, fam := range families {
		if fam.GetName() != "taskforge_queue_running" {
			continue
		}
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if lp.GetName() == "queue_name" && lp.GetValue() == queueName {
					require.Equal(t, want, met.GetGauge().GetValue())
					return
				}
			}
		}
	}
	t.Fatalf("metric taskforge_queue_running{queue_name=%s} not found", queueName)
}

func assertQueueLimit(t *testing.T, families []*dto.MetricFamily, queueName string, want float64) {
	t.Helper()
	for _, fam := range families {
		if fam.GetName() != "taskforge_queue_concurrency_limit" {
			continue
		}
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if lp.GetName() == "queue_name" && lp.GetValue() == queueName {
					require.Equal(t, want, met.GetGauge().GetValue())
					return
				}
			}
		}
	}
	t.Fatalf("metric taskforge_queue_concurrency_limit{queue_name=%s} not found", queueName)
}
