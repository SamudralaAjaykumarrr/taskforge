// Phase 9 ("Chaos, Load, and Failure Testing", docs/roadmap.md): the
// CI-safe, seeded, bounded adversarial suite. Every test in this package
// combines faults already proven individually by Phases 1-8's own
// scenario tests (see docs/scenario-corpus.md SF-001 through SF-030) --
// this package's job is to combine them under one or more of the
// documented failure classes at once and continuously check durable
// invariants (internal/invariant), rather than re-proving any single
// mechanism in isolation.
//
// Per docs/roadmap.md's "Chaos Must Be Reproducible" requirement, every
// randomized campaign here uses a small, fixed set of seeds (never a
// time-seeded or "run until it happens to fail" source), logs its seed
// via t.Logf before running, and is bounded in size specifically so the
// full suite stays CI-safe (docs/roadmap.md's "CI vs Manual Stress":
// "Keep CI deterministic and bounded... CI must not become flaky or take
// unreasonable time"). Heavier, longer-running variants of the same
// campaigns live in cmd/chaos (see that command's own doc comment) for
// manual/soak invocation.
package chaos_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

// discardLogger silences structured log output during chaos runs -- the
// events themselves are already asserted individually by Phase 8's own
// scenario tests; this package's assertions are against durable state and
// (where noted) metrics, not log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newChaosStore returns a fresh *store.Store backed by a real, disposable
// PostgreSQL database (testutil.DB), per docs/testing-strategy.md's "no
// mock" requirement.
func newChaosStore(t *testing.T) *store.Store {
	t.Helper()
	db := testutil.DB(t)
	return store.New(db, store.WithLogger(discardLogger()))
}

// newChaosParams returns job.NewParams for a fast, CI-safe chaos job: a
// short execution_timeout_seconds keeps heartbeat/timeout-driven tests
// quick, and maxAttempts is caller-controlled since retry-budget
// exhaustion is exactly what several campaigns exercise.
func newChaosParams(jobType string, maxAttempts int) job.NewParams {
	return job.NewParams{
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: 30,
	}
}

// newWorker returns a *worker.Worker with a fast poll interval, wired
// against registry, suitable for chaos campaigns that need the full
// claim-execute-heartbeat-complete loop rather than direct Store calls.
func newWorker(id string, st worker.Store, registry *handler.Registry) *worker.Worker {
	return worker.New(id, st, registry, 2*time.Millisecond, discardLogger())
}

// checkNoViolations runs the full durable-state invariant suite
// (internal/invariant.Checker.CheckAll) and fails the test with every
// violation found, formatted so a failure is self-contained: invariant
// ID, seed (if seed >= 0), and the concrete row(s) involved -- per
// docs/roadmap.md's "a test failure must report enough information to
// reproduce the exact sequence."
func checkNoViolations(t *testing.T, ctx context.Context, checker *invariant.Checker, seed int64) {
	t.Helper()
	violations, err := checker.CheckAll(ctx)
	require.NoError(t, err)
	if len(violations) == 0 {
		return
	}
	msg := ""
	if seed >= 0 {
		msg += fmt.Sprintf("seed=%d\n", seed)
	}
	for _, v := range violations {
		msg += v.String() + "\n"
	}
	t.Fatalf("invariant violations found:\n%s", msg)
}

// mustParseUUID parses a job/workflow ID string this same test generated
// -- a parse failure indicates a test bug, not bad input to handle
// gracefully.
func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}
