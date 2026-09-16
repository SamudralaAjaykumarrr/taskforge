// Package retention is Phase 13's terminal-record lifecycle sweeper
// (docs/phase-13-plan.md §4/§7/§8/§10; enterprise-roadmap.md "Phase 13 —
// Workload Governance + Retention"). It is a batch-DELETE loop, not a new
// network listener, and it reads/writes only jobs, job_attempts,
// workflow_instances, workflow_nodes -- run under the narrowest role this
// project has, taskforge_retention (deploy/postgres-roles.sql), which has
// no access to anything else.
//
// Core safety rules, load-bearing and not re-derived per call site:
//
//   - Terminal-only pruning. A job is eligible for deletion only once
//     terminal_at is old enough; job_attempts rows are eligible only for
//     an ALREADY-terminal job, never above terminal_attempt_count (in
//     practice, every row a terminal job has -- TF-INV-005's own
//     "historically reopened" check depends on this restriction, not on
//     retained historical rows, so pruning is safe by construction only
//     under it -- docs/phase-13-plan.md §10).
//   - No second clock. TASKFORGE_RETENTION_TERMINAL_JOB_TTL IS the
//     idempotency-dedup-authoritative window -- a job's idempotency_key
//     remains authoritative for exactly as long as its row exists, and
//     Store.GetByIdempotencyKey's re-read is against the live jobs table,
//     unchanged. There is no separate, independently-configured
//     "idempotency retention window" anywhere in this package.
//   - job_attempts is paced independently from its owning job (the
//     roadmap's own explicit requirement: attempts accumulate faster than
//     terminal job rows), but never outlives it: JobAttemptsTTL <=
//     TerminalJobTTL is enforced by Config.Validate, and even if bypassed
//     the job_attempts -> jobs foreign key (no ON DELETE CASCADE) makes
//     PostgreSQL itself refuse to delete a jobs row while its attempts
//     remain -- a loud failure (surfaced via
//     taskforge_retention_sweep_errors_total), never silent data loss.
//   - Workflow retention runs before job retention within the same sweep
//     (Sweep's own ordering), so a job row still referenced by a live
//     workflow_nodes row is never even attempted -- see pruneJobs's own
//     WHERE clause.
//   - Bounded, batched deletion. No single unbounded DELETE anywhere in
//     this package; every delete targets at most BatchSize rows via a
//     LIMIT'd subquery. Retention only ever touches TERMINAL rows
//     (terminal_at IS NOT NULL) -- exactly the row set the claim query's
//     own WHERE clauses (QUEUED/RETRY_WAIT/expired-lease RUNNING) never
//     match, so there is no row-level lock contention between a sweep
//     and ordinary claim/complete traffic to begin with; batching bounds
//     each individual statement's own duration/I/O footprint, which is
//     the actual, measured mechanism behind SF-043 (see
//     internal/retention's own tests), not a claim about lock modes this
//     package does not take.
package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
)

// Config is one sweep pass's configuration, read from environment
// variables by cmd/taskforge-retention (docs/phase-13-plan.md §8).
type Config struct {
	// Enabled gates whether Sweep does anything at all. Default false
	// (docs/phase-13-plan.md §14): retention is an opt-in operator
	// decision, never a silently-enabled behavior change for an existing
	// deployment upgrading into Phase 13.
	Enabled bool

	// TerminalJobTTL is how long a terminal (SUCCEEDED/DEAD_LETTERED/
	// CANCELLED) jobs row is retained after terminal_at, and -- by this
	// package's own "no second clock" design -- also the idempotency
	// dedup-authoritative window.
	TerminalJobTTL time.Duration

	// WorkflowTTL is the analogous TTL for terminal workflow_instances
	// rows (and their workflow_nodes) after their own terminal_at.
	WorkflowTTL time.Duration

	// JobAttemptsTTL independently paces job_attempts pruning for an
	// already-terminal job. Must be <= TerminalJobTTL (Validate enforces
	// this); if unset/zero when passed to Validate's defaulting helper,
	// it is resolved to TerminalJobTTL (one clock, not two, unless an
	// operator deliberately opts into a shorter one).
	JobAttemptsTTL time.Duration

	// BatchSize bounds how many rows any single DELETE in one sweep
	// targets. Must be positive.
	BatchSize int
}

// ErrJobAttemptsTTLExceedsTerminalJobTTL is returned by Validate when
// JobAttemptsTTL > TerminalJobTTL -- attempts must never outlive their
// job (docs/phase-13-plan.md §8).
var ErrJobAttemptsTTLExceedsTerminalJobTTL = errors.New("retention: job_attempts TTL must not exceed terminal job TTL")

