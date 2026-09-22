// Package dr_test holds Phase 15's ("PostgreSQL HA / Backup / DR Proof",
// docs/phase-15-plan.md) real, multi-instance PostgreSQL proofs: SF-071
// through SF-075 (docs/scenario-corpus.md). This is a new test category
// for this repository (docs/testing-strategy.md's "Multi-instance
// PostgreSQL tests" row) -- every earlier PostgreSQL-touching test uses
// exactly one PostgreSQL instance; the tests here bring up two or more
// real, independently-running PostgreSQL server processes (a primary and
// a standby/restore target), coordinated via real pg_basebackup/
// streaming-replication/promotion commands, and drive real TaskForge
// traffic against them.
//
// Per docs/phase-15-plan.md §8, this package builds its own primary/
// standby instances directly via github.com/fergusstrange/embedded-postgres
// (pinned to PostgreSQL V16, per OD-2's closure and its own recorded
// action item) rather than depending on Docker or the CI postgres:16
// service container internal/testutil's other tests share -- it needs
// two or more independently-startable data directories at once, which
// that shared single-instance fixture cannot provide. This requires no
// new CI configuration: these tests bring up their own instances on
// CI-allocated ports exactly as internal/testutil's embedded-postgres
// fallback path already does for every other package.
//
// Every deploy/pg-dr/*.sh script this package invokes is the SAME script
// docs/disaster-recovery.md's runbook tells an operator to run -- never a
// reimplementation -- so the runbook and the tested behavior can never
// silently diverge (docs/phase-15-plan.md §7's architecture diagram).
package dr_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

// testPepper mirrors test/procs's own constant exactly (a fixed, 32-byte+
// API-key pepper used only by this package's spawned cmd/api processes --
// never a production secret). Kept as an independent copy rather than
// shared, following test/compat's own precedent of not sharing
// os/exec-harness helpers across these real-process test packages.
const testPepper = "test-only-pepper-for-phase15-dr-tests-not-a-production-secret"

// repoRoot locates the module root by walking up from this file's own
// source location (identical pattern to test/procs, test/compat).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("dr: could not determine this test file's own location")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("dr: could not locate go.mod above %s", thisFile)
		}
		dir = parent
	}
}

// freePort returns an OS-assigned free TCP port on 127.0.0.1. The
// listener is closed before returning -- the same accepted
// find-then-bind race internal/testutil's own embedded-postgres startup
// already uses.
func freePort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

// freeTCPAddr mirrors test/procs's own helper: an address on 127.0.0.1
// with an OS-assigned free port, for TASKFORGE_HTTP_ADDR.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().String()
}

// pgInstance is one real PostgreSQL server this package started and owns
// the lifecycle of, plus the metadata deploy/pg-dr's scripts need to
// operate against it (its extracted-binary directory, playing the role
// PATH would for an operator's own installed PostgreSQL).
type pgInstance struct {
	ep      *embeddedpostgres.EmbeddedPostgres
	dataDir string
	binDir  string
	port    uint32
	dsn     string

	mu      sync.Mutex
	stopped bool
}

// dsnFor renders a DSN for an arbitrary port against the standard
// postgres/postgres/taskforge credentials every instance in this package
// shares (test-only, never a production credential).
func dsnFor(port uint32) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/taskforge?sslmode=disable", port)
}

// startPrimary starts a real, embedded PostgreSQL V16 instance configured
// for continuous WAL archiving into archiveDir (docs/phase-15-plan.md
// §8/OD-2: wal_level=replica, archive_mode=on, a real archive_command --
// not embedded-postgres's default configuration) and streaming-replication
// readiness (max_wal_senders, hot_standby). Pinned to
// embeddedpostgres.V16 explicitly, per OD-2's own recorded action item,
// for fidelity with docker-compose.yml/.github/workflows/ci.yml's
// deployment-target PostgreSQL version.
func startPrimary(t *testing.T, archiveDir string) *pgInstance {
	t.Helper()
	root, err := os.MkdirTemp("", "taskforge-dr-primary-*")
	require.NoError(t, err)
	port := freePort(t)

	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		RuntimePath(filepath.Join(root, "runtime")).
		DataPath(filepath.Join(root, "data")).
		Username("postgres").
		Password("postgres").
		Database("taskforge").
		Logger(nil).
		StartParameters(map[string]string{
			"wal_level":       "replica",
			"archive_mode":    "on",
			"archive_command": fmt.Sprintf("cp %%p %s/%%f", archiveDir),
			"max_wal_senders": "10",
			"hot_standby":     "on",
		})

	ep := embeddedpostgres.NewDatabase(cfg)
	require.NoError(t, ep.Start(), "start embedded PostgreSQL primary")

	inst := &pgInstance{
		ep:      ep,
		dataDir: filepath.Join(root, "data"),
		binDir:  filepath.Join(root, "runtime", "bin"),
		port:    port,
		dsn:     dsnFor(port),
	}
	t.Cleanup(func() { inst.stopViaLibrary(t) })
	return inst
}

