// Package metrics implements the exact Prometheus-compatible metric set
// documented in docs/observability.md's Metrics table -- stable names,
// documented types, and bounded label sets. No metric here is invented
// beyond that table: docs/observability.md is authoritative, and this
// package exists to make it executable, not to define a second
// observability model.
//
// Cardinality policy (docs/observability.md, and the Phase 8 task's
// "Cardinality Audit"): every label used below is a small, finite,
// developer-controlled vocabulary -- job_type (bounded by how many job
// types a deployment registers, not by caller input) and outcome/state
// (fixed enums from internal/jobstate and this package's Outcome*
// constants). Nothing here is ever labeled by job_id, workflow_id,
// idempotency_key, worker_id, or raw error text -- see
// docs/observability.md's "Cardinality Policy" section for the full
// audit. High-cardinality identifiers (job_id, workflow_id, node_id,
// worker_id) belong in structured logs, not metric labels -- see
// internal/store and internal/worker's logging.
//
// Recording a metric here can never fail or block: every method is a
// simple in-memory counter/histogram update (the underlying
// prometheus.Collector types are safe for concurrent use by many
// goroutines), so a Metrics value can be threaded through
// correctness-critical code paths (internal/store) without introducing a
// new failure mode -- per docs/roadmap.md's Phase 8 charter,
// "Observability must never become authoritative state" and "a metrics
// backend failure must not prevent job completion." There is no network
// call, no disk write, and no possibility of blocking on an external
// telemetry backend anywhere in this package; only a pull-based
// /metrics HTTP endpoint (wired up in cmd/api and cmd/worker) ever
// touches the network, and a scrape failure there affects only that HTTP
// response, never a job's durable state.
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Outcome label values, matching docs/observability.md's
// taskforge_jobs_completed_total documentation exactly:
// "succeeded/failed_retryable/failed_permanent/timed_out/lease_expired/cancelled".
// These mirror job_attempts.outcome (internal/store's attemptOutcome*
// constants) but are lowercased per that table's literal wording -- kept
// as a small, fixed set of string constants here so no call site needs to
// invent or duplicate the mapping.
const (
	OutcomeSucceeded       = "succeeded"
	OutcomeFailedRetryable = "failed_retryable"
	OutcomeFailedPermanent = "failed_permanent"
	OutcomeTimedOut        = "timed_out"
	OutcomeLeaseExpired    = "lease_expired"
	OutcomeCancelled       = "cancelled"
)

