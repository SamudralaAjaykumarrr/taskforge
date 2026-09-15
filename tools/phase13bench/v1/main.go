// Phase 13 OD-1 concurrency/fairness evidence harness.
//
// ANALYSIS-ONLY. This program is not part of TaskForge's production build,
// is not imported by any internal/ or cmd/ package, and does not touch the
// real `jobs` table or production schema. It stands up its own throwaway
// tables (bench_jobs / bench_queue_slots / bench_queue_state) in a
// dedicated, disposable database and exercises the three concurrency-limit
// candidates and two fairness candidates named in docs/phase-13-plan.md
// section 6a, so that OD-1's mandatory ADR has measured (not assumed)
// evidence. It is deliberately excluded from the module's normal `go
// build ./...`/`go test ./...` surface (no other package imports it) and
// is not staged/committed as part of this evidence pass.
//
// Usage: go run ./tools/phase13bench [-dsn postgres://...]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const benchDBName = "taskforge_phase13_bench"

func main() {
	adminDSN := flag.String("dsn", "postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable", "admin DSN (connects to 'postgres' db to (re)create the bench database)")
	outPath := flag.String("out", "phase13bench-results.json", "output JSON results path")
	flag.Parse()

	ctx := context.Background()

	if err := recreateBenchDB(ctx, *adminDSN); err != nil {
		log.Fatalf("recreate bench db: %v", err)
	}

	benchDSN := dsnForDB(*adminDSN, benchDBName)
	db, err := sql.Open("pgx", benchDSN)
	if err != nil {
		log.Fatalf("open bench db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(120)
	db.SetMaxIdleConns(120)

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping bench db: %v", err)
	}

	results := &Results{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	results.Environment = collectEnvironment(ctx, db)

	log.Println("=== E1: concurrency-limit exactness ===")
	for _, cand := range []CandidateID{CandAdvisory, CandSlot, CandSerializable} {
		r := runExactness(ctx, db, cand)
		results.Exactness = append(results.Exactness, r)
		log.Printf("  %-12s maxObservedRunning=%d limit=%d claims=%d violations=%d serFailures=%d",
			cand, r.MaxObservedRunning, r.Limit, r.TotalClaims, r.LimitViolations, r.SerializationFailures)
	}

	log.Println("=== E2: throughput & latency vs worker count ===")
	for _, cand := range []CandidateID{CandAdvisory, CandSlot, CandSerializable} {
		for _, n := range []int{1, 5, 10, 25, 50, 80} {
			r := runThroughput(ctx, db, cand, n)
			results.Throughput = append(results.Throughput, r)
			log.Printf("  %-12s workers=%3d throughput=%8.1f/s p50=%6.2fms p95=%6.2fms p99=%6.2fms serFailRate=%.3f%%",
				cand, n, r.ClaimsPerSec, r.P50Ms, r.P95Ms, r.P99Ms, r.SerFailRatePct)
		}
	}

	log.Println("=== E3: lock/contention characterization (sampled during E2 hot runs) ===")
	for _, cand := range []CandidateID{CandAdvisory, CandSlot, CandSerializable} {
		r := runLockContention(ctx, db, cand, 40)
		results.LockContention = append(results.LockContention, r)
		log.Printf("  %-12s avgWaitingLocks=%.2f maxWaitingLocks=%d avgActiveConns=%.1f",
			cand, r.AvgWaitingLocks, r.MaxWaitingLocks, r.AvgActiveConns)
	}

	log.Println("=== E4: fairness / starvation (flooded queue vs trickle queue, no concurrency cap) ===")
	for _, fair := range []FairnessID{FairBaseline, FairRoundRobin, FairLastClaimed} {
		r := runFairness(ctx, db, fair)
		results.Fairness = append(results.Fairness, r)
		log.Printf("  %-12s trickleClaims=%d/%d p50WaitMs=%8.1f p95WaitMs=%8.1f maxWaitMs=%10.1f floodShare=%.1f%%",
			fair, r.TrickleClaimed, r.TrickleTotal, r.P50WaitMs, r.P95WaitMs, r.MaxWaitMs, r.FloodSharePct)
	}

	log.Println("=== E5: queue/tenant isolation under load (capped hot queue vs independent low-volume queue) ===")
	for _, cand := range []CandidateID{CandAdvisory, CandSlot} {
		r := runIsolation(ctx, db, cand)
		results.Isolation = append(results.Isolation, r)
		log.Printf("  %-12s lowVolumeThroughputAlone=%.1f/s lowVolumeThroughputUnderFlood=%.1f/s degradationPct=%.1f%%",
			cand, r.LowVolumeAloneThroughput, r.LowVolumeUnderFloodThroughput, r.DegradationPct)
	}

	log.Println("=== E6: expired-lease reclaim + queue-subscription compatibility (correctness checks) ===")
	results.Correctness = runCorrectnessChecks(ctx, db)
	for _, c := range results.Correctness {
		status := "PASS"
		if !c.Pass {
			status = "FAIL"
		}
		log.Printf("  [%s] %s: %s", status, c.Name, c.Detail)
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("marshal results: %v", err)
	}
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		log.Fatalf("write results: %v", err)
	}
	log.Printf("results written to %s", *outPath)
}

// ---------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------

func dsnForDB(adminDSN, dbName string) string {
	// adminDSN ends in /postgres?sslmode=disable; swap the db name only.
	// Simple, deliberate string surgery -- this tool's DSNs are fixed-shape
	// and never user-supplied beyond -dsn itself.
	idx := len(adminDSN)
	for i := len(adminDSN) - 1; i >= 0; i-- {
		if adminDSN[i] == '/' {
			idx = i
			break
		}
	}
	return adminDSN[:idx+1] + dbName + "?sslmode=disable"
}

func recreateBenchDB(ctx context.Context, adminDSN string) error {
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// Terminate any lingering connections from a previous aborted run.
	_, _ = db.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, benchDBName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, benchDBName)); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, benchDBName)); err != nil {
		return err
	}
	return nil
}

