package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/retention"
)

// RetentionSweepIntervalEnv is the environment variable naming how often
// cmd/taskforge-retention re-runs its sweep when run as a long-lived
// process rather than a single cron-invoked pass (docs/phase-13-plan.md
// §8, §18 OD-5). Exported so cmd/taskforge-retention's own usage text can
// reference the same name FromEnvRetention parses.
const RetentionSweepIntervalEnv = "TASKFORGE_RETENTION_SWEEP_INTERVAL"

// RetentionEnvConfig is FromEnvRetention's result: a ready-to-use
// retention.Config plus the sweep-interval knob that governs whether
// cmd/taskforge-retention runs once and exits (Interval == 0, the
// cron-invoked shape docs/phase-13-plan.md §18 OD-5 favors) or loops on a
// ticker (Interval > 0).
type RetentionEnvConfig struct {
	Sweep    retention.Config
	Interval time.Duration
}

// FromEnvRetention reads cmd/taskforge-retention's own environment
// variables, independent of FromEnv (cmd/api and cmd/worker have no need
// for any of these):
//
//	TASKFORGE_RETENTION_ENABLED (default "false")
//	TASKFORGE_RETENTION_TERMINAL_JOB_TTL (required if enabled, parsed by
//	  time.ParseDuration)
//	TASKFORGE_RETENTION_WORKFLOW_TTL (required if enabled)
//	TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL (optional; defaults to
//	  TASKFORGE_RETENTION_TERMINAL_JOB_TTL if unset -- one clock, not two,
//	  unless an operator deliberately opts into a shorter value; rejected
//	  if it exceeds the terminal job TTL)
//	TASKFORGE_RETENTION_BATCH_SIZE (default "500")
//	TASKFORGE_RETENTION_SWEEP_INTERVAL (default "0" -- run once and exit)
func FromEnvRetention() (RetentionEnvConfig, error) {
	enabled := false
	if raw := os.Getenv("TASKFORGE_RETENTION_ENABLED"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid TASKFORGE_RETENTION_ENABLED %q: %w", raw, err)
		}
		enabled = b
	}

	cfg := retention.Config{Enabled: enabled, BatchSize: 500}

	if raw := os.Getenv("TASKFORGE_RETENTION_TERMINAL_JOB_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid TASKFORGE_RETENTION_TERMINAL_JOB_TTL %q: %w", raw, err)
		}
		cfg.TerminalJobTTL = d
	} else if enabled {
		return RetentionEnvConfig{}, fmt.Errorf("config: TASKFORGE_RETENTION_TERMINAL_JOB_TTL is required when retention is enabled")
	}

	if raw := os.Getenv("TASKFORGE_RETENTION_WORKFLOW_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid TASKFORGE_RETENTION_WORKFLOW_TTL %q: %w", raw, err)
		}
		cfg.WorkflowTTL = d
	} else if enabled {
		return RetentionEnvConfig{}, fmt.Errorf("config: TASKFORGE_RETENTION_WORKFLOW_TTL is required when retention is enabled")
	}

	if raw := os.Getenv("TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL %q: %w", raw, err)
		}
		cfg.JobAttemptsTTL = d
	}

	if raw := os.Getenv("TASKFORGE_RETENTION_BATCH_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid TASKFORGE_RETENTION_BATCH_SIZE %q: %w", raw, err)
		}
		cfg.BatchSize = n
	}

	interval := time.Duration(0)
	if raw := os.Getenv(RetentionSweepIntervalEnv); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: invalid %s %q: %w", RetentionSweepIntervalEnv, raw, err)
		}
		interval = d
	}

	if enabled {
		if err := cfg.Validate(); err != nil {
			return RetentionEnvConfig{}, fmt.Errorf("config: %w", err)
		}
	}

	return RetentionEnvConfig{Sweep: cfg, Interval: interval}, nil
}
