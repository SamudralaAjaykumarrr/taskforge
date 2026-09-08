# SLO / Capacity Model

Status: **review document.** Defines candidate SLIs/SLOs and how capacity
should be measured. Contains **no invented benchmark numbers** — every
number below is either a proposed target (explicitly marked `TARGET
(proposed, unvalidated)`) or an actually-measured figure from Phase 9's one
soak run (explicitly marked `MEASURED (Phase 9, single run, illustrative
only)`). Per [roadmap.md](roadmap.md) Phase 9's own quality gate: "no
fabricated benchmarks — numbers are only published once actually measured."
This document does not violate that discipline.

## Why SLOs Do Not Exist Yet

TaskForge has never had a documented SLO. [observability.md](observability.md)
supplies the metrics an SLO would be computed from (13 Prometheus metrics,
all independently tested against scenarios), but no target thresholds have
ever been set against them, and no sustained-load run long enough to
validate a threshold has been executed — Phase 9's longest run is
approximately 4 minutes.

## Candidate SLIs/SLOs

Each SLI below is defined precisely enough to be computed from an existing
metric. Targets are marked `TARGET (proposed, unvalidated)` — they are
starting points for a real capacity-testing exercise (see below), not
commitments.

| SLI | Definition | Existing metric it derives from | Target |
|---|---|---|---|
| **Enqueue availability** | Fraction of `POST /jobs` requests that receive a 2xx or a well-formed 4xx (client error) rather than a 5xx/timeout, over a rolling window. | `taskforge_jobs_submitted_total` combined with HTTP-layer request/error counters (not yet instrumented — HTTP-level request metrics are not in the current 13-metric list and would need to be added). | TARGET (proposed, unvalidated): 99.9% over 30 days |
| **Claim latency** | Time from a job becoming eligible (`eligible_at <= now()`) to being claimed (`state` → `RUNNING`), i.e. `claimed_at - eligible_at`. | `taskforge_claim_latency_seconds` (histogram) — already implemented and, per the internal research, observes the identical sample as `taskforge_queue_age_seconds`. | TARGET (proposed, unvalidated): p99 < 2s under nominal load (worker count sized to job arrival rate) |
| **Queue wait latency** | Same underlying measurement as claim latency today (the two metrics currently share one observation point) — a genuine gap: TaskForge cannot currently distinguish "waiting because no worker is free" from "waiting because of `RETRY_WAIT` backoff" purely from this metric pair without also joining on `taskforge_retry_count`. | `taskforge_claim_latency_seconds` / `taskforge_queue_age_seconds` | TARGET (proposed, unvalidated): same as claim latency; splitting the two metrics is a prerequisite for a meaningful separate target |
| **Execution dispatch availability** | Fraction of claimed jobs that successfully begin handler execution (vs. e.g. a worker crashing between claim and execution start). | Derivable from `taskforge_jobs_completed_total{outcome="lease_expired"}` relative to total claims — not directly instrumented as its own ratio today. | TARGET (proposed, unvalidated): 99.9% |
| **Retry recovery** | Fraction of jobs that eventually reach `SUCCEEDED` after at least one `RETRY_WAIT` cycle, vs. those that exhaust to `DEAD_LETTERED`. | `taskforge_retry_count` (histogram) joined with `taskforge_jobs_completed_total{outcome=...}`. | No target proposed — this is workload-dependent (a job type with a high permanent-failure rate is a data problem, not an availability problem) |
| **Stale-worker fencing correctness** | Rate of `taskforge_stale_completion_rejections_total` relative to total completions — this metric existing and staying near-zero-but-nonzero under load is itself a *health signal* (a nonzero rate proves fencing is exercised and working; a sudden spike indicates a worker-fleet problem, e.g. mass GC pauses or network partition). | `taskforge_stale_completion_rejections_total` | No SLO target — this is a diagnostic signal, not an availability metric. A target here would perversely incentivize hiding real races. |
| **Workflow activation latency** | Time from a predecessor node's terminal transition to a dependent node's `eligible_at` being advanced. | Not separately instrumented today — `propagateWorkflowTransition` runs in the same transaction as the predecessor's completion, so this latency is currently bounded only by "however long that one transaction takes," which is not exposed as its own histogram. | TARGET (proposed, unvalidated): p99 < 500ms; requires new instrumentation |
| **Cancellation propagation** | Time from `POST /jobs/{id}/cancel` (or a workflow-level cancel) committing to the affected job(s) reaching `CANCELLED`, for a job that is `RUNNING` at cancel time. | Not separately instrumented; bounded today by the worker's heartbeat interval (the only point at which `cancel_requested` is cooperatively observed) — see [execution-semantics.md](execution-semantics.md). | TARGET (proposed, unvalidated): p99 < 2× the configured heartbeat interval |
| **API availability** | Fraction of all API requests (all endpoints) served successfully. | Not currently instrumented at the HTTP layer at all — the 13 existing metrics are all job/state-domain metrics, not HTTP-request-domain metrics. | TARGET (proposed, unvalidated): 99.9%; requires new HTTP middleware instrumentation (request count/duration/status by route) |