const schemaSQL = `
CREATE TABLE bench_jobs (
    id               BIGSERIAL PRIMARY KEY,
    queue_name       TEXT NOT NULL,
    priority         SMALLINT NOT NULL DEFAULT 0,
    eligible_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    state            TEXT NOT NULL DEFAULT 'QUEUED',
    claimed_by       TEXT,
    claimed_at       TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ
);
CREATE INDEX idx_bench_claimable ON bench_jobs (queue_name, priority DESC, eligible_at ASC) WHERE state = 'QUEUED';
CREATE INDEX idx_bench_running_by_queue ON bench_jobs (queue_name) WHERE state = 'RUNNING';
CREATE INDEX idx_bench_reclaimable ON bench_jobs (lease_expires_at) WHERE state = 'RUNNING';

CREATE TABLE bench_queue_state (
    queue_name        TEXT PRIMARY KEY,
    concurrency_limit INT NOT NULL,
    last_claimed_at   TIMESTAMPTZ NOT NULL DEFAULT '-infinity'
);

CREATE TABLE bench_queue_slots (
    queue_name    TEXT NOT NULL,
    slot_index    INT NOT NULL,
    held_by_job_id BIGINT,
    PRIMARY KEY (queue_name, slot_index)
);
CREATE INDEX idx_bench_slots_free ON bench_queue_slots (queue_name) WHERE held_by_job_id IS NULL;
`

