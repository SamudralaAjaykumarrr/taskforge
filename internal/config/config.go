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

// Config holds Phase 1's runtime configuration.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string, e.g.
	// postgres://user:pass@localhost:5432/taskforge?sslmode=disable
	DatabaseURL string

	// HTTPAddr is the address the API server listens on.
	HTTPAddr string

	// WorkerPollInterval is how long the worker sleeps after finding
	// nothing eligible to claim.
	WorkerPollInterval time.Duration
}

// FromEnv reads configuration from environment variables:
//
//	TASKFORGE_DATABASE_URL (required)
//	TASKFORGE_HTTP_ADDR (default ":8080")
//	TASKFORGE_WORKER_POLL_INTERVAL (default "500ms", parsed by time.ParseDuration)
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

	return Config{
		DatabaseURL:        dbURL,
		HTTPAddr:           addr,
		WorkerPollInterval: pollInterval,
	}, nil
}