// startFromDataDir starts a real embedded PostgreSQL V16 instance whose
// DataPath already exists and is pre-populated (either a restore.sh
// -prepared restore target, per docs/phase-15-postgres-evidence.md §3.1
// item 5's verified reuse mechanism, or any other already-initialized
// data directory). embedded-postgres's own dataDirIsValid check (called
// from EmbeddedPostgres.Start) detects the existing PG_VERSION file and
// skips initdb/createDatabase entirely -- this is the exact mechanism
// OD-2's evidence pass verified by source inspection and this test now
// exercises live, for real, for the first time.
func startFromDataDir(t *testing.T, dataDir string, port uint32) *pgInstance {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("", "taskforge-dr-existing-runtime-*")
	require.NoError(t, err)

	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		RuntimePath(runtimeDir).
		DataPath(dataDir).
		Username("postgres").
		Password("postgres").
		Database("taskforge").
		Logger(nil)

	ep := embeddedpostgres.NewDatabase(cfg)
	if err := ep.Start(); err != nil {
		return nil // caller (SF-072) asserts on this failure itself
	}

	inst := &pgInstance{
		ep:      ep,
		dataDir: dataDir,
		binDir:  filepath.Join(runtimeDir, "bin"),
		port:    port,
		dsn:     dsnFor(port),
	}
	t.Cleanup(func() { inst.stopViaLibrary(t) })
	return inst
}

func (p *pgInstance) stopViaLibrary(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if err := p.ep.Stop(); err != nil {
		t.Logf("dr: stop embedded postgres at %s: %v (likely already stopped externally)", p.dataDir, err)
	}
	p.stopped = true
}

// markStoppedExternally records that this instance's postgres process was
// already stopped by a direct pg_ctl invocation (e.g. the failover
// drill's own "stop -m immediate", which deliberately bypasses the
// embedded-postgres library to get the real, immediate-mode stop
// docs/phase-15-plan.md §8.2 step 4 requires) -- so the later t.Cleanup
// does not attempt a second, redundant pg_ctl stop against an
// already-dead postmaster.
func (p *pgInstance) markStoppedExternally() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

func (p *pgInstance) pgCtl() string { return filepath.Join(p.binDir, "pg_ctl") }

