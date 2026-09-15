// Phase 13 OD-1 concurrency/fairness evidence harness — v2, corrected per
// the independent review at docs/phase-13-concurrency-evidence-review.md.
//
// ANALYSIS-ONLY. Not part of TaskForge's production build, not imported by
// any internal/ or cmd/ package, touches only its own disposable database.
// v1 (the original harness the review was written against) is preserved,
// unmodified, at v1/main.go for historical comparison — this file
// supersedes it, it does not edit it in place, so "what changed and why"
// stays inspectable.
//
// Corrections this version makes, each tied to a review finding:
//   - H1: last_claimed_at and round-robin are rewritten to use a
//     non-locking per-queue LATERAL lookup (index-scan-per-queue) instead
//     of a cross-table ORDER BY / windowed rank over the full backlog.
//   - H2: a genuine reconstruction of TODAY's actual production
//     claimQuery and indexes (migrations/0001, internal/store/claim.go)
//     runs against its own dedicated table/index set, kept structurally
//     separate from every Phase-13 candidate's schema.
//   - M1: an optimized (2-round-trip) advisory-lock variant is added
//     alongside the original (5-round-trip) one, so the mechanism itself
//     -- not implementation chatter -- can be judged.
//   - M2: round-robin's hardcoded LIMIT 50 is removed entirely (the
//     LATERAL rewrite makes it unnecessary -- it costs O(subscribed
//     queues), not O(backlog)); tested explicitly above 50 queues.
//   - M3: SERIALIZABLE's retry accounting now records every attempt
//     (including every retry inside a single successful claim), not a
//     single boolean per successful call.
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
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const benchDBName = "taskforge_phase13_bench_v2"

