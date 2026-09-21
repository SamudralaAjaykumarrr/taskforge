// Command worker runs TaskForge's Phase 1 single-worker claim-execute-
// complete loop (docs/roadmap.md, docs/worker-protocol.md). Only one
// instance of this process should run against a given database in Phase 1
// — concurrent multi-worker safety is not implemented or claimed until
// Phase 2.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// echoHandler is a minimal demonstration handler for local manual testing.
// It performs no real external side effect; it only logs the payload it
// received and always succeeds.
type echoHandler struct {
	logger *slog.Logger
}

func (h echoHandler) Execute(_ context.Context, j *job.Job) (handler.Result, error) {
	h.logger.Info("demo.echo executing", "job_id", j.ID.String(), "payload", string(j.Payload))
	return handler.Result{}, nil
}

// sleepHandler is a Phase 14 demonstration/test handler: it sleeps for the
// duration named by its payload's "sleep_ms" field (default 0), then
// succeeds -- unless its context is cancelled first, in which case it
// returns that cancellation error promptly, cooperatively, exactly as
// docs/execution-semantics.md expects a well-behaved handler to. It
// performs no real external side effect. Registered under demo.sleep so
// test/procs's real-OS-process graceful-drain proofs (SF-064, SF-069;
// docs/phase-14-plan.md §16) have a way to control, from outside the
// process, exactly how long a job stays in flight relative to
// TASKFORGE_WORKER_DRAIN_TIMEOUT -- the same reason demo.flaky exists for
// retry demonstrations.
type sleepHandler struct {
	logger *slog.Logger
}

func (h sleepHandler) Execute(ctx context.Context, j *job.Job) (handler.Result, error) {
	var payload struct {
		SleepMS int `json:"sleep_ms"`
	}
	_ = json.Unmarshal(j.Payload, &payload)
	h.logger.Info("demo.sleep executing", "job_id", j.ID.String(), "sleep_ms", payload.SleepMS)
	select {
	case <-time.After(time.Duration(payload.SleepMS) * time.Millisecond):
		return handler.Result{}, nil
	case <-ctx.Done():
		return handler.Result{}, ctx.Err()
	}
}

// flakyHandler is a Phase 3 demonstration handler: it reports a retryable
// failure on every attempt strictly before j.AttemptCount reaches its
// FailUntilAttempt threshold, then succeeds. It performs no real external
// side effect. Registered under demo.flaky so
// "How to demonstrate retries" in README.md has a concrete, runnable
// example beyond demo.echo's always-succeeds path.
type flakyHandler struct {
	logger           *slog.Logger
	failUntilAttempt int
}

func (h flakyHandler) Execute(_ context.Context, j *job.Job) (handler.Result, error) {
	if j.AttemptCount < h.failUntilAttempt {
		h.logger.Info("demo.flaky executing: reporting retryable failure",
			"job_id", j.ID.String(), "attempt_count", j.AttemptCount, "fail_until_attempt", h.failUntilAttempt)
		return handler.Result{}, handler.Retryable(fmt.Errorf("demo.flaky: simulated transient failure on attempt %d", j.AttemptCount))
	}
	h.logger.Info("demo.flaky executing: succeeding", "job_id", j.ID.String(), "attempt_count", j.AttemptCount)
	return handler.Result{}, nil
}