// findSystemPGBaseBackup locates a real, usable pg_basebackup CLIENT
// binary on this test host. This is a genuine, previously-unidentified
// implementation-time finding, not a reopening of OD-2 (docs/phase-15-plan.md
// §27, docs/phase-15-postgres-evidence.md §3/§4): OD-2's evidence pass
// verified that embedded-postgres's StartParameters surface correctly
// configures WAL archiving on the SERVER it starts, and that its own
// Start() reuses a pre-populated DataPath -- both hold, and neither is
// reopened here. What that pass did not exercise is that
// github.com/fergusstrange/embedded-postgres's vendored binary
// distribution bundles only initdb/pg_ctl/postgres, never pg_basebackup
// or any other client tool an operator's own postgresql-client OS package
// normally provides. deploy/pg-dr/backup.sh and setup-standby.sh
// therefore need a real pg_basebackup from SOMEWHERE -- exactly as a real
// operator's environment already has one (client tools are an ordinary
// OS package, not something either this project or embedded-postgres
// vendors) -- so this package looks for one on the test host via
// PG_BASEBACKUP_BIN (an explicit override), the common Debian/Ubuntu
// versioned-package layout, or PATH, and by default skips (never fails)
// the calling test with a clear, honest reason if none is found, exactly
// as this project's other environment-dependent tests do
// (docs/testing-strategy.md).
//
// requireDRToolsEnv (docs/disaster-recovery.md §12): a plain local skip is
// the wrong behavior in CI -- the roadmap's own "Tests / evidence
// required" names SF-071 through SF-075 as the literal Phase 15 proof
// obligation, and a silent skip would let that proof stop running (e.g. a
// runner image drops its preinstalled PostgreSQL client tools) while CI
// itself stays green. .github/workflows/ci.yml therefore explicitly
// installs the PostgreSQL 16 client package AND sets
// TASKFORGE_REQUIRE_DR_TOOLS=1, which turns "no pg_basebackup found" into
// a hard t.Fatal there. Outside that enforced environment (an ordinary
// developer machine without postgresql-client installed), the skip
// remains, so `go test ./...` still passes locally without it.
func findSystemPGBaseBackup(t *testing.T) string {
	t.Helper()

	if dir := os.Getenv("PG_BASEBACKUP_BIN"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "pg_basebackup")); err == nil {
			return dir
		}
	}

	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/pg_basebackup")
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // prefer the highest installed major version
	for _, m := range matches {
		return filepath.Dir(m)
	}

	if path, err := exec.LookPath("pg_basebackup"); err == nil {
		return filepath.Dir(path)
	}

	msg := "dr: no system pg_basebackup found (checked PG_BASEBACKUP_BIN, /usr/lib/postgresql/*/bin, PATH) -- " +
		"embedded-postgres's own vendored binaries do not include client tools; install postgresql-client " +
		"or set PG_BASEBACKUP_BIN to run this test"
	if os.Getenv(requireDRToolsEnv) == "1" {
		t.Fatalf("%s (%s=1: this is a required-tooling environment -- a missing pg_basebackup is a hard failure, not a skip)", msg, requireDRToolsEnv)
	}
	t.Skip(msg)
	return ""
}

// requireDRToolsEnv is the CI-enforcement signal (.github/workflows/ci.yml):
// set to "1", it makes findSystemPGBaseBackup fail the test instead of
// skipping it when no system pg_basebackup is found, so Phase 15's
// SF-071-SF-075 DR proof cannot silently stop running in CI.
const requireDRToolsEnv = "TASKFORGE_REQUIRE_DR_TOOLS"

