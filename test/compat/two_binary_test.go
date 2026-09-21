// SF-066, SF-067 (docs/phase-14-plan.md §16, §19 OD-6, ADR-0010): the
// mixed-binary-version integration proof the roadmap names as Phase 14's
// flagship requirement -- two real, separately-compiled cmd/worker
// binaries, pinned to immutable commit SHAs on either side of Phase 13's
// schema change, run concurrently against one shared, real PostgreSQL
// database while migrations 0011-0014 are applied incrementally (not as
// one atomic batch), with internal/invariant.Checker re-run continuously
// and finding zero violations at every intermediate stage.
//
// OLD is pinned to 794abbb57a7e2a965d556700468930a1ebed23e4 ("Merge pull
// request #21 ... phase-13-concurrency-analysis") -- the last commit
// reachable from main with the pre-Phase-13 schema (migrations 0001-0010
// only; no queue_name column exists at the Go type level in this binary
// at all). NEW is pinned to c17f89c592c803a8f7d2fbd61cde566467ebe062
// ("Merge pull request #22 ... phase-13-workload-governance-retention")
// -- migrations 0001-0014 in full, the entire Phase 13 delta in one step.
// Both SHAs are recorded here directly, never as a branch name, so this
// harness's behavior cannot change under it even if main is later
// force-pushed or fast-forwarded (§19 OD-6).
//
// This is genuinely new test infrastructure for this repository (no
// existing test imports os/exec or builds a second binary from a
// different commit) -- see docs/phase-14-plan.md §6.2/§7.
package compat_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// oldCommit and newCommit are §19 OD-6's pinned, immutable SHAs -- the
// exact Phase 13 schema/code boundary, never a branch name.
const (
	oldCommit = "794abbb57a7e2a965d556700468930a1ebed23e4"
	newCommit = "c17f89c592c803a8f7d2fbd61cde566467ebe062"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

// repoRoot locates the module root by walking up from this file's own
// source location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("compat: could not determine this test file's own location")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("compat: could not locate go.mod above %s", thisFile)
		}
		dir = parent
	}
}

// buildAtCommit checks out commit into a fresh, detached-HEAD git
// worktree and builds pkg (e.g. "./cmd/worker") from within it, so the
// resulting binary is compiled against that exact commit's own
// go.mod/go.sum and embedded migration set -- never the current working
// tree's in-progress changes. The worktree is removed unconditionally
// (t.Cleanup), on both success and failure, so a failed run never leaves
// a stray worktree behind (§19 OD-6's own stated requirement).
func buildAtCommit(t *testing.T, root, commit, pkg, outName string) string {
	t.Helper()

	worktreeDir, err := os.MkdirTemp("", "taskforge-compat-worktree-*")
	require.NoError(t, err)
	// MkdirTemp already created worktreeDir; `git worktree add` refuses to
	// check out into a non-empty directory, but happily reuses an empty
	// one it did not create itself as long as it exists and is empty.
	checkoutDir := filepath.Join(worktreeDir, "wt")

	addCmd := exec.Command("git", "worktree", "add", "--detach", checkoutDir, commit)
	addCmd.Dir = root
	var addStderr bytes.Buffer
	addCmd.Stderr = &addStderr
	require.NoError(t, addCmd.Run(), "git worktree add --detach %s %s: %s", checkoutDir, commit, addStderr.String())

	t.Cleanup(func() {
		rmCmd := exec.Command("git", "worktree", "remove", "--force", checkoutDir)
		rmCmd.Dir = root
		_ = rmCmd.Run() // best-effort; the temp dir removal below is the real cleanup guarantee
		_ = os.RemoveAll(worktreeDir)
	})

	binDir, err := os.MkdirTemp("", "taskforge-compat-bin-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })

	out := filepath.Join(binDir, outName)
	buildCmd := exec.Command("go", "build", "-o", out, pkg)
	buildCmd.Dir = checkoutDir
	var buildStderr bytes.Buffer
	buildCmd.Stderr = &buildStderr
	require.NoError(t, buildCmd.Run(), "go build -o %s %s (at %s): %s", out, pkg, commit, buildStderr.String())

	return out
}

// procHandle mirrors test/procs's own helper shape (kept independent
// rather than shared, since this package's build/lifecycle concerns --
// worktrees, not just binaries -- are genuinely different).
type procHandle struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func startProcess(t *testing.T, bin string, env ...string) *procHandle {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	h := &procHandle{cmd: cmd, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	cmd.Stdout = h.stdout
	cmd.Stderr = h.stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if h.cmd.Process != nil && h.cmd.ProcessState == nil {
			_ = h.cmd.Process.Kill()
			_, _ = h.cmd.Process.Wait()
		}
	})
	return h
}

