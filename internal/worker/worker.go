// Package worker implements the claim-execute-(heartbeat)-complete loop,
// per docs/roadmap.md and docs/worker-protocol.md. As of Phase 3
// ("Retries, Backoff, DLQ"), a handler failure is no longer unconditionally
// dead-lettered: the worker classifies it (see internal/handler's
// Retryable/Permanent) and either schedules a durable, backed-off retry
// (RUNNING -> RETRY_WAIT) or dead-letters it, exactly as
// docs/retry-semantics.md specifies.
//
// Per docs/worker-protocol.md ("Why Not Hold a Transaction Open for the
// Whole Job"), claiming, heartbeating, and completing are each short,
// independent transactions; the job handler runs entirely outside any
// database transaction. A worker crash between claim and completion no
// longer strands the job (Phase 1's limitation): once its lease expires,
// the same claim query reclaims it under a new lease_generation — see
// Claim in internal/store. A worker that discovers mid-execution that its
// lease has been lost (a heartbeat call returns store.ErrStaleTransition)
// stops treating itself as the authoritative owner: it cancels the
// context passed to the handler and does not attempt a completion call
// that fencing would reject anyway (TF-INV-003/014) — this includes
// scheduling a retry or dead-lettering: a lease-lost worker is no more
// authoritative for those decisions than for a success report.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/retry"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

// Store is the persistence contract this package depends on
// (*store.Store satisfies it). Depending on an interface, rather than the
// concrete type, lets loop/dispatch tests substitute a fake without a real
// database; tests that assert persistence correctness still exercise this
// against a real PostgreSQL-backed *store.Store.
type Store interface {
	Claim(ctx context.Context, workerID string) (*job.Job, bool, error)
	Heartbeat(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, extension time.Duration) (*job.Job, error)
	CompleteSuccess(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, resultMetadata []byte) (*job.Job, error)
	CompleteFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage, errClass string) (*job.Job, error)
	CompleteRetryableFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage string, delay time.Duration) (*job.Job, error)
}

// heartbeatIntervalFraction is the fraction of the lease duration at which
// the worker renews it, per docs/worker-protocol.md: "Heartbeat interval
// should be a small fraction (e.g. 1/3) of lease_extension_seconds so
// that a single missed heartbeat (F10) does not cause spurious reclaim."
//
// v1 has no separate "lease_extension_seconds" configuration surface
// (that would be new API/config expansion beyond Phase 2's scope): the
// lease TTL and every heartbeat's renewal window are both the job's own
// execution_timeout_seconds, exactly as the claim query already computes
// the initial lease_expires_at (docs/worker-protocol.md "Claim Query").
const heartbeatIntervalFraction = 3

// Worker runs the claim-execute-heartbeat-complete loop for a single
// logical worker identity.
type Worker struct {
	ID           string
	store        Store
	registry     *handler.Registry
	pollInterval time.Duration
	logger       *slog.Logger
	retryConfig  retry.Config
	rand         retry.RandSource
}

// New constructs a Worker. id is the lease_owner value recorded on every
// claimed job (docs/data-model.md: "opaque worker identifier, e.g.
// hostname+pid+random"). Retry backoff uses docs/retry-semantics.md's v1
// defaults (retry.DefaultConfig) and a time-seeded jitter source; see
// SetRetryConfig/SetRandSource to override either (primarily useful for
// tests that need a fast, deterministic backoff window).
func New(id string, st Store, registry *handler.Registry, pollInterval time.Duration, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		ID:           id,
		store:        st,
		registry:     registry,
		pollInterval: pollInterval,
		logger:       logger,
		retryConfig:  retry.DefaultConfig(),
		rand:         retry.NewRand(time.Now().UnixNano()),
	}
}

// SetRetryConfig overrides the backoff configuration used when scheduling a
// retryable failure's next attempt. docs/retry-semantics.md's per-job-type
// backoff configuration is an explicitly deferred open question (v1 has
// exactly one, global configuration per worker); this exists primarily so
// tests can use a small base delay for fast, still-real (not DB-time-
// manipulated) retry-then-succeed scenarios.
func (w *Worker) SetRetryConfig(cfg retry.Config) { w.retryConfig = cfg }