func resetSchema(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS bench_queue_slots, bench_queue_state, bench_jobs`); err != nil {
		log.Fatalf("drop schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		log.Fatalf("create schema: %v", err)
	}
}

func seedQueue(ctx context.Context, db *sql.DB, queue string, limit, slots, jobs int) {
	if _, err := db.ExecContext(ctx, `INSERT INTO bench_queue_state (queue_name, concurrency_limit) VALUES ($1, $2) ON CONFLICT (queue_name) DO UPDATE SET concurrency_limit = $2`, queue, limit); err != nil {
		log.Fatalf("seed queue_state: %v", err)
	}
	for i := 0; i < slots; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO bench_queue_slots (queue_name, slot_index, held_by_job_id) VALUES ($1, $2, NULL) ON CONFLICT (queue_name, slot_index) DO UPDATE SET held_by_job_id = NULL`, queue, i); err != nil {
			log.Fatalf("seed queue_slots: %v", err)
		}
	}
	if jobs > 0 {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO bench_jobs (queue_name, priority, eligible_at, state)
			SELECT $1, 0, now(), 'QUEUED' FROM generate_series(1, $2)`, queue, jobs); err != nil {
			log.Fatalf("seed jobs: %v", err)
		}
	}
}

func collectEnvironment(ctx context.Context, db *sql.DB) Environment {
	var version, maxConn, sharedBuf, fsync, syncCommit string
	_ = db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version)
	_ = db.QueryRowContext(ctx, `SHOW max_connections`).Scan(&maxConn)
	_ = db.QueryRowContext(ctx, `SHOW shared_buffers`).Scan(&sharedBuf)
	_ = db.QueryRowContext(ctx, `SHOW fsync`).Scan(&fsync)
	_ = db.QueryRowContext(ctx, `SHOW synchronous_commit`).Scan(&syncCommit)
	return Environment{
		PostgresVersion:   version,
		MaxConnections:    maxConn,
		SharedBuffers:     sharedBuf,
		Fsync:             fsync,
		SynchronousCommit: syncCommit,
		Note:              "single-node WSL2 VM on consumer laptop hardware (Intel Core Ultra 7 258V, 8 vCPU, 15GiB RAM), ext4 on virtualized disk; see report Section C for full caveats",
	}
}

// ---------------------------------------------------------------------
// types
// ---------------------------------------------------------------------

type CandidateID string

const (
	CandAdvisory     CandidateID = "advisory_lock"
	CandSlot         CandidateID = "slot_table"
	CandSerializable CandidateID = "serializable"
)

type FairnessID string

const (
	FairBaseline    FairnessID = "baseline_global_order"
	FairRoundRobin  FairnessID = "round_robin_group_key"
	FairLastClaimed FairnessID = "last_claimed_at_ascending"
)

type Environment struct {
	PostgresVersion   string `json:"postgres_version"`
	MaxConnections    string `json:"max_connections"`
	SharedBuffers     string `json:"shared_buffers"`
	Fsync             string `json:"fsync"`
	SynchronousCommit string `json:"synchronous_commit"`
	Note              string `json:"note"`
}

type ExactnessResult struct {
	Candidate             CandidateID `json:"candidate"`
	Limit                 int         `json:"limit"`
	Workers               int         `json:"workers"`
	DurationSec           float64     `json:"duration_sec"`
	TotalClaims           int64       `json:"total_claims"`
	MaxObservedRunning    int         `json:"max_observed_running"`
	LimitViolations       int         `json:"limit_violations"`
	SerializationFailures int64       `json:"serialization_failures"`
	SamplesTaken          int64       `json:"samples_taken"`
}

type ThroughputResult struct {
	Candidate      CandidateID `json:"candidate"`
	Workers        int         `json:"workers"`
	DurationSec    float64     `json:"duration_sec"`
	TotalClaims    int64       `json:"total_claims"`
	ClaimsPerSec   float64     `json:"claims_per_sec"`
	P50Ms          float64     `json:"p50_ms"`
	P95Ms          float64     `json:"p95_ms"`
	P99Ms          float64     `json:"p99_ms"`
	MaxMs          float64     `json:"max_ms"`
	SerFailures    int64       `json:"serialization_failures"`
	SerFailRatePct float64     `json:"ser_fail_rate_pct"`
}

type LockContentionResult struct {
	Candidate       CandidateID    `json:"candidate"`
	Workers         int            `json:"workers"`
	Samples         int            `json:"samples"`
	AvgWaitingLocks float64        `json:"avg_waiting_locks"`
	MaxWaitingLocks int            `json:"max_waiting_locks"`
	AvgActiveConns  float64        `json:"avg_active_conns"`
	LockTypeCounts  map[string]int `json:"lock_type_wait_counts"`
}

type FairnessResult struct {
	Fairness       FairnessID `json:"fairness"`
	DurationSec    float64    `json:"duration_sec"`
	FloodClaimed   int64      `json:"flood_claimed"`
	TrickleClaimed int64      `json:"trickle_claimed"`
	TrickleTotal   int64      `json:"trickle_total"`
	FloodSharePct  float64    `json:"flood_share_pct"`
	P50WaitMs      float64    `json:"p50_wait_ms"`
	P95WaitMs      float64    `json:"p95_wait_ms"`
	MaxWaitMs      float64    `json:"max_wait_ms"`
}

type IsolationResult struct {
	Candidate                     CandidateID `json:"candidate"`
	LowVolumeAloneThroughput      float64     `json:"low_volume_alone_throughput"`
	LowVolumeUnderFloodThroughput float64     `json:"low_volume_under_flood_throughput"`
	DegradationPct                float64     `json:"degradation_pct"`
}

type CorrectnessCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type Results struct {
	Timestamp      string                 `json:"timestamp"`
	Environment    Environment            `json:"environment"`
	Exactness      []ExactnessResult      `json:"exactness"`
	Throughput     []ThroughputResult     `json:"throughput"`
	LockContention []LockContentionResult `json:"lock_contention"`
	Fairness       []FairnessResult       `json:"fairness"`
	Isolation      []IsolationResult      `json:"isolation"`
	Correctness    []CorrectnessCheck     `json:"correctness"`
}

// ---------------------------------------------------------------------
// claim implementations -- the three §6a concurrency-limit candidates
// ---------------------------------------------------------------------

// claimResult is nil,nil,nil when nothing was claimed (no error, no id).
type claimFunc func(ctx context.Context, db *sql.DB, queue, owner string) (id int64, ok bool, serFail bool, err error)

func claimAdvisory(ctx context.Context, db *sql.DB, queue, owner string) (int64, bool, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, queue); err != nil {
		return 0, false, false, err
	}

	var limit, running int
	if err := tx.QueryRowContext(ctx, `SELECT concurrency_limit FROM bench_queue_state WHERE queue_name = $1`, queue).Scan(&limit); err != nil {
		return 0, false, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bench_jobs WHERE queue_name = $1 AND state = 'RUNNING'`, queue).Scan(&running); err != nil {
		return 0, false, false, err
	}
	if running >= limit {
		return 0, false, false, tx.Commit()
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM bench_jobs
		WHERE queue_name = $1 AND state = 'QUEUED' AND eligible_at <= now()
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED LIMIT 1`, queue).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, false, tx.Commit()
	}
	if err != nil {
		return 0, false, false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE bench_jobs SET state = 'RUNNING', claimed_by = $2, claimed_at = now(), lease_expires_at = now() + interval '30 seconds' WHERE id = $1`, id, owner); err != nil {
		return 0, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, false, err
	}
	return id, true, false, nil
}

func claimSlot(ctx context.Context, db *sql.DB, queue, owner string) (int64, bool, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var id int64
	var slotIndex int
	err = tx.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT id FROM bench_jobs
			WHERE queue_name = $1 AND state = 'QUEUED' AND eligible_at <= now()
			ORDER BY priority DESC, eligible_at ASC
			FOR UPDATE SKIP LOCKED LIMIT 1
		), slot AS (
			SELECT slot_index FROM bench_queue_slots
			WHERE queue_name = $1 AND held_by_job_id IS NULL
			FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE bench_jobs
		SET state = 'RUNNING', claimed_by = $2, claimed_at = now(), lease_expires_at = now() + interval '30 seconds'
		FROM candidate, slot
		WHERE bench_jobs.id = candidate.id
		RETURNING bench_jobs.id, slot.slot_index`, queue, owner).Scan(&id, &slotIndex)
	if err == sql.ErrNoRows {
		return 0, false, false, tx.Commit()
	}
	if err != nil {
		return 0, false, false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id = $1 WHERE queue_name = $2 AND slot_index = $3`, id, queue, slotIndex); err != nil {
		return 0, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, false, err
	}
	return id, true, false, nil
}

