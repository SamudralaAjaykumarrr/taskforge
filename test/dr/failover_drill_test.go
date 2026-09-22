// SF-073, SF-074 (docs/phase-15-plan.md §8.2, §22; docs/scenario-corpus.md):
// the controlled standby-promotion/failover drill the roadmap's own
// "Tests / evidence required" names as a literal deliverable -- "at least
// one controlled standby-promotion/failover drill, reconnect/recovery
// time measured and recorded" -- built as permanent, CI-runnable test
// code, driving real cmd/api/cmd/worker binaries (docs/phase-15-plan.md
// §8.2 step 3, §27 OD-3's settled half: real binaries are required here,
// not direct-store, because the property under test is TaskForge's own
// process-level reconnect behavior).
package dr_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
)

// waitForListening polls addr until a TCP connection succeeds (mirrors
// test/procs's own helper of the same name and shape).
func waitForListening(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("dr: %s never started accepting connections", addr)
}

type submitRequest struct {
	JobType                 string          `json:"job_type"`
	Payload                 json.RawMessage `json:"payload"`
	ExecutionTimeoutSeconds int             `json:"execution_timeout_seconds,omitempty"`
}

// submitJob POSTs a real job submission to a real, running cmd/api
// process and returns the parsed job id on success (empty string and the
// raw status/error otherwise -- used both for baseline traffic and for
// the post-promotion recovery-time probe, where failure is the expected
// outcome until the drill's own window closes).
func submitJob(client *http.Client, addr, credential, jobType string, payload json.RawMessage, execTimeout int) (id string, status int, err error) {
	body, mErr := json.Marshal(submitRequest{JobType: jobType, Payload: payload, ExecutionTimeoutSeconds: execTimeout})
	if mErr != nil {
		return "", 0, mErr
	}
	req, rErr := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/jobs", bytes.NewReader(body))
	if rErr != nil {
		return "", 0, rErr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, dErr := client.Do(req)
	if dErr != nil {
		return "", 0, dErr
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		return "", resp.StatusCode, fmt.Errorf("submit: unexpected status %d", resp.StatusCode)
	}
	return out.ID, resp.StatusCode, nil
}

// TestDR_SF073_SF074_FailoverDrill_LiveTraffic is the plan's flagship
// failover proof (docs/phase-15-plan.md §8.2):
//
//  1. A real primary + a real streaming standby (via setup-standby.sh),
//     confirmed caught up via pg_stat_replication.
//  2. Real cmd/api and cmd/worker binaries, driving live HTTP submission
//     and real worker claim/execution traffic against the primary.
//  3. SF-074's precondition: a demo.sleep job claimed and actively
//     in-flight (heartbeating) at the moment the primary is stopped.
//  4. The primary forcibly stopped (pg_ctl stop -m immediate -- a real
//     process stop, not a severed connection: internal/chaos.TerminateBackend
//     is deliberately NOT used here, per §8.2 step 4's own distinction),
//     and the standby immediately promoted (pg_ctl promote) then
//     repointed onto the primary's own now-vacated port (§8.2 step 5's
//     deliberate, documented simplification -- OD-4).
//  5. TASKFORGE_DATABASE_URL on the already-running cmd/api/cmd/worker
//     processes is never changed -- this is deliberate and load-bearing
//     (§8.2 step 5).
//  6. Both API-submission and worker-claim recovery windows are measured
//     independently and recorded (§8.2 step 6).
//  7. internal/invariant.Checker.CheckAll finds zero violations across the
//     whole drill, and the in-flight job's own fate is confirmed sane
//     (§8.2 step 7, SF-074).
func TestDR_SF073_SF074_FailoverDrill_LiveTraffic(t *testing.T) {
	ctx := context.Background()
	archiveDir := t.TempDir() // unused for replication itself, but wal_level=replica requires archive_mode plumbing to be consistent with startPrimary's shared config

	primary := startPrimary(t, archiveDir)
	primaryDB := primary.db(t)
	require.NoError(t, migrate.Up(ctx, primaryDB))

	// Step 1: real streaming standby, via the exact script the runbook
	// tells an operator to run.
	sysBaseBackupBin := findSystemPGBaseBackup(t)

	standbyDataDir := t.TempDir() + "/standby"
	standbyPort := freePort(t)
	out, err := runScript(t, "setup-standby.sh", []string{
		"PGUSER=postgres",
		"PGPASSWORD=postgres",
		"PG_BIN=" + primary.binDir,
		"PG_BASEBACKUP_BIN=" + sysBaseBackupBin,
		"START=1",
		"PGPORT=" + fmt.Sprint(standbyPort),
	}, "127.0.0.1", fmt.Sprint(primary.port), standbyDataDir)
	require.NoError(t, err, "setup-standby.sh output:\n%s", out)

	waitUntil(t, 15*time.Second, 100*time.Millisecond, "standby never reached streaming state in pg_stat_replication", func() bool {
		var state string
		err := primaryDB.QueryRowContext(ctx, `SELECT state FROM pg_stat_replication LIMIT 1`).Scan(&state)
		return err == nil && state == "streaming"
	})
	t.Log("standby confirmed streaming and caught up")

	// Step 2: real cmd/api and cmd/worker binaries, driving live traffic
	// against the primary.
	workerBin, apiBin := buildBinaries(t)
	credential := mintCredential(t, primary.dsn)
	apiAddr := freeTCPAddr(t)

	apiProc := startProcess(t, apiBin,
		"TASKFORGE_DATABASE_URL="+primary.dsn,
		"TASKFORGE_HTTP_ADDR="+apiAddr,
		"TASKFORGE_API_KEY_PEPPER="+testPepper,
		"TASKFORGE_METRICS_ADDR=",
	)
	workerProc := startProcess(t, workerBin,
		"TASKFORGE_DATABASE_URL="+primary.dsn,
		"TASKFORGE_WORKER_POLL_INTERVAL=100ms",
		"TASKFORGE_METRICS_ADDR=",
	)
	defer func() {
		if t.Failed() {
			apiProc.logOutput(t)
			workerProc.logOutput(t)
		}
	}()
	waitForListening(t, apiAddr, 10*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}

	// Baseline: confirm ordinary traffic works before touching anything.
	baselineID, status, err := submitJob(client, apiAddr, credential, "demo.echo", []byte(`{}`), 0)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	waitUntil(t, 5*time.Second, 50*time.Millisecond, "baseline job never reached SUCCEEDED", func() bool {
		return jobState(t, ctx, primaryDB, baselineID) == "SUCCEEDED"
	})

	// SF-074 precondition: a job claimed and actively in-flight
	// (heartbeating) at the instant the primary dies. execution_timeout_seconds=9
	// gives a heartbeat interval of 3s (leaseDuration/heartbeatIntervalFraction,
	// internal/worker.go); sleep_ms=6000 keeps the handler running across
	// at least one heartbeat and across the promotion itself.
	inFlightID, status, err := submitJob(client, apiAddr, credential, "demo.sleep", []byte(`{"sleep_ms":6000}`), 9)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	waitUntil(t, 5*time.Second, 50*time.Millisecond, "in-flight job never reached RUNNING before promotion", func() bool {
		return jobState(t, ctx, primaryDB, inFlightID) == "RUNNING"
	})
	t.Log("SF-074 precondition confirmed: job is RUNNING (claimed, in-flight) before the primary is stopped")

	// Step 4: forcibly stop the primary (a real process stop -- the
	// closest local analogue to an actual primary-host failure) and
	// immediately promote the standby.
	stopInstant := time.Now()
	stopCmd := exec.Command(primary.pgCtl(), "stop", "-m", "immediate", "-w", "-D", primary.dataDir)
	stopOut, stopErr := stopCmd.CombinedOutput()
	require.NoError(t, stopErr, "pg_ctl stop -m immediate: %s", stopOut)
	primary.markStoppedExternally() // bypasses embedded-postgres's own Stop() bookkeeping -- see its doc comment

	promoteCmd := exec.Command(primary.pgCtl(), "promote", "-w", "-D", standbyDataDir)
	promoteOut, promoteErr := promoteCmd.CombinedOutput()
	require.NoError(t, promoteErr, "pg_ctl promote: %s", promoteOut)

	// Repoint the promoted standby onto the primary's own now-vacated
	// port -- docs/phase-15-plan.md §8.2 step 5's deliberate,
	// documented simplification (OD-4): a real deployment's HA topology
	// (a VIP, DNS, HAProxy/PgBouncer) is what makes "the same connection
	// string now points at the new primary" true; this drill reproduces
	// that fact locally without depending on any of that infrastructure.
	standbyStopCmd := exec.Command(primary.pgCtl(), "stop", "-m", "fast", "-w", "-D", standbyDataDir)
	standbyStopOut, standbyStopErr := standbyStopCmd.CombinedOutput()
	require.NoError(t, standbyStopErr, "pg_ctl stop standby for repoint: %s", standbyStopOut)

	repointCmd := exec.Command(primary.pgCtl(), "start", "-w", "-D", standbyDataDir,
		"-l", standbyDataDir+"-repointed.log", "-o", fmt.Sprintf("-p %d", primary.port))
	repointOut, repointErr := repointCmd.CombinedOutput()
	require.NoError(t, repointErr, "pg_ctl start (repointed onto primary's port): %s", repointOut)
	t.Cleanup(func() {
		_ = exec.Command(primary.pgCtl(), "stop", "-m", "immediate", "-D", standbyDataDir).Run()
	})

	promotedDB := primary.db(t) // same DSN (primary.port); a fresh *sql.DB dialing the now-promoted instance, for harness-side assertions independent of cmd/api/cmd/worker's own pools
	waitUntil(t, 15*time.Second, 50*time.Millisecond, "promoted standby never reported itself out of recovery", func() bool {
		var inRecovery bool
		err := promotedDB.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery)
		return err == nil && !inRecovery
	})
	t.Logf("infrastructure-side promotion complete %s after primary stop", time.Since(stopInstant))

	// Step 6: measure TaskForge's OWN reconnect/recovery windows,
	// independently for the API-submission path and the worker-claim
	// path (§8.2 step 6 -- they may differ, since internal/worker's
	// poll-and-backoff loop and cmd/api's per-request handling have
	// different retry shapes).
	var apiRecovered time.Time
	waitUntil(t, 30*time.Second, 100*time.Millisecond, "cmd/api never resumed successful submissions after the promotion", func() bool {
		_, status, err := submitJob(client, apiAddr, credential, "demo.echo", []byte(`{}`), 0)
		if err == nil && status == http.StatusCreated {
			apiRecovered = time.Now()
			return true
		}
		return false
	})
	apiRecoveryWindow := apiRecovered.Sub(stopInstant)

	probeID, status, err := submitJob(client, apiAddr, credential, "demo.echo", []byte(`{}`), 0)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	var workerRecovered time.Time
	waitUntil(t, 30*time.Second, 100*time.Millisecond, "cmd/worker never resumed claiming after the promotion", func() bool {
		if jobState(t, ctx, promotedDB, probeID) == "SUCCEEDED" {
			workerRecovered = time.Now()
			return true
		}
		return false
	})
	workerRecoveryWindow := workerRecovered.Sub(stopInstant)

	t.Logf("SF-073 MEASURED RECOVERY WINDOWS (from primary stop instant): API submission resumed after %s; worker claim resumed after %s", apiRecoveryWindow, workerRecoveryWindow)

	// Step 7: confirm the in-flight job's fate is sane -- either it
	// completed against the promoted standby (lease/fencing state was
	// replicated), or it is left for ordinary TF-INV-004 lease-expiry
	// reclaim. Either is acceptable; silent loss or duplication is not.
	waitUntil(t, 30*time.Second, 200*time.Millisecond, "in-flight job never reached a settled state (SUCCEEDED or reclaimable RUNNING) after the promotion", func() bool {
		state := jobState(t, ctx, promotedDB, inFlightID)
		return state == "SUCCEEDED" || state == "RUNNING" || state == "RETRY_WAIT"
	})
	finalState := jobState(t, ctx, promotedDB, inFlightID)
	t.Logf("SF-074: in-flight job's final observed state after the drill: %s", finalState)

	violations, err := invariant.New(promotedDB).CheckAll(ctx)
	require.NoError(t, err, "invariant checker itself failed against the promoted database")
	require.Empty(t, violations, "invariant violations found after the failover drill: %v", violations)
}

// jobState reads a job's current state by its string id (as returned by
// the HTTP submission response), returning "" if the query itself fails
// (e.g. the database endpoint is mid-failover) so callers can poll
// through a transient error rather than fail the test on it.
func jobState(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = $1`, id).Scan(&state); err != nil {
		return ""
	}
	return state
}