// ResolveJobAttemptsTTL returns cfg's effective job_attempts TTL: the
// configured value if set, otherwise TerminalJobTTL (§8's "one clock"
// default). Call this before Validate/Sweep so a caller that never sets
// JobAttemptsTTL gets the documented default rather than zero.
func (cfg Config) ResolveJobAttemptsTTL() time.Duration {
	if cfg.JobAttemptsTTL > 0 {
		return cfg.JobAttemptsTTL
	}
	return cfg.TerminalJobTTL
}

// Validate checks cfg's own internal consistency (independent of whether
// Enabled). Callers should call ResolveJobAttemptsTTL first if they want
// the default-to-TerminalJobTTL behavior; Validate itself does not apply
// that default, so a caller can distinguish "explicitly configured too
// long" from "left unset."
func (cfg Config) Validate() error {
	if cfg.TerminalJobTTL <= 0 {
		return fmt.Errorf("retention: terminal job TTL must be positive")
	}
	if cfg.WorkflowTTL <= 0 {
		return fmt.Errorf("retention: workflow TTL must be positive")
	}
	if cfg.BatchSize <= 0 {
		return fmt.Errorf("retention: batch size must be positive")
	}
	effectiveAttemptsTTL := cfg.ResolveJobAttemptsTTL()
	if effectiveAttemptsTTL > cfg.TerminalJobTTL {
		return fmt.Errorf("%w: configured %s > terminal job TTL %s",
			ErrJobAttemptsTTLExceedsTerminalJobTTL, effectiveAttemptsTTL, cfg.TerminalJobTTL)
	}
	return nil
}

// Result is one Sweep call's outcome: rows deleted per table.
type Result struct {
	JobAttemptsDeleted       int64
	WorkflowNodesDeleted     int64
	WorkflowInstancesDeleted int64
	JobsDeleted              int64
}

// Total is the sum across every table -- the "did retention actually do
// anything" signal.
func (r Result) Total() int64 {
	return r.JobAttemptsDeleted + r.WorkflowNodesDeleted + r.WorkflowInstancesDeleted + r.JobsDeleted
}

// Sweeper runs retention sweeps against a *sql.DB it does not own the
// lifecycle of, ideally one opened under the least-privilege
// taskforge_retention role (deploy/postgres-roles.sql).
type Sweeper struct {
	db      *sql.DB
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// Option configures optional Sweeper dependencies.
type Option func(*Sweeper)

// WithMetrics attaches m as the Sweeper's metrics recorder.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Sweeper) { s.metrics = m }
}

// WithLogger attaches l as the Sweeper's structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Sweeper) { s.logger = l }
}