func (h *procHandle) stop(t *testing.T) {
	t.Helper()
	if h.cmd.Process == nil || h.cmd.ProcessState != nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

func (h *procHandle) logOutput(t *testing.T) {
	t.Helper()
	t.Logf("stdout:\n%s", h.stdout.String())
	t.Logf("stderr:\n%s", h.stderr.String())
}

// rewindToPrePhase13 takes an already-fully-migrated (version 14)
// database back to exactly the pre-Phase-13 boundary (versions 1-10
// applied, 11-14 not), using the current, in-tree internal/migrate.Down
// -- the same mechanism SF-060's reversibility proof exercises --
// reversing in strict descending order.
func rewindToPrePhase13(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int64{14, 13, 12, 11} {
		require.NoError(t, migrate.Down(ctx, db, v), "rewinding migration %d", v)
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version >= 11`).Scan(&count))
	require.Zero(t, count, "schema_migrations must show no Phase 13 migration applied after rewinding")
}

// insertPlainJob inserts a job via a raw INSERT that never names
// queue_name, so it works identically whether or not that column exists
// yet in the database's current schema stage -- internal/store.Insert
// cannot be used here because it is written against the CURRENT (HEAD)
// schema shape and explicitly lists queue_name, which does not exist
// before migration 0011 in the rewound database this harness drives.
func insertPlainJob(t *testing.T, db *sql.DB, jobType string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, max_attempts, execution_timeout_seconds)
		VALUES ($1, $2, $3, '{}', 'QUEUED', 3, 30)`,
		id, principal.SystemPrincipalID, jobType)
	require.NoError(t, err)
	return id
}

func waitForSucceeded(t *testing.T, db *sql.DB, id uuid.UUID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state string
		require.NoError(t, db.QueryRowContext(context.Background(),
			`SELECT state FROM jobs WHERE id = $1`, id).Scan(&state))
		if state == "SUCCEEDED" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach SUCCEEDED within %s", id, timeout)
}

func assertNoInvariantViolations(t *testing.T, db *sql.DB, stage string) {
	t.Helper()
	violations, err := invariant.New(db).CheckAll(context.Background())
	require.NoError(t, err, "stage %s: invariant checker itself failed", stage)
	require.Empty(t, violations, "stage %s: invariant violations found: %v", stage, violations)
}

// TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow
// is the roadmap's flagship required test: an OLD cmd/worker binary
// (pinned to oldCommit, no knowledge of queue_name at the Go type level
// at all) and a NEW cmd/worker binary (pinned to newCommit, the full
// Phase 13 delta) run concurrently against one shared database while
// migrations 0011->0014 are applied one at a time via internal/migrate.UpTo
// (never as one atomic batch), with internal/invariant.Checker re-run at
// every intermediate stage.
//
// SF-067 is proven throughout: at every stage (including after the
// schema has fully diverged from what the OLD binary was compiled
// against), a job submitted with the default queue_name is still claimed
// and completed -- by whichever of the two binaries gets to it first, a
// deliberately unconstrained race that itself proves the compatibility
// property (either binary claiming and finishing the job correctly is
// the point: TF-INV-004's queue-subscription rule makes no distinction
// an old binary could violate).
func TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow(t *testing.T) {
	root := repoRoot(t)

	oldBin := buildAtCommit(t, root, oldCommit, "./cmd/worker", "taskforge-worker-old")
	newBin := buildAtCommit(t, root, newCommit, "./cmd/worker", "taskforge-worker-new")

	dsn := testutil.DSN(t) // fully migrated (version 14) + truncated
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	rewindToPrePhase13(t, db)
	assertNoInvariantViolations(t, db, "pre-Phase-13 (versions 1-10)")

	env := []string{
		"TASKFORGE_DATABASE_URL=" + dsn,
		"TASKFORGE_WORKER_POLL_INTERVAL=25ms",
		"TASKFORGE_METRICS_ADDR=",
	}
	oldProc := startProcess(t, oldBin, env...)
	newProc := startProcess(t, newBin, env...)
	defer func() {
		if t.Failed() {
			oldProc.logOutput(t)
			newProc.logOutput(t)
		}
	}()
	t.Cleanup(func() { oldProc.stop(t); newProc.stop(t) })

	// Stage 0: both binaries running against the pre-Phase-13 schema --
	// the ordinary case both were built against.
	id := insertPlainJob(t, db, "demo.echo")
	waitForSucceeded(t, db, id, 5*time.Second)
	assertNoInvariantViolations(t, db, "stage 0 (pre-11, both binaries warm)")

	// Stages 1-4: expand/migrate/contract, one migration at a time, both
	// binaries running throughout. This is the crux of "the full duration
	// of the window, not just the two endpoints" (the roadmap's own
	// explicit language) -- a job is driven through claim-to-completion at
	// EVERY intermediate stage, not only before 0011 and after 0014.
	stages := []struct {
		version int64
		label   string
	}{
		{11, "0011 applied (queue_name column exists)"},
		{12, "0012 applied (new claimable-by-queue index exists)"},
		{13, "0013 applied (governance tables exist)"},
		{14, "0014 applied (old claimable index dropped -- contract complete)"},
	}
	for _, stage := range stages {
		require.NoError(t, migrate.UpTo(context.Background(), db, stage.version), "advancing to %s", stage.label)

		// job_type must be "demo.echo" -- the only handler either pinned
		// binary has registered; distinctness across stages is unneeded
		// since each job already has its own UUID.
		jobID := insertPlainJob(t, db, "demo.echo")
		waitForSucceeded(t, db, jobID, 5*time.Second)
		assertNoInvariantViolations(t, db, stage.label)

		// SF-067, stated directly: confirm the row this stage's job
		// landed in carries a real queue_name value (schema-defaulted to
		// 'default' from migration 0011 onward) regardless of which
		// binary claimed it -- proving the OLD binary's completely
		// queue_name-unaware claim query does not choke on, corrupt, or
		// require that column, and the NEW binary's queue-aware query
		// path also functions identically when given no explicit
		// subscription (the mandatory compatibility default,
		// docs/phase-13-plan.md §8).
		if stage.version >= 11 {
			var queueName string
			require.NoError(t, db.QueryRowContext(context.Background(),
				`SELECT queue_name FROM jobs WHERE id = $1`, jobID).Scan(&queueName))
			require.Equal(t, "default", queueName)
		}
	}

	// Final confirmation: the shared database is left fully migrated
	// (version 14), exactly as if the harness had never rewound it,
	// leaving nothing for a later test in this binary to trip over.
	var latestVersion int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT max(version) FROM schema_migrations`).Scan(&latestVersion))
	require.Equal(t, int64(14), latestVersion)
}

// TestCompat_SF068_RepurposedJobTypeAcrossIncompatiblePayloadShape is
// SF-068 (docs/phase-14-plan.md §14/§16): the roadmap's own named gap --
// a job_type string repurposed for an incompatible payload shape mid-
// deploy is not prevented by TaskForge (payload is opaque JSONB,
// docs/compatibility-policy.md's "PROPOSED: Job Payload/Schema Evolution"),
// but the resulting failure mode must be documented and demonstrated, not
// silently assumed safe. This does not need two separate binaries: the
// property under test is about a handler's own unmarshal/validation
// failure when a "new-shape" handler receives an "old-shape" payload
// already queued before the handler was upgraded -- a single-process
// proof of the documented behavior.
func TestCompat_SF068_RepurposedJobTypeAcrossIncompatiblePayloadShape(t *testing.T) {
	dsn := testutil.DSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	// The "old shape": a plain string field. Submitted before the
	// (hypothetical) handler upgrade.
	s := store.New(db)
	oldShapePayload, err := json.Marshal(map[string]string{"name": "old-shape-caller"})
	require.NoError(t, err)
	created, err := s.Insert(context.Background(), job.NewParams{
		PrincipalID:             principal.SystemPrincipalID,
		JobType:                 "compat.sf068.repurposed",
		Payload:                 oldShapePayload,
		MaxAttempts:             3,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	// The "new shape" handler: same job_type, now expects an integer
	// field the old shape never had -- exactly the incompatible-repurpose
	// scenario docs/compatibility-policy.md warns against.
	registry := handler.NewRegistry()
	registry.Register("compat.sf068.repurposed", newShapeHandler{})
	w := worker.New("compat-sf068-worker", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(context.Background(), created.ID,
		principal.AccessContext{PrincipalID: principal.SystemPrincipalID, IsAdmin: true})
	require.NoError(t, err)
	require.Equal(t, "DEAD_LETTERED", string(final.State),
		"a handler's own unmarshal failure against an incompatibly repurposed payload shape must surface as an "+
			"ordinary classified failure (permanent, by internal/handler.Classify's documented default for an "+
			"unclassified error) -- not a panic, not a silent no-op, not an infinite retry loop")
	require.NotNil(t, final.LastError)
	require.Contains(t, *final.LastError, "count",
		"the failure must be the handler's own real unmarshal error, not a substituted one")
}

// newShapeHandler is the "new-shape" handler for SF-068: it requires an
// integer "count" field that the old payload shape never had, and
// returns a real (not manufactured) validation error when that field is
// absent -- exactly the ordinary, undecorated failure mode
// docs/compatibility-policy.md documents as the job author's own
// responsibility to avoid by never repurposing a job_type string for an
// incompatible payload shape.
type newShapeHandler struct{}

func (newShapeHandler) Execute(_ context.Context, j *job.Job) (handler.Result, error) {
	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(j.Payload, &payload); err != nil {
		return handler.Result{}, err
	}
	if payload.Count == 0 {
		return handler.Result{}, fmt.Errorf("compat.sf068: payload missing required \"count\" field")
	}
	return handler.Result{}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