func claimSerializable(ctx context.Context, db *sql.DB, queue, owner string) (int64, bool, bool, error) {
	const maxRetries = 8
	for attempt := 0; attempt < maxRetries; attempt++ {
		id, ok, err := claimSerializableOnce(ctx, db, queue, owner)
		if err == nil {
			return id, ok, attempt > 0, nil
		}
		if isSerializationFailure(err) {
			continue // retry, per §6a candidate 3's own described mechanism
		}
		return 0, false, false, err
	}
	return 0, false, true, fmt.Errorf("serializable claim: exhausted %d retries", maxRetries)
}

func claimSerializableOnce(ctx context.Context, db *sql.DB, queue, owner string) (int64, bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: 4}) // sql.LevelSerializable
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var limit, running int
	if err := tx.QueryRowContext(ctx, `SELECT concurrency_limit FROM bench_queue_state WHERE queue_name = $1`, queue).Scan(&limit); err != nil {
		return 0, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bench_jobs WHERE queue_name = $1 AND state = 'RUNNING'`, queue).Scan(&running); err != nil {
		return 0, false, err
	}
	if running >= limit {
		return 0, false, tx.Commit()
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM bench_jobs
		WHERE queue_name = $1 AND state = 'QUEUED' AND eligible_at <= now()
		ORDER BY priority DESC, eligible_at ASC
		LIMIT 1`, queue).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, tx.Commit()
	}
	if err != nil {
		return 0, false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE bench_jobs SET state = 'RUNNING', claimed_by = $2, claimed_at = now(), lease_expires_at = now() + interval '30 seconds' WHERE id = $1`, id, owner); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "40001") || contains(s, "could not serialize") || contains(s, "40P01") || contains(s, "deadlock detected")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func completeJob(ctx context.Context, db *sql.DB, cand CandidateID, queue string, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE bench_jobs SET state = 'DONE' WHERE id = $1`, id); err != nil {
		return err
	}
	if cand == CandSlot {
		if _, err := tx.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id = NULL WHERE queue_name = $1 AND held_by_job_id = $2`, queue, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func claimFor(cand CandidateID) claimFunc {
	switch cand {
	case CandAdvisory:
		return claimAdvisory
	case CandSlot:
		return claimSlot
	case CandSerializable:
		return claimSerializable
	default:
		panic("unknown candidate " + cand)
	}
}

// ---------------------------------------------------------------------
// E1: concurrency-limit exactness
// ---------------------------------------------------------------------

func runExactness(ctx context.Context, db *sql.DB, cand CandidateID) ExactnessResult {
	resetSchema(ctx, db)
	const queue = "hot"
	const limit = 5
	const workers = 30
	const runFor = 5 * time.Second

	seedQueue(ctx, db, queue, limit, limit, 4000)

	claim := claimFor(cand)
	var totalClaims int64
	var serFail int64
	var maxRunning int32
	var violations int32
	var samples int64

	runCtx, cancel := context.WithTimeout(ctx, runFor)
	defer cancel()

	// Monitor goroutine: high-frequency polling of the RUNNING count for
	// this queue, using its own dedicated connection so it is never
	// blocked behind worker claim/complete transactions.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var running int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM bench_jobs WHERE queue_name = $1 AND state = 'RUNNING'`, queue).Scan(&running); err != nil {
					continue
				}
				atomic.AddInt64(&samples, 1)
				for {
					cur := atomic.LoadInt32(&maxRunning)
					if int32(running) <= cur || atomic.CompareAndSwapInt32(&maxRunning, cur, int32(running)) {
						break
					}
				}
				if running > limit {
					atomic.AddInt32(&violations, 1)
				}
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			owner := fmt.Sprintf("exactness-%s-%d", cand, workerID)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				id, ok, retried, err := claim(runCtx, db, queue, owner)
				if retried {
					atomic.AddInt64(&serFail, 1)
				}
				if err != nil {
					if runCtx.Err() != nil {
						return
					}
					continue
				}
				if !ok {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				atomic.AddInt64(&totalClaims, 1)
				time.Sleep(20 * time.Millisecond) // simulated execution hold
				_ = completeJob(ctx, db, cand, queue, id)
			}
		}(w)
	}

	wg.Wait()

	return ExactnessResult{
		Candidate:             cand,
		Limit:                 limit,
		Workers:               workers,
		DurationSec:           runFor.Seconds(),
		TotalClaims:           atomic.LoadInt64(&totalClaims),
		MaxObservedRunning:    int(atomic.LoadInt32(&maxRunning)),
		LimitViolations:       int(atomic.LoadInt32(&violations)),
		SerializationFailures: atomic.LoadInt64(&serFail),
		SamplesTaken:          atomic.LoadInt64(&samples),
	}
}

// ---------------------------------------------------------------------
// E2: throughput & latency vs worker count
// ---------------------------------------------------------------------

