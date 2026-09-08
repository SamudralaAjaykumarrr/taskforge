// Command api runs TaskForge's Phase 1 HTTP API server: POST /jobs and
// GET /jobs/{id}. It is a stateless process — every request reads from or
// writes to PostgreSQL directly (docs/architecture.md).
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("api server exited with error", "error", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	if err := migrate.Up(ctx, db); err != nil {
		return err
	}

	// Phase 8: m is shared between the Store (every metric it records --
	// see internal/store) and this process's /metrics endpoint below. A
	// fresh, private registry per process (never
	// prometheus.DefaultRegisterer) -- see internal/metrics.New's doc
	// comment.
	m := metrics.New()
	st := store.New(db, store.WithMetrics(m), store.WithLogger(logger))
	m.Registry.MustRegister(metrics.NewStateCollector(st, cfg.ActiveWorkerWindow, logger))

	handlers := api.NewHandlers(st, logger)
	router := api.NewRouter(handlers)
	router.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api server listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-serveCtx.Done():
		logger.Info("shutting down api server")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	}
}
