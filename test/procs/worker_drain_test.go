// SF-064, SF-065, and SF-069 (docs/phase-14-plan.md §16): the real
// cmd/worker OS-process proofs for Phase 14's graceful-drain redesign
// (§6.4, §19 OD-3). Each test inserts a job directly (no API server
// needed -- cmd/worker talks to PostgreSQL directly), starts a real
// compiled cmd/worker binary against it, sends a real signal, and asserts
// on the job's durable row exactly as the plan's proof obligations
// require.
package procs_test

import (
	"context"
	"encoding/json"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

func insertSleepJob(t *testing.T, s *store.Store, jobType string, sleepMS int, executionTimeoutSeconds int) uuid.UUID {
	t.Helper()
	payload, err := json.Marshal(map[string]int{"sleep_ms": sleepMS})
	require.NoError(t, err)
	created, err := s.Insert(context.Background(), job.NewParams{
		PrincipalID:             principal.SystemPrincipalID,
		JobType:                 jobType,
		Payload:                 payload,
		MaxAttempts:             3,
		ExecutionTimeoutSeconds: executionTimeoutSeconds,
	})
	require.NoError(t, err)
	return created.ID
}

func jobState(t *testing.T, s *store.Store, id uuid.UUID) (state string, leaseGen int64) {
	t.Helper()
	access := principal.AccessContext{PrincipalID: principal.SystemPrincipalID, IsAdmin: true}
	j, err := s.GetByID(context.Background(), id, access)
	require.NoError(t, err)
	return string(j.State), j.LeaseGeneration
}

// TestProc_SF064_WorkerDrain_InFlightJobFinishesBeforeDeadline is SF-064:
// a real cmd/worker binary (which alone calls SetDrainTimeout) executing
// a job receives SIGTERM; the in-flight job completes normally before the
// process exits, because it finishes within
// TASKFORGE_WORKER_DRAIN_TIMEOUT; no new claim is attempted after SIGTERM.
func TestProc_SF064_WorkerDrain_InFlightJobFinishesBeforeDeadline(t *testing.T) {
	workerBin, _ := buildBinaries(t)
	db, dsn := openDB(t)
	s := store.New(db)

	const jobType = "demo.sleep"
	// The handler sleeps 1.5s; the configured drain window is 5s, so the
	// job must finish well inside the drain budget.
	jobID := insertSleepJob(t, s, jobType, 1500, 30)

	h := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_WORKER_DRAIN_TIMEOUT=5s",
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=", // disable the standalone metrics listener; not needed here
	)

	waitUntil(t, 5*time.Second, 25*time.Millisecond, "job was never claimed", func() bool {
		state, _ := jobState(t, s, jobID)
		return state == "RUNNING"
	})

	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))

	waitErr := make(chan error, 1)
	go func() { waitErr <- h.cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			h.logOutput(t)
			t.Fatalf("worker process exited with error: %v", err)
		}
	case <-time.After(8 * time.Second):
		h.logOutput(t)
		t.Fatal("worker process did not exit after SIGTERM within the drain budget")
	}

	state, leaseGen := jobState(t, s, jobID)
	require.Equal(t, "SUCCEEDED", state, "an in-flight job that finishes within the drain window must be completed normally, not abandoned")
	require.Equal(t, int64(1), leaseGen, "the original worker's own attempt must be what completed the job -- no reclaim was needed")
}

