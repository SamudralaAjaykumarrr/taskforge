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

// claimUntilResolved retries s.Claim until it returns either a claimed job
// (ok=true) or a definitive error (err != nil). Claim's own contract
// documents ok=false/err=nil ("nothing eligible right now") as a normal,
// valid outcome, never a failure -- so a test asserting against a single
// Claim call's result must not assume a freshly-eligible job (or a job
// just made eligible by a predecessor's completion, a workflow-dependency
// activation, or a chaos.Force* helper) is necessarily selected by the
// very first call. docs/failure-model.md's Clock Model explicitly permits
// "NTP-slew style bounded correction" of PostgreSQL's own clock; such a
// correction landing between the transaction that set eligible_at (via
// now()) and a subsequent Claim's own now() can make the row appear
// briefly (single-digit-to-tens of milliseconds) in the future -- a
// transient, benign delay, not a lost or stuck job. This loop is bounded
// well beyond any such correction so a genuine "nothing will ever be
// eligible" bug still fails the test promptly, not silently. Used at
// every call site in this package where a single Claim is expected to
// immediately return a specific just-made-eligible job; drain-style loops
// that already tolerate ok=false as "queue exhausted" do not need it.
func claimUntilResolved(t *testing.T, ctx context.Context, s *store.Store, workerID string) (*job.Job, bool, error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, ok, err := s.Claim(ctx, workerID)
		if ok || err != nil {
			return j, ok, err
		}
		if time.Now().After(deadline) {
			return j, ok, err
		}
		time.Sleep(time.Millisecond)
	}
}

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