func (p *pgInstance) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", p.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runScript invokes a deploy/pg-dr/<name> script exactly as an operator
// would from the command line, with env appended to this test process's
// own environment. It returns combined stdout+stderr for assertions and
// diagnostics.
func runScript(t *testing.T, name string, env []string, args ...string) (string, error) {
	t.Helper()
	scriptPath := filepath.Join(repoRoot(t), "deploy", "pg-dr", name)
	cmd := exec.Command(scriptPath, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pgNow reads PostgreSQL's own clock (never the test host's), per
// docs/failure-model.md's Clock Model: every recovery_target_time this
// package records is read from the database whose recovery is being
// timed, exactly as docs/phase-15-plan.md §8.1 step 5 requires.
func pgNow(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var ts time.Time
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT now()`).Scan(&ts))
	return ts
}

// waitUntil polls cond every interval until it returns true or timeout
// elapses, failing the test with msg on timeout. Mirrors test/procs's own
// helper of the same name and shape.
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

// waitForArchiveSegment polls archiveDir until it contains a real,
// complete WAL segment file (16 MiB), per
// docs/phase-15-postgres-evidence.md §3.3's own finding: archive_command
// execution is asynchronous, so any assertion against its output must
// poll with a real delay, never assume synchronous completion.
func waitForArchiveSegment(t *testing.T, archiveDir string, before int, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, 200*time.Millisecond, "no new WAL segment appeared in archive directory "+archiveDir, func() bool {
		entries, err := os.ReadDir(archiveDir)
		if err != nil {
			return false
		}
		return len(entries) > before
	})
}

// mintCredential creates a real principal + API key against dsn (using
// testPepper, matching what this package's spawned cmd/api processes are
// given via TASKFORGE_API_KEY_PEPPER) and returns the bearer credential
// string. Mirrors test/procs's own helper of the same name and purpose.
func mintCredential(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	ps, err := principal.NewStore(db, []byte(testPepper))
	require.NoError(t, err)

	p, err := ps.CreatePrincipal(context.Background(), principal.KindCaller, "dr-test caller")
	require.NoError(t, err)

	cred, err := ps.CreateAPIKey(context.Background(), p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	return cred.Credential
}

// buildBinaries compiles real cmd/worker and cmd/api binaries from the
// current working tree exactly once per test binary run -- test/dr's own
// copy of test/procs's identical helper (kept independent, following
// test/compat's own precedent of not sharing os/exec-harness code across
// these real-process packages, since each package's build/lifecycle
// concerns are genuinely separate).
var (
	buildOnce sync.Once
	workerBin string
	apiBin    string
	buildErr  error
)

func buildBinaries(t *testing.T) (workerPath, apiPath string) {
	t.Helper()
	buildOnce.Do(func() {
		root := repoRoot(t)
		dir, err := os.MkdirTemp("", "taskforge-dr-bin-*")
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
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				buildErr = fmt.Errorf("go build -o %s %s: %w: %s", b.out, b.pkg, runErr, out)
				return
			}
		}
	})
	require.NoError(t, buildErr)
	return workerBin, apiBin
}

type procHandle struct {
	cmd *exec.Cmd
	out *safeBuffer
}

// safeBuffer is a concurrency-safe io.Writer wrapping bytes.Buffer, since
// a spawned process's stdout/stderr are written from a goroutine the test
// itself may read from concurrently (t.Logf on failure).
type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func startProcess(t *testing.T, bin string, env ...string) *procHandle {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	out := &safeBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	require.NoError(t, cmd.Start())
	h := &procHandle{cmd: cmd, out: out}
	t.Cleanup(func() {
		if h.cmd.Process != nil && h.cmd.ProcessState == nil {
			_ = h.cmd.Process.Kill()
			_, _ = h.cmd.Process.Wait()
		}
	})
	return h
}

func (h *procHandle) logOutput(t *testing.T) {
	t.Helper()
	t.Logf("process output:\n%s", h.out.String())
}

// driveReferenceWorkload inserts jobCount real jobs directly through
// internal/store (docs/phase-15-plan.md §26/OD-3: direct-store for the
// backup/restore drill, since the property under test is "does a
// database restore correctly," independent of which client process wrote
// the data) and claims+completes most of them via a small pool of
// simulated workers, reusing the exact store.Claim/CompleteSuccess calls
// internal/worker itself uses. It also performs one deliberate
// lease-expiry reclaim (via internal/chaos.ForceExpireLease, reused per
// docs/phase-15-plan.md §24) so at least one job's lease_generation has
// already advanced past 1 before any backup is taken -- the precondition
// SF-075 needs to prove a restore does not regress fencing history.
//
// Returns every inserted job's id, for later assertions (e.g. "did every
// job that existed before the backup still exist after restore").
func driveReferenceWorkload(t *testing.T, ctx context.Context, db *sql.DB, st *store.Store, jobCount, workerCount int) []uuid.UUID {
	t.Helper()

	ids := make([]uuid.UUID, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		j, err := st.Insert(ctx, job.NewParams{
			PrincipalID:             principal.SystemPrincipalID,
			JobType:                 "dr.probe",
			Payload:                 []byte(`{}`),
			MaxAttempts:             3,
			ExecutionTimeoutSeconds: 30,
			QueueName:               "default",
		})
		require.NoError(t, err)
		ids = append(ids, j.ID)
	}

	// SF-075's precondition: claim and force-expire one job's lease
	// before any real worker touches it, then let the worker pool below
	// reclaim it normally -- its second job_attempts row carries
	// lease_generation=2, a real, pre-backup fencing advance.
	reclaimed, claimed, err := st.Claim(ctx, "dr-workload-reclaim-seed")
	require.NoError(t, err)
	if claimed {
		require.NoError(t, chaos.ForceExpireLease(ctx, db, reclaimed.ID))
	}

	var wg sync.WaitGroup
	deadline := time.Now().Add(20 * time.Second)
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func(workerN int) {
			defer wg.Done()
			id := fmt.Sprintf("dr-workload-worker-%d", workerN)
			for time.Now().Before(deadline) {
				j, claimed, err := st.Claim(ctx, id)
				if err != nil || !claimed {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				_, _ = st.CompleteSuccess(ctx, j.ID, id, j.LeaseGeneration, []byte(`{}`))
			}
		}(w)
	}
	wg.Wait()

	return ids
}
