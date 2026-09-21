// Package procs_test holds Phase 14's real, separately-compiled
// OS-process proofs (docs/phase-14-plan.md §16: SF-063, SF-064, SF-065,
// SF-069, SF-070) -- a new test category for this repository. Every other
// integration/chaos/stress test in internal/store, internal/worker, and
// internal/chaos exercises Go packages directly, in one process, against
// a real PostgreSQL instance; the tests in this directory instead build
// real cmd/worker and cmd/api binaries and run them as real OS processes,
// sending them real signals (SIGTERM, SIGKILL), because that is what the
// roadmap's graceful-drain requirement is actually about: proving the
// documented SIGTERM contract holds for the real, compiled artifact an
// operator deploys, not merely for the internal/worker.Worker Go type in
// isolation (that in-process contract is separately proven by
// internal/worker/drain_test.go's SF-064a and the unmodified Phase 5
// stress test, SF-064b).
package procs_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

var (
	buildOnce sync.Once
	workerBin string
	apiBin    string
	buildErr  error
)

// repoRoot locates the module root by walking up from this file's own
// source location, so `go build ./cmd/...` resolves correctly regardless
// of the test binary's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("procs: could not determine this test file's own location")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("procs: could not locate go.mod above %s", thisFile)
		}
		dir = parent
	}
}

// buildBinaries compiles real cmd/worker and cmd/api binaries exactly
// once for this test binary's whole run, into a shared temporary
// directory. The binaries are deliberately not removed on a per-test
// basis (only once, implicitly, when the OS reclaims its temp directory)
// since every test in this package reuses them.
func buildBinaries(t *testing.T) (workerPath, apiPath string) {
	t.Helper()
	buildOnce.Do(func() {
		root := repoRoot(t)
		dir, err := os.MkdirTemp("", "taskforge-procs-bin-*")
		if err != nil {
			buildErr = fmt.Errorf("mkdir temp bin dir: %w", err)
			return
		}
		workerBin = filepath.Join(dir, "taskforge-worker")
		apiBin = filepath.Join(dir, "taskforge-api")
		for _, b := range []struct{ out, pkg string }{
			{workerBin, "./cmd/worker"},
			{apiBin, "./cmd/api"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = root
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if runErr := cmd.Run(); runErr != nil {
				buildErr = fmt.Errorf("go build -o %s %s: %w: %s", b.out, b.pkg, runErr, stderr.String())
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("build test OS-process binaries: %v", buildErr)
	}
	return workerBin, apiBin
}

// openDB opens a *sql.DB against a fresh, migrated, empty-tables test
// database (testutil.DSN's own contract), for direct setup/assertion
// queries the test makes alongside whatever real OS-process binary it
// spawns against the same database via TASKFORGE_DATABASE_URL.
func openDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := testutil.DSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("procs: open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dsn
}

// freeTCPAddr returns an address on 127.0.0.1 with an OS-assigned free
// port, formatted for TASKFORGE_HTTP_ADDR/TASKFORGE_METRICS_ADDR. The
// listener is closed before returning: there is an inherent, accepted
// race between finding the port and the spawned binary binding it (the
// same technique internal/testutil's embedded-postgres startup already
// uses for exactly this reason).
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("procs: find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// waitUntil polls cond every interval until it returns true or timeout
// elapses, failing the test with msg on timeout.
func waitUntil(t *testing.T, timeout, interval time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatal(msg)
}

// procHandle bundles a spawned OS process with its captured output, for
// clear failure diagnostics.
type procHandle struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// startProcess starts bin with env (in addition to the current process's
// own environment) and returns a handle. It does not wait for readiness;
// callers poll for whatever readiness signal fits the test (a claimed
// job's row, an HTTP listener accepting connections, etc.).
func startProcess(t *testing.T, bin string, env ...string) *procHandle {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	h := &procHandle{cmd: cmd, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	cmd.Stdout = h.stdout
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("procs: start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		if h.cmd.Process != nil && h.cmd.ProcessState == nil {
			_ = h.cmd.Process.Kill()
			_, _ = h.cmd.Process.Wait()
		}
	})
	return h
}

// logOutput dumps captured stdout/stderr to the test log -- called on
// failure paths so a real process's own structured logs are visible
// without re-running manually.
func (h *procHandle) logOutput(t *testing.T) {
	t.Helper()
	t.Logf("stdout:\n%s", h.stdout.String())
	t.Logf("stderr:\n%s", h.stderr.String())
}