// New wraps an already-open *sql.DB.
func New(db *sql.DB, opts ...Option) *Sweeper {
	s := &Sweeper{db: db, metrics: metrics.New(), logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Sweep runs exactly one bounded pass: job_attempts, then
// workflow_nodes+workflow_instances, then jobs, each batched at cfg.BatchSize
// and looped until no more rows in that category match or a
// per-category safety iteration cap is hit (so a single call cannot loop
// forever even under a pathological backlog -- it simply leaves the rest
// for the next scheduled invocation). A no-op, returning a zero Result,
// if !cfg.Enabled.
//
// Ordering matters and is not incidental: job_attempts must be pruned
// before the jobs rows they reference (job_attempts.job_id has no ON
// DELETE CASCADE), and workflow_nodes must be pruned before jobs so that
// pruneJobs's own exclusion of workflow-referenced jobs (see its doc
// comment) does not indefinitely defer deletion of a job whose owning
// workflow has itself already aged out in this same pass.
func (s *Sweeper) Sweep(ctx context.Context, cfg Config) (Result, error) {
	if !cfg.Enabled {
		return Result{}, nil
	}
	attemptsTTL := cfg.ResolveJobAttemptsTTL()
	if err := cfg.Validate(); err != nil {
		return Result{}, fmt.Errorf("retention: sweep: invalid config: %w", err)
	}

	start := time.Now()
	s.logger.Info("retention sweep started", "event", "retention_sweep_started",
		"terminal_job_ttl", cfg.TerminalJobTTL, "workflow_ttl", cfg.WorkflowTTL,
		"job_attempts_ttl", attemptsTTL, "batch_size", cfg.BatchSize)

	var result Result
	var err error

	result.JobAttemptsDeleted, err = s.pruneJobAttempts(ctx, attemptsTTL, cfg.BatchSize)
	if err != nil {
		return result, s.sweepError(start, result, fmt.Errorf("retention: sweep: prune job_attempts: %w", err))
	}

	result.WorkflowNodesDeleted, result.WorkflowInstancesDeleted, err = s.pruneWorkflows(ctx, cfg.WorkflowTTL, cfg.BatchSize)
	if err != nil {
		return result, s.sweepError(start, result, fmt.Errorf("retention: sweep: prune workflows: %w", err))
	}

	result.JobsDeleted, err = s.pruneJobs(ctx, cfg.TerminalJobTTL, cfg.BatchSize)
	if err != nil {
		return result, s.sweepError(start, result, fmt.Errorf("retention: sweep: prune jobs: %w", err))
	}

	s.metrics.RetentionSweepDurationSeconds.Observe(time.Since(start).Seconds())
	s.metrics.RetentionRowsDeletedTotal.WithLabelValues("job_attempts").Add(float64(result.JobAttemptsDeleted))
	s.metrics.RetentionRowsDeletedTotal.WithLabelValues("workflow_nodes").Add(float64(result.WorkflowNodesDeleted))
	s.metrics.RetentionRowsDeletedTotal.WithLabelValues("workflow_instances").Add(float64(result.WorkflowInstancesDeleted))
	s.metrics.RetentionRowsDeletedTotal.WithLabelValues("jobs").Add(float64(result.JobsDeleted))

	s.logger.Info("retention sweep completed", "event", "retention_sweep_completed",
		"job_attempts_deleted", result.JobAttemptsDeleted,
		"workflow_nodes_deleted", result.WorkflowNodesDeleted,
		"workflow_instances_deleted", result.WorkflowInstancesDeleted,
		"jobs_deleted", result.JobsDeleted,
		"duration_seconds", time.Since(start).Seconds())

	return result, nil
}

func (s *Sweeper) sweepError(start time.Time, partial Result, err error) error {
	s.metrics.RetentionSweepErrorsTotal.Inc()
	s.logger.Error("retention sweep failed", "error", err,
		"job_attempts_deleted_before_error", partial.JobAttemptsDeleted,
		"workflow_nodes_deleted_before_error", partial.WorkflowNodesDeleted,
		"workflow_instances_deleted_before_error", partial.WorkflowInstancesDeleted,
		"duration_seconds", time.Since(start).Seconds())
	return err
}

// maxBatchIterationsPerSweep bounds how many batches any one Sweep call
// issues per category, so a pathologically large backlog cannot make a
// single invocation run unboundedly long -- the remainder is left for the
// next scheduled sweep. Chosen generously relative to typical BatchSize
// values; this is an operational safety valve, not a correctness bound.
const maxBatchIterationsPerSweep = 1000

// logBatchDeleted emits the retention_batch_deleted structured log event
// (docs/phase-13-plan.md §12) for one non-empty batch -- observability
// for "what was pruned, when, how much" at the per-batch granularity, not
// just the whole-sweep summary retention_sweep_completed already gives.
func (s *Sweeper) logBatchDeleted(table string, n int64) {
	if n == 0 {
		return
	}
	s.logger.Info("retention batch deleted", "event", "retention_batch_deleted", "table", table, "rows_deleted", n)
}

// pruneJobAttempts deletes job_attempts rows belonging to an
// ALREADY-terminal job whose OWN terminal_at is older than attemptsTTL --
// never a non-terminal job's attempts (docs/phase-13-plan.md §5/§10's
// restriction, on which TF-INV-005's historical-reopening check depends),
// and, by construction of the join (job_attempts JOIN jobs), never above
// terminal_attempt_count in practice: every job_attempts row a terminal
// job has satisfies attempt_number <= terminal_attempt_count by
// TF-INV-005's own invariant, so this delete's candidate set already
// excludes anything that check depends on remaining -- see
// docs/phase-13-plan.md §10's own proof for why this is safe by
// construction under the terminal-only restriction.
func (s *Sweeper) pruneJobAttempts(ctx context.Context, ttl time.Duration, batchSize int) (int64, error) {
	var total int64
	for i := 0; i < maxBatchIterationsPerSweep; i++ {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM job_attempts
			WHERE id IN (
				SELECT ja.id
				FROM job_attempts ja
				JOIN jobs j ON j.id = ja.job_id
				WHERE j.terminal_at IS NOT NULL
				  AND j.terminal_at < now() - make_interval(secs => $1::double precision)
				LIMIT $2
			)`, ttl.Seconds(), batchSize)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		s.logBatchDeleted("job_attempts", n)
		total += n
		if n == 0 {
			break
		}
	}
	return total, nil
}

// pruneWorkflows deletes workflow_nodes then workflow_instances for
// terminal workflows older than ttl, in that order (workflow_nodes has no
// ON DELETE CASCADE from workflow_instances either).
func (s *Sweeper) pruneWorkflows(ctx context.Context, ttl time.Duration, batchSize int) (nodesDeleted, instancesDeleted int64, err error) {
	for i := 0; i < maxBatchIterationsPerSweep; i++ {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_nodes
			WHERE id IN (
				SELECT wn.id
				FROM workflow_nodes wn
				JOIN workflow_instances wi ON wi.id = wn.workflow_instance_id
				WHERE wi.terminal_at IS NOT NULL
				  AND wi.terminal_at < now() - make_interval(secs => $1::double precision)
				LIMIT $2
			)`, ttl.Seconds(), batchSize)
		if err != nil {
			return nodesDeleted, instancesDeleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nodesDeleted, instancesDeleted, err
		}
		s.logBatchDeleted("workflow_nodes", n)
		nodesDeleted += n
		if n == 0 {
			break
		}
	}

	for i := 0; i < maxBatchIterationsPerSweep; i++ {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_instances
			WHERE id IN (
				SELECT wi.id
				FROM workflow_instances wi
				WHERE wi.terminal_at IS NOT NULL
				  AND wi.terminal_at < now() - make_interval(secs => $1::double precision)
				  AND NOT EXISTS (SELECT 1 FROM workflow_nodes wn WHERE wn.workflow_instance_id = wi.id)
				LIMIT $2
			)`, ttl.Seconds(), batchSize)
		if err != nil {
			return nodesDeleted, instancesDeleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nodesDeleted, instancesDeleted, err
		}
		s.logBatchDeleted("workflow_instances", n)
		instancesDeleted += n
		if n == 0 {
			break
		}
	}
	return nodesDeleted, instancesDeleted, nil
}

// pruneJobs deletes terminal jobs rows older than ttl. The first NOT
// EXISTS clause excludes any job still referenced by a workflow_nodes row
// -- pruneWorkflows (run first, in the same Sweep call) already cleared
// that reference for any workflow old enough to qualify; a job whose
// workflow has NOT yet aged out is correctly left alone here rather than
// hitting the job_attempts/workflow_nodes foreign key's loud-failure
// backstop on every single sweep cycle.
//
// The second NOT EXISTS clause excludes any job that still has ANY
// job_attempts rows at all (independent review finding M1): pruneJobs and
// pruneJobAttempts share the same maxBatchIterationsPerSweep cap, each
// applied independently, so a pathologically large pre-existing
// job_attempts backlog for one job (most plausible on first adoption
// against an old deployment) can leave pruneJobAttempts's own pass
// incomplete for that job -- attemptsTTL <= TerminalJobTTL is always
// enforced (Config.Validate), so a job old enough for pruneJobs to
// consider is also old enough for every one of its attempts to be
// eligible, but "eligible" and "already deleted within this one Sweep
// call's iteration budget" are not the same thing. Without this clause,
// pruneJobs would attempt that job's row anyway, hit the
// job_attempts->jobs foreign key (no ON DELETE CASCADE), and abort the
// WHOLE batch DELETE statement -- not just that one job -- failing the
// entire Sweep call on an otherwise-ordinary large backlog, not a
// misconfiguration. Excluding it here instead makes this exactly the same
// self-healing shape as the workflow_nodes exclusion above: this job is
// simply left for a later sweep, once pruneJobAttempts has fully drained
// it. TestSweep_JobAttemptsBacklogExceedingIterationCap_DoesNotAbortJobsBatch
// proves this directly. The job_attempts->jobs foreign key remains the
// correctness backstop for a genuine Config.Validate bypass (see
// TestForeignKeyBackstop_CannotDeleteJobWhileAttemptsRemain) -- this
// clause only prevents the ordinary, non-misconfigured case from ever
// reaching that backstop in the first place.
func (s *Sweeper) pruneJobs(ctx context.Context, ttl time.Duration, batchSize int) (int64, error) {
	var total int64
	for i := 0; i < maxBatchIterationsPerSweep; i++ {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM jobs
			WHERE id IN (
				SELECT j.id
				FROM jobs j
				WHERE j.terminal_at IS NOT NULL
				  AND j.terminal_at < now() - make_interval(secs => $1::double precision)
				  AND NOT EXISTS (SELECT 1 FROM workflow_nodes wn WHERE wn.job_id = j.id)
				  AND NOT EXISTS (SELECT 1 FROM job_attempts ja WHERE ja.job_id = j.id)
				LIMIT $2
			)`, ttl.Seconds(), batchSize)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		s.logBatchDeleted("jobs", n)
		total += n
		if n == 0 {
			break
		}
	}
	return total, nil
}