func runThroughput(ctx context.Context, db *sql.DB, cand CandidateID, workers int) ThroughputResult {
	resetSchema(ctx, db)
	const queue = "hot"
	const limit = 20
	const runFor = 4 * time.Second

	seedQueue(ctx, db, queue, limit, limit, 200000)

	claim := claimFor(cand)
	var totalClaims int64
	var serFail int64
	var latMu sync.Mutex
	var latencies []float64

	runCtx, cancel := context.WithTimeout(ctx, runFor)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			owner := fmt.Sprintf("thr-%s-%d", cand, workerID)
			var local []float64
			for {
				select {
				case <-runCtx.Done():
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
					return
				default:
				}
				start := time.Now()
				id, ok, retried, err := claim(runCtx, db, queue, owner)
				elapsed := time.Since(start)
				if retried {
					atomic.AddInt64(&serFail, 1)
				}
				if err != nil {
					if runCtx.Err() != nil {
						continue
					}
					continue
				}
				if !ok {
					continue
				}
				local = append(local, float64(elapsed.Microseconds())/1000.0)
				atomic.AddInt64(&totalClaims, 1)
				time.Sleep(5 * time.Millisecond) // simulated short handler execution
				_ = completeJob(ctx, db, cand, queue, id)
			}
		}(w)
	}
	wg.Wait()

	sort.Float64s(latencies)
	p := func(pct float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(math.Ceil(pct/100*float64(len(latencies)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}

	tc := atomic.LoadInt64(&totalClaims)
	sf := atomic.LoadInt64(&serFail)
	rate := 0.0
	if tc+sf > 0 {
		rate = float64(sf) / float64(tc+sf) * 100
	}

	return ThroughputResult{
		Candidate:      cand,
		Workers:        workers,
		DurationSec:    runFor.Seconds(),
		TotalClaims:    tc,
		ClaimsPerSec:   float64(tc) / runFor.Seconds(),
		P50Ms:          p(50),
		P95Ms:          p(95),
		P99Ms:          p(99),
		MaxMs:          p(100),
		SerFailures:    sf,
		SerFailRatePct: rate,
	}
}

// ---------------------------------------------------------------------
// E3: lock/contention characterization
// ---------------------------------------------------------------------

func runLockContention(ctx context.Context, db *sql.DB, cand CandidateID, workers int) LockContentionResult {
	resetSchema(ctx, db)
	const queue = "hot"
	const limit = 10
	const runFor = 4 * time.Second

	seedQueue(ctx, db, queue, limit, limit, 200000)

	claim := claimFor(cand)
	runCtx, cancel := context.WithTimeout(ctx, runFor)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			owner := fmt.Sprintf("lock-%s-%d", cand, workerID)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				id, ok, _, err := claim(runCtx, db, queue, owner)
				if err != nil || !ok {
					continue
				}
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, queue, id)
			}
		}(w)
	}

	var samples int
	var sumWaiting float64
	var maxWaiting int
	var sumActive float64
	lockTypeCounts := map[string]int{}
	var sampleMu sync.Mutex

	monCtx, monCancel := context.WithTimeout(ctx, runFor)
	defer monCancel()
	sampleTicker := time.NewTicker(50 * time.Millisecond)
	defer sampleTicker.Stop()