**Gap this table surfaces**: TaskForge's observability today is entirely
job-domain (submissions, completions, leases, retries) and has **zero
HTTP-request-domain instrumentation** (no per-route request count, latency,
or status code metric). An SLO program cannot be built on job-domain metrics
alone — "enqueue availability" and "API availability" above cannot be
computed from any metric that exists today. This is a concrete, actionable
gap, not a restatement of "add more metrics" in the abstract.

## How Capacity Tests Should Measure Each Dimension

This section specifies methodology, not results. Every measurement below
should be produced by `cmd/chaos` (already built, already bounds duration
and connection-pool size, already checks invariants continuously — see
Phase 9's own README section) run at a scale and duration beyond what has
been done so far, with results **published as they are actually measured**,
per the roadmap's own discipline.

| Dimension | How to measure with existing tooling | What's missing today |
|---|---|---|
| **Throughput** | `cmd/chaos -mode=soak` already reports jobs submitted/terminal/retryable/timeout/crash counts over a run; throughput = terminal jobs / wall-clock duration. | Needs to be run at a duration and job count large enough to reach steady state (Phase 9's 254-job/4-minute run is far below steady state for a production capacity claim). |
| **Latency percentiles** | `taskforge_claim_latency_seconds` and `taskforge_execution_duration_seconds` histograms already exist; scrape `GET /metrics` during a `cmd/chaos` run and compute p50/p95/p99 via standard Prometheus histogram_quantile. | Needs a Prometheus instance actually scraping during a chaos/soak run — not part of the current harness's own reporting (`cmd/chaos/report.go` reports invariant/outcome counts, not scraped metric percentiles). |
| **Queue depth** | `taskforge_jobs_by_state{state="QUEUED"}` / `{state="RETRY_WAIT"}` gauges already exist and are computed fresh per scrape (no drift risk). | Same as above — needs to actually be scraped and recorded during a long run, not just exist as a live gauge. |
| **PostgreSQL CPU** | Not measurable from TaskForge's own metrics — requires host/container-level or `pg_stat_*` monitoring alongside a `cmd/chaos` run. | No integration with any Postgres-side monitoring exists or is proposed as TaskForge's own responsibility (correctly — this is infrastructure monitoring, not application instrumentation) but a capacity test plan must explicitly include it as an external measurement, not omit it. |
| **Connection pool saturation** | `cmd/chaos`'s `-pool-size` flag already bounds and can be swept across runs (e.g. run the same workload at pool sizes 10, 30, 100) to find the saturation point. `internal/store` uses `db.SetMaxOpenConns` (confirmed present, added during Phase 5 to fix a real test-infrastructure bug — not currently exposed as a production-tunable default in `cmd/api`/`cmd/worker` beyond whatever `internal/config` allows). | A documented sweep across pool sizes has never been run; production defaults for `cmd/api`/`cmd/worker` connection pool sizing are not documented anywhere today. |
| **Lock contention** | Observable via PostgreSQL's own `pg_locks`/`pg_stat_activity` during a `cmd/chaos` run with a high worker-to-job ratio (Phase 5's `TestStress_ManyWorkersRaceForOneJob` shape, scaled up and held for longer). | No documented procedure exists for capturing this during a run; `FOR UPDATE SKIP LOCKED`'s design intent (avoid lock waits entirely, by skipping already-locked rows rather than blocking) should mean contention shows up as *reduced batch efficiency* (workers finding fewer candidates per poll) rather than classic lock-wait time — this distinction should be explicitly measured, not assumed. |
| **Worker count scaling** | `cmd/chaos -workers=N` already parameterizes this; a scaling curve (throughput vs. worker count, holding job volume and pool size fixed) has not been run beyond the fixed worker counts used in Phase 5's stress tests (up to ~25) and Phase 9's soak run (20). | No scaling curve beyond ~25 workers has been produced. |
| **Overload behavior** | What happens when job submission rate exceeds sustainable claim throughput indefinitely — does queue depth grow unboundedly (expected and correct — PostgreSQL is durable storage, not a bounded buffer), does claim latency degrade gracefully, or does something break (index bloat, connection exhaustion)? | Never tested. This is the single most important untested capacity question for an enterprise buyer, because "what happens when you send us more than we can handle" is the first question any serious ops review asks. No backpressure signal exists today (see [enterprise-readiness.md](enterprise-readiness.md) Section 3) — TaskForge's honest current answer is "the queue grows in the database until disk or index performance degrades," which should be stated plainly rather than left undiscovered. |

## Explicit Statement on Phase 9's Existing Numbers

Per Phase 9's own README section, the one soak run actually executed was:

> `-mode=soak -seed=20260908 -jobs=150 -workflows=15 -idem-groups=15
> -workers=20`, 4 minutes wall-clock: 254 jobs submitted, 197 reached a
> terminal outcome (156 succeeded, 29 dead-lettered, 12 cancelled), 56
> intermediate retryable outcomes, 15 timeouts, 17 simulated crashes
> recovered via reclaim, **zero invariant violations**.

This is **MEASURED (Phase 9, single run, illustrative only)** evidence that
the invariants hold under light concurrent load with fault injection. It is
**not** capacity evidence — it says nothing about throughput ceiling,
latency under sustained load, or behavior at saturation, and the README
itself is explicit that these numbers are "illustrative, not benchmarks."
This document does not restate or extrapolate a throughput/latency claim
from it, and no future document should either, without a new, explicitly
capacity-purposed run.

## What a Real Capacity/Saturation Test Plan Requires (not run yet)

1. HTTP-request-domain instrumentation added (a real gap, see SLI table
   above) so API availability/latency can be measured at all.
2. `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
   split into genuinely distinct observations (currently the same sample).
3. A `cmd/chaos -mode=soak` run of multi-hour duration (closing Phase 9's
   own stated gap to its Stable maturity bar) with a Prometheus scraper
   attached, at a job volume large enough to reach steady-state queue
   depth.
4. A worker-count and connection-pool-size sweep, not a single fixed
   configuration.
5. A deliberate overload run (submission rate held above sustainable claim
   throughput for an extended period) to observe and document actual
   degradation behavior — currently undocumented and, per the design,
   likely to manifest as unbounded queue growth in PostgreSQL rather than
   a controlled backpressure response, which should be verified rather
   than assumed.
6. External PostgreSQL-side monitoring (CPU, `pg_stat_activity`, `pg_locks`,
   index bloat) run alongside every test above.

No step above has been completed as of this review. See
[enterprise-roadmap.md](enterprise-roadmap.md) for where this work is
sequenced relative to the other enterprise gaps.
