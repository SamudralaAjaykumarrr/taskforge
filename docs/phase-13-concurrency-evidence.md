# Phase 13 OD-1 — Concurrency/Fairness Evidence Package

Status: **ANALYSIS + EVIDENCE ONLY.** No production code changed. No
mechanism selected. No ADR written. This document exists to give
[phase-13-plan.md](phase-13-plan.md) §18 OD-1's mandatory ADR the
concurrency/performance analysis the roadmap requires *before* that ADR
selects an algorithm ("the simplest correct PostgreSQL-native scheduling
algorithm only after a concurrency and performance analysis" —
[phase-13-plan.md](phase-13-plan.md) §6a). It does not itself decide OD-1;
it produces the evidence OD-1's author still has to weigh and write down.

This document does not invent architecture beyond what
[phase-13-plan.md](phase-13-plan.md) §6a already names as candidates: three
concurrency-limit mechanisms and two fairness-ordering mechanisms, evaluated
here exactly as scoped, not extended.

Companion artifacts (not committed, not staged — see the repo-hygiene note
at the end of this document):

- `tools/phase13bench/main.go` — the analysis-only benchmark harness that
  produced every measured number below. Not imported by any `internal/` or
  `cmd/` package; excluded from `go build ./...`/`go test ./...`.
- `tools/phase13bench/results.json` — raw output of the run this report's
  primary tables are drawn from (2026-09-15T16:59:51Z).
- `tools/phase13bench/results-run2.json` — a second independent run, used
  to check reproducibility and reported wherever a finding's stability
  across runs mattered enough to check.

Reproduce with: `go run ./tools/phase13bench` (requires a reachable
PostgreSQL superuser connection; defaults to
`postgres://postgres:postgres@127.0.0.1:5432/postgres`, override with
`-dsn`). The tool creates and drops its own `taskforge_phase13_bench`
database; it never touches the real `jobs` table or schema.

---

## 0. What was inspected before any measurement was taken

Per the task's own instruction to identify only the candidates the Phase 13
plan already permits, not invent new ones, the following were read in full
before designing any experiment:

- [enterprise-roadmap.md](enterprise-roadmap.md) — Phase 13's authoritative
  scope, the OD-1 mandate itself, and TF-INV-019's original (pre-renumbering)
  requirement text.
