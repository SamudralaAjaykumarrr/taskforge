# Observability

Status: foundational. Defines what must be measurable and loggable, in
vendor-neutral terms. No specific metrics backend, log aggregator, or
tracing vendor is chosen here — TaskForge should be instrumentable via
standard interfaces (e.g., an OpenTelemetry-compatible approach) without
hard-coding to a specific SaaS.

## Why This Matters Before Any Code Exists

A reliability-focused system that cannot be observed is not verifiably
reliable in production — it merely claims to be. Every metric and log field
below exists to let an operator (or a test) distinguish "working as
designed" from "silently degraded" for a specific failure mode in
[failure-model.md](failure-model.md).

## Metrics

| Metric | Type | Purpose |
|---|---|---|
| `taskforge_jobs_submitted_total` | counter, labeled by `job_type` | Submission volume. |
| `taskforge_jobs_completed_total` | counter, labeled by `job_type`, `outcome` (`succeeded`/`failed_retryable`/`failed_permanent`/`timed_out`/`lease_expired`/`cancelled`) | Attempt-level outcome volume — maps directly to `job_attempts.outcome`. |
| `taskforge_jobs_by_state` | gauge, labeled by `state` | Live count of jobs in each state — `SELECT state, count(*) FROM jobs GROUP BY state`. Directly surfaces queue depth and DLQ volume. |
| `taskforge_jobs_dead_lettered_total` | counter, labeled by `job_type` | Cumulative DLQ volume — distinct from the `jobs_by_state` gauge because it never decreases, useful for alerting on DLQ growth rate. |
| `taskforge_claim_latency_seconds` | histogram | Time from a job becoming eligible (`eligible_at`) to being claimed. Directly measures worker fleet capacity relative to load. |
| `taskforge_queue_age_seconds` | histogram, sampled per claim | `claimed_at - eligible_at` for each claim, i.e. how long a job waited past eligibility. |
| `taskforge_execution_duration_seconds` | histogram, labeled by `job_type`, `outcome` | Wall-clock time from claim to completion report. |
| `taskforge_lease_expirations_total` | counter, labeled by `job_type` | Count of jobs reclaimed due to expired lease — a direct signal of worker crash rate (F1/F2/F10/F11). |
| `taskforge_stale_completion_rejections_total` | counter | Count of completion/heartbeat calls rejected by the fencing check (TF-INV-003/014) — should be non-zero only under real crash/reclaim activity; a sustained high rate indicates lease durations are too short relative to job execution time. |
| `taskforge_retry_count` | histogram, labeled by `job_type` | Distribution of `attempt_count` at terminal state — shows how often jobs need more than one attempt. |
| `taskforge_active_workers` | gauge | Distinct `lease_owner` values with a `heartbeat_at` within the last lease-extension interval. |
| `taskforge_heartbeats_total` | counter | Raw heartbeat volume — used to detect heartbeat-interval misconfiguration relative to lease TTL. |
| `taskforge_idempotent_submission_hits_total` | counter | Count of `POST /jobs` calls that resolved to an existing row via idempotency key rather than creating a new one — validates that `Idempotency-Key` usage is actually happening when expected. |

## Structured Logs

Every log line relevant to a job's lifecycle includes, at minimum:
`job_id`, `job_type`, `state` (post-transition), `attempt_number` (if
applicable), `lease_generation` (if applicable). Key events to log:

- Job submitted (with `idempotency_key` if present, and whether it resolved
  to a new or existing row).
- Job claimed (`worker_id`, `lease_generation`, `attempt_number`).
- Job completed (`outcome`, `error_class` if failed).
- Lease expired / job reclaimed (previous `lease_owner`, previous
  `lease_generation`, new values).
- Stale completion/heartbeat rejected (the rejected `lease_generation` vs.
  the current one) — this is a security/correctness-relevant event, not
  just informational, and should be logged at a level that draws attention
  if it becomes frequent.
- Job dead-lettered (`last_error`, `attempt_count`).
- Cancellation requested / cancellation race outcome (which side won).

Logs are structured (key-value or JSON), not free-text, so they can be
queried without vendor-specific log parsing.

## Trace Boundaries

If distributed tracing is introduced, the natural span boundaries are:

- **Submission span**: `POST /jobs` request handling, ending at transaction
  commit.
- **Attempt span**: claim → handler execution → completion report, one span
  per `job_attempts` row, with the span's trace/span ID optionally recorded
  in `job_attempts` for cross-referencing logs to traces (a nice-to-have,
  not required for v1).
- A job's full lifecycle (across multiple retried attempts) is naturally
  represented as multiple linked attempt spans sharing a common `job_id`
  attribute, rather than forcing all attempts into a single span — this
  matches how the data model itself separates `jobs` (logical) from
  `job_attempts` (physical execution).