// Metrics bundles every collector in docs/observability.md's Metrics
// table, registered against a private *prometheus.Registry (never the
// global default registry -- see New's doc comment for why: it lets
// tests construct an isolated instance per test, and lets a single
// process serve exactly the documented metric set with no accidental
// process-level collectors mixed in).
type Metrics struct {
	Registry *prometheus.Registry

	// JobsSubmittedTotal is taskforge_jobs_submitted_total (counter,
	// labeled by job_type): submission volume, incremented once per
	// durable job-row creation (internal/store.InsertIdempotent and
	// workflow node creation), regardless of whether the submission was
	// deduplicated via an idempotency key -- see IdempotentSubmissionHitsTotal
	// for that subset.
	JobsSubmittedTotal *prometheus.CounterVec

	// JobsCompletedTotal is taskforge_jobs_completed_total (counter,
	// labeled by job_type, outcome): attempt-level outcome volume, mapping
	// directly to job_attempts.outcome -- incremented once per finalized
	// attempt (every Complete* call and every lazy dead-letter sweep),
	// never once per job (a job retried 3 times before succeeding
	// contributes 3 increments here, 2 at failed_retryable and 1 at
	// succeeded).
	JobsCompletedTotal *prometheus.CounterVec

	// JobsDeadLetteredTotal is taskforge_jobs_dead_lettered_total
	// (counter, labeled by job_type): cumulative DLQ volume, incremented
	// exactly once per job that reaches DEAD_LETTERED (a job-level event,
	// unlike JobsCompletedTotal's attempt-level granularity) -- never
	// decreases, unlike the jobs_by_state gauge.
	JobsDeadLetteredTotal *prometheus.CounterVec

	// ClaimLatencySeconds is taskforge_claim_latency_seconds (histogram,
	// unlabeled): time from a job becoming eligible (eligible_at) to
	// being claimed.
	ClaimLatencySeconds prometheus.Histogram

	// QueueAgeSeconds is taskforge_queue_age_seconds (histogram,
	// unlabeled): claimed_at - eligible_at, sampled per claim.
	//
	// As implemented, this observes the exact same sample as
	// ClaimLatencySeconds for every claim -- docs/observability.md
	// documents both names with definitions that are, read literally,
	// identical ("time from eligible_at to being claimed" vs.
	// "claimed_at - eligible_at ... how long a job waited past
	// eligibility"). Rather than inventing an undocumented distinction
	// between them, this implementation keeps both names (both appear in
	// the authoritative metrics table) and records the same measured
	// value into each -- see docs/observability.md's "Implementation
	// Notes" for this documented redundancy.
	QueueAgeSeconds prometheus.Histogram

	// ExecutionDurationSeconds is taskforge_execution_duration_seconds
	// (histogram, labeled by job_type, outcome): wall-clock time from
	// claim to completion report, i.e. job_attempts.finished_at -
	// job_attempts.started_at for the attempt just finalized. Not
	// observed for CancelQueuedOrRetryWait (a pre-claim cancellation has
	// no attempt, hence no claim-to-completion duration to measure) or
	// for the lazy dead-letter sweep (see docs/observability.md's
	// "Implementation Notes" for why that specific path is excluded).
	ExecutionDurationSeconds *prometheus.HistogramVec

	// LeaseExpirationsTotal is taskforge_lease_expirations_total
	// (counter, labeled by job_type): count of jobs reclaimed due to an
	// expired lease -- incremented only by Store.Claim's genuine reclaim
	// branch (the pre-claim state was RUNNING with an expired lease),
	// never by an ordinary RETRY_WAIT claim after backoff, even though
	// both advance lease_generation identically. See
	// docs/observability.md's "Implementation Notes" for the defect this
	// distinction fixes.
	LeaseExpirationsTotal *prometheus.CounterVec

	// StaleCompletionRejectionsTotal is
	// taskforge_stale_completion_rejections_total (counter, unlabeled):
	// count of completion/heartbeat calls rejected by the fencing check
	// (TF-INV-003/014) -- CompleteSuccess, CompleteFailure,
	// completeRetryableOutcome (both CompleteRetryableFailure and
	// CompleteTimeout), CompleteCancelled, and Heartbeat, each time one
	// returns ErrStaleTransition. Deliberately NOT incremented by
	// CancelQueuedOrRetryWait/RequestCancellation's ErrStaleTransition
	// returns -- those reflect "job already left the expected pre-claim
	// state," not a lease-fencing rejection, per
	// docs/observability.md's scope for this metric.
	StaleCompletionRejectionsTotal prometheus.Counter

	// RetryCount is taskforge_retry_count (histogram, labeled by
	// job_type): distribution of attempt_count observed at every
	// job-level terminal transition (SUCCEEDED, DEAD_LETTERED,
	// CANCELLED), showing how often jobs need more than one attempt.
	RetryCount *prometheus.HistogramVec

	// HeartbeatsTotal is taskforge_heartbeats_total (counter, unlabeled):
	// raw heartbeat call volume (every Store.Heartbeat invocation,
	// successful or rejected) -- used to detect heartbeat-interval
	// misconfiguration relative to lease TTL.
	HeartbeatsTotal prometheus.Counter

	// IdempotentSubmissionHitsTotal is
	// taskforge_idempotent_submission_hits_total (counter, unlabeled):
	// count of InsertIdempotent calls that resolved to an existing row
	// via idempotency key rather than creating a new one.
	IdempotentSubmissionHitsTotal prometheus.Counter
}

// New constructs a fresh Metrics instance backed by its own private
// *prometheus.Registry -- never prometheus.DefaultRegisterer. This is
// deliberate: production wiring (cmd/api, cmd/worker) calls New() exactly
// once per process and shares the single resulting *Metrics with both
// internal/store and the process's /metrics HTTP handler; tests call
// New() to get an isolated set of collectors per test, so concurrent
// tests (and repeated test runs against a shared package-level registry)
// never leak counts into each other or panic on a duplicate registration.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,

		JobsSubmittedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskforge_jobs_submitted_total",
			Help: "Submission volume, labeled by job_type.",
		}, []string{"job_type"}),

		JobsCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskforge_jobs_completed_total",
			Help: "Attempt-level outcome volume, labeled by job_type and outcome.",
		}, []string{"job_type", "outcome"}),

		JobsDeadLetteredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskforge_jobs_dead_lettered_total",
			Help: "Cumulative count of jobs that reached DEAD_LETTERED, labeled by job_type.",
		}, []string{"job_type"}),

		ClaimLatencySeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "taskforge_claim_latency_seconds",
			Help:    "Time from a job becoming eligible (eligible_at) to being claimed.",
			Buckets: prometheus.DefBuckets,
		}),

		QueueAgeSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "taskforge_queue_age_seconds",
			Help:    "claimed_at - eligible_at, sampled per claim.",
			Buckets: prometheus.DefBuckets,
		}),

		ExecutionDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "taskforge_execution_duration_seconds",
			Help:    "Wall-clock time from claim to completion report, labeled by job_type and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"job_type", "outcome"}),

		LeaseExpirationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "taskforge_lease_expirations_total",
			Help: "Count of jobs reclaimed due to expired lease, labeled by job_type.",
		}, []string{"job_type"}),

		StaleCompletionRejectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "taskforge_stale_completion_rejections_total",
			Help: "Count of completion/heartbeat calls rejected by the fencing check (TF-INV-003/014).",
		}),

		RetryCount: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "taskforge_retry_count",
			Help:    "Distribution of attempt_count at terminal state, labeled by job_type.",
			Buckets: []float64{1, 2, 3, 4, 5, 8, 13, 21},
		}, []string{"job_type"}),

		HeartbeatsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "taskforge_heartbeats_total",
			Help: "Raw heartbeat call volume.",
		}),

		IdempotentSubmissionHitsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "taskforge_idempotent_submission_hits_total",
			Help: "Count of POST /jobs calls that resolved to an existing row via idempotency key.",
		}),
	}

	reg.MustRegister(
		m.JobsSubmittedTotal,
		m.JobsCompletedTotal,
		m.JobsDeadLetteredTotal,
		m.ClaimLatencySeconds,
		m.QueueAgeSeconds,
		m.ExecutionDurationSeconds,
		m.LeaseExpirationsTotal,
		m.StaleCompletionRejectionsTotal,
		m.RetryCount,
		m.HeartbeatsTotal,
		m.IdempotentSubmissionHitsTotal,
	)

	return m
}