loop:
	for {
		select {
		case <-monCtx.Done():
			break loop
		case <-sampleTicker.C:
			rows, err := db.QueryContext(ctx, `
				SELECT locktype, count(*) FROM pg_locks
				WHERE NOT granted GROUP BY locktype`)
			if err != nil {
				continue
			}
			waiting := 0
			func() {
				defer rows.Close()
				for rows.Next() {
					var lt string
					var cnt int
					if rows.Scan(&lt, &cnt) == nil {
						sampleMu.Lock()
						lockTypeCounts[lt] += cnt
						sampleMu.Unlock()
						waiting += cnt
					}
				}
			}()

			var active int
			_ = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state = 'active'`).Scan(&active)

			samples++
			sumWaiting += float64(waiting)
			sumActive += float64(active)
			if waiting > maxWaiting {
				maxWaiting = waiting
			}
		}
	}

	wg.Wait()

	avgWaiting := 0.0
	avgActive := 0.0
	if samples > 0 {
		avgWaiting = sumWaiting / float64(samples)
		avgActive = sumActive / float64(samples)
	}

	return LockContentionResult{
		Candidate:       cand,
		Workers:         workers,
		Samples:         samples,
		AvgWaitingLocks: avgWaiting,
		MaxWaitingLocks: maxWaiting,
		AvgActiveConns:  avgActive,
		LockTypeCounts:  lockTypeCounts,
	}
}

// ---------------------------------------------------------------------
// E4: fairness / starvation
// ---------------------------------------------------------------------

func claimFairness(ctx context.Context, db *sql.DB, fair FairnessID, subscribed []string, owner string) (id int64, queue string, ok bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", false, err
	}
	defer tx.Rollback() //nolint:errcheck

	switch fair {
	case FairBaseline:
		// Today's actual queue-blind behavior: a single global pool ordered
		// by priority/eligible_at only, queue_name never enters ORDER BY.
		err = tx.QueryRowContext(ctx, `
			SELECT id, queue_name FROM bench_jobs
			WHERE queue_name = ANY($1) AND state = 'QUEUED' AND eligible_at <= now()
			ORDER BY priority DESC, eligible_at ASC
			FOR UPDATE SKIP LOCKED LIMIT 1`, pqStringArray(subscribed)).Scan(&id, &queue)

	case FairRoundRobin:
		// Hatchet-style group-key round robin: rank each queue's own
		// backlog independently, then interleave by rank so claims
		// alternate across queues with eligible work rather than draining
		// one queue first. PostgreSQL rejects "FOR UPDATE ... window
		// function" in one query (SQLSTATE: "FOR UPDATE is not allowed
		// with window functions" -- confirmed against this project's own
		// PostgreSQL 16 instance), so this candidate genuinely requires
		// two statements per claim attempt, not one: a non-locking read
		// to compute the interleaved rank order, then a locking
		// FOR UPDATE SKIP LOCKED restricted to that ranked candidate set,
		// preserving the same order via array_position. This is itself
		// part of this candidate's measured cost (§6a's "one query" framing
		// for this idiom does not hold once ranking requires a window
		// function) -- see the evidence report's fairness findings.
		var ids []int64
		rows, rerr := tx.QueryContext(ctx, `
			SELECT id FROM (
				SELECT id, priority, eligible_at,
				       row_number() OVER (PARTITION BY queue_name ORDER BY priority DESC, eligible_at ASC) AS rn
				FROM bench_jobs
				WHERE queue_name = ANY($1) AND state = 'QUEUED' AND eligible_at <= now()
			) ranked
			ORDER BY rn ASC, priority DESC, eligible_at ASC
			LIMIT 50`, pqStringArray(subscribed))
		if rerr != nil {
			return 0, "", false, rerr
		}
		for rows.Next() {
			var rid int64
			if serr := rows.Scan(&rid); serr != nil {
				rows.Close()
				return 0, "", false, serr
			}
			ids = append(ids, rid)
		}
		rows.Close()
		if len(ids) == 0 {
			return 0, "", false, tx.Commit()
		}
		err = tx.QueryRowContext(ctx, `
			SELECT id, queue_name FROM bench_jobs
			WHERE id = ANY($1) AND state = 'QUEUED' AND eligible_at <= now()
			ORDER BY array_position($1::bigint[], id)
			FOR UPDATE SKIP LOCKED LIMIT 1`, ids).Scan(&id, &queue)

	case FairLastClaimed:
		err = tx.QueryRowContext(ctx, `
			SELECT j.id, j.queue_name FROM bench_jobs j
			JOIN bench_queue_state q ON q.queue_name = j.queue_name
			WHERE j.queue_name = ANY($1) AND j.state = 'QUEUED' AND j.eligible_at <= now()
			ORDER BY q.last_claimed_at ASC, j.priority DESC, j.eligible_at ASC
			FOR UPDATE SKIP LOCKED LIMIT 1`, pqStringArray(subscribed)).Scan(&id, &queue)
	}

	if err == sql.ErrNoRows {
		return 0, "", false, tx.Commit()
	}
	if err != nil {
		return 0, "", false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE bench_jobs SET state = 'RUNNING', claimed_by = $2, claimed_at = now() WHERE id = $1`, id, owner); err != nil {
		return 0, "", false, err
	}
	if fair == FairLastClaimed {
		if _, err := tx.ExecContext(ctx, `UPDATE bench_queue_state SET last_claimed_at = now() WHERE queue_name = $1`, queue); err != nil {
			return 0, "", false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, err
	}
	return id, queue, true, nil
}

// pqStringArray renders a Go []string as a Postgres text[] literal
// acceptable to database/sql's pgx driver via the ARRAY[...] cast route
// is unnecessary here; pgx/v5's stdlib driver accepts []string directly
// for a text[] parameter, so this is just a passthrough kept as a named
// helper for readability at call sites.
func pqStringArray(ss []string) []string { return ss }

func runFairness(ctx context.Context, db *sql.DB, fair FairnessID) FairnessResult {
	resetSchema(ctx, db)
	const floodQueue = "flood"
	const trickleQueue = "trickle"
	const runFor = 6 * time.Second
	const workers = 10

	// No concurrency cap in this experiment: bench_queue_state.concurrency_limit
	// is unused by claimFairness (fairness ordering is tested independent of
	// the concurrency-limit mechanism, per phase-13-plan.md §6a's own framing
	// of the two as separately layered decisions).
	seedQueue(ctx, db, floodQueue, 1_000_000, 0, 0)
	seedQueue(ctx, db, trickleQueue, 1_000_000, 0, 0)

	runCtx, cancel := context.WithTimeout(ctx, runFor)
	defer cancel()

	// Flood producer: keeps floodQueue permanently non-empty.
	var floodProducerWG sync.WaitGroup
	floodProducerWG.Add(1)
	go func() {
		defer floodProducerWG.Done()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_, _ = db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) SELECT $1, 'QUEUED' FROM generate_series(1,5)`, floodQueue)
			}
		}
	}()

	// Trickle producer: one job every 300ms, with the insertion time
	// recorded so wait-until-claimed can be measured precisely.
	type trickleJob struct {
		id         int64
		insertedAt time.Time
	}
	trickleCh := make(chan trickleJob, 1000)
	var trickleProducerWG sync.WaitGroup
	trickleProducerWG.Add(1)
	go func() {
		defer trickleProducerWG.Done()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var id int64
				t0 := time.Now()
				if err := db.QueryRowContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ($1, 'QUEUED') RETURNING id`, trickleQueue).Scan(&id); err == nil {
					trickleCh <- trickleJob{id: id, insertedAt: t0}
				}
			}
		}
	}()

	var floodClaimed, trickleClaimed int64
	claimTimes := struct {
		sync.Mutex
		m map[int64]time.Time
	}{m: map[int64]time.Time{}}

	var errOnce sync.Once
	var workerWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		workerWG.Add(1)
		go func(workerID int) {
			defer workerWG.Done()
			owner := fmt.Sprintf("fair-%s-%d", fair, workerID)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				id, queue, ok, err := claimFairness(runCtx, db, fair, []string{floodQueue, trickleQueue}, owner)
				if err != nil {
					if runCtx.Err() == nil {
						// A real, unexpected SQL error (not context
						// cancellation at run end) must never be silently
						// swallowed -- a prior version of this harness did
						// exactly that for the round-robin candidate,
						// producing a fabricated "0 claims" result that
						// was actually a query error, not a fairness
						// finding. Surface it loudly instead.
						errOnce.Do(func() { log.Printf("claimFairness(%s) error: %v", fair, err) })
					}
					continue
				}
				if !ok {
					time.Sleep(300 * time.Microsecond)
					continue
				}
				now := time.Now()
				if queue == floodQueue {
					atomic.AddInt64(&floodClaimed, 1)
				} else {
					atomic.AddInt64(&trickleClaimed, 1)
					claimTimes.Lock()
					claimTimes.m[id] = now
					claimTimes.Unlock()
				}
				_ = completeJob(ctx, db, CandAdvisory /* no slot table involved */, queue, id)
			}
		}(w)
	}

	workerWG.Wait()
	floodProducerWG.Wait()
	trickleProducerWG.Wait()
	close(trickleCh)

	var waits []float64
	var trickleTotal int64
	claimTimes.Lock()
	for tj := range trickleCh {
		trickleTotal++
		if claimedAt, ok := claimTimes.m[tj.id]; ok {
			waits = append(waits, claimedAt.Sub(tj.insertedAt).Seconds()*1000)
		}
	}
	claimTimes.Unlock()

	sort.Float64s(waits)
	p := func(pct float64) float64 {
		if len(waits) == 0 {
			return 0
		}
		idx := int(math.Ceil(pct/100*float64(len(waits)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(waits) {
			idx = len(waits) - 1
		}
		return waits[idx]
	}

	fc := atomic.LoadInt64(&floodClaimed)
	tc := atomic.LoadInt64(&trickleClaimed)
	floodShare := 0.0
	if fc+tc > 0 {
		floodShare = float64(fc) / float64(fc+tc) * 100
	}

	return FairnessResult{
		Fairness:       fair,
		DurationSec:    runFor.Seconds(),
		FloodClaimed:   fc,
		TrickleClaimed: tc,
		TrickleTotal:   trickleTotal,
		FloodSharePct:  floodShare,
		P50WaitMs:      p(50),
		P95WaitMs:      p(95),
		MaxWaitMs:      p(100),
	}
}

// ---------------------------------------------------------------------
// E5: queue/tenant isolation under load
// ---------------------------------------------------------------------

func runIsolation(ctx context.Context, db *sql.DB, cand CandidateID) IsolationResult {
	const lowQueue = "tenant-low"
	const hotQueue = "tenant-hot"
	const lowLimit = 5
	const hotLimit = 20
	const workers = 15
	const runFor = 3 * time.Second

	measureAlone := func() float64 {
		resetSchema(ctx, db)
		seedQueue(ctx, db, lowQueue, lowLimit, lowLimit, 200000)
		return measureQueueThroughput(ctx, db, cand, lowQueue, workers, runFor)
	}

	measureUnderFlood := func() float64 {
		resetSchema(ctx, db)
		seedQueue(ctx, db, lowQueue, lowLimit, lowLimit, 200000)
		seedQueue(ctx, db, hotQueue, hotLimit, hotLimit, 200000)

		runCtx, cancel := context.WithTimeout(ctx, runFor)
		defer cancel()

		// Flood the hot queue concurrently with heavy worker pressure while
		// measuring the low-volume queue's own throughput in isolation.
		var hotWG sync.WaitGroup
		claim := claimFor(cand)
		for w := 0; w < workers; w++ {
			hotWG.Add(1)
			go func(id int) {
				defer hotWG.Done()
				owner := fmt.Sprintf("iso-hot-%s-%d", cand, id)
				for {
					select {
					case <-runCtx.Done():
						return
					default:
					}
					jid, ok, _, err := claim(runCtx, db, hotQueue, owner)
					if err != nil || !ok {
						continue
					}
					time.Sleep(5 * time.Millisecond)
					_ = completeJob(ctx, db, cand, hotQueue, jid)
				}
			}(w)
		}

		var lowClaims int64
		var lowWG sync.WaitGroup
		for w := 0; w < workers; w++ {
			lowWG.Add(1)
			go func(id int) {
				defer lowWG.Done()
				owner := fmt.Sprintf("iso-low-%s-%d", cand, id)
				for {
					select {
					case <-runCtx.Done():
						return
					default:
					}
					jid, ok, _, err := claim(runCtx, db, lowQueue, owner)
					if err != nil || !ok {
						continue
					}
					atomic.AddInt64(&lowClaims, 1)
					time.Sleep(5 * time.Millisecond)
					_ = completeJob(ctx, db, cand, lowQueue, jid)
				}
			}(w)
		}

		lowWG.Wait()
		hotWG.Wait()
		return float64(atomic.LoadInt64(&lowClaims)) / runFor.Seconds()
	}

	alone := measureAlone()
	underFlood := measureUnderFlood()
	degradation := 0.0
	if alone > 0 {
		degradation = (alone - underFlood) / alone * 100
	}

	return IsolationResult{
		Candidate:                     cand,
		LowVolumeAloneThroughput:      alone,
		LowVolumeUnderFloodThroughput: underFlood,
		DegradationPct:                degradation,
	}
}

func measureQueueThroughput(ctx context.Context, db *sql.DB, cand CandidateID, queue string, workers int, dur time.Duration) float64 {
	claim := claimFor(cand)
	runCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var claims int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("measure-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				jid, ok, _, err := claim(runCtx, db, queue, owner)
				if err != nil || !ok {
					continue
				}
				atomic.AddInt64(&claims, 1)
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, queue, jid)
			}
		}(w)
	}
	wg.Wait()
	return float64(atomic.LoadInt64(&claims)) / dur.Seconds()
}

