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

## Cross-References

- Failure modes each metric detects: [failure-model.md](failure-model.md)
- Roadmap placement: [roadmap.md](roadmap.md) Phase 8