// SetRandSource overrides the jitter source used for backoff computation.
// Production use relies on the time-seeded default from New; tests inject
// a deterministic retry.RandSource to assert exact backoff boundaries.
func (w *Worker) SetRandSource(src retry.RandSource) { w.rand = src }

// RunOnce attempts to claim and fully execute a single job (which may be a
// fresh QUEUED/RETRY_WAIT job, or a reclaim of a previously RUNNING job
// whose lease expired — internal/store.Claim treats both identically). It
// returns claimed=false (with a nil error) when there was nothing eligible
// to claim, which is the normal steady-state outcome, not a failure.
func (w *Worker) RunOnce(ctx context.Context) (claimed bool, err error) {
	j, ok, err := w.store.Claim(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	log := w.logger.With(
		"job_id", j.ID.String(),
		"job_type", j.JobType,
		"attempt", j.AttemptCount,
		"lease_generation", j.LeaseGeneration,
	)
	if j.LeaseGeneration > 1 {
		// Phase 2 has no RETRY_WAIT reclaim path yet (docs/roadmap.md
		// non-goal), so lease_generation > 1 unambiguously means this
		// claim came from the expired-lease reclaim branch, not a fresh
		// QUEUED/RETRY_WAIT claim — log it distinctly so an operator can
		// see crash recovery happening without having to infer it from
		// the generation number alone.
		log.Warn("job reclaimed after lease expiration", "previous_lease_generation", j.LeaseGeneration-1)
	} else {
		log.Info("job claimed")
	}

	h, found := w.registry.Lookup(j.JobType)
	if !found {
		log.Warn("no handler registered for job_type; dead-lettering")
		errMsg := handler.ErrNoHandler{JobType: j.JobType}.Error()
		_, ferr := w.store.CompleteFailure(ctx, j.ID, w.ID, j.LeaseGeneration, errMsg, job.ErrorClassPermanent)
		return true, ferr
	}

	result, execErr, leaseLost := w.runWithHeartbeat(ctx, log, j, h)

	if leaseLost {
		// TF-INV-003/014: some other worker has already reclaimed this
		// job under a newer lease_generation. Any completion call we
		// make now would be rejected anyway (fenced on the generation we
		// no longer hold); skip it rather than making a call we already
		// know is stale. This is the concrete "stop behaving as the
		// authoritative owner" behavior docs/worker-protocol.md requires.
		log.Warn("lease lost during execution; not attempting completion")
		return true, nil
	}

	if execErr != nil {
		return true, w.reportFailure(ctx, log, j, execErr)
	}

	log.Info("job execution succeeded")
	_, cerr := w.store.CompleteSuccess(ctx, j.ID, w.ID, j.LeaseGeneration, result.Metadata)
	if errors.Is(cerr, store.ErrStaleTransition) {
		log.Warn("success report rejected: lease no longer current")
		return true, nil
	}
	return true, cerr
}

// reportFailure classifies execErr (internal/handler.Classify, per
// docs/retry-semantics.md) and reports the corresponding outcome:
// ClassRetryable schedules a durable, backed-off retry (or dead-letters,
// if attempt_count has already reached max_attempts -- internal/store's
// CompleteRetryableFailure makes that decision atomically, not this
// method); ClassPermanent (including any error the handler did not
// explicitly classify, per Classify's documented default) dead-letters
// immediately via CompleteFailure, exactly as Phase 1/2 always did for
// every failure.
//
// A rejected completion (store.ErrStaleTransition) is not surfaced as an
// error: it means this worker's lease is no longer current -- some other
// generation now owns (or has already resolved) the job, and per
// TF-INV-003/014 a stale generation is never authoritative for scheduling
// a retry or a dead-letter transition any more than it is for reporting
// success. See the package doc comment.
func (w *Worker) reportFailure(ctx context.Context, log *slog.Logger, j *job.Job, execErr error) error {
	class, _ := handler.Classify(execErr)

	if class == handler.ClassPermanent {
		log.Info("permanent failure; dead-lettering", "error", execErr)
		_, ferr := w.store.CompleteFailure(ctx, j.ID, w.ID, j.LeaseGeneration, execErr.Error(), job.ErrorClassPermanent)
		if errors.Is(ferr, store.ErrStaleTransition) {
			log.Warn("failure report rejected: lease no longer current")
			return nil
		}
		return ferr
	}

	delay := retry.Delay(j.AttemptCount, w.retryConfig, w.rand)
	log.Info("retryable failure", "error", execErr, "attempt_count", j.AttemptCount, "max_attempts", j.MaxAttempts)

	result, ferr := w.store.CompleteRetryableFailure(ctx, j.ID, w.ID, j.LeaseGeneration, execErr.Error(), delay)
	if errors.Is(ferr, store.ErrStaleTransition) {
		log.Warn("retryable failure report rejected: lease no longer current")
		return nil
	}
	if ferr != nil {
		return ferr
	}

	if result.State == jobstate.DeadLettered {
		log.Warn("retries exhausted; job dead-lettered",
			"attempt_count", result.AttemptCount, "max_attempts", result.MaxAttempts)
	} else {
		log.Info("retry scheduled",
			"eligible_at", result.EligibleAt, "delay_seconds", delay.Seconds())
	}
	return nil
}

// runWithHeartbeat executes h against j, renewing the job's lease on a
// ticker in the background for the duration of execution
// (docs/worker-protocol.md "Heartbeating"). If a heartbeat discovers the
// lease has been lost, it cancels the context passed to the handler so a
// cooperative handler can stop, and reports leaseLost=true so the caller
// does not attempt a completion call that fencing would reject anyway.
//
// TaskForge cannot forcibly halt a handler that ignores context
// cancellation — losing authoritative ownership is detected and acted on
// promptly (this function), but physically stopping arbitrary handler
// side effects is only possible if the handler itself cooperates with
// ctx. This distinction is documented, not hidden — see README "Known
// duplicate-side-effect window".
//
// The heartbeat goroutine is guaranteed to have exited before this
// function returns (via the <-done receive below), so RunOnce never
// leaks a heartbeat goroutine or ticker across calls.
func (w *Worker) runWithHeartbeat(ctx context.Context, log *slog.Logger, j *job.Job, h handler.Handler) (result handler.Result, execErr error, leaseLost bool) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	leaseDuration := time.Duration(j.ExecutionTimeoutSeconds) * time.Second
	interval := leaseDuration / heartbeatIntervalFraction
	if interval <= 0 {
		interval = time.Second
	}

	var mu sync.Mutex
	lost := false

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-execCtx.Done():
				return
			case <-ticker.C:
				hbCtx, hbCancel := context.WithTimeout(ctx, interval)
				_, herr := w.store.Heartbeat(hbCtx, j.ID, w.ID, j.LeaseGeneration, leaseDuration)
				hbCancel()
				if herr == nil {
					log.Debug("heartbeat renewed lease")
					continue
				}
				if errors.Is(herr, store.ErrStaleTransition) {
					log.Warn("heartbeat rejected: lease lost to another worker or job reached a terminal state")
					mu.Lock()
					lost = true
					mu.Unlock()
					cancelExec()
					return
				}
				// A transient error (e.g. a momentary DB blip) is not
				// treated as lease loss (F10: a single missed heartbeat
				// is tolerated as long as a later one succeeds before
				// lease_expires_at) — log and retry next interval.
				log.Error("heartbeat failed (transient); will retry next interval", "error", herr)
			}
		}
	}()

	result, execErr = h.Execute(execCtx, j)
	cancelExec()
	<-done // deterministic cleanup: never return before the heartbeat goroutine has exited

	mu.Lock()
	leaseLost = lost
	mu.Unlock()

	return result, execErr, leaseLost
}

// Run polls in a loop, sleeping pollInterval whenever RunOnce finds
// nothing to claim, until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		claimed, err := w.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			w.logger.Error("worker iteration failed", "error", err)
		}
		if !claimed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.pollInterval):
			}
		}
	}
}