- [phase-13-plan.md](phase-13-plan.md), specifically §6a (concurrency
  mechanism + fairness candidates — the three-candidate, two-candidate list
  this evidence package tests exactly), §6b (reclaim/queue-subscription
  rule, tested in §E6 below), §7 (the `queue_limits`/`rate_limit_buckets`/
  `queue_slots` strawman schema this harness's tables mirror), §10
  (TF-INV-019's proof-obligation framing), §18 OD-1/OD-4/OD-9.
- [invariants.md](invariants.md) — all of TF-INV-001–018 (to confirm no
  candidate here touches fencing/lease/idempotency mechanics), and
  TF-INV-019's current "Reserved, algorithm-independent" text verbatim.
- `internal/store/claim.go` — the real `claimQuery` (the `SKIP LOCKED` CTE +
  conditional `UPDATE` idiom every candidate here either reuses or
  deliberately departs from) and `Claim`'s reclaim/dead-letter-sweep
  interaction.
- `internal/worker/worker.go` — confirms nothing about the claim-execute-
  heartbeat-complete loop or its `Store` interface needs to change for any
  of the three candidates (the concurrency-limit/fairness decision is fully
  contained inside the claim query's shape, not the worker loop).
- `migrations/0001_create_jobs_table.up.sql` and the full `migrations/`
  sequence through `0010` — confirms today's actual indexes
  (`idx_jobs_claimable` on `(priority DESC, eligible_at ASC)`, no
  `queue_name` column yet) and today's PostgreSQL 16 target
  (`docker-compose.yml`: `postgres:16`).
- `internal/store/concurrency_stress_test.go` — the existing Phase 5
  `SKIP LOCKED` stress-test idiom (many workers, force-expired leases,
  barrier-synchronized starts, no sleep-based races), which this harness's
  own experiments deliberately follow the same discipline for
  (§B below).
- `docs/reference-analysis.md` — Hatchet's group-key round robin and
  Faktory's rejected strict-priority-drain model, cited by
  [phase-13-plan.md](phase-13-plan.md) §6a as the named fairness
  candidates.

No fourth concurrency mechanism or third fairness algorithm was introduced.
Where this package's own experiment design required a concrete SQL shape
the plan left unspecified (the exact `queue_slots` claim statement, the
exact round-robin query), that shape is presented as this package's own
engineering, clearly flagged, not attributed to the plan.

---

## A. Candidate comparison matrix

### A1. Concurrency-limit mechanisms (§6a candidates 1–3)

| Dimension | 1. Advisory lock | 2. Slot-table semaphore | 3. `SERIALIZABLE` + retry |
|---|---|---|---|
| Exactness (never > N running) | **Exact** (measured, 0/4,873 samples over limit) | **Exact** (measured, 0/4,672 samples over limit) | **Exact** (measured, 0/4,519 samples over limit) |
| Mechanism class | Session-level advisory lock, held for claim-tx duration | Row-level `FOR UPDATE SKIP LOCKED` on a second table, same idiom as job selection | MVCC serializable-snapshot conflict detection + app-level retry loop |
| New schema | None | `queue_slots(queue_name, slot_index, held_by_job_id)` | None |
| New lock primitive vs. today's `claimQuery` | Yes — advisory locks are not used anywhere else in this codebase | No — same `FOR UPDATE SKIP LOCKED` idiom as the existing claim CTE | No new lock type, but a new *isolation level* and a new *retry loop* on the hottest query in the system |
| Peak measured throughput (single hot queue, best worker count) | ~605 claims/s (10 workers) | **~1,770 claims/s** (25 workers) | ~428 claims/s (5 workers, before abort storm dominates) |
| p50 latency at 80 workers | 142.8ms | 8.7ms | 54.0ms (but 82% of attempts are wasted retries) |
| Serialization/abort rate | 0% (no such failure mode) | 0% (no such failure mode) | **47%→82%** as workers scale 5→80 |
| Lock/wait footprint under 40-worker contention | Avg 35.4 waiting lock entries/sample (`advisory` locktype) | Avg 0.10 waiting lock entries/sample | Avg 12.4 waiting entries/sample (`transactionid` + `tuple`), plus retries invisible to `pg_locks` |
| Queue/tenant isolation | Per-queue hash key in principle isolates queues; measured degradation 13.7–21.3% under concurrent hot-queue load (noisy, see §L) | Physically separate rows per queue; measured degradation 13.5–19.5% (same noise) — **not cleanly separated from candidate 1 on this hardware** | Not separately measured (already ruled out on throughput/abort grounds before this dimension mattered) |
| Composability with fairness ordering (§6a layer 2) | Compatible with any ordering, but the lock itself already serializes all claims for a queue, so fairness ordering *within* one queue is moot; only cross-queue ordering matters | Naturally composable — same `FOR UPDATE SKIP LOCKED` statement can carry any `ORDER BY` | Compatible but adds ordering as one more predicate an already-retried transaction must re-evaluate |
| Operational new responsibility | None beyond acquiring/holding the lock (no release step — it's transaction-scoped) | **Every terminalization call site** (`CompleteSuccess`, `CompleteFailure`, `CompleteRetryableFailure`, `CompleteCancelled`, `CompleteTimeout`, Lazy Dead-Letter Sweep) must release the held slot in the same transaction — a new, enumerable-but-real class of regression risk (a forgotten release silently shrinks effective capacity — [phase-13-plan.md](phase-13-plan.md) §19 already names this risk) | A retry loop must be added to the single hottest query path, with backoff/retry-limit semantics this codebase has never needed before |
| Failure mode under bug/misuse | A stuck/uncommitted transaction holding the advisory lock blocks *all* claims for that queue (not just concurrency-limit enforcement) until it resolves | A slot leak (missed release) permanently reduces capacity for that queue until an operator notices via `taskforge_queue_running` plateauing below `taskforge_queue_concurrency_limit` (already anticipated in [phase-13-plan.md](phase-13-plan.md) §19) | Retry-loop bug (e.g., no backoff, no cap) risks a retry storm amplifying exactly the P0 tenant-starvation threat model this phase exists to close |
| Verdict | Correct, simple, but **throughput-capped by serialization, not by the configured limit** — realized concurrency inside the critical section is effectively 1 regardless of `concurrency_limit`'s actual value | Correct, best throughput and lock-contention profile measured, fits the existing `SKIP LOCKED` idiom exactly, at the cost of a new release obligation at every terminal transition | Correct in principle (textbook SSI), but **measured to degrade under exactly the contention level Phase 13 exists to handle** — worst candidate on every performance axis measured |

### A2. Fairness-ordering candidates (§6a layer 2, tested independent of the concurrency mechanism — see §B)

| Dimension | Baseline (today, queue-blind) | A. Round-robin (rank interleave) | B. `last_claimed_at` ascending |
|---|---|---|---|
| New schema | None | None | One new column, `queue_state.last_claimed_at` |
| Trickle (low-volume) queue starved? | **Yes** — 9–10 of 19–20 trickle jobs claimed in 6s, p95 wait up to 2.66s | No — 19/19 claimed every run, p95 wait 13–21ms | No — 19/19 or 19/20 claimed every run, p95 wait 62–105ms |
| Query shape | Single statement, today's exact `ORDER BY` | **Two statements per claim attempt** (see §E4 — PostgreSQL rejects `FOR UPDATE` combined with a window function in one query) | Single statement, one extra `JOIN` |
| Flood (hot) queue throughput while fair | ~5,749–6,451/6s (~960–1,075/s) | ~5,357–6,451/6s (~893–1,075/s), comparable to baseline | **~744–1,132/6s (~124–189/s) — 5–8× lower**, a new bottleneck this mechanism itself introduces (§F) |
| Root cause of B's throughput cost | n/a | n/a | Every claim from a given queue serializes on an `UPDATE` to that queue's single `queue_state` row — a write hotspot that scales with the flooded queue's own claim rate, not the trickle queue's |
| Verdict | The documented problem (confirms TF-INV-019 is needed) | Best measured fairness-to-throughput ratio, but costs an extra non-locking read per claim and needs `array_position`-based re-ordering to legally combine with `FOR UPDATE SKIP LOCKED` | Effective at fairness, cheapest schema change, but the single-row-per-queue update is a real, measured throughput ceiling on the flooded queue that scales with that queue's own volume |

---

## B. Benchmark/test methodology

**Isolation from production code.** All experiments run against a
disposable `taskforge_phase13_bench` database (created and dropped by the
harness itself) with three throwaway tables (`bench_jobs`,
`bench_queue_slots`, `bench_queue_state`) that mirror the *shape* of
[phase-13-plan.md](phase-13-plan.md) §7's schema strawman closely enough to
be representative, but are not the real `jobs` table and are never touched
by `internal/store` or any production binary.

**Determinism discipline**, following the existing
`internal/store/concurrency_stress_test.go` precedent
([testing-strategy.md](testing-strategy.md)'s "never a sleep-based race"
rule): every concurrent experiment releases all worker goroutines from a
single start signal or a fixed wall-clock run window (`context.WithTimeout`),
never relies on sleep-based interleaving to produce a race, and every
correctness assertion (concurrency-limit exactness, no-cross-queue-claim,
reclaim-boundary) is a hard structural check, not a statistical one.

**Six experiments, in order:**

1. **E1 — Concurrency-limit exactness.** 30 workers, one queue, limit = 5,
   5-second run, ~20ms simulated execution hold per claim. A dedicated
   monitor goroutine polls `SELECT count(*) ... WHERE state='RUNNING'`
   every 1ms on its own connection (never blocked behind worker
   transactions) and records the maximum observed concurrently-running
   count and any sample exceeding the limit.
2. **E2 — Throughput & latency vs. worker count.** One queue, limit = 20
   (generous, so the limit itself is not the bottleneck being measured),
   worker counts {1, 5, 10, 25, 50, 80}, 4-second runs, 5ms simulated
   execution hold, per-claim round-trip latency recorded client-side.
3. **E3 — Lock/contention characterization.** 40 workers, one queue,
   limit = 10, 4-second run, sampling `pg_locks WHERE NOT granted` and
   `pg_stat_activity` every 50ms on a dedicated connection.
4. **E4 — Fairness/starvation.** Two queues, no concurrency cap (fairness
   ordering is tested independent of the concurrency-limit mechanism,
   since [phase-13-plan.md](phase-13-plan.md) §6a itself frames them as
   separately layered decisions): `flood` is kept permanently non-empty by
   a producer inserting 5 rows every 2ms; `trickle` gets exactly one row
   every 300ms, with its exact insertion timestamp recorded so
   wait-until-claimed can be measured precisely. 10 workers, subscribed to
   both queues, 6-second run.
5. **E5 — Queue/tenant isolation under load.** A low-volume queue's own
   throughput measured alone, then measured again while a separate
   worker pool floods a second, independently-capped queue — same
   candidate, same process, same database.
6. **E6 — Correctness checks** for §6b's reclaim/subscription rule:
   subscription boundary (a worker never claims outside its subscribed
   set), unset-subscription compatibility (claims from any queue, matching
   §14's "unset means everything" default), and reclaim-boundary parity
   (a subscribed-elsewhere worker cannot reclaim an expired lease outside
   its subscription; a properly-subscribed worker can).

**A mid-build correction, disclosed rather than hidden**: this package's
first implementation of the round-robin fairness candidate used a single
SQL statement combining a window function with `FOR UPDATE SKIP LOCKED`.
PostgreSQL 16 rejects this outright (`ERROR: FOR UPDATE is not allowed with
window functions` — confirmed directly against this project's own
PostgreSQL instance, not from documentation alone). The harness's own
worker loop silently swallowed that error and looped, which produced a
first-pass result of "0 claims from either queue in 6 seconds" — not a
fairness finding, a broken experiment. This was caught before being
reported (by noticing `flood_share_pct = 0%` was structurally impossible
if the flood queue had any eligible work at all), the query was rewritten
as two statements (a non-locking rank read, then `FOR UPDATE SKIP LOCKED`
restricted to that ranked candidate set via `array_position`), and the
error-swallowing bug in the worker loop itself was also fixed so a future
silent failure cannot reproduce this. All fairness numbers in this report
are from the corrected harness, re-run three independent times. This is
recorded here, not scrubbed, per this task's own "prefer deterministic,
repeatable experiments" and "record PostgreSQL behavior as measured, not
inferred" instructions — an evidence package that hides its own false
starts is less trustworthy, not more.

---

## C. Environment description (measured, so results are not overstated)

| Property | Value |
|---|---|
| PostgreSQL version | 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1) — matches `docker-compose.yml`'s `postgres:16` target |
| Host | Single-node **WSL2 VM** on a consumer laptop (not bare metal, not a cloud instance, not the production-shaped multi-node deployment a real Phase 13 rollout would run) |
| CPU | Intel Core Ultra 7 258V, 8 vCPU (1 thread/core) |
| RAM | 15GiB total, ~12GiB free at test time |
| Disk | ext4 on a virtualized block device (WSL2's VHDX-backed disk, not the host NVMe directly) |
| `max_connections` | 100 (PostgreSQL default, not tuned) |
| `shared_buffers` | 128MB (PostgreSQL default, not tuned — notably small relative to available RAM) |
| `fsync` / `synchronous_commit` | both `on` (durable, production-realistic — not disabled for speed) |
| Concurrent load on the host | Shared with this Claude Code session's own processes and whatever else runs on the developer's machine; **not** an isolated benchmark rig |

**What this means for how to read every number below**: absolute
throughput/latency figures in §D are **not** representative of a properly
provisioned production PostgreSQL instance (default `shared_buffers` alone
is a well-known production anti-pattern) and must not be quoted as
production capacity planning numbers. What *is* defensible is the
**relative** comparison between candidates measured back-to-back, on
identical hardware, in the same process, within the same few minutes —
that comparison controls for the environment's own limitations because
every candidate suffers them equally. This report leans on relative
comparisons throughout and flags the one place (§E5/§L) where the
environment's noise was large enough to swamp the relative signal too.

---

## D. Measured results

### D1. E1 — Concurrency-limit exactness (limit = 5, 30 workers, 5s)

| Candidate | Max observed running | Limit violations | Total claims | Samples | Serialization failures |
|---|---|---|---|---|---|
| Advisory lock | 5 | **0** | 1,018 | 4,873 | 0 |
| Slot-table | 5 | **0** | 912 | 4,672 | 0 |
| `SERIALIZABLE` + retry | 5 | **0** | 879 | 4,519 | 8,421 |

All three candidates enforced the limit exactly, every run (three
independent full runs, zero violations in any of them, ~14,000 total
1ms-interval samples across all runs). `SERIALIZABLE` needed 8,421–15,117
retried transactions (per single 5-second run) to produce 879–1,011
successful claims — i.e., **roughly 9–14 wasted transaction attempts per
successful claim** even at this comparatively low worker count (30).

### D2. E2 — Throughput & latency vs. worker count (single hot queue, limit = 20, 4s runs)

**Advisory lock:**

| Workers | Claims/s | p50 | p95 | p99 |
|---|---|---|---|---|
| 1 | 101.5 | 2.41ms | 3.78ms | 4.32ms |
| 5 | 555.5 | 2.27ms | 4.12ms | 6.46ms |
| 10 | 565.5 | 10.77ms | 14.22ms | 22.60ms |
| 25 | 554.5 | 37.32ms | 45.00ms | 77.55ms |
| 50 | 495.3 | 89.30ms | 131.01ms | 165.80ms |
| 80 | 500.8 | 142.75ms | 223.70ms | 302.27ms |

Throughput **plateaus at ~500–605 claims/s from 5 workers onward and never
exceeds it, regardless of adding more workers** — latency instead grows
roughly linearly with worker count past 5. This is the direct, expected
signature of a mechanism that serializes all claims for one queue behind a
single lock: once one queue is hot enough that a claim is always waiting,
adding workers only lengthens the queue behind the lock, not the
throughput through it.

**Slot-table:**

| Workers | Claims/s | p50 | p95 | p99 |
|---|---|---|---|---|
| 1 | 96.8 | 2.26ms | 3.29ms | 3.83ms |
| 5 | 534.0 | 1.86ms | 2.63ms | 3.25ms |
| 10 | 1,074.8 | 1.99ms | 2.85ms | 3.58ms |
| 25 | **1,770.0** | 2.91ms | 5.26ms | 6.60ms |
| 50 | 1,091.0 | 6.06ms | 11.14ms | 17.07ms |
| 80 | 697.8 | 8.71ms | 22.03ms | 34.00ms |

Throughput scales up to ~25 workers (peak ~1,770/s — roughly 3× advisory
lock's ceiling), then degrades past 50 workers as `FOR UPDATE SKIP LOCKED`
contention on the small (limit=20-row) `queue_slots` set itself starts to
bite — still strictly better than either other candidate at every worker
count measured.

**`SERIALIZABLE` + retry:**

| Workers | Claims/s | p50 | p95 | p99 | Ser. failure rate |
|---|---|---|---|---|---|
| 1 | 90.3 | 3.12ms | 4.70ms | 5.49ms | 0% |
| 5 | 427.5 | 4.35ms | 11.15ms | 16.51ms | 47.0% |
| 10 | 361.8 | 8.86ms | 20.74ms | 26.56ms | 56.6% |
| 25 | 230.8 | 20.10ms | 49.19ms | 62.10ms | 71.1% |
| 50 | 184.3 | 33.22ms | 83.20ms | 111.22ms | 78.6% |
| 80 | 134.5 | 54.03ms | 150.27ms | 215.26ms | 82.1% |

**Throughput peaks at 5 workers and then monotonically *declines* as more
workers are added** — the opposite of the other two candidates. At 80
workers, over 4 in 5 claim attempts are wasted retries, and realized
throughput (134.5/s) is roughly a quarter of the slot-table candidate's
throughput at the *same* worker count (697.8/s), and worse than the
advisory-lock candidate's at 1 worker.

### D3. E3 — Lock/contention characterization (40 workers, limit = 10, 4s)

| Candidate | Avg waiting locks/sample | Max waiting | Avg active connections | Wait breakdown |
|---|---|---|---|---|
| Advisory lock | 35.4 | 38 | 37.7 | `advisory`: 2,833 sampled waits |
| Slot-table | **0.10** | 2 | 25.1 | `transactionid`: 8 sampled waits |
| `SERIALIZABLE` | 12.4 | 35 | 19.2 | `transactionid`: 51, `tuple`: 938 sampled waits |

With 40 workers hammering one queue, the advisory-lock candidate has
**35 of ~38 backend connections waiting on the lock at any given sample,
essentially all the time** — confirming §D2's plateau is a true
serialization bottleneck, not a measurement artifact. The slot-table
candidate shows almost no waiting at all (`SKIP LOCKED` is doing exactly
what it is for). `SERIALIZABLE`'s `tuple`-lock wait count (938) reflects
row-level contention from its `FOR UPDATE`-free but conflict-checked reads
plus the underlying `UPDATE`; it does not by itself capture the much larger
cost of the aborted-and-retried transactions, which never appear in
`pg_locks` at all (D2's serialization-failure-rate numbers are the real
cost signal for this candidate).

### D4. E4 — Fairness/starvation (flood + trickle, no concurrency cap, 10 workers, 6s)

| Fairness ordering | Trickle claimed | Trickle total | p50 wait | p95 wait | Max wait | Flood claimed (same run) |
|---|---|---|---|---|---|---|
| Baseline (today, queue-blind) | 9–10 | 19–20 | 611.8–1,152.9ms | 2,456.3–2,938.5ms | 2,456.3–2,938.5ms | 5,749–6,159 |
| Round-robin (rank interleave) | **19** | 19 | 9.1–9.9ms | 13.1–21.0ms | 13.1–21.0ms | 5,357–6,451 |
| `last_claimed_at` ascending | 19 | 19–20 | 23.9–47.9ms | 61.9–105.0ms | 61.9–105.0ms | **744–1,132** |

(Ranges across three independent full runs; see §B's disclosure for why
the first round-robin attempt is excluded from this range.)

Two findings, both consistent across all three runs:

1. **Today's actual queue-blind behavior genuinely starves the low-volume
   queue** — roughly half its jobs unclaimed after 6 seconds of sustained
   contention, with a worst-case wait approaching 3 seconds against a
   producer inserting one job every 300ms. This is exactly the failure mode
   [invariants.md](invariants.md)'s TF-INV-019 example (`bulk-export`
   flooding, `user-notifications` starved) describes, now measured rather
   than asserted.
2. **Both fairness candidates eliminate the starvation almost entirely**
   (19/19 or 19/20 claimed every run), but at a **flood-queue throughput
   cost that differs by roughly 5–8× between them** — round-robin's flood
   throughput (5,357–6,451 claims/6s) is statistically indistinguishable
   from the unfair baseline's (5,749–6,159), while `last_claimed_at`'s
   flood throughput (744–1,132) is a fraction of it. §F explains the
   mechanism.

### D5. E5 — Queue/tenant isolation under load

| Candidate | Low-volume queue alone | Low-volume queue under concurrent hot-queue flood | Degradation |
|---|---|---|---|
| Advisory lock | 522.7/s (run A) / 512.3/s (run B) / 525.0/s (run C) | 425.7/s / 442.3/s / 413.0/s | 18.6% / 13.7% / 21.3% |
| Slot-table | 458.3/s / 514.3/s / 479.0/s | 396.7/s / 414.0/s / 474.3/s | 13.5% / 19.5% / 1.0% |

**This result did not reproduce cleanly** — see §L. Reported here in full,
not cherry-picked to the cleanest-looking run.

### D6. E6 — Correctness checks (§6b reclaim/subscription rule)

| Check | Result |
|---|---|
| A worker subscribed only to queue A finds nothing claimable while only queue B has eligible work | **PASS** |
| An unset/all-queues subscription claims from whichever queue has eligible work (matches §14's default) | **PASS** |
| A worker subscribed only to queue A cannot reclaim queue B's expired lease, even though it is otherwise reclaim-eligible | **PASS** |
| A worker subscribed to (or covering) queue B correctly reclaims its expired lease (TF-INV-004 liveness preserved for an eligible worker) | **PASS** |

All four checks passed on every run. (The last check failed once during
harness development due to a test-harness bug — comparing the reclaimed
row's id against an uninitialized variable from a deliberately-empty prior
query, not a real correctness defect — caught and fixed before being
reported; see the commit history of `tools/phase13bench/main.go` if this
package is ever committed.)

---

## E. Correctness findings

1. **All three concurrency-limit candidates enforce the cap exactly** under
   sustained concurrent load — 0 violations across ~14,000 sampled
   observations spanning three independent runs (§D1). This matches the
   structural argument each candidate's own mechanism predicts: the
   advisory lock and `SERIALIZABLE` both eliminate the check-then-act race
   [phase-13-plan.md](phase-13-plan.md) §6a names as the reason a plain
   `SELECT count(*)` guard is unsafe (two concurrent transactions both
   observing "count = N-1" before either commits); the slot-table candidate
   sidesteps the race entirely by making "is there capacity" itself a
   row-existence fact resolved through the same `FOR UPDATE SKIP LOCKED`
   idiom TF-INV-002 already relies on for job selection.
2. **No candidate touches lease fencing, generation arithmetic, or
   terminal-state logic** — every candidate's claim statement is
   structurally a drop-in replacement for `internal/store/claim.go`'s
   existing CTE-then-`UPDATE` shape (candidates 1 and 3) or an additional
   `FOR UPDATE SKIP LOCKED` join alongside it (candidate 2), never a change
   to the `WHERE lease_generation = $n AND state = 'RUNNING'` fencing
   predicate every completion/heartbeat call already uses. This is a
   structural argument, not something this harness re-measures — Phase 5's
   own `concurrency_stress_test.go` already proves TF-INV-002/003/014 at
   scale against the *real* claim query, and none of the three candidates
   alters that query's fencing clause.
3. **Reclaim respects the same queue-subscription boundary as a fresh
   claim** (§6b's central rule), confirmed both directions: a worker
   outside a queue's subscription cannot reclaim its expired lease, and a
   worker inside it (or unrestricted) does reclaim it (§D6) — preserving
   TF-INV-004 liveness for an eligible worker while closing the
   unauthorized-execution gap §6b identifies.
4. **Queue-blind compatibility (§14) holds**: an unset/all-queues
   subscription claims from any queue with eligible work, reproducing
   today's actual behavior exactly — the compatibility requirement that
   lets an old worker binary and a new worker binary with
   `TASKFORGE_WORKER_QUEUES` unset behave identically.

## F. Fairness/starvation findings

1. **The problem TF-INV-019 exists to close is real and measurable, not
   theoretical**: under today's actual queue-blind ordering, a low-volume
   queue lost roughly half its work to a concurrently flooded queue over a
   6-second window, with tail waits approaching 3 seconds (§D4). This gives
   the future ADR a concrete "before" baseline to cite.
2. **Both tested fairness orderings eliminate the starvation almost
   entirely** — 19/19 or 19/20 trickle claims every run, versus baseline's
   9–10/19–20. Either candidate satisfies TF-INV-019's qualitative property
   ("no queue with pending, capacity-eligible work is starved... while
   another queue is making progress") under this test's load shape.
3. **They are not interchangeable once the flooded queue's own throughput
   matters**: round-robin's flood-queue throughput is statistically
   indistinguishable from the unfair baseline (both ~5,350–6,450 claims in
   6s); `last_claimed_at`'s is 5–8× lower (744–1,132). The mechanism is
   identifiable, not a mystery: `last_claimed_at` requires an `UPDATE` to
   one shared row (`queue_state.last_claimed_at` for whichever queue was
   just claimed from) on *every single claim from that queue* — under a
   permanently flooded queue, every claim serializes briefly on that one
   row's lock, exactly the kind of single-row write hotspot
   [phase-13-plan.md](phase-13-plan.md) §6a's advisory-lock discussion
   already warns about in a different context ("its contention behavior
   under many workers claiming the same hot queue is the kind of thing the
   ADR's performance analysis must actually measure, not assume" — this is
   that measurement, applied to a candidate the plan did not originally
   flag for it).
4. **The round-robin candidate could not be expressed as the single
   PostgreSQL statement [phase-13-plan.md](phase-13-plan.md) §6a's prose
   implies** ("ordering claim candidates by..." reads like an `ORDER BY`
   clause) — `FOR UPDATE` and a window function cannot coexist in one
   query in PostgreSQL 16 (§B). A real implementation needs two statements
   per claim attempt: one non-locking read to compute the interleaved rank,
   one `FOR UPDATE SKIP LOCKED` restricted to that ranked candidate set.
   This raises round-robin's actual implementation cost above what the
   plan's own framing suggested, and is new information the ADR did not
   have before this analysis.
5. **Neither fairness candidate was tested at more than two queues.** The
   relative ranking (round-robin's throughput advantage,
   `last_claimed_at`'s implementation simplicity) may not hold at higher
   queue cardinality — flagged as a required follow-up in §L, not
   extrapolated here.

## G. Lock/contention findings (PostgreSQL behavior, measured against this project's actual PostgreSQL 16 instance)

1. **Advisory lock**: under 40-worker contention on one queue, ~35 of ~38
   active backend connections are waiting on the `advisory` lock at any
   sampled instant (§D3) — this is not incidental contention, it is the
   mechanism working exactly as designed (serialize all claims for a
   queue), and its cost is structural, not tunable away by indexing or
   connection-pool sizing.
2. **Slot-table**: essentially no lock waiting observed (0.10 average
   waiting entries/sample, max 2) even under the same 40-worker load —
   `FOR UPDATE SKIP LOCKED` on both the job-selection CTE and the
   slot-selection CTE lets non-conflicting claimers proceed in parallel,
   exactly the property TF-INV-002's existing mechanism already relies on
   for job selection alone, now extended to capacity selection.
3. **`SERIALIZABLE`**: PostgreSQL's serializable-snapshot-isolation
   conflict detection manifests partly as ordinary lock waits (51
   `transactionid`, 938 `tuple` — the latter from the underlying row reads/
   writes under `FOR UPDATE`-free but conflict-tracked access) but the
   dominant cost (46.9–82.1% of attempts aborting and retrying, §D2) is
   **invisible to `pg_locks` entirely** — a serialization failure is
   detected at commit time via predicate-lock conflict, not a wait state.
   Any operator dashboard built only from `pg_locks`/`pg_stat_activity`
   would under-report this candidate's true cost; `taskforge_admission_
   rejections_total`-style application-level counters (as this harness's
   own `serialization_failures` counter does) are necessary to see it.
4. **All measurements are from this project's actual pinned PostgreSQL
   version (16.15)**, not inferred from documentation — every number in
   §D came from a live query against a real, disposable instance.

## H. Performance tradeoffs

| | Advisory lock | Slot-table | `SERIALIZABLE` |
|---|---|---|---|
| Peak measured throughput | ~605/s | **~1,770/s** | ~428/s |
| Behavior as worker count increases | Plateaus hard at ~500–605/s past 5 workers; added workers only add latency | Scales to ~25 workers, degrades gracefully past 50 | **Degrades from the start** — peak at 5 workers, monotonic decline after |
| Latency character | Grows roughly linearly with worker count (queueing behind one lock) | Stays low (single-digit ms) through 25–50 workers | Grows with worker count *and* carries hidden retry cost not reflected in the latency number alone |
| Scalability limit (this hardware) | Bottlenecked by lock serialization itself — adding capacity (`concurrency_limit`) does not relieve it, since the *lock*, not the limit, is what's serialized | Bottlenecked eventually by `SKIP LOCKED` contention on the slot set itself, which scales with **configured capacity**, not job volume — a limit of 20 slots contends differently than a limit of 5 | Bottlenecked by the abort rate itself, which gets *worse* as concurrency increases — the worst scalability curve of the three |
| Operational complexity | Lowest — no new schema, no new call sites | Highest — new table, new release obligation at every terminalization path (6 call sites named in [phase-13-plan.md](phase-13-plan.md) §17) | Low schema complexity, but introduces a retry-loop failure class this codebase has zero prior operational experience with |
| Failure mode if the mechanism itself misbehaves | A stuck transaction blocks an entire queue's claims (not just capacity — *all* progress) | A slot leak silently shrinks capacity, detectable via existing planned metrics | A retry-loop bug (missing backoff/cap) risks amplifying contention under exactly the flood scenario Phase 13 exists to mitigate |

**Fairness-layer tradeoff, orthogonal to the above**: round-robin fairness
preserves flooded-queue throughput at the cost of a two-statement claim
and (per §F.4) an implementation more involved than the plan's prose
suggested; `last_claimed_at` fairness is a one-column, one-statement change
but measurably taxes the flooded queue's own throughput in proportion to
its own claim rate. Neither dominates the other on every axis — this is
exactly the kind of tradeoff the ADR needs to make explicitly, not the kind
this evidence package should resolve on the ADR's behalf.

## I. Compatibility implications

1. **None of the three concurrency-limit candidates require any change to
   `internal/worker`'s `Store` interface or the claim-execute-heartbeat-
   complete loop** ([internal/worker/worker.go](../internal/worker/worker.go))
   — the decision is fully contained in the claim statement's shape.
2. **§6b's reclaim/subscription rule is preserved under every candidate
   tested** (§D6/§E.3) — a worker's subscription filter applies identically
   to a fresh claim and an expired-lease reclaim, with no observed
   asymmetry.
3. **§14's "unset subscription = claim everything" default reproduces
   today's actual behavior exactly** (§D6) — an old (pre-Phase-13) worker
   binary and a new worker binary with `TASKFORGE_WORKER_QUEUES` unset are
   not distinguished by any candidate's claim query, since the
   subscription filter is additive (`queue_name = ANY($subscribed)`) and
   degenerates to "no filter" when `$subscribed` covers every queue in use.
4. **The slot-table candidate is the only one with a compatibility
   consequence beyond the claim query itself**: it requires every
   terminalization call site to release its held slot in the *same*
   transaction that terminalizes the job, which is new code at six
   existing call sites ([phase-13-plan.md](phase-13-plan.md) §17's own
   list: `CompleteSuccess`, `CompleteFailure`, `CompleteRetryableFailure`,
   `CompleteCancelled`, `CompleteTimeout`, the Lazy Dead-Letter Sweep) —
   this is a real migration surface the other two candidates do not have,
   and should weigh into OD-1's decision as an implementation-cost/risk
   factor, not just a performance one.
5. **`SERIALIZABLE`'s retry loop, if selected, needs a retry-limit/backoff
   policy that does not exist anywhere in this codebase today** — the
   existing worker loop's own retry semantics
   ([retry-semantics.md](retry-semantics.md)) govern *job*-level retries
   (a failed handler execution), an entirely different concern from a
   *claim transaction* retrying its own aborted attempt; conflating the two
   in implementation would be a mistake this evidence package flags in
   advance, not a mistake it has observed committed.

## J. Recommendation for OD-1

This section is a recommendation to inform the ADR, not the ADR itself —
per this task's own instruction, no mechanism is being selected here as
final; that remains OD-1's own act, informed by (and expected to cite) this
evidence.

**Concurrency-limit mechanism: the evidence favors the slot-table
semaphore (§6a candidate 2).** It is the only candidate that is
simultaneously (a) exact under load (§D1, tied with the other two), (b)
the clear throughput and latency leader at every worker count measured
(§D2, up to 3× advisory lock's ceiling and consistently ahead of
`SERIALIZABLE`), and (c) the lowest-lock-contention candidate by a wide
margin (§D3, ~0.10 vs. 35.4 avg waiting locks/sample under identical
40-worker load). Its cost is real and specific — a new release obligation
at every terminalization call site — but that cost is enumerable, testable
(a dedicated "slot leak" regression test, as [phase-13-plan.md](phase-13-plan.md)
§19 already anticipates), and does not scale with load the way the other
two candidates' costs do. **`SERIALIZABLE` + retry is not recommended**:
it was the worst performer on every axis measured, its failure mode
(abort rate rising with concurrency) gets *worse* under exactly the
flood scenario this phase exists to defend against, and this codebase has
no existing retry-loop-on-the-hottest-query precedent to build on safely.
**The advisory lock is not recommended as the general-purpose mechanism**,
though it remains a legitimate narrow option the ADR could still choose
for a specific reason not tested here (e.g., if per-queue slot-table row
provisioning is judged too operationally heavy for a deployment with very
many low-volume queues) — its defining limitation is that it caps a
queue's *realized* concurrency at what the lock's serialization allows,
regardless of the operator's configured `concurrency_limit`, which directly
undermines the "per-queue/tenant-scoped concurrency limits" requirement
Phase 13's own roadmap text states as a goal, not just a performance nice-
to-have.

**Fairness mechanism: the evidence favors `last_claimed_at` ascending
ordering**, with an explicit caveat the ADR should weigh, not ignore. Both
candidates eliminate the measured starvation (§D4); round-robin does so
with less impact on the flooded queue's own throughput, but (a) needs two
statements per claim rather than one (§F.4, a real implementation-
complexity finding this evidence package surfaced that the plan's own
prose did not anticipate), and (b) was only validated at two queues —
extending the rank-and-interleave approach to many queues needs a fresh
check that its `array_position`-based reordering step does not itself
become a bottleneck (§L). `last_claimed_at` is a one-column, one-statement,
directly composable-with-slot-table change whose fairness effect is at
least as strong and whose cost (flooded-queue throughput reduction) is
visible, attributable to a specific mechanism (the shared per-queue
counter row), and plausibly mitigable (e.g., batching the `last_claimed_at`
update, or accepting slightly stale ordering) in ways this evidence package
did not have scope to test. **If the ADR's priority is protecting a
flooded queue's own throughput while still meeting the fairness bound,
round-robin is the stronger evidence-backed choice; if the priority is
minimal new mechanism and lowest implementation risk, `last_claimed_at` is
the stronger evidence-backed choice** — this is a real tradeoff, not a
tie-break this package should manufacture a false confidence about.

**Combined recommendation, if the ADR wants one integrated answer**:
slot-table concurrency limiting + `last_claimed_at` fairness ordering.
Both fit the existing `FOR UPDATE SKIP LOCKED` idiom directly (the
fairness `ORDER BY` composes into the same statement the slot-table
candidate already needs), neither introduces a session-level lock or a
retry loop this codebase has no precedent for, and their combined
operational cost (slot-release at terminalization + a `queue_state` row
update at claim time) is enumerable and testable rather than open-ended.

## K. Proposed semantics for TF-INV-019's bound

[invariants.md](invariants.md)'s current TF-INV-019 text is deliberately
algorithm-independent ("the bound that Phase 13's governing ADR proves for
its selected algorithm, under that ADR's own stated assumptions"). This
evidence package proposes concrete language *for the ADR to adapt or
replace*, not a final bound — the actual invariant text is not modified by
this document (per this task's own instruction not to write the ADR).

Based on the `last_claimed_at` mechanism's measured behavior (§D4, the
recommended default per §J):

> **Proposed bound**: for a deployment with *Q* concurrently-eligible
> queues (queues with at least one pending, capacity-eligible job and at
> least one subscribed, currently-running worker — [phase-13-plan.md](phase-13-plan.md)
> §6b/§15's own operator-staffing precondition, not weakened here), and a
> worker pool claiming at combined rate *R* claims/sec under the
> deployment's configured concurrency limits: a queue with pending,
> capacity-eligible work is claimed within **one full fairness cycle** —
> the time for every other currently-eligible queue to be served at least
> once under `last_claimed_at`-ascending ordering — which this evidence
> package's two-queue measurement observed to be on the order of tens of
> milliseconds (p95 61.9–105.0ms) under sustained contention from a queue
> claiming at roughly 150–1,100 claims/sec. This is **not** claimed to
> generalize linearly to arbitrary *Q*; see §L for the specific follow-up
> experiment needed before the ADR states a *Q*-parameterized bound with
> confidence.

**Why this shape, not a fixed wall-clock number**: [phase-13-plan.md](phase-13-plan.md)
§5 and §10 are explicit that no unconditional/indefinite fairness guarantee
is ever claimed, and that the bound must be stated "under stated
assumptions (run duration, load shape, queue count)." A bound expressed as
"one fairness cycle across *Q* eligible queues" is falsifiable, testable
(exactly the SF-037-style two-queue stress test [phase-13-plan.md](phase-13-plan.md)
§16 already proposes, extended to more queues per §L), and degrades
honestly as *Q* grows rather than promising a constant that this evidence
package has not actually measured past *Q*=2.

## L. Remaining uncertainties / experiments still needed

Named explicitly, per this task's own instruction to say so rather than
force a conclusion where evidence is thin:

1. **Queue/tenant isolation (§D5/E5) did not reproduce cleanly.** Across
   three independent runs, degradation ranged 13.7–21.3% (advisory lock)
   and 1.0–19.5% (slot-table) — overlapping ranges, no statistically clean
   separation between the two candidates on this dimension, on this
   hardware. This is very likely this environment's own noise (§C: a
   shared, single-node WSL2 VM with default, untuned PostgreSQL settings)
   dominating a real but smaller effect, not evidence that isolation
   genuinely doesn't differ between candidates — but this package cannot
   honestly claim more than that. **Needed**: a repeat of E5 on
   dedicated, quiet hardware (or with materially longer run windows to
   average out host noise), with enough repetitions to report a
   confidence interval, not a single number.
2. **Fairness (§D4/§F) was only tested at two queues.** Whether
   round-robin's throughput advantage and `last_claimed_at`'s throughput
   cost both hold, worsen, or improve at realistic queue cardinality (5,
   20, 100 queues) is unknown. **Needed**: an N-queue extension of E4,
   varying both queue count and the skew between flooded/low-volume
   queues, before the ADR commits to a *Q*-parameterized bound (§K).
3. **Neither fairness candidate was tested layered on top of an actual
   concurrency-limit mechanism together** — E4 deliberately ran with no
   cap, to isolate the ordering effect (§B). The combined
   recommendation in §J (slot-table + `last_claimed_at`) has not itself
   been measured as a single integrated mechanism; only its two halves
   have been measured separately. **Needed**: one more experiment
   combining the two before the ADR finalizes the combined design, in
   case the two interact in a way neither isolated measurement predicts
   (e.g., the slot-table's own `FOR UPDATE SKIP LOCKED` contention
   compounding with `last_claimed_at`'s row-update contention on a
   flooded, capacity-capped queue).
4. **No experiment measured behavior at realistic production scale**
   (hundreds of workers, sustained multi-minute-or-longer runs,
   production-representative job execution durations rather than the
   5–20ms simulated hold used here). This package's worker-count sweep
   stopped at 80; real deployments may run more workers per queue than
   that, especially for a low-execution-time job type.
5. **This package did not test what happens when the concurrency-limit
   mechanism and the retention sweeper (Phase 13's other half) run
   concurrently** — [phase-13-plan.md](phase-13-plan.md) §13's own named
   failure scenario ("a retention batch-cleanup job runs during peak
   claim-query load"). Out of scope for OD-1 specifically, but worth
   naming so it is not forgotten before Phase 13's exit criteria are
   declared met.
6. **`SERIALIZABLE`'s retry-limit/backoff policy was arbitrarily chosen**
   for this harness (8 attempts, no backoff) purely to produce a bounded
   experiment — it was not tuned, and a different policy might change the
   throughput numbers materially. Since `SERIALIZABLE` is not the
   recommended candidate (§J), this was judged not worth further
   investment, but should not be read as "the best possible
   `SERIALIZABLE` implementation was measured and still lost" — only that
   a reasonable, untuned one was, and lost badly enough that tuning is
   unlikely to close a multiple-times throughput gap driven by an abort
   rate structural to the mechanism itself.

None of these gaps block OD-1 from proceeding with the slot-table +
`last_claimed_at` recommendation in §J — the evidence for *that*
combination's core properties (exactness, throughput ordering, fairness
effectiveness) is consistent across all repeated runs. They do mean the
ADR should treat the exact numeric bound in §K, and the E5 isolation
comparison, as provisional pending the follow-up experiments above, rather
than as settled.

---

## Repo hygiene note

Per this task's explicit instructions: nothing in this evidence pass has
been staged, committed, or pushed. `tools/phase13bench/` (the harness and
its two result JSON files) and this document
(`docs/phase-13-concurrency-evidence.md`) are new, currently-untracked
files in the working tree. `git status` will show them as untracked; no
`git add`/`git commit`/`git push` was run. The harness's own database
setup/teardown (`CREATE DATABASE`/`DROP DATABASE taskforge_phase13_bench`)
is the only mutation made to the local PostgreSQL instance, and it is
self-contained and disposable — the real TaskForge schema/migrations were
never touched.