// TestProc_SF069_WorkerDrainTimeout_NoCompletionWritten_LeaseReclaimable
// is SF-069: a real cmd/worker binary's TASKFORGE_WORKER_DRAIN_TIMEOUT
// elapses while a job is still executing; dispositionDraining fires, no
// Complete* call is made, the job row is left RUNNING under the draining
// worker's now-abandoned lease, and once lease_expires_at passes, a
// second, freshly-started worker reclaims and completes it through the
// ordinary, unmodified TF-INV-004 path.
func TestProc_SF069_WorkerDrainTimeout_NoCompletionWritten_LeaseReclaimable(t *testing.T) {
	workerBin, _ := buildBinaries(t)
	db, dsn := openDB(t)
	s := store.New(db)

	const jobType = "demo.sleep"
	// The handler is asked to sleep far longer than the configured drain
	// window (2s), but it cooperatively returns the instant its context
	// is cancelled -- which the drain-timeout watcher does once its 2s
	// elapses -- so the real wall-clock cost of this test is ~2s, not
	// the full requested sleep duration.
	jobID := insertSleepJob(t, s, jobType, 60_000, 90)

	h := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_WORKER_DRAIN_TIMEOUT=2s",
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=",
	)

	waitUntil(t, 5*time.Second, 25*time.Millisecond, "job was never claimed", func() bool {
		state, _ := jobState(t, s, jobID)
		return state == "RUNNING"
	})

	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))

	waitErr := make(chan error, 1)
	go func() { waitErr <- h.cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			h.logOutput(t)
			t.Fatalf("worker process exited with error: %v", err)
		}
	case <-time.After(6 * time.Second):
		h.logOutput(t)
		t.Fatal("worker process did not exit after its drain timeout elapsed")
	}

	stranded, leaseGen := jobState(t, s, jobID)
	require.Equal(t, "RUNNING", stranded, "a drain-timeout expiry must leave the job's row exactly as it was -- no completion of any kind")
	require.Equal(t, int64(1), leaseGen)

	var finishedAt *time.Time
	var outcome *string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT finished_at, outcome FROM job_attempts WHERE job_id = $1`, jobID).Scan(&finishedAt, &outcome))
	require.Nil(t, finishedAt, "no job_attempts row may be finalized by a drain-timeout expiry")
	require.Nil(t, outcome)

	// TF-INV-004: once the abandoned lease expires, an ordinary fresh
	// worker reclaims and completes the job normally.
	_, err := db.ExecContext(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID)
	require.NoError(t, err)

	h2 := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=",
	)
	// This second worker will reclaim the job and try to sleep 60s again
	// (the payload is unchanged) -- it only needs to observe the claim,
	// not finish, to prove reclaimability, so it is killed as soon as the
	// reclaim (lease_generation 2) is observed.
	waitUntil(t, 5*time.Second, 25*time.Millisecond, "the job was never reclaimed by a fresh worker", func() bool {
		_, leaseGen := jobState(t, s, jobID)
		return leaseGen == 2
	})
	require.NoError(t, h2.cmd.Process.Kill())
	_ = h2.cmd.Wait()

	reclaimedState, reclaimedGen := jobState(t, s, jobID)
	require.Equal(t, "RUNNING", reclaimedState)
	require.Equal(t, int64(2), reclaimedGen, "a fresh worker must have reclaimed the job under a new lease_generation")
}

// TestProc_SF065_WorkerSIGKILL_LeaseExpiryReclaimUnaffected is SF-065: a
// worker forcibly killed via SIGKILL (not SIGTERM) mid-execution is
// explicitly out of the drain contract's scope (docs/phase-14-plan.md
// §14) -- the job is reclaimed by ordinary lease-expiry exactly as
// pre-Phase-14, proving the drain redesign did not accidentally change
// the SIGKILL/lease-expiry path.
func TestProc_SF065_WorkerSIGKILL_LeaseExpiryReclaimUnaffected(t *testing.T) {
	workerBin, _ := buildBinaries(t)
	db, dsn := openDB(t)
	s := store.New(db)

	const jobType = "demo.sleep"
	jobID := insertSleepJob(t, s, jobType, 60_000, 90)

	h := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_WORKER_DRAIN_TIMEOUT=30s", // irrelevant to SIGKILL, which bypasses Go signal handling entirely
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=",
	)

	waitUntil(t, 5*time.Second, 25*time.Millisecond, "job was never claimed", func() bool {
		state, _ := jobState(t, s, jobID)
		return state == "RUNNING"
	})

	require.NoError(t, h.cmd.Process.Kill()) // SIGKILL
	_ = h.cmd.Wait()

	stranded, leaseGen := jobState(t, s, jobID)
	require.Equal(t, "RUNNING", stranded)
	require.Equal(t, int64(1), leaseGen)

	_, err := db.ExecContext(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID)
	require.NoError(t, err)

	recoveryHandle := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=",
	)
	waitUntil(t, 5*time.Second, 25*time.Millisecond, "the SIGKILLed worker's job was never reclaimed", func() bool {
		_, leaseGen := jobState(t, s, jobID)
		return leaseGen == 2
	})
	require.NoError(t, recoveryHandle.cmd.Process.Kill())
	_ = recoveryHandle.cmd.Wait()
}