// StateQuerier is the read-only contract NewStateCollector depends on;
// *store.Store satisfies it structurally (no import of internal/store
// here, avoiding an import cycle since internal/store depends on this
// package for the counters/histograms above).
type StateQuerier interface {
	JobStateCounts(ctx context.Context) (map[string]int64, error)
	ActiveWorkerCount(ctx context.Context, window time.Duration) (int64, error)
}

// scrapeTimeout bounds each Collect call's database round trips, so a
// slow/unavailable database degrades a single /metrics scrape rather
// than hanging it indefinitely. Diagnostic-only: per the Phase 8 charter,
// a scrape timing out or erroring never touches job correctness -- it
// just means this scrape omits (or reports stale-absent) the two
// DB-derived gauges below.
const scrapeTimeout = 5 * time.Second

// stateCollector implements prometheus.Collector for the two gauges
// docs/observability.md requires to be computed from durable state at
// scrape time, never from in-memory bookkeeping that could drift after a
// restart, a crash, or a rolled-back transaction (docs/roadmap.md's
// "STATE METRICS" requirement):
//
//   - taskforge_jobs_by_state (gauge, labeled by state): SELECT state,
//     count(*) FROM jobs GROUP BY state.
//   - taskforge_active_workers (gauge, unlabeled): distinct lease_owner
//     values with a heartbeat_at within activeWorkerWindow.
//
// activeWorkerWindow is a Phase 8 implementation decision (like
// internal/api's MaxIdempotencyKeyLength) -- docs/observability.md
// describes "the last lease-extension interval" without a fixed value,
// since v1 has no separate lease-extension-interval configuration
// surface (see internal/worker's heartbeatIntervalFraction doc comment).
// Callers (cmd/api, cmd/worker) supply one via config.
type stateCollector struct {
	q      StateQuerier
	window time.Duration
	logger *slog.Logger

	jobsByState   *prometheus.Desc
	activeWorkers *prometheus.Desc
}

// NewStateCollector returns a prometheus.Collector for
// taskforge_jobs_by_state and taskforge_active_workers, backed by q
// (typically a *store.Store). Register it on m.Registry separately from
// New() (which has no database access) -- see cmd/api and cmd/worker's
// main.go for the wiring.
func NewStateCollector(q StateQuerier, activeWorkerWindow time.Duration, logger *slog.Logger) prometheus.Collector {
	if logger == nil {
		logger = slog.Default()
	}
	return &stateCollector{
		q:      q,
		window: activeWorkerWindow,
		logger: logger,
		jobsByState: prometheus.NewDesc(
			"taskforge_jobs_by_state",
			"Live count of jobs in each state, labeled by state.",
			[]string{"state"}, nil,
		),
		activeWorkers: prometheus.NewDesc(
			"taskforge_active_workers",
			"Distinct lease_owner values with a recent heartbeat.",
			nil, nil,
		),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobsByState
	ch <- c.activeWorkers
}

// Collect runs both durable-state queries fresh, every scrape -- per
// docs/roadmap.md's "STATE METRICS" requirement, this is what keeps
// these two gauges correct after a process restart, a worker crash, or a
// rolled-back transaction: there is no in-memory counter here to drift,
// only a read of PostgreSQL's current, committed state. A query failure
// (e.g. the database is briefly unreachable) is logged and this scrape
// simply omits the affected metric -- it never panics, never blocks
// correctness-critical code (nothing here runs on any job's transition
// path), and never fabricates a value.
func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	counts, err := c.q.JobStateCounts(ctx)
	if err != nil {
		c.logger.Warn("metrics: failed to scrape jobs_by_state", "error", err)
	} else {
		for state, count := range counts {
			ch <- prometheus.MustNewConstMetric(c.jobsByState, prometheus.GaugeValue, float64(count), state)
		}
	}

	active, err := c.q.ActiveWorkerCount(ctx, c.window)
	if err != nil {
		c.logger.Warn("metrics: failed to scrape active_workers", "error", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.activeWorkers, prometheus.GaugeValue, float64(active))
}
