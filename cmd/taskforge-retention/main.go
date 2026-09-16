// Command taskforge-retention is Phase 13's retention sweeper
// (docs/phase-13-plan.md §18 OD-5): a standalone binary, favored over a
// ticker inside cmd/worker or cmd/api specifically to isolate maintenance-
// window scheduling from request/claim serving. It performs one bounded,
// batched sweep pass and exits (the operator/cron-invoked shape) unless
// TASKFORGE_RETENTION_SWEEP_INTERVAL is set, in which case it loops on
// that interval instead -- both are supported so an operator can choose
// cron or a long-lived supervised process without either shape needing
// separate code.
//
// It connects as the taskforge_retention PostgreSQL role
// (deploy/postgres-roles.sql): SELECT/DELETE on exactly jobs,
// job_attempts, workflow_instances, and workflow_nodes, nothing else --
// the narrowest role this project has.
//
// Retention is opt-in (TASKFORGE_RETENTION_ENABLED, default false,
// docs/phase-13-plan.md §14): running this binary against a deployment
// that has not set it is a deliberate no-op, not an error, so it is safe
// to add to a deployment's tooling ahead of actually opting in.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/retention"
)

func main() {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println("taskforge-retention " + buildinfo.String())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("retention sweeper exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	dbCfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	retCfg, err := config.FromEnvRetention()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", dbCfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// migrate.Up is a read-only probe against an already-migrated
	// database under this least-privilege role (internal/migrate's
	// ensureMigrationsTable, G4) -- it applies nothing here, since
	// taskforge_retention has no CREATE privilege and schema changes are
	// a separate, owner-privileged deployment step.
	if err := migrate.Up(ctx, db); err != nil {
		return err
	}

	if !retCfg.Sweep.Enabled {
		logger.Info("retention is disabled (TASKFORGE_RETENTION_ENABLED is not set); exiting without sweeping")
		return nil
	}

	m := metrics.New()
	sweeper := retention.New(db, retention.WithMetrics(m), retention.WithLogger(logger))

	if retCfg.Interval <= 0 {
		logger.Info("running a single retention sweep pass", "version", buildinfo.Version, "commit", buildinfo.Commit)
		_, err := sweeper.Sweep(context.Background(), retCfg.Sweep)
		return err
	}

	logger.Info("running retention sweeper on a fixed interval", "interval", retCfg.Interval,
		"version", buildinfo.Version, "commit", buildinfo.Commit)
	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(retCfg.Interval)
	defer ticker.Stop()

	if _, err := sweeper.Sweep(stopCtx, retCfg.Sweep); err != nil {
		logger.Error("retention sweep failed", "error", err)
	}
	for {
		select {
		case <-stopCtx.Done():
			logger.Info("shutting down retention sweeper")
			return nil
		case <-ticker.C:
			if _, err := sweeper.Sweep(stopCtx, retCfg.Sweep); err != nil {
				logger.Error("retention sweep failed", "error", err)
			}
		}
	}
}
