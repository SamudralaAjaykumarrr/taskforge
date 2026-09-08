// Package config reads the environment variables needed to run TaskForge
// Phase 1 locally: a database connection string, an HTTP listen address,
// and worker polling parameters. There is no config file format or remote
// config source — Phase 1 has nothing that warrants one.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds TaskForge's runtime configuration.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string, e.g.
	// postgres://user:pass@localhost:5432/taskforge?sslmode=disable
	DatabaseURL string

	// HTTPAddr is the address the API server listens on.
	HTTPAddr string

	// WorkerPollInterval is how long the worker sleeps after finding
	// nothing eligible to claim.
	WorkerPollInterval time.Duration

	// MetricsAddr is the address cmd/worker's standalone /metrics HTTP
	// endpoint listens on (Phase 8, docs/observability.md). Empty
	// disables it. cmd/api instead adds GET /metrics directly onto its
	// existing router/listen address -- see cmd/api/main.go -- since it
	// already has an HTTP server; cmd/worker does not, hence this
	// separate address. Safe default: enabled at a private, non-standard
	// port, so no external Prometheus/collector is required to run
	// TaskForge -- the endpoint simply serves this process's in-memory
	// metrics and costs nothing if never scraped.
	MetricsAddr string

	// ActiveWorkerWindow bounds "recent" for
	// taskforge_active_workers (docs/observability.md: "a heartbeat_at
	// within the last lease-extension interval"). v1 has no separate
	// lease-extension-interval configuration surface to derive this
	// from automatically (see internal/worker's heartbeatIntervalFraction
	// doc comment) -- this is a Phase 8 implementation decision, like
	// internal/api's MaxIdempotencyKeyLength.
	ActiveWorkerWindow time.Duration
}

// FromEnv reads configuration from environment variables:
//
//	TASKFORGE_DATABASE_URL (required)
//	TASKFORGE_HTTP_ADDR (default ":8080")
//	TASKFORGE_WORKER_POLL_INTERVAL (default "500ms", parsed by time.ParseDuration)
//	TASKFORGE_METRICS_ADDR (default ":9090"; empty disables cmd/worker's
//	  standalone metrics listener)
//	TASKFORGE_ACTIVE_WORKER_WINDOW (default "30s", parsed by
//	  time.ParseDuration)
func FromEnv() (Config, error) {
	dbURL := os.Getenv("TASKFORGE_DATABASE_URL")
	if dbURL == "" {
		return Config{}, fmt.Errorf("config: TASKFORGE_DATABASE_URL is required")
	}

	addr := os.Getenv("TASKFORGE_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	pollInterval := 500 * time.Millisecond
	if raw := os.Getenv("TASKFORGE_WORKER_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TASKFORGE_WORKER_POLL_INTERVAL %q: %w", raw, err)
		}
		pollInterval = d
	}

	metricsAddr := ":9090"
	if raw, set := os.LookupEnv("TASKFORGE_METRICS_ADDR"); set {
		metricsAddr = raw
	}

	activeWorkerWindow := 30 * time.Second
	if raw := os.Getenv("TASKFORGE_ACTIVE_WORKER_WINDOW"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TASKFORGE_ACTIVE_WORKER_WINDOW %q: %w", raw, err)
		}
		activeWorkerWindow = d
	}

	return Config{
		DatabaseURL:        dbURL,
		HTTPAddr:           addr,
		WorkerPollInterval: pollInterval,
		MetricsAddr:        metricsAddr,
		ActiveWorkerWindow: activeWorkerWindow,
	}, nil
}
