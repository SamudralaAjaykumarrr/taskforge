// Command worker runs TaskForge's Phase 1 single-worker claim-execute-
// complete loop (docs/roadmap.md, docs/worker-protocol.md). Only one
// instance of this process should run against a given database in Phase 1
// — concurrent multi-worker safety is not implemented or claimed until
// Phase 2.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
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

	workerID := fmt.Sprintf("worker-%d-%s", os.Getpid(), hostname())
	w := worker.New(workerID, store.New(db), registry, cfg.WorkerPollInterval, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("worker starting", "worker_id", workerID, "poll_interval", cfg.WorkerPollInterval)
	err = w.Run(ctx)
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