// ---------------------------------------------------------------------
// E6: correctness checks -- expired-lease reclaim & queue-subscription
// compatibility (§6b of phase-13-plan.md)
// ---------------------------------------------------------------------

func runCorrectnessChecks(ctx context.Context, db *sql.DB) []CorrectnessCheck {
	var checks []CorrectnessCheck

	// Check 1: a worker subscribed only to queue A never claims from queue B,
	// even when B has eligible work and A does not (subscription boundary).
	resetSchema(ctx, db)
	seedQueue(ctx, db, "qa", 100, 0, 0)
	seedQueue(ctx, db, "qb", 100, 0, 0)
	if _, err := db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ('qb', 'QUEUED')`); err != nil {
		checks = append(checks, CorrectnessCheck{"subscription_boundary", false, "seed failed: " + err.Error()})
	} else {
		_, _, ok, err := claimFairness(ctx, db, FairBaseline, []string{"qa"}, "subscribed-a-only")
		pass := err == nil && !ok
		detail := "worker subscribed to {qa} correctly found nothing claimable while only qb had eligible work"
		if !pass {
			detail = fmt.Sprintf("worker subscribed to {qa} claimed something it should not have (ok=%v err=%v)", ok, err)
		}
		checks = append(checks, CorrectnessCheck{"subscription_boundary_no_cross_queue_claim", pass, detail})
	}

	// Check 2: unset subscription (nil/all queues) reproduces old queue-blind
	// behavior -- claims from whichever queue has eligible work, matching
	// §14's "unset TASKFORGE_WORKER_QUEUES means claim everything" default.
	resetSchema(ctx, db)
	seedQueue(ctx, db, "qa", 100, 0, 0)
	seedQueue(ctx, db, "qb", 100, 0, 0)
	_, _ = db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ('qb', 'QUEUED')`)
	{
		_, queue, ok, err := claimFairness(ctx, db, FairBaseline, []string{"qa", "qb"}, "unset-subscription")
		pass := err == nil && ok && queue == "qb"
		detail := "worker with all-queues subscription claimed qb's job as expected"
		if !pass {
			detail = fmt.Sprintf("expected claim from qb, got ok=%v queue=%q err=%v", ok, queue, err)
		}
		checks = append(checks, CorrectnessCheck{"unset_subscription_claims_any_queue", pass, detail})
	}

	// Check 3: reclaim of an expired lease respects the same queue-subscription
	// filter as a fresh claim (§6b's central rule) -- a worker excluded from
	// queue B must not reclaim B's expired-lease job even though it is
	// otherwise reclaim-eligible.
	resetSchema(ctx, db)
	var seededID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO bench_jobs (queue_name, state, claimed_by, claimed_at, lease_expires_at)
		VALUES ('qb', 'RUNNING', 'crashed-worker', now() - interval '1 minute', now() - interval '30 seconds')
		RETURNING id`).Scan(&seededID); err != nil {
		checks = append(checks, CorrectnessCheck{"reclaim_respects_subscription", false, "seed failed: " + err.Error()})
	} else {
		reclaimQuery := `
			SELECT id, queue_name FROM bench_jobs
			WHERE queue_name = ANY($1)
			  AND ( (state = 'QUEUED' AND eligible_at <= now())
			     OR (state = 'RUNNING' AND lease_expires_at < now()) )
			ORDER BY priority DESC, eligible_at ASC
			FOR UPDATE SKIP LOCKED LIMIT 1`
		tx, _ := db.BeginTx(ctx, nil)
		var id int64
		var queue string
		err := tx.QueryRowContext(ctx, reclaimQuery, []string{"qa"}).Scan(&id, &queue)
		tx.Rollback() //nolint:errcheck
		pass := err == sql.ErrNoRows
		detail := "worker subscribed to {qa} correctly could not reclaim qb's expired lease"
		if !pass {
			detail = fmt.Sprintf("worker subscribed to {qa} reclaimed queue %q (err=%v) -- §6b violated", queue, err)
		}
		checks = append(checks, CorrectnessCheck{"reclaim_respects_subscription_boundary", pass, detail})

		// And a worker subscribed to (or unrestricted, covering) qb DOES
		// reclaim it -- confirms TF-INV-004 liveness is preserved for an
		// eligible worker, not just that the boundary check works.
		tx2, _ := db.BeginTx(ctx, nil)
		var id2 int64
		var queue2 string
		err2 := tx2.QueryRowContext(ctx, reclaimQuery, []string{"qa", "qb"}).Scan(&id2, &queue2)
		tx2.Rollback() //nolint:errcheck
		pass2 := err2 == nil && queue2 == "qb" && id2 == seededID
		detail2 := "worker subscribed to {qa,qb} correctly reclaimed qb's expired lease (TF-INV-004 liveness preserved)"
		if !pass2 {
			detail2 = fmt.Sprintf("expected reclaim of qb's expired job (id=%d), got id=%d queue=%q err=%v", seededID, id2, queue2, err2)
		}
		checks = append(checks, CorrectnessCheck{"reclaim_succeeds_for_subscribed_worker", pass2, detail2})
	}

	return checks
}