func main() {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println("taskforge-worker " + buildinfo.String())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		return err
	}
	if err := migrate.Up(pingCtx, db); err != nil {
		return err
	}

	registry := handler.NewRegistry()
	registry.Register("demo.echo", echoHandler{logger: logger})
	registry.Register("demo.flaky", flakyHandler{logger: logger, failUntilAttempt: 3})
	registry.Register("demo.sleep", sleepHandler{logger: logger})

	// Phase 8: m is shared between the Store (every metric it records --
	// see internal/store) and this process's standalone /metrics HTTP
	// listener below. A fresh, private registry per process -- see
	// internal/metrics.New's doc comment.
	m := metrics.New()
	st := store.New(db, store.WithMetrics(m), store.WithLogger(logger))
	m.Registry.MustRegister(metrics.NewStateCollector(st, cfg.ActiveWorkerWindow, logger))

	workerID := fmt.Sprintf("worker-%d-%s", os.Getpid(), hostname())
	w := worker.New(workerID, st, registry, cfg.WorkerPollInterval, logger)
	w.SetQueues(cfg.WorkerQueues)
	if len(cfg.WorkerQueues) > 0 {
		logger.Info("worker queue subscription configured", "queues", cfg.WorkerQueues)
	}
	// Phase 14 (docs/phase-14-plan.md §6.4/§9, §19 OD-3): cmd/worker is
	// the sole caller in this repository that opts a *Worker into the
	// graceful-drain contract. Every other caller (every test, cmd/chaos)
	// never calls SetDrainTimeout and keeps today's immediate-cancellation
	// behavior unchanged.
	w.SetDrainTimeout(cfg.WorkerDrainTimeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// worker_drain_started/worker_drain_completed (docs/phase-14-plan.md
	// §13) give an operator a durable, timestamped record of how long
	// this process actually waited between SIGTERM and exit -- not
	// merely that the drain code exists. drainStartedAt is sent exactly
	// once, the instant ctx is cancelled (before w.Run below observes
	// the same cancellation and stops accepting new claims); the main
	// goroutine consumes it, non-blockingly, only after w.Run returns.
	drainStartedAt := make(chan time.Time, 1)
	go func() {
		<-ctx.Done()
		t := time.Now()
		logger.Info("worker drain started", "event", "worker_drain_started", "drain_timeout", cfg.WorkerDrainTimeout.String())
		drainStartedAt <- t
	}()

	if cfg.MetricsAddr != "" {
		// This listener is UNAUTHENTICATED, deliberately and as documented
		// (docs/security-model.md §5, docs/observability.md). Phase 12
		// added credential authentication to cmd/api's GET /metrics but
		// NOT here, because verifying an API key requires reading the
		// principals/api_keys tables and OD-3 plus
		// deploy/postgres-roles.sql deliberately deny taskforge_worker any
		// access to them. Protecting a metrics endpoint by dissolving the
		// worker trust boundary would be a bad trade.
		//
		// The control is therefore a deployment obligation, in the same
		// category as the external TLS boundary: bind or firewall
		// TASKFORGE_METRICS_ADDR to a private, operator-controlled network
		// reachable only by the Prometheus scraper. This process cannot
		// verify that from the inside and does not claim to.
		//
		// It exposes operational volume (submission/completion/dead-letter
		// rates and queue-depth gauges), never a payload, a credential, or
		// any per-tenant identifier -- metric labels are cardinality-safe
		// and audited (docs/observability.md's Cardinality Policy).
		metricsSrv := &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("worker metrics endpoint listening (unauthenticated; restrict to a private network)",
				"addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("worker metrics endpoint exited with error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(shutdownCtx)
		}()
	}

	logger.Info("worker starting", "worker_id", workerID, "poll_interval", cfg.WorkerPollInterval,
		"drain_timeout", cfg.WorkerDrainTimeout.String(), "version", buildinfo.Version, "commit", buildinfo.Commit)
	err = w.Run(ctx)

	// Report the drain outcome, if a shutdown was actually in progress
	// (ctx was cancelled) -- a normal exit for any other reason (e.g. a
	// store error w.Run declined to retry past) has nothing to drain and
	// leaves drainStartedAt empty.
	select {
	case startedAt := <-drainStartedAt:
		duration := time.Since(startedAt)
		m.WorkerDrainDurationSeconds.Observe(duration.Seconds())
		// duration is measured from the moment SIGTERM was observed, so
		// a drain that genuinely hit its configured timeout takes at
		// least that long; one that finished the in-flight job on its
		// own takes measurably less (or there was no job in flight at
		// all, in which case Run returns almost immediately).
		outcome := "in_flight_job_finished"
		switch {
		case duration >= cfg.WorkerDrainTimeout:
			outcome = "drain_timeout_exceeded"
			m.WorkerDrainTimedOutTotal.Inc()
		case duration < cfg.WorkerPollInterval:
			outcome = "no_job_in_flight"
		}
		logger.Info("worker drain completed", "event", "worker_drain_completed",
			"outcome", outcome, "duration_seconds", duration.Seconds())
	default:
	}

	if err != nil && ctx.Err() != nil {
		logger.Info("worker stopped")
		return nil
	}
	return err
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
