// Package config reads the environment variables needed to run TaskForge
// Phase 1 locally: a database connection string, an HTTP listen address,
// and worker polling parameters. There is no config file format or remote
// config source — Phase 1 has nothing that warrants one.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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

	// APIKeyPepper is the server-held secret HMAC key
	// internal/principal hashes API-key secrets under (Phase 12,
	// docs/phase-12-plan.md §6a). Required by cmd/api: authentication is
	// a hard cutover with no "disabled" mode (OD-6), so a missing pepper
	// is a startup failure, not a silent downgrade to an unauthenticated
	// API.
	//
	// It is handled with the same discipline as DatabaseURL: read from
	// the environment, never committed, never logged, and never written
	// to the database. Losing it invalidates every stored secret_hash's
	// verifiability and requires reissuing every key -- an accepted,
	// documented risk for this phase (docs/phase-12-plan.md §14); no
	// secrets-manager integration is in scope.
	APIKeyPepper []byte

	// MaxInflightSubmissions bounds concurrent in-flight POST
	// /jobs/POST /workflows handler executions in cmd/api
	// (docs/phase-13-plan.md §8 OD-6); exceeding it is the 503
	// system-capacity signal. 0 (the default) disables the bound
	// entirely -- the pre-Phase-13 behavior. This numeric default is an
	// implementation decision this plan explicitly leaves open, like
	// ActiveWorkerWindow's.
	MaxInflightSubmissions int

	// WorkerQueues is Phase 13's queue-subscription filter
	// (docs/phase-13-plan.md §6b/§8/§14): the queue_name values a worker
	// process claims from (both branches of the claim query identically —
	// a fresh claim and a reclaim of an expired lease alike). nil means
	// "claim from every queue" -- the mandatory compatibility default: an
	// old (pre-Phase-13) worker binary has no queue_name predicate at all
	// and behaves identically, and a new worker binary with
	// TASKFORGE_WORKER_QUEUES unset must reproduce that exact behavior,
	// not silently narrow to "default" only. See FromEnv's doc comment.
	WorkerQueues []string
}

// MinAPIKeyPepperLength is the shortest pepper cmd/api will start with.
// The pepper is an HMAC key, so its value comes entirely from being
// unguessable; 32 bytes matches the 256-bit entropy of the secrets it
// protects. This is a floor against an obviously-too-weak operator value
// (a word, a short passphrase), not a substitute for generating it
// randomly -- see .env.example.
const MinAPIKeyPepperLength = 32

// FromEnv reads configuration from environment variables:
//
//	TASKFORGE_DATABASE_URL (required)
//	TASKFORGE_HTTP_ADDR (default ":8080")
//	TASKFORGE_WORKER_POLL_INTERVAL (default "500ms", parsed by time.ParseDuration)
//	TASKFORGE_METRICS_ADDR (default ":9090"; empty disables cmd/worker's
//	  standalone metrics listener)
//	TASKFORGE_ACTIVE_WORKER_WINDOW (default "30s", parsed by
//	  time.ParseDuration)
//	TASKFORGE_API_KEY_PEPPER (required by cmd/api; at least
//	  MinAPIKeyPepperLength bytes -- see FromEnvRequiringPepper)
//	TASKFORGE_WORKER_QUEUES (Phase 13, docs/phase-13-plan.md §8; default
//	  unset, comma-separated queue_name values). Unset (or a value that is
//	  empty/all-whitespace after splitting and trimming) means "claim from
//	  every queue" -- NOT "claim only from 'default'". This is stated as a
//	  hard requirement, not a preference: an operator who upgrades worker
//	  binaries without also updating configuration must not silently
//	  strand any job submitted to a non-default queue. Only a worker an
//	  operator has deliberately configured with a non-empty value is
//	  restricted at all.
//	TASKFORGE_MAX_INFLIGHT_SUBMISSIONS (Phase 13, docs/phase-13-plan.md
//	  §8 OD-6; default "0", parsed as an integer). 0 or unset disables the
//	  system-capacity 503 bound entirely.
//
// FromEnv itself does NOT require the pepper, because cmd/worker and the
// chaos harness legitimately have no HTTP surface and no credentials to
// verify. cmd/api calls FromEnvRequiringPepper instead, so the process
// that actually authenticates callers cannot start without it.
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

	maxInflight := 0
	if raw := os.Getenv("TASKFORGE_MAX_INFLIGHT_SUBMISSIONS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TASKFORGE_MAX_INFLIGHT_SUBMISSIONS %q: %w", raw, err)
		}
		if n < 0 {
			return Config{}, fmt.Errorf("config: TASKFORGE_MAX_INFLIGHT_SUBMISSIONS must not be negative")
		}
		maxInflight = n
	}

	return Config{
		DatabaseURL:            dbURL,
		HTTPAddr:               addr,
		WorkerPollInterval:     pollInterval,
		MetricsAddr:            metricsAddr,
		ActiveWorkerWindow:     activeWorkerWindow,
		APIKeyPepper:           []byte(os.Getenv(envAPIKeyPepper)),
		WorkerQueues:           parseWorkerQueues(os.Getenv("TASKFORGE_WORKER_QUEUES")),
		MaxInflightSubmissions: maxInflight,
	}, nil
}

// parseWorkerQueues splits raw on commas and trims each element, dropping
// empty entries. An empty/all-whitespace raw (including "" -- the unset
// case) returns nil, never an empty-but-non-nil slice: internal/store's
// Claim/ClaimFromQueues split treats nil specifically as "no subscription
// filter" (see internal/store/claim.go's queueNameArrayLiteral), so this
// must never collapse "unset" into "subscribed to nothing."
func parseWorkerQueues(raw string) []string {
	var queues []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			queues = append(queues, trimmed)
		}
	}
	return queues
}

// envAPIKeyPepper is the environment variable holding the API-key pepper.
const envAPIKeyPepper = "TASKFORGE_API_KEY_PEPPER"

// FromEnvRequiringPepper is FromEnv plus a hard requirement that
// TASKFORGE_API_KEY_PEPPER is set and long enough. cmd/api uses it so the
// API server refuses to start without the secret it needs to verify
// credentials -- rather than starting and rejecting every request, or
// (worse) starting with authentication somehow bypassed.
func FromEnvRequiringPepper() (Config, error) {
	cfg, err := FromEnv()
	if err != nil {
		return Config{}, err
	}
	if len(cfg.APIKeyPepper) == 0 {
		return Config{}, fmt.Errorf("config: %s is required (Phase 12: the API server cannot verify API keys without it)", envAPIKeyPepper)
	}
	if len(cfg.APIKeyPepper) < MinAPIKeyPepperLength {
		// The pepper's length is reported; its value never is.
		return Config{}, fmt.Errorf("config: %s must be at least %d bytes (got %d)", envAPIKeyPepper, MinAPIKeyPepperLength, len(cfg.APIKeyPepper))
	}
	return cfg, nil
}
