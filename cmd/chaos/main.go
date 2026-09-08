// Command chaos is Phase 9's ("Chaos, Load, and Failure Testing",
// docs/roadmap.md) manually invoked stress/load/soak harness -- the
// heavier counterpart to the CI-safe, bounded seeded campaigns in
// internal/chaos's own test suite (`go test ./internal/chaos/...`).
//
// It drives a mixed workload (plain jobs, some scheduled in the future;
// duplicate idempotent submissions; diamond workflows) through a
// configurable pool of concurrent "workers" that resolve each claim via a
// seeded, weighted mix of outcomes (success, retryable failure, permanent
// failure, simulated crash/abandon, cancellation, timeout), continuously
// checking the exact TF-INV-* durable invariants in internal/invariant,
// and prints a report of what happened -- throughput, outcome counts,
// and any invariant violations found (with enough detail, per
// docs/roadmap.md's reproducibility requirement, to rerun the exact same
// seed).
//
// Per docs/roadmap.md's "CI vs Manual Stress" and "Load Harness Safety"
// requirements, this command:
//   - accepts an explicit seed (reproducible) and workload size;
//   - bounds every run by wall-clock duration (-duration), never runs
//     unbounded;
//   - returns a nonzero exit code the instant a durable invariant
//     violation is found;
//   - never opens more database connections than -pool-size, regardless
//     of -workers.
//
// It requires an explicit, real PostgreSQL connection string
// (-db-url, or TASKFORGE_DATABASE_URL/TASKFORGE_TEST_DATABASE_URL) --
// point it at a disposable or test database, never a production one: this
// command creates real job/workflow rows and does not clean them up
// afterward (so a completed run's data remains inspectable).
//
// Usage:
//
//	go run ./cmd/chaos -mode=stress -seed=42 -jobs=500 -workers=20 -duration=30s
//	go run ./cmd/chaos -mode=soak -seed=42 -jobs=200 -workers=10 -duration=10m
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

func main() {
	os.Exit(run())
}

// config holds every flag this command accepts. See the package doc
// comment above for the reproducibility/boundedness contract each one is
// part of.
type config struct {
	mode       string
	seed       int64
	jobs       int
	workflows  int
	idemGroups int
	workers    int
	poolSize   int
	duration   time.Duration
	checkEvery time.Duration
	dbURL      string
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("chaos", flag.ContinueOnError)
	cfg := config{}
	fs.StringVar(&cfg.mode, "mode", "stress", `"stress" (one bounded campaign) or "soak" (repeated campaigns for -duration)`)
	fs.Int64Var(&cfg.seed, "seed", 0, "deterministic seed (0 = derive one from the current time and print it, per docs/roadmap.md's reproducibility requirement)")
	fs.IntVar(&cfg.jobs, "jobs", 500, "number of plain jobs to submit per campaign iteration")
	fs.IntVar(&cfg.workflows, "workflows", 20, "number of diamond workflows to submit per campaign iteration")
	fs.IntVar(&cfg.idemGroups, "idem-groups", 20, "number of duplicate-idempotent-submission groups per campaign iteration (each submitted 2-4 times concurrently)")
	fs.IntVar(&cfg.workers, "workers", 25, "number of concurrent claim/resolve goroutines")
	fs.IntVar(&cfg.poolSize, "pool-size", 30, "maximum open database connections (bounds resource usage regardless of -workers, per docs/roadmap.md's Load Harness Safety)")
	fs.DurationVar(&cfg.duration, "duration", 60*time.Second, "bounded wall-clock duration: the cap on one stress campaign, or the total soak duration across repeated campaigns")
	fs.DurationVar(&cfg.checkEvery, "check-every", 5*time.Second, "how often to run the full durable invariant check while a campaign is in flight")
	fs.StringVar(&cfg.dbURL, "db-url", "", "PostgreSQL connection string (default: $TASKFORGE_DATABASE_URL, then $TASKFORGE_TEST_DATABASE_URL) -- point this at a disposable/test database")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.dbURL == "" {
		cfg.dbURL = os.Getenv("TASKFORGE_DATABASE_URL")
	}
	if cfg.dbURL == "" {
		cfg.dbURL = os.Getenv("TASKFORGE_TEST_DATABASE_URL")
	}
	if cfg.dbURL == "" {
		return config{}, fmt.Errorf("chaos: -db-url is required (or set TASKFORGE_DATABASE_URL/TASKFORGE_TEST_DATABASE_URL) -- point it at a disposable/test database")
	}
	if cfg.mode != "stress" && cfg.mode != "soak" {
		return config{}, fmt.Errorf("chaos: -mode must be \"stress\" or \"soak\", got %q", cfg.mode)
	}
	if cfg.seed == 0 {
		cfg.seed = time.Now().UnixNano() //nolint:gosec // reproducibility record, not security-sensitive
	}
	return cfg, nil
}

func run() int {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := sql.Open("pgx", cfg.dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chaos: open database: %v\n", err)
		return 2
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.poolSize)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := db.PingContext(ctx); err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "chaos: ping database: %v\n", err)
		return 2
	}
	if err := migrate.Up(ctx, db); err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "chaos: run migrations: %v\n", err)
		return 2
	}
	cancel()

	fmt.Printf("taskforge chaos harness: mode=%s seed=%d jobs=%d workflows=%d idem_groups=%d workers=%d pool_size=%d duration=%s\n",
		cfg.mode, cfg.seed, cfg.jobs, cfg.workflows, cfg.idemGroups, cfg.workers, cfg.poolSize, cfg.duration)
	fmt.Println("reproduce a failure by rerunning with the exact same -seed and workload flags above.")

	checker := invariant.New(db)

	switch cfg.mode {
	case "soak":
		return runSoak(db, logger, checker, cfg)
	default:
		return runStress(db, logger, checker, cfg, cfg.seed, 0)
	}
}