func main() {
	adminDSN := flag.String("dsn", "postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable", "admin DSN")
	outPath := flag.String("out", "results-v2.json", "output JSON results path")
	quick := flag.Bool("quick", false, "shorten run windows for a fast smoke test (not for reported evidence)")
	only := flag.String("only", "", "comma-separated experiment names to run (default: all)")
	flag.Parse()

	ctx := context.Background()
	if err := recreateBenchDB(ctx, *adminDSN); err != nil {
		log.Fatalf("recreate bench db: %v", err)
	}
	db, err := sql.Open("pgx", dsnForDB(*adminDSN, benchDBName))
	if err != nil {
		log.Fatalf("open bench db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(150)
	db.SetMaxIdleConns(150)
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	run := selector(*only)
	results := &Results{Timestamp: time.Now().UTC().Format(time.RFC3339)}
	results.Environment = collectEnvironment(ctx, db)
	d := durations(*quick)

	if run("exactness") {
		log.Println("=== 1. Concurrency-limit exactness (incl. reclaim branch) ===")
		for _, cand := range allConcurrencyCandidates {
			r := runExactness(ctx, db, cand, d)
			results.Exactness = append(results.Exactness, r)
			log.Printf("  %-20s maxRunning=%d limit=%d claims=%d reclaims=%d violations=%d attemptedTxns=%d serFail=%d retries=%d exhausted=%d",
				cand, r.MaxObservedRunning, r.Limit, r.TotalClaims, r.TotalReclaims, r.LimitViolations,
				r.AttemptedTxns, r.SerializationFailures, r.Retries, r.RetryExhausted)
		}
	}

	if run("throughput") {
		log.Println("=== 2. Throughput & latency vs worker count ===")
		for _, cand := range allConcurrencyCandidates {
			for _, n := range workerSweep(*quick) {
				r := runThroughput(ctx, db, cand, n, d)
				results.Throughput = append(results.Throughput, r)
				log.Printf("  %-20s workers=%3d throughput=%8.1f/s p50=%7.2fms p95=%7.2fms p99=%7.2fms attemptedTxns=%d successful=%d serFail=%d retries=%d exhausted=%d",
					cand, n, r.ClaimsPerSec, r.P50Ms, r.P95Ms, r.P99Ms, r.AttemptedTxns, r.TotalClaims, r.SerializationFailures, r.Retries, r.RetryExhausted)
			}
		}
	}

	if run("lockcontention") {
		log.Println("=== 3. Lock/contention characterization ===")
		for _, cand := range allConcurrencyCandidates {
			r := runLockContention(ctx, db, cand, 40, d)
			results.LockContention = append(results.LockContention, r)
			log.Printf("  %-20s avgWaitingLocks=%.2f maxWaitingLocks=%d avgActiveConns=%.1f types=%v",
				cand, r.AvgWaitingLocks, r.MaxWaitingLocks, r.AvgActiveConns, r.LockTypeCounts)
		}
	}

	if run("prodbaseline") {
		log.Println("=== 4. Production-baseline fidelity (exact claimQuery + real indexes) ===")
		for i := 0; i < repeats(*quick); i++ {
			r := runProdBaseline(ctx, db, d)
			results.ProdBaseline = append(results.ProdBaseline, r)
			log.Printf("  run=%d floodClaimed=%d trickleClaimed=%d/%d p50Wait=%.1fms p95Wait=%.1fms maxWait=%.1fms",
				i, r.FloodClaimed, r.TrickleClaimed, r.TrickleTotal, r.P50WaitMs, r.P95WaitMs, r.MaxWaitMs)
		}
	}

	if run("fairness") {
		log.Println("=== 5. Corrected fairness at multiple queue counts ===")
		for _, shape := range []QueueShape{shapeTwoQueue, shapeManyQueue} {
			for _, fair := range []FairnessID{FairRoundRobinV2, FairLastClaimedV2} {
				for i := 0; i < repeats(*quick); i++ {
					r := runFairnessMultiQueue(ctx, db, fair, shape, false, d)
					results.Fairness = append(results.Fairness, r)
					log.Printf("  shape=%-10s fairness=%-16s run=%d queues=%d noProgressQueues(max observed)=%d longestGapMs=%.0f p50WaitMs=%.1f p95WaitMs=%.1f maxWaitMs=%.1f",
						shape.Name, fair, i, shape.NumQueues, r.MaxNoProgressQueuesObserved, r.LongestGapMs, r.P50WaitMs, r.P95WaitMs, r.MaxWaitMs)
				}
			}
		}
	}

	if run("integrated") {
		log.Println("=== 6. Integrated combinations: slot-table concurrency + corrected fairness ===")
		for _, shape := range []QueueShape{shapeTwoQueue, shapeManyQueueSmaller} {
			for _, fair := range []FairnessID{FairRoundRobinV2, FairLastClaimedV2} {
				for i := 0; i < repeats(*quick); i++ {
					r := runFairnessMultiQueue(ctx, db, fair, shape, true /* concurrency-capped + reclaim active */, d)
					results.Integrated = append(results.Integrated, r)
					log.Printf("  [integrated] shape=%-10s fairness=%-16s run=%d limitViolations=%d reclaims=%d noProgressQueues(max)=%d p95WaitMs=%.1f maxWaitMs=%.1f",
						shape.Name, fair, i, r.LimitViolations, r.TotalReclaims, r.MaxNoProgressQueuesObserved, r.P95WaitMs, r.MaxWaitMs)
				}
			}
		}
	}

	if run("compat") {
		log.Println("=== 7. Correctness checks (re-verified against v2 schema) ===")
		results.Correctness = runCorrectnessChecks(ctx, db)
		for _, c := range results.Correctness {
			status := "PASS"
			if !c.Pass {
				status = "FAIL"
			}
			log.Printf("  [%s] %s: %s", status, c.Name, c.Detail)
		}
	}

	if run("isolation") {
		log.Println("=== 8. Queue-isolation noise re-check (more repetitions) ===")
		for _, cand := range []CandidateID{CandAdvisoryFast, CandSlot} {
			for i := 0; i < isolationRepeats(*quick); i++ {
				r := runIsolation(ctx, db, cand, d)
				results.Isolation = append(results.Isolation, r)
				log.Printf("  %-20s run=%d alone=%.1f/s underFlood=%.1f/s degradation=%.1f%%",
					cand, i, r.LowVolumeAloneThroughput, r.LowVolumeUnderFloodThroughput, r.DegradationPct)
			}
		}
	}

	// TF-INV-019 bound sweep: queue-count sweep, capacity-configuration
	// sweep, and adversarial stress tests against the recommended
	// integrated mechanism (slot-table + last_claimed_at). See sweep.go.
	// Fixed at capacity=3, workers=10 for the queue-count sweep, matching
	// v2's own integrated-experiment configuration so results are
	// comparable to docs/phase-13-concurrency-evidence-v2.md §D6.
	sweepRepeats := 3
	sweepDuration := 8 * time.Second
	if *quick {
		sweepRepeats = 1
		sweepDuration = 2 * time.Second
	}

	if run("sweep-queuecount") {
		log.Println("=== 9. TF-INV-019 bound: queue-count sweep (last_claimed_at, capacity=3, workers=10) ===")
		for _, nq := range []int{2, 4, 8, 16, 32, 60} {
			for i := 0; i < sweepRepeats; i++ {
				r := runSweepPoint(ctx, db, SweepOptions{
					Label:     fmt.Sprintf("queuecount-%02dq-run%d", nq, i),
					NumQueues: nq, HotQueues: 1, Capacity: 3, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2,
				})
				results.SweepQueueCount = append(results.SweepQueueCount, r)
				log.Printf("  queues=%3d run=%d claims=%d(flood=%d,sparse=%d) throughput=%.1f/s zeroProgress=%d/%d maxNoProgress=%d otherBetween(sparse)=%d/%d longestGap=%.0fms p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms limitViol=%d reclaims=%d reclaimFail=%d",
					nq, i, r.TotalClaims, r.FloodClaims, r.SparseClaims, r.ThroughputPerSec, r.ZeroProgressQueues, nq-1, r.MaxNoProgressQueuesObserved,
					r.MaxOtherQueuesServedBetweenTurnsSparse, nq-1,
					r.LongestGapMs, r.P50WaitMs, r.P95WaitMs, r.P99WaitMs, r.MaxWaitMs, r.LimitViolations, r.ReclaimCount, r.ReclaimCorrectnessFailures)
			}
		}
	}

	if run("sweep-capacity") {
		log.Println("=== 10. TF-INV-019 bound: capacity-configuration sweep (last_claimed_at) ===")
		type capCfg struct {
			name              string
			capacity, workers int
		}
		configs := []capCfg{
			{"low-cap_worker-dominant", 1, 20},    // workers(20) >> available capacity
			{"medium-cap_balanced", 5, 5},         // workers ~= capacity
			{"high-cap_capacity-dominant", 10, 3}, // available capacity > workers
		}
		for _, nq := range []int{2, 16} {
			for _, c := range configs {
				for i := 0; i < sweepRepeats; i++ {
					r := runSweepPoint(ctx, db, SweepOptions{
						Label:     fmt.Sprintf("capacity-%dq-%s-run%d", nq, c.name, i),
						NumQueues: nq, HotQueues: 1, Capacity: c.capacity, Workers: c.workers, Duration: sweepDuration, Fair: FairLastClaimedV2,
					})
					results.SweepCapacity = append(results.SweepCapacity, r)
					log.Printf("  queues=%2d cfg=%-26s cap=%2d workers=%2d run=%d claims=%d throughput=%.1f/s zeroProgress=%d/%d otherBetween(sparse)=%d/%d p50=%.1fms p95=%.1fms max=%.1fms limitViol=%d reclaims=%d reclaimFail=%d",
						nq, c.name, c.capacity, c.workers, i, r.TotalClaims, r.ThroughputPerSec, r.ZeroProgressQueues, nq-1,
						r.MaxOtherQueuesServedBetweenTurnsSparse, nq-1,
						r.P50WaitMs, r.P95WaitMs, r.MaxWaitMs, r.LimitViolations, r.ReclaimCount, r.ReclaimCorrectnessFailures)
				}
			}
		}
	}

	if run("sweep-adversarial") {
		log.Println("=== 11. TF-INV-019 bound: adversarial stress tests ===")
		advRepeats := 2
		if *quick {
			advRepeats = 1
		}
		scenarios := []SweepOptions{
			{Label: "severe-hot-tight-capacity", NumQueues: 32, HotQueues: 1, Capacity: 1, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2},
			{Label: "60queue-tight-capacity", NumQueues: 60, HotQueues: 1, Capacity: 1, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2},
			{Label: "dynamic-worker-count", NumQueues: 8, HotQueues: 1, Capacity: 3, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2, DynamicWorkers: true},
			{Label: "queue-eligible-mid-run", NumQueues: 8, HotQueues: 1, Capacity: 3, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2, LateEligibleQueue: true},
			{Label: "queue-temp-capacity-ineligible", NumQueues: 8, HotQueues: 1, Capacity: 3, Workers: 10, Duration: sweepDuration, Fair: FairLastClaimedV2, TempIneligibleWindow: true},
		}
		for _, s := range scenarios {
			for i := 0; i < advRepeats; i++ {
				opt := s
				opt.Label = fmt.Sprintf("%s-run%d", s.Label, i)
				r := runSweepPoint(ctx, db, opt)
				results.SweepAdversarial = append(results.SweepAdversarial, r)
				log.Printf("  %-32s run=%d claims=%d zeroProgress=%d/%d maxNoProgress=%d otherBetween(sparse)=%d/%d longestGap=%.0fms p95=%.1fms max=%.1fms limitViol=%d reclaims=%d reclaimFail=%d",
					s.Label, i, r.TotalClaims, r.ZeroProgressQueues, opt.NumQueues-1, r.MaxNoProgressQueuesObserved,
					r.MaxOtherQueuesServedBetweenTurnsSparse, opt.NumQueues-1, r.LongestGapMs,
					r.P95WaitMs, r.MaxWaitMs, r.LimitViolations, r.ReclaimCount, r.ReclaimCorrectnessFailures)
			}
		}
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		log.Fatalf("write results: %v", err)
	}
	log.Printf("results written to %s", *outPath)
}

func selector(only string) func(string) bool {
	if only == "" {
		return func(string) bool { return true }
	}
	set := map[string]bool{}
	cur := ""
	for _, c := range only + "," {
		if c == ',' {
			if cur != "" {
				set[cur] = true
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	return func(name string) bool { return set[name] }
}

func durations(quick bool) runDurations {
	if quick {
		return runDurations{exactness: 2 * time.Second, throughput: 2 * time.Second, lockContention: 2 * time.Second, fairness: 3 * time.Second, isolation: 2 * time.Second}
	}
	return runDurations{exactness: 6 * time.Second, throughput: 4 * time.Second, lockContention: 4 * time.Second, fairness: 8 * time.Second, isolation: 4 * time.Second}
}
func workerSweep(quick bool) []int {
	if quick {
		return []int{1, 10, 50}
	}
	return []int{1, 5, 10, 25, 50, 80}
}
func repeats(quick bool) int {
	if quick {
		return 1
	}
	return 3
}
func isolationRepeats(quick bool) int {
	if quick {
		return 1
	}
	return 5
}

type runDurations struct {
	exactness, throughput, lockContention, fairness, isolation time.Duration
}

// ---------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------

func dsnForDB(adminDSN, dbName string) string {
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
	_, _ = db.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, benchDBName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, benchDBName)); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, benchDBName))
	return err
}

// bench_* schema: the Phase-13 CANDIDATE shape only (post-cutover steady
// state -- the new queue-aware index only, per phase-13-plan.md §7/§11's
// "cut the claim query over" step). Kept structurally separate from
// prod_jobs below.
const benchSchemaSQL = `
CREATE TABLE bench_jobs (
    id               BIGSERIAL PRIMARY KEY,
    queue_name       TEXT NOT NULL,
    priority         SMALLINT NOT NULL DEFAULT 0,
    eligible_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    state            TEXT NOT NULL DEFAULT 'QUEUED',
    claimed_by       TEXT,
    claimed_at       TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    attempt_count    INT NOT NULL DEFAULT 0,
    max_attempts     INT NOT NULL DEFAULT 5
);
-- One partial index covering BOTH the fresh-claim and reclaim branches,
-- not two separate single-state indexes. An earlier draft of this harness
-- used idx_bench_claimable (state='QUEUED' only) and idx_bench_reclaimable
-- (state='RUNNING' only) separately; PostgreSQL cannot use two different
-- partial indexes to satisfy one ORDER BY spanning an OR across both
-- conditions, and fell back to a full sequential scan + external sort at
-- realistic backlog size (confirmed via EXPLAIN ANALYZE: 69.8ms at
-- 300,000 rows, collapsing every concurrency candidate's measured
-- throughput uniformly -- see the corrected evidence package's
-- methodology section). eligible_at is never modified by a claim
-- (production doesn't touch it either), so it remains a meaningful,
-- comparable ordering key for a reclaimed row exactly as for a still-
-- queued one, which is what makes one shared index valid for both states.
CREATE INDEX idx_bench_claim_or_reclaim ON bench_jobs (queue_name, priority DESC, eligible_at ASC) WHERE state IN ('QUEUED', 'RUNNING');

-- Dedicated index for the concurrency-limit candidates' own capacity
-- check (SELECT count(*) ... WHERE queue_name=$1 AND state='RUNNING'),
-- used by claimAdvisoryNaive/Fast and claimSerializableOnce -- slot-table
-- doesn't need it, since it tracks capacity via bench_queue_slots
-- instead). idx_bench_claim_or_reclaim does NOT serve this query:
-- confirmed via EXPLAIN ANALYZE that PostgreSQL does not recognize
-- state='RUNNING' as satisfying a state IN ('QUEUED','RUNNING') partial
-- index for this shape of count(*), and falls back to a full (parallel)
-- sequential scan -- 13ms+ per call at 300,000 rows, on nearly every
-- claim attempt (any attempt whose candidate is a fresh QUEUED claim,
-- which is most of them), collapsing advisory-lock's and SERIALIZABLE's
-- measured throughput by roughly an order of magnitude before this index
-- was added. This was caught by comparing an isolated micro-benchmark
-- (advisory lock + pick + update alone: ~440/s) against the full
-- claimAdvisoryNaive path (~10/s) and bisecting the difference -- see the
-- corrected evidence package's methodology section.
CREATE INDEX idx_bench_running_count ON bench_jobs (queue_name) WHERE state = 'RUNNING';

CREATE TABLE bench_queue_state (
    queue_name        TEXT PRIMARY KEY,
    concurrency_limit INT NOT NULL,
    last_claimed_at   TIMESTAMPTZ NOT NULL DEFAULT '-infinity'
);

CREATE TABLE bench_queue_slots (
    queue_name     TEXT NOT NULL,
    slot_index     INT NOT NULL,
    held_by_job_id BIGINT,
    PRIMARY KEY (queue_name, slot_index)
);
CREATE INDEX idx_bench_slots_free ON bench_queue_slots (queue_name) WHERE held_by_job_id IS NULL;
`

// prod_jobs: a faithful, minimal reconstruction of TODAY's actual jobs
// table (migrations/0001_create_jobs_table.up.sql) and TODAY's actual
// claimQuery (internal/store/claim.go), with only cosmetic substitutions
// (bigserial id instead of application-supplied uuid; jobs -> prod_jobs).
// Every predicate, every index, and the two-branch (fresh-claim OR
// expired-lease-reclaim) WHERE shape is copied verbatim. No queue_name
// column exists here, matching production today exactly. A harness-only
// `bench_logical_queue` column is added SOLELY so this experiment can
// measure per-"queue" fairness outcomes -- it is never referenced by the
// claim query's WHERE or ORDER BY, so it has zero effect on the query's
// behavior or plan; it exists only for this test's own bookkeeping.
const prodSchemaSQL = `
CREATE TABLE prod_jobs (
    id                        BIGSERIAL PRIMARY KEY,
    bench_logical_queue       TEXT NOT NULL,
    job_type                  TEXT NOT NULL DEFAULT 'bench',
    priority                  SMALLINT NOT NULL DEFAULT 0,
    state                     TEXT NOT NULL DEFAULT 'QUEUED',
    eligible_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner               TEXT NULL,
    lease_generation          BIGINT NOT NULL DEFAULT 0,
    lease_expires_at          TIMESTAMPTZ NULL,
    attempt_count             INTEGER NOT NULL DEFAULT 0,
    max_attempts              INTEGER NOT NULL DEFAULT 5,
    execution_timeout_seconds INTEGER NOT NULL DEFAULT 30,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    version                   BIGINT NOT NULL DEFAULT 0
);
-- Verbatim from migrations/0001_create_jobs_table.up.sql (RETRY_WAIT
-- included in the partial-index predicate exactly as production has it,
-- even though this harness only ever inserts QUEUED rows).
CREATE INDEX idx_prod_jobs_claimable ON prod_jobs (priority DESC, eligible_at ASC) WHERE state IN ('QUEUED', 'RETRY_WAIT');
CREATE INDEX idx_prod_jobs_expired_lease ON prod_jobs (lease_expires_at) WHERE state = 'RUNNING';
CREATE INDEX idx_prod_jobs_state ON prod_jobs (state);
`

// prodClaimQuery is internal/store/claim.go's claimQuery verbatim, with
// only `jobs` -> `prod_jobs` and the RETURNING list trimmed to what this
// harness's measurement code actually needs.
const prodClaimQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM prod_jobs
		WHERE (
				state IN ('QUEUED', 'RETRY_WAIT')
				AND eligible_at <= now()
			  )
		   OR (
				state = 'RUNNING'
				AND lease_expires_at < now()
				AND attempt_count < max_attempts
			  )
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE prod_jobs
	SET state = 'RUNNING',
		lease_owner = $1,
		lease_generation = prod_jobs.lease_generation + 1,
		lease_expires_at = now() + make_interval(secs => prod_jobs.execution_timeout_seconds),
		attempt_count = prod_jobs.attempt_count + 1,
		updated_at = now(),
		version = prod_jobs.version + 1
	FROM candidate
	WHERE prod_jobs.id = candidate.id
	RETURNING prod_jobs.id, prod_jobs.bench_logical_queue, candidate.old_state`

func resetBenchSchema(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS bench_queue_slots, bench_queue_state, bench_jobs`); err != nil {
		log.Fatalf("drop bench schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, benchSchemaSQL); err != nil {
		log.Fatalf("create bench schema: %v", err)
	}
}