No specific tracing vendor or exporter is chosen in this document —
whatever is chosen later should be pluggable (e.g. via OpenTelemetry SDK
conventions) rather than hard-coded into core logic.

## What Observability Does Not Replace

Metrics and logs are for humans and dashboards; they are not a substitute
for the invariant tests in [testing-strategy.md](testing-strategy.md).
`taskforge_stale_completion_rejections_total` being zero in production does
not prove TF-INV-003 holds — it only means it hasn't been exercised
recently. The deterministic tests are the actual proof; observability is
how you'd notice a violation in the wild if the tests ever missed one.

## Implementation Notes (Phase 8)

This section records the concrete decisions Phase 8's implementation made
where this document above leaves an open choice, plus defects the
implementation work found and fixed. It does not change any metric name,
type, or label listed above — those are implemented exactly as specified.

- **Backend**: Prometheus-compatible, via `github.com/prometheus/client_golang`
  (`internal/metrics`). Every metric above is registered on a private
  `*prometheus.Registry` per process (never the global default
  registerer), constructed once by `metrics.New()` and shared between
  `internal/store` (which records every metric) and that process's
  `GET /metrics` HTTP endpoint. `cmd/api` serves it on the existing API
  listen address; `cmd/worker` serves it on its own address
  (`TASKFORGE_METRICS_ADDR`, default `:9090`, since the worker process
  has no other HTTP server) — no external Prometheus/collector is
  required to run TaskForge or its test suite; the endpoint costs
  nothing if never scraped.
- **Where metrics are recorded**: entirely inside `internal/store`, the
  single choke point every durable transition already runs through — not
  in `internal/worker` or `internal/api`. This means every metric is
  observable by store-level tests calling `Store.Claim`/`Store.Complete*`
  directly (as most of the existing scenario-corpus tests do), not only
  by tests that drive a full worker loop, and it means a metric is never
  recorded before its transaction has actually committed.
- **`taskforge_jobs_by_state` / `taskforge_active_workers`**: computed by
  a `prometheus.Collector` (`internal/metrics.NewStateCollector`) that
  queries PostgreSQL fresh at every scrape (`Store.JobStateCounts`,
  `Store.ActiveWorkerCount`) — never from in-memory bookkeeping, so
  neither gauge can drift after a restart, a crash, or a rolled-back
  transaction. `taskforge_active_workers`' "recent" window
  (`TASKFORGE_ACTIVE_WORKER_WINDOW`, default 30s) is an implementation
  choice, like `MaxIdempotencyKeyLength` in
  [idempotency.md](idempotency.md): this document's "last
  lease-extension interval" has no fixed value in v1 (no separate
  lease-extension-interval configuration surface exists).