// runStress runs exactly one bounded campaign (iteration index and, for
// soak mode, a per-iteration seed derivation are threaded through so soak
// mode can call this repeatedly with distinct, still-deterministic seeds
// derived from the top-level seed).
func runStress(db *sql.DB, logger *slog.Logger, checker *invariant.Checker, cfg config, seed int64, iteration int) int {
	ctx := context.Background()
	m := metrics.New()
	s := store.New(db, store.WithMetrics(m), store.WithLogger(logger))
	rng := chaos.NewRand(seed)

	report := newReport(seed, iteration)
	runCtx, runCancel := context.WithTimeout(ctx, cfg.duration)
	defer runCancel()

	submitWorkload(runCtx, s, rng, cfg, report)

	violations := checkAndReport(ctx, checker, report, "post-submission")
	if len(violations) > 0 {
		report.print()
		return 1
	}

	stopChecks := make(chan struct{})
	var checkerWG sync.WaitGroup
	checkerWG.Add(1)
	violationFound := make(chan struct{}, 1)
	go func() {
		defer checkerWG.Done()
		ticker := time.NewTicker(cfg.checkEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stopChecks:
				return
			case <-ticker.C:
				v := checkAndReport(ctx, checker, report, "in-flight")
				if len(v) > 0 {
					select {
					case violationFound <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			runWorkerLoop(runCtx, s, rng, workerID, report)
		}()
	}
	close(start)
	wg.Wait()

	close(stopChecks)
	checkerWG.Wait()

	report.elapsed = time.Since(report.startedAt)
	finalViolations := checkAndReport(ctx, checker, report, "final")
	report.print()

	select {
	case <-violationFound:
		return 1
	default:
	}
	if len(finalViolations) > 0 {
		return 1
	}
	return 0
}

// runSoak repeats bounded stress campaigns back to back until cfg.duration
// of total wall-clock time has elapsed, per docs/roadmap.md's Soak
// Testing scope: "A manually invoked soak mode may run repeated
// campaigns for a documented duration," reporting "seed(s), iterations,
// jobs processed, failures, invariant violations, elapsed time."
func runSoak(db *sql.DB, logger *slog.Logger, checker *invariant.Checker, cfg config) int {
	deadline := time.Now().Add(cfg.duration)
	iteration := 0
	exitCode := 0
	for time.Now().Before(deadline) {
		iterationSeed := cfg.seed + int64(iteration)
		remaining := time.Until(deadline)
		iterCfg := cfg
		if remaining < iterCfg.duration {
			iterCfg.duration = remaining
		}
		fmt.Printf("\n=== soak iteration %d (seed=%d, remaining budget=%s) ===\n", iteration, iterationSeed, remaining.Round(time.Second))
		if code := runStress(db, logger, checker, iterCfg, iterationSeed, iteration); code != 0 {
			exitCode = code
			fmt.Printf("soak: iteration %d failed -- stopping (a later iteration would only obscure which seed reproduces the failure)\n", iteration)
			break
		}
		iteration++
	}
	fmt.Printf("\nsoak summary: %d iteration(s) completed, base seed=%d, total budget=%s, exit=%d\n", iteration, cfg.seed, cfg.duration, exitCode)
	return exitCode
}

// submitWorkload creates this campaign's mixed workload: plain jobs (a
// fraction scheduled in the future), duplicate-idempotent-submission
// groups, and diamond workflows -- mirroring
// internal/chaos.TestChaos_CombinedCampaign_AllInvariantsSimultaneously_Seeded's
// shape at manually configurable scale.
func submitWorkload(ctx context.Context, s *store.Store, rng *chaos.Rand, cfg config, report *report) {
	for i := 0; i < cfg.jobs; i++ {
		var scheduledAt *time.Time
		if rng.Bool(0.15) {
			st := time.Now().Add(time.Duration(rng.Intn(2000)) * time.Millisecond)
			scheduledAt = &st
		}
		created, err := s.Insert(ctx, job.NewParams{
			JobType: "chaos.stress.plain", Payload: []byte(`{}`),
			MaxAttempts: 2 + rng.Intn(4), ExecutionTimeoutSeconds: 10, ScheduledAt: scheduledAt,
		})
		if err != nil {
			report.recordError(fmt.Errorf("submit plain job: %w", err))
			continue
		}
		report.recordSubmitted(created.ID)
	}

	var idemWG sync.WaitGroup
	for i := 0; i < cfg.idemGroups; i++ {
		key := fmt.Sprintf("stress-idem-%d-%d", report.seed, i)
		dupes := 2 + rng.Intn(3)
		for d := 0; d < dupes; d++ {
			idemWG.Add(1)
			go func() {
				defer idemWG.Done()
				k := key
				created, _, err := s.InsertIdempotent(ctx, job.NewParams{
					JobType: "chaos.stress.idem", Payload: []byte(`{}`),
					MaxAttempts: 3, ExecutionTimeoutSeconds: 10, IdempotencyKey: &k,
				})
				if err != nil {
					report.recordError(fmt.Errorf("submit idempotent job: %w", err))
					return
				}
				report.recordSubmitted(created.ID)
			}()
		}
	}
	idemWG.Wait()

	for i := 0; i < cfg.workflows; i++ {
		prefix := fmt.Sprintf("chaos.stress.wf.%d.%d", report.seed, i)
		inst, err := s.CreateWorkflow(ctx, diamondSpec(prefix))
		if err != nil {
			report.recordError(fmt.Errorf("create workflow: %w", err))
			continue
		}
		for _, n := range inst.Nodes {
			report.recordSubmitted(n.JobID)
		}
	}
}