func resetProdSchema(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS prod_jobs`); err != nil {
		log.Fatalf("drop prod schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, prodSchemaSQL); err != nil {
		log.Fatalf("create prod schema: %v", err)
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
		// eligible_at is staggered by 1ms per row, not a single now() shared
		// by the whole batch. A bulk INSERT ... generate_series with a bare
		// now() evaluates that call ONCE for the entire statement (a stable
		// function), giving every seeded row an IDENTICAL eligible_at --
		// discovered during this pass's own debugging when it silently
		// starved reclaim candidates: with thousands of rows tied on
		// (priority, eligible_at) against a handful of reclaim-eligible
		// rows, ORDER BY priority DESC, eligible_at ASC has no
		// discriminating power among the tie, and the query's actual tie
		// order (driven by physical tuple layout, not application logic)
		// can starve the numerically tiny reclaim share almost entirely --
		// not a finding about any candidate's correctness, an artifact of
		// unrealistic seeding (real job arrivals are never truly
		// simultaneous). Staggering removes the tie and better matches how
		// eligible_at actually behaves in production.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO bench_jobs (queue_name, priority, eligible_at, state)
			SELECT $1, 0, now() - ((($2::int) - s) * interval '1 millisecond'), 'QUEUED'
			FROM generate_series(1, $2) AS s`, queue, jobs); err != nil {
			log.Fatalf("seed jobs: %v", err)
		}
	}
	// A freshly created (or truncated-and-refilled) table has no
	// statistics until autovacuum's autoanalyze naturally fires, which is
	// not guaranteed to happen inside a single experiment's run window --
	// discovered mid-pass when the same query plan that used the expected
	// index in an isolated, explicitly-ANALYZEd psql session instead chose
	// a full sequential scan inside the actual harness (no explicit
	// ANALYZE anywhere in it), collapsing throughput by roughly an order
	// of magnitude, inconsistently, across otherwise-identical runs. Every
	// candidate's plan choice depends on this table's statistics being
	// current, so seeding is not complete until this runs.
	if _, err := db.ExecContext(ctx, `ANALYZE bench_jobs, bench_queue_slots, bench_queue_state`); err != nil {
		log.Fatalf("analyze after seed: %v", err)
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
		PostgresVersion: version, MaxConnections: maxConn, SharedBuffers: sharedBuf,
		Fsync: fsync, SynchronousCommit: syncCommit,
		Note: "single-node WSL2 VM, Intel Core Ultra 7 258V, 8 vCPU, 15GiB RAM; see report Section C for caveats -- unchanged from v1",
	}
}