- **`taskforge_claim_latency_seconds` / `taskforge_queue_age_seconds`**:
  implemented as the exact same observed sample
  (`claimed_at - eligible_at`, both timestamps read from the same
  transaction's `RETURNING` clause, never an application clock) recorded
  into both histograms. Read literally, this document's own definitions
  of the two are identical; rather than inventing an undocumented
  distinction between them, both names are kept (both appear in the
  authoritative table above) and both observe the same value.
- **`taskforge_execution_duration_seconds`**: computed from
  `job_attempts.started_at` (returned by the same `UPDATE ... RETURNING`
  that finalizes the attempt) and the completion transaction's own
  `updated_at` — both PostgreSQL-clock values from the same transaction,
  so this requires no extra query. Not observed for
  `CancelQueuedOrRetryWait` (a pre-claim cancellation has no attempt) or
  for the Lazy Dead-Letter Sweep's silent dead-letter transitions
  (observing it there would require one additional query per swept job
  purely for this histogram, which this document's performance guidance
  — "do not create a database query per log event" — rules out; the
  sweep still fully populates `taskforge_jobs_completed_total`,
  `taskforge_jobs_dead_lettered_total`, and `taskforge_retry_count`).
- **Defect found and fixed: reclaim vs. ordinary retry conflation**.
  Before this phase, `internal/worker`'s claim-logging used
  `lease_generation > 1` as a proxy for "this claim is a genuine
  lease-expiry reclaim." That proxy is wrong as of Phase 3: `Claim`
  increments `lease_generation` and `attempt_count` together on *every*
  claim, fresh or reclaimed, so an ordinary `RETRY_WAIT` claim after
  backoff (no crash, no lease loss) also has `lease_generation > 1` and
  was being mislabeled "job reclaimed after lease expiration." This
  matters for exactly the distinction this task's objective list draws
  explicitly ("Are jobs being reclaimed after worker failure?" vs. "Are
  retries increasing?" are different questions). `Store.Claim` already
  computes the real, exact signal internally (the claim query's own
  `state = 'RUNNING' AND lease_expires_at < now()` branch, which cannot
  match a `RETRY_WAIT` claim) — the fix surfaces that existing
  computation instead of re-deriving an ambiguous proxy in the caller.
  `taskforge_lease_expirations_total` is incremented only by the genuine
  branch. Regression test:
  `TestMetrics_ReclaimVsRetry_LeaseExpirationsNotConflatedWithBackoffRetry`
  (`internal/store/observability_test.go`).
- **Idempotency key never logged**: `POST /jobs` submission logs record
  only whether an `Idempotency-Key` was supplied (`had_idempotency_key`),
  never its value, even though [idempotency.md](idempotency.md)'s
  implementation notes contemplate the key being short enough to be
  "loggable at the API layer." This phase's sensitive-data policy takes
  the more conservative reading deliberately, since a caller-chosen key
  could embed sensitive context (an order ID, a token) the caller did
  not intend to appear in TaskForge's own logs.
- **Workflow observability**: this document defines no workflow-specific
  metric, and `workflow_id` is never a metric label (unbounded per
  workflow, per this document's cardinality guidance). Workflow
  diagnostics are structured logs instead (`internal/store`):
  `workflow_created`, `workflow_cancellation_requested`, and
  `workflow_finalized` (state `SUCCEEDED`/`FAILED`/`CANCELLED`, logged
  exactly once, only by the transaction that actually finalized it, only
  after that transaction has committed). Each workflow node's underlying
  job is otherwise a fully ordinary job for every metric/log purpose
  above (its own `job_id`, its own `taskforge_jobs_submitted_total`
  increment, etc.). Per-node activation/cascade-cancellation events are
  **not** individually logged — this was deliberately left out of Phase
  8's scope (it would require threading a commit-deferred event list
  through `propagateWorkflowTransition`'s five call sites for a purely
  diagnostic benefit); per-node history remains fully queryable via
  `GET /workflows/{id}` and `job_attempts`.
- **Tracing**: **not implemented in Phase 8.** This document frames
  tracing as conditional ("if distributed tracing is introduced") and
  chooses no vendor; the completion criteria this phase is actually
  graded against (an operator answering "how many jobs are
  dead-lettered" / "what is our claim latency" without querying the
  database) are metrics-and-logging questions, not tracing ones. The
  correlation objective tracing would serve — following one logical
  execution through submission → claim → attempt → retry/reclaim →
  completion — is met today via structured log correlation on `job_id`
  (every lifecycle log line above carries it) rather than trace spans.
  This is an explicit deferral, not an oversight — see
  [roadmap.md](roadmap.md) Phase 8's non-goals and the README's Phase 8
  section for the same statement.
- **Structured logging model**: every lifecycle log line carries a
  stable `event` field plus the applicable subset of `job_id`,
  `job_type`, `worker_id`, `attempt`, `lease_generation`, `state`
  (post-transition), `error_class`, `retryable`, `workflow_id`. Event
  vocabulary: `submission`, `duplicate_submission_hit`, `job_claimed`,
  `job_reclaimed`, `execution_start`, `execution_success`,
  `retryable_failure`, `retry_scheduled`, `permanent_failure`,
  `dead_lettered`, `cancellation_requested` (API-level, which cascade
  path resolved it), `cancellation_requested_observed` /
  `cancellation_observed` / `cancellation_acknowledged` (worker-level),
  `execution_timeout`, `stale_completion_rejected`, `lease_lost`,
  `heartbeat`, `workflow_created`, `workflow_cancellation_requested`,
  `workflow_finalized`. No arbitrary payload body is ever logged.
- **Telemetry failure behavior**: recording a metric is a synchronous,
  in-memory `prometheus` counter/histogram update — it cannot fail, block
  on a network call, or hold a database transaction open, so it
  introduces no new failure mode into any correctness path (see
  `internal/metrics`'s package doc comment). The `/metrics` HTTP
  endpoint's only failure mode is its own scrape request; a scrape
  timing out or a scrape-time database error (the two `StateCollector`
  queries) is logged and simply omits the affected series — it never
  panics and never touches a job's durable state. Tested by
  `TestStateCollector_QueryFailureDoesNotPanicOrBlock`
  (`internal/metrics/metrics_test.go`).
- **Cardinality policy, audited**: every label above is job_type
  (bounded by how many job types a deployment registers) or a small
  fixed enum (`outcome`, `state`). `job_id`, `workflow_id`, `node_id`,
  `worker_id`, `idempotency_key`, and raw error text are never metric
  labels anywhere in this codebase — audited by
  `TestCardinality_NoMetricLabelIsIdempotencyKeyOrJobID` and
  `TestCardinality_ManyUniqueJobsDoNotCreateNewMetricSeries`
  (`internal/store/observability_test.go`).

## Cross-References

- Failure modes each metric detects: [failure-model.md](failure-model.md)
- Roadmap placement: [roadmap.md](roadmap.md) Phase 8