// ---------------------------------------------------------------------
// types
// ---------------------------------------------------------------------

type CandidateID string

const (
	CandAdvisoryNaive CandidateID = "advisory_lock_naive_5rt"
	CandAdvisoryFast  CandidateID = "advisory_lock_optimized_2rt"
	CandSlot          CandidateID = "slot_table"
	CandSerializable  CandidateID = "serializable"
)

var allConcurrencyCandidates = []CandidateID{CandAdvisoryNaive, CandAdvisoryFast, CandSlot, CandSerializable}

type FairnessID string

const (
	FairRoundRobinV2  FairnessID = "round_robin_lateral_v2"
	FairLastClaimedV2 FairnessID = "last_claimed_at_lateral_v2"
)

type QueueShape struct {
	Name       string
	NumQueues  int
	HotQueues  int // first N queues are "flood" producers; the rest are sparse/trickle
	FloodRate  time.Duration
	SparseRate time.Duration
}

var (
	shapeTwoQueue         = QueueShape{Name: "2queue", NumQueues: 2, HotQueues: 1, FloodRate: 2 * time.Millisecond, SparseRate: 300 * time.Millisecond}
	shapeManyQueue        = QueueShape{Name: "60queue", NumQueues: 60, HotQueues: 1, FloodRate: 2 * time.Millisecond, SparseRate: 800 * time.Millisecond}
	shapeManyQueueSmaller = QueueShape{Name: "20queue", NumQueues: 20, HotQueues: 1, FloodRate: 2 * time.Millisecond, SparseRate: 500 * time.Millisecond}
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
	TotalReclaims         int64       `json:"total_reclaims"`
	MaxObservedRunning    int         `json:"max_observed_running"`
	LimitViolations       int         `json:"limit_violations"`
	AttemptedTxns         int64       `json:"attempted_txns"`
	SerializationFailures int64       `json:"serialization_failures"`
	Retries               int64       `json:"retries"`
	RetryExhausted        int64       `json:"retry_exhausted"`
	SamplesTaken          int64       `json:"samples_taken"`
}

type ThroughputResult struct {
	Candidate             CandidateID `json:"candidate"`
	Workers               int         `json:"workers"`
	DurationSec           float64     `json:"duration_sec"`
	TotalClaims           int64       `json:"total_claims"`
	ClaimsPerSec          float64     `json:"claims_per_sec"`
	P50Ms                 float64     `json:"p50_ms"`
	P95Ms                 float64     `json:"p95_ms"`
	P99Ms                 float64     `json:"p99_ms"`
	MaxMs                 float64     `json:"max_ms"`
	AttemptedTxns         int64       `json:"attempted_txns"`
	SerializationFailures int64       `json:"serialization_failures"`
	Retries               int64       `json:"retries"`
	RetryExhausted        int64       `json:"retry_exhausted"`
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

type ProdBaselineResult struct {
	DurationSec    float64 `json:"duration_sec"`
	FloodClaimed   int64   `json:"flood_claimed"`
	TrickleClaimed int64   `json:"trickle_claimed"`
	TrickleTotal   int64   `json:"trickle_total"`
	P50WaitMs      float64 `json:"p50_wait_ms"`
	P95WaitMs      float64 `json:"p95_wait_ms"`
	MaxWaitMs      float64 `json:"max_wait_ms"`
}

type FairnessResult struct {
	Fairness                    FairnessID `json:"fairness"`
	Shape                       string     `json:"shape"`
	NumQueues                   int        `json:"num_queues"`
	Integrated                  bool       `json:"integrated"`
	DurationSec                 float64    `json:"duration_sec"`
	TotalClaims                 int64      `json:"total_claims"`
	TotalReclaims               int64      `json:"total_reclaims"`
	LimitViolations             int        `json:"limit_violations"`
	SparseClaimed               int64      `json:"sparse_claimed"`
	SparseTotal                 int64      `json:"sparse_total"`
	P50WaitMs                   float64    `json:"p50_wait_ms"`
	P95WaitMs                   float64    `json:"p95_wait_ms"`
	P99WaitMs                   float64    `json:"p99_wait_ms"`
	MaxWaitMs                   float64    `json:"max_wait_ms"`
	MaxNoProgressQueuesObserved int        `json:"max_no_progress_queues_observed"`
	LongestGapMs                float64    `json:"longest_progress_gap_ms"`
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
	Timestamp        string                 `json:"timestamp"`
	Environment      Environment            `json:"environment"`
	Exactness        []ExactnessResult      `json:"exactness"`
	Throughput       []ThroughputResult     `json:"throughput"`
	LockContention   []LockContentionResult `json:"lock_contention"`
	ProdBaseline     []ProdBaselineResult   `json:"prod_baseline"`
	Fairness         []FairnessResult       `json:"fairness"`
	Integrated       []FairnessResult       `json:"integrated"`
	Isolation        []IsolationResult      `json:"isolation"`
	Correctness      []CorrectnessCheck     `json:"correctness"`
	SweepQueueCount  []SweepResult          `json:"sweep_queue_count"`
	SweepCapacity    []SweepResult          `json:"sweep_capacity"`
	SweepAdversarial []SweepResult          `json:"sweep_adversarial"`
}

// claimOutcome is the uniform result of one concurrency-candidate claim
// attempt, now carrying full attempt accounting (M3's fix) instead of a
// single retried-or-not boolean.
type claimOutcome struct {
	id            int64
	ok            bool
	wasReclaim    bool
	attemptedTxns int64 // total transaction attempts this call made, including aborted ones
	serFailures   int64 // of attemptedTxns, how many ended in a serialization failure
	exhausted     bool  // true if maxRetries was reached without success
}

type claimFunc func(ctx context.Context, db *sql.DB, queue, owner string) (claimOutcome, error)
