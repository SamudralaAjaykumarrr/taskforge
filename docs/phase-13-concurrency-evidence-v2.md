# Phase 13 OD-1 — Concurrency/Fairness Evidence Package v2 (Corrected)

Status: **ANALYSIS + EVIDENCE ONLY, SUPERSEDES v1.** No production code
changed. No mechanism finally selected. No ADR written. This document
replaces [phase-13-concurrency-evidence.md](phase-13-concurrency-evidence.md)
("v1") as the evidence OD-1's mandatory ADR should draw on, following the
independent review at
[phase-13-concurrency-evidence-review.md](phase-13-concurrency-evidence-review.md)
("the review"), which returned a REVISE EVIDENCE verdict. v1 and the review
are both left in place, unedited, as the historical record of what was
measured first and what was found wrong with it — this document does not
retroactively rewrite either.

**How to read this document against the other two**: every section below
that changed from v1 says so explicitly, citing the review finding that
forced the change (H1, H2, M1, M2, M3). Sections that didn't change carry
forward v1's original conclusion, re-verified against the corrected harness
rather than merely copied.

Companion artifacts:

- `tools/phase13bench/main.go`, `claim_concurrency.go`, `claim_fairness.go`,
  `experiments.go` — the corrected v2 harness. Still analysis-only, still
  excluded from `go build ./...`/`go test ./...`, still untracked/
  uncommitted.
- `tools/phase13bench/v1/` — the original v1 harness and its two raw result
  files, frozen and unmodified, kept for provenance.
- `tools/phase13bench/results-v2.json` — the raw output this document's
  tables are drawn from.

Reproduce with: `go run ./tools/phase13bench` (same connection defaults as
v1; pass `-quick` for a fast smoke test, not for reported evidence; pass
`-only=<name,...>` to run a subset — see `main.go`'s experiment names).

---

## A. Corrected methodology

Six defects the review identified, and how each was actually fixed —
verified via `EXPLAIN (ANALYZE, BUFFERS)` against this project's own
PostgreSQL 16 instance before being accepted, not assumed correct because
the code compiled and ran.

### A1. H1 — `last_claimed_at`'s cross-table sort, replaced with a per-queue indexed lookup

v1's fairness queries ordered by a column on the *joined* `queue_state`
table, which PostgreSQL cannot satisfy from any index on `bench_jobs` —
confirmed via `EXPLAIN`: a full sequential scan plus an in-memory (or, at
larger backlog, disk-spilling external merge) sort, on every claim attempt.
v2 replaces this with a two-step pattern: step 1 is a **non-locking** read
that fetches each subscribed queue's own single best candidate via a
`LATERAL` subquery restricted to that one queue (so PostgreSQL can use the
per-queue partial index directly, costing O(subscribed queues) instead of
O(backlog)), then picks the winner across that small candidate set; step 2
is a **targeted, locking** claim against that one specific row id, using
`FOR UPDATE SKIP LOCKED` so a concurrent claimer racing for the same row is
skipped, not blocked. This is `claim_fairness.go`'s `perQueueCandidate` +
`claimFairnessV2`.

### A2. H2 — the production baseline, rebuilt as a genuine reconstruction

v1's "baseline" ran the queue-blind ordering query against the **Phase-13
future** index shape (`queue_name`-prefixed), which cannot serve a
queue-blind `ORDER BY` and forced the same sequential-scan-and-sort
collapse — meaning v1's baseline number was measured against a strictly
*worse* index than production's real, current one. v2 adds a dedicated
`prod_jobs` table carrying **today's actual schema** (migration 0001's
exact columns and three indexes — `idx_prod_jobs_claimable` on
`(priority DESC, eligible_at ASC) WHERE state IN ('QUEUED','RETRY_WAIT')`,
`idx_prod_jobs_expired_lease`, `idx_prod_jobs_state`) and runs
`internal/store/claim.go`'s `claimQuery` **verbatim** (only `jobs` →
`prod_jobs`) against it. A harness-only `bench_logical_queue` column, never
referenced by the query's `WHERE`/`ORDER BY`, lets this experiment measure
per-"queue" fairness outcomes without production's real query ever seeing
or using a queue concept it doesn't have yet.

### A3. M1 — advisory-lock, both implementations kept and compared

v2 keeps v1's original 5-round-trip `claimAdvisoryNaive` (unchanged in
shape, now index-corrected) *and* adds `claimAdvisoryFast`, a 2-round-trip
variant folding the capacity check and candidate claim into one combined
statement, matching slot-table's round-trip count. Both are reported
separately in every table below, so the ADR can see the lock mechanism's
own cost independent of client/server chatter.

### A4. M2 — round-robin's `LIMIT 50`, removed

The `LATERAL`-per-queue rewrite (A1) makes the cap structurally
unnecessary — cost scales with the number of *subscribed* queues, not the
size of any backlog, so there is no longer a candidate-set size to cap.
Tested explicitly at 60 queues (§E) specifically because that exceeds v1's
old limit.

### A5. M3 — SERIALIZABLE's retry accounting, corrected

`claimSerializable`'s retry loop now returns the **actual count** of
attempted transactions and serialization failures for every call, summed
by the caller into `AttemptedTxns`/`SerializationFailures`/`Retries`/
`RetryExhausted` fields reported independently in every table — not a
single boolean collapsing an unknown number of retries into "0 or 1" the
way v1 did.

### A6. A defect the review did not name, found while building this pass: the missing `RUNNING`-count index

Midway through this pass, fixing H1/H2 exposed a **new, distinct** indexing
gap the review never flagged (because v1 never reached this code path
cleanly): the concurrency-limit candidates' own capacity check —
`SELECT count(*) FROM bench_jobs WHERE queue_name = $1 AND state =
'RUNNING'` — does not use `idx_bench_claim_or_reclaim` (a `state IN
('QUEUED','RUNNING')` partial index) even though `state='RUNNING'` is
logically a subset of that condition; PostgreSQL's partial-index matching
did not recognize it for this query shape, and fell back to a full
(parallel) sequential scan — confirmed via `EXPLAIN`, ~13ms per call at
300,000 rows, incurred on nearly every claim attempt whose candidate turned
out to be a fresh `QUEUED` row (the common case). This alone collapsed
advisory-lock's and `SERIALIZABLE`'s throughput by roughly an order of
magnitude, uniformly across every worker count, and was fixed with one more
dedicated partial index, `idx_bench_running_count ON bench_jobs
(queue_name) WHERE state = 'RUNNING'`.

A second, related defect surfaced at the same time: **freshly seeded
tables carried no query-planner statistics**, since this harness never
called `ANALYZE` after a bulk seed. Without it, PostgreSQL's plan choices
for a just-created 300,000-row table were inconsistent from run to run
(observed directly: the identical query plan that used the intended index
in an isolated, explicitly-`ANALYZE`d psql session instead chose a
sequential scan inside the actual harness, and recovered unpredictably —
once, at exactly one worker count out of six, before the fix). `seedQueue`
now runs `ANALYZE bench_jobs, bench_queue_slots, bench_queue_state`
immediately after every seed.

This document's disclosure norm (established in v1, reaffirmed by the
review, continued here): a defect found and fixed mid-pass is reported
here, not silently corrected and left undocumented. Both of these were
caught by comparing an isolated micro-benchmark of a claim mechanism's
*own* SQL shape (raw, outside the harness) against the harness's full
measured throughput for the identical shape, and bisecting the gap when
the two didn't match — the same falsification discipline the review itself
used.

### A7. A design correction discovered while building the integrated (requirement 6) experiments: slot ownership across a reclaim

Neither v1 nor the review's H1/H2 findings addressed this, because v1
never combined concurrency limiting with reclaim. Building the "slot-table
+ fairness, together, with reclaim active" experiment (§F) surfaced a real
semantic question the original slot-table design (`phase-13-plan.md` §6a
candidate 2) left implicit: when an abandoned job's expired lease is
reclaimed, does it need to **acquire a new capacity slot**, or does it
**already hold** the one it was originally admitted under? The job never
stopped being "in flight" from a concurrency-accounting perspective — only
its owning worker crashed — so re-marking it `RUNNING` under a new attempt
is a continuation of the same concurrency unit, not a new admission.
`claimSlot` and `claimFairnessV2` (integrated mode) both now branch on
this explicitly: a fresh (`QUEUED`) claim atomically acquires a free slot;
a reclaim (`RUNNING`) never touches `bench_queue_slots` at all. This is a
**design clarification this evidence pass is surfacing for the ADR's own
slot-table design**, not merely a harness fix — a production implementation
of candidate 2 needs the same branch, or a reclaim will either wrongly
consume a second slot for the same job or spuriously fail for lack of one.

---

## B. Exact benchmark/harness changes (file-level summary)

| File | Change |
|---|---|
| `tools/phase13bench/v1/*` | Frozen, unmodified — the original harness and its two raw result files, kept for historical comparison. |
| `tools/phase13bench/main.go` | New schema: `idx_bench_claim_or_reclaim` (replaces v1's two single-state indexes), `idx_bench_running_count` (A6), `prod_jobs` + its verbatim migration-0001 indexes (A2), `ANALYZE` after every seed (A6), staggered `eligible_at` seeding (carried over from the review's own mid-pass fix, re-verified here), experiment orchestration for 8 experiment groups (exactness, throughput, lock contention, production baseline, fairness, integrated, correctness, isolation), `-quick`/`-only` flags for fast iteration during this pass itself. |
| `tools/phase13bench/claim_concurrency.go` | All four concurrency candidates rewritten around `pickAndLockCandidateSQL` (one indexed, atomic `FOR UPDATE SKIP LOCKED` statement — not the two-step non-locking-pick pattern, which was tried and found to *reintroduce* a different collapse under single-queue contention, §A8 below), each now reclaim-aware (a `RUNNING` candidate skips the capacity gate and, for slot-table, skips slot acquisition entirely). `claimAdvisoryFast` added per M1. `claimSerializable` accounting corrected per M3. |
| `tools/phase13bench/claim_fairness.go` | `perQueueCandidate` (shared LATERAL-friendly indexed candidate SQL), `claimFairnessV2` (round-robin and `last_claimed_at`, both non-integrated and integrated/capacity-and-reclaim-aware per A7), `claimProdBaseline` (verbatim production query, A2). |
| `tools/phase13bench/experiments.go` | `runExactness` (now reclaim-exercising via deterministic in-run abandonment, not the invalid pre-seeded-above-the-limit state an early draft of this pass used and self-corrected), `runThroughput`, `runLockContention` (all four candidates), `runProdBaseline`, `runFairnessMultiQueue` (parameterized by queue shape and integrated/non-integrated, computing TF-INV-019 metrics — max/p50/p95/p99 wait, max simultaneously-stalled sparse queues observed, longest inter-claim gap per queue), `runIsolation` (more repetitions), `runCorrectnessChecks` (re-verified against the v2 schema, plus one new check for the production baseline's reclaim branch). |

### A8. A design path tried and rejected during this pass, disclosed rather than hidden

The natural-seeming fix for H2/A6 (an OR-across-two-partial-indexes
sequential scan) was the same two-step "non-locking pick, then targeted
lock" pattern already used for the fairness candidates (A1) — applied to
the single-queue concurrency candidates too. It was built, and it
**backfired**: with only one queue and no other queue for different
workers to naturally diverge to, every concurrent worker's non-locking
pick step computed the *identical* single best candidate, and then all of
them raced to lock that one row — collapsing `slot_table`'s measured
throughput from over 1,700/s (v1) to under 350/s and falling as worker
count rose, the opposite of the expected scaling curve. This was caught by
comparing against v1's own numbers for the same candidate, not assumed
correct because exactness still held (it did — the bug was a throughput
regression, not a correctness one). The fix actually used instead: a
**single combined partial index**, `idx_bench_claim_or_reclaim ON
bench_jobs (queue_name, priority DESC, eligible_at ASC) WHERE state IN
('QUEUED','RUNNING')`, lets one atomic `FOR UPDATE SKIP LOCKED` statement
cover both the fresh-claim and reclaim branches directly — preserving the
natural SKIP-LOCKED divergence property a single statement has (confirmed
via `EXPLAIN`: an ordinary index scan, not a sequential scan) instead of
splitting into two statements at all. The fairness candidates keep the
two-step split (A1) because there, with multiple queues, different workers
*do* have somewhere else to diverge to — the same design choice produces
opposite outcomes depending on how much diversity the candidate set
actually has, which is itself evidence worth the ADR's attention (§H).

---

## C. Environment description

Unchanged from v1 — same machine, same PostgreSQL 16.15 instance, same
caveats about absolute-vs-relative numbers. See
[phase-13-concurrency-evidence.md](phase-13-concurrency-evidence.md) §C for
the full table. Re-stated briefly: single-node WSL2 VM, Intel Core Ultra 7
258V, 8 vCPU, 15GiB RAM, PostgreSQL default `shared_buffers`/
`max_connections`, `fsync`/`synchronous_commit` both on, not an isolated
benchmark rig. Every number below should be read the same way v1's were:
relative comparisons between candidates measured back-to-back are
defensible; absolute throughput is not a production capacity-planning
number.

---

## D. Measured results

Full raw output: `tools/phase13bench/results-v2.json`. One run of the
entire suite (exactness, throughput, lock contention, production
baseline ×3, fairness ×12, integrated ×12, correctness, isolation ×10 —
90 sub-experiments total), immediately following the `ANALYZE`/indexing
fixes in §A, with no cherry-picking.

### D1. Concurrency-limit exactness (limit = 5, 30 workers, 6s, reclaim genuinely exercised)

| Candidate | Max running | Violations | Claims | Reclaims | Attempted txns | Ser. failures |
|---|---|---|---|---|---|---|
| Advisory lock (naive) | 5 | **0** | 50 | 20 | 5,516 | 0 |
| Advisory lock (optimized) | 5 | **0** | 50 | 20 | 6,823 | 0 |
| Slot-table | 5 | **0** | 49 | 20 | 59,042 | 0 |
| `SERIALIZABLE` | 5 | **0** | 50 | 20 | 134,250 | 968 |

All four enforce the limit exactly, now including a genuine mix of fresh
claims and reclaims (20 of ~50 claims each were reclaims of a
deterministically-abandoned job, not a pre-seeded, invalid starting
state — §A's disclosed self-correction). `SERIALIZABLE`'s corrected
counter (M3) shows 968 real serialization failures behind those 50
successful claims at just 30 workers — a number v1 could not have
reported at all.

### D2. Throughput & latency vs. worker count (single hot queue, limit = 20, 4s runs)

**Advisory lock (naive, 5 round trips):**

| Workers | Claims/s | p50 | p95 | p99 |
|---|---|---|---|---|
| 1 | 95.3 | 2.51ms | 4.43ms | 6.44ms |
| 5 | 584.3 | 1.80ms | 4.58ms | 7.32ms |
| 10 | 735.0 | 6.99ms | 11.20ms | 14.69ms |
| 25 | 705.0 | 27.65ms | 35.49ms | 56.98ms |
| 50 | 650.3 | 66.25ms | 100.07ms | 141.45ms |
| 80 | 630.8 | 108.45ms | 179.84ms | 259.86ms |

**Advisory lock (optimized, 2 round trips):**

| Workers | Claims/s | p50 | p95 | p99 |
|---|---|---|---|---|
| 1 | 85.8 | 2.71ms | 5.75ms | 7.13ms |
| 5 | 620.3 | 1.49ms | 2.86ms | 5.24ms |
| 10 | 839.0 | 5.07ms | 8.56ms | 12.77ms |
| 25 | 831.0 | 21.77ms | 31.99ms | 61.07ms |
| 50 | 835.3 | 50.26ms | 65.33ms | 100.93ms |
| 80 | 720.5 | 96.71ms | 123.54ms | 191.15ms |

Both plateau in the 600–840/s range and never exceed it — the same
serialization signature v1 found, now on a corrected baseline. The
optimized variant is consistently faster (as expected — less work inside
the same lock) but the *shape* of the curve (plateau, then latency growth)
is identical, confirming M1's finding: round-trip count shifts the
plateau's height, not its existence. The mechanism, not the
implementation, is what caps this candidate.

**Slot-table:**

| Workers | Claims/s | p50 | p95 | p99 |
|---|---|---|---|---|
| 1 | 65.5 | 4.38ms | 7.17ms | 9.27ms |
| 5 | 485.0 | 2.37ms | 3.59ms | 4.03ms |
| 10 | 959.0 | 2.43ms | 4.87ms | 7.80ms |
| 25 | **1,974.0** | 2.54ms | 4.61ms | 8.80ms |
| 50 | 1,443.8 | 5.09ms | 9.70ms | 16.27ms |
| 80 | 949.3 | 9.22ms | 19.82ms | 30.24ms |

Peaks at ~1,974/s (25 workers) — 2.4–2.8× either advisory-lock variant's
plateau — then degrades past 50 workers as `SKIP LOCKED` contention on the
20-row slot set itself starts to bite. Same qualitative shape as v1
(scales, peaks, gracefully degrades), now on a corrected, apples-to-apples
comparison.

**`SERIALIZABLE`:**

| Workers | Claims/s | p50 | p95 | p99 | Ser. failure rate* |
|---|---|---|---|---|---|
| 1 | 92.8 | 2.76ms | 4.70ms | 5.98ms | 0% |
| 5 | 447.5 | 3.89ms | 10.06ms | 14.31ms | 61.6% |
| 10 | 385.8 | 8.43ms | 18.98ms | 23.56ms | 87.2% |
| 25 | 300.3 | 14.43ms | 34.86ms | 49.98ms | 94.5% |
| 50 | 222.8 | 24.45ms | 64.28ms | 85.39ms | 96.1% |
| 80 | 166.8 | 43.12ms | 122.25ms | 176.90ms | 96.8% |

*Failure rate = `serialization_failures / attempted_txns`, computed from
the now-correct per-attempt counters (M3) — e.g. at 80 workers, 22,007 of
22,752 attempted transactions aborted to produce 166.8 successful
claims/s. **These percentages are the corrected version of the same
figures v1 reported as 47–82%** — the true rate is higher than v1 could
see, at every worker count, because v1's counter only recorded whether a
call needed *any* retry, not how many. Throughput still peaks low (1
worker) and falls monotonically as concurrency rises, the same
qualitative signature v1 found, now with an accurate cost figure behind
it.

### D3. Lock/contention characterization (40 workers, limit = 10, 4s)

| Candidate | Avg waiting locks | Max waiting | Avg active conns | Wait breakdown |
|---|---|---|---|---|
| Advisory lock (naive) | 35.3 | 38 | 37.6 | `advisory`: 2,792 |
| Advisory lock (optimized) | 34.2 | 37 | 36.9 | `advisory`: 2,701 |
| Slot-table | **0.18** | 4 | 25.2 | `transactionid`: 14 |
| `SERIALIZABLE` | 11.8 | 32 | 19.4 | `transactionid`: 40, `tuple`: 892 |

Unchanged conclusion from v1, now measured against the corrected schema:
advisory lock keeps ~35 of ~37 active connections waiting on the lock at
any sampled instant regardless of which variant; slot-table shows almost
none. The optimized advisory-lock variant shows essentially the same wait
profile as the naive one (34.2 vs. 35.3 avg waiting) — confirming again
that the *lock itself*, not round-trip count, is what produces the convoy.

### D4. Production-baseline fidelity (today's actual `claimQuery`, today's actual indexes, 8s runs)

| Run | Flood claimed | Trickle claimed | p50 wait | p95 wait | Max wait |
|---|---|---|---|---|---|
| 1 | 18,918 | 25/26 | 8.2ms | 256.6ms | 339.6ms |
| 2 | 13,317 | 18/26 | 179.9ms | 1,998.4ms | 1,998.4ms |
| 3 | 12,832 | 18/26 | 202.6ms | 2,173.7ms | 2,173.7ms |

This is the number v1 could not honestly report: production's **real**
current query and indexes, not a Phase-13-future index that happened to
fit the wrong query. It confirms v1's qualitative finding (queue-blind
ordering starves a low-volume queue) survives correction — trickle lost
1–8 of its 26 jobs to the flood queue's dominance within 8 seconds, with
tail waits approaching 2.2 seconds — while giving the ADR a properly
sourced number instead of one measured against an index that doesn't
exist in production yet. The run-to-run spread (18/26 to 25/26) reflects
the same convoy/scan-cost variance the whole package's other multi-run
tables show; three runs is enough to confirm starvation is real, not
enough to pin an exact expected-loss percentage (§L).

### D5. Corrected fairness at multiple queue counts (no concurrency cap — isolating the ordering effect, 8s runs, 3 repeats each)

**2 queues (1 flood + 1 sparse):**

| Fairness | Run | No-progress queues (max) | Longest gap | p50 wait | p95 wait | Max wait |
|---|---|---|---|---|---|---|
| Round-robin | 1 | 1 | 2,974ms | 1,759ms | 4,438ms | 4,438ms |
| Round-robin | 2 | 1 | 4,403ms | 2,122ms | 6,229ms | 6,229ms |
| Round-robin | 3 | 1 | 4,974ms | 2,526ms | 7,154ms | 7,154ms |
| `last_claimed_at` | 1 | 0 | 320ms | 3.0ms | 6.6ms | 7.7ms |
| `last_claimed_at` | 2 | 0 | 315ms | 3.3ms | 7.8ms | 7.9ms |
| `last_claimed_at` | 3 | 0 | 313ms | 3.6ms | 6.3ms | 9.6ms |

**60 queues (1 flood + 59 sparse — specifically exceeding v1's old `LIMIT 50`):**

| Fairness | Run | No-progress queues (max, of 59) | Longest gap | p50 wait | p95 wait | Max wait |
|---|---|---|---|---|---|---|
| Round-robin | 1 | **59** | 0ms† | 5,389ms | 5,710ms | 5,764ms |
| Round-robin | 2 | **59** | 0ms† | 4,844ms | 5,235ms | 5,347ms |
| Round-robin | 3 | **59** | 0ms† | 4,194ms | 4,516ms | 4,592ms |
| `last_claimed_at` | 1 | 0 | 888ms | 209ms | 401ms | 457ms |
| `last_claimed_at` | 2 | 0 | 898ms | 187ms | 350ms | 435ms |
| `last_claimed_at` | 3 | 0 | 892ms | 182ms | 338ms | 407ms |

†A "longest gap" of 0ms for round-robin at 60 queues means most sparse
queues never recorded *two* claims to measure a gap between — consistent
with the "max no-progress" column showing nearly all 59 sparse queues
stalled simultaneously; the metric that would show the gap has too few
data points to compute, itself a symptom of the same starvation.

**This is a genuinely different, corrected finding from both v1 and the
review's preliminary (H1-investigation) measurement, at both queue
counts, fully reproducible (6/6 runs) — see §F for why.**

### D6. Integrated: slot-table concurrency + fairness, together, with reclaim active (capacity limit = 3/queue, 300ms lease, ~10% simulated crash rate, 8s runs, 3 repeats each)

**2 queues:**

| Fairness | Run | Limit violations | Reclaims | No-progress (max) | p95 wait | Max wait |
|---|---|---|---|---|---|---|
| Round-robin | 1 | 0 | 20 | 0 | 1,208ms | 1,210ms |
| Round-robin | 2 | 0 | 24 | 0 | 1,209ms | 1,212ms |
| Round-robin | 3 | 0 | 24 | 1 | 1,205ms | 1,205ms |
| `last_claimed_at` | 1 | 0 | 17 | 0 | 308ms | 1,218ms |
| `last_claimed_at` | 2 | 0 | 21 | 0 | 1,207ms | 1,219ms |
| `last_claimed_at` | 3 | 0 | 17 | 0 | 304ms | 1,207ms |

**20 queues (1 flood + 19 sparse):**

| Fairness | Run | Limit violations | Reclaims | No-progress (max, of 19) | p95 wait | Max wait |
|---|---|---|---|---|---|---|
| Round-robin | 1 | 0 | 94 | 1 | 1,252ms | 1,444ms |
| Round-robin | 2 | 0 | 102 | 1 | 1,243ms | 1,390ms |
| Round-robin | 3 | 0 | 112 | 1 | 1,283ms | 1,422ms |
| `last_claimed_at` | 1 | 0 | 98 | 2 | 1,269ms | 1,511ms |
| `last_claimed_at` | 2 | 0 | 104 | 0 | 1,292ms | 1,628ms |
| `last_claimed_at` | 3 | 0 | 118 | 2 | 1,420ms | 1,780ms |

**Zero limit violations across all 6 runs, in both shapes, despite
capacity limiting, fairness ordering, and reclaim all operating
together for the first time in this evidence package.** Wait times are
much closer between the two fairness mechanisms here than in §D5's
uncapped comparison — see §F for why the capacity cap changes the
picture.

### D7. Queue-isolation, repeated (5 runs each, corrected schema)

| Candidate | Run 1 | Run 2 | Run 3 | Run 4 | Run 5 |
|---|---|---|---|---|---|
| Advisory lock (optimized) | 0.6% | 2.4% | 0.7% | **−4.5%** | **−4.8%** |
| Slot-table | 12.6% | 10.2% | 10.2% | 9.6% | 4.7% |

**This reproduces cleanly now**, unlike v1's overlapping, inconclusive
3-run spread. Advisory lock's degradation clusters tightly around 0%
(two runs even show the "under flood" condition slightly *faster* than
"alone," which is noise around zero, not a real negative cost). Slot-table
shows a consistent, positive 5–13% degradation across all 5 runs. See §F
for the isolation-mechanism explanation and §L for what's still unproven
about *why*.

---

## E. EXPLAIN/locking evidence

Every plan change claimed in §A was verified via `EXPLAIN (ANALYZE,
BUFFERS)` against this project's actual PostgreSQL 16.15 instance, not
assumed from the index definition alone.

**Combined-index candidate pick (used by all four concurrency
candidates)** — a single-queue, single-statement, `FOR UPDATE SKIP
LOCKED` claim against 300,000 seeded rows:

```
Limit (actual time=0.014..0.015 rows=1)
  -> LockRows (actual time=0.014..0.014 rows=1)
      -> Index Scan using idx_bench_claim_or_reclaim (actual time=0.012..0.012 rows=1)
            Index Cond: (queue_name = 'hot')
            Filter: (state IN ('QUEUED','RUNNING')) AND (claimableWhere...)
```
0.015ms total — versus the OR-across-two-single-state-indexes shape this
replaced, confirmed separately at 69.8ms (full sequential scan + external
disk sort at the same row count — this is the exact plan v1's fairness
queries and this pass's own first-draft concurrency queries both fell
into; see §A1/A6).

**The missing `RUNNING`-count index (§A6), before the fix:**

```
Finalize Aggregate (actual time=11.091..13.398 rows=1)
  -> Gather (Workers Launched: 1)
      -> Partial Aggregate
          -> Parallel Seq Scan on bench_jobs (actual time=3.771..9.109 rows=1798, loops=2)
                Filter: (queue_name = 'hot') AND (state = 'RUNNING')
                Rows Removed by Filter: 148202
```
13.4ms, a parallel sequential scan — despite `idx_bench_claim_or_reclaim`
existing, PostgreSQL did not recognize `state = 'RUNNING'` as satisfied by
a `state IN ('QUEUED','RUNNING')` partial index for this query shape.
**After** adding `idx_bench_running_count ON bench_jobs (queue_name) WHERE
state = 'RUNNING'`:

```
Aggregate (actual time=1.106..1.106 rows=1)
  -> Index Only Scan using idx_bench_running_count (actual time=0.055..0.783 rows=3596)
        Index Cond: (queue_name = 'hot')
        Heap Fetches: 2822
```
1.1ms — a 12× improvement, incurred on nearly every claim attempt by three
of the four concurrency candidates (all but slot-table, which tracks
capacity via slot rows instead), which is why this single missing index
uniformly collapsed advisory-lock's and `SERIALIZABLE`'s measured
throughput by roughly an order of magnitude before it was found.

**Per-queue LATERAL candidate pick (fairness candidates)** — the
`perQueueCandidate` shape, correlated to one queue via a `LATERAL`
reference rather than a bind parameter:

```
Limit (actual time=0.131..0.132 rows=1)
  -> Merge Append
      -> Index Scan using idx_bench_claim_or_reclaim (Index Cond: queue_name = qn.queue_name)
      -> Sort -> Index Scan using idx_bench_claim_or_reclaim tj_1 (reclaim branch)
```
0.24ms per queue — confirming the fairness candidates' per-queue cost
scales with the index, not the backlog, at any queue count including the
60-queue shape §D5 actually exercises.

**pg_locks confirms the advisory-lock convoy directly** (not inferred):
35.3 of ~37.6 active connections waiting on `locktype = advisory` at any
sampled instant during 40-worker contention (§D3) — unchanged conclusion
from v1, now measured on a corrected baseline that rules out "the convoy
number was inflated by an unrelated scan cost" as a confound.

---

## F. Fairness findings — including a real reversal from the review's preliminary reading

**The starvation problem is confirmed against production's actual query
and indexes** (§D4), not v1's mis-indexed stand-in — trickle lost up to
31% of its work to flood dominance within 8 seconds, tail waits over 2
seconds. This is the strongest version of the "problem exists" evidence
this package has produced across three passes.

**Round-robin is worse than `last_claimed_at` at both tested queue
counts, reproducibly (6/6 runs), and the gap widens sharply with queue
count** — this reverses the *lean* (not a firm conclusion — the review
correctly flagged it as confounded) toward round-robin that the review
pass's preliminary, still-buggy measurement showed. With the query-shape
bug now fixed on both sides (§A1, confirmed via `EXPLAIN`, §E), the
comparison is apples-to-apples, and round-robin loses clearly:

- At 2 queues: round-robin's sparse queue was flagged "no progress" in
  every single sampled window that mattered (`max_no_progress = 1` out of
  1 possible sparse queue, every run) with p50 waits of 1.8–2.5
  *seconds*; `last_claimed_at`'s sparse queue was never flagged stalled,
  p50 waits of 3–4 *milliseconds* — roughly a **600–800× gap**.
- At 60 queues: round-robin left **59 of 59** sparse queues
  simultaneously stalled at some sampled point, every run, with p50
  waits of 4.2–5.4 seconds; `last_claimed_at` never stalled any queue,
  p50 waits of 182–209ms — the gap widens, it doesn't narrow, as queue
  count grows.

**Root cause, reasoned through and consistent with every number above**:
this package's round-robin candidate — a faithful, correctly-indexed
implementation of "compare each queue's own oldest still-pending
candidate, serve whichever is globally oldest" (the natural PostgreSQL-
native reading of `phase-13-plan.md` §6a's round-robin candidate, and
structurally identical to the review pass's own design) — is not actually
a turn-taking round robin. It has no persistent "whose turn is next"
state at all; it substitutes "age of oldest pending item" as a proxy for
fairness. Under **sustained, unbounded flooding** (this package's own
adversarial load shape throughout, matching `security-model.md`'s P0
tenant-starvation threat model), a continuously-replenished flood queue's
own oldest unclaimed row gets **older without bound** for as long as
claim throughput trails the flood's insert rate — which it does, by
design, in every experiment in this package. A sparse queue's single
pending item, by contrast, is usually *recently* inserted (arrival is
slow) and gets claimed-and-reset well before it can compete on age. Once
the flooded queue's backlog is old enough, it wins the "who's oldest"
comparison essentially every time, and round-robin degrades toward
exactly the behavior it was meant to prevent. `last_claimed_at` does not
have this failure mode because its signal is **when a queue was last
*served*, not how old its backlog is** — a quantity that resets
deterministically on every successful claim regardless of how deep or old
the flooded queue's own backlog has grown.

**This is not a bug in the harness — it is a property of the mechanism as
specified.** `phase-13-plan.md` §6a names Hatchet's group-key round robin
as "the one named candidate" without specifying its exact algorithm; a
*true* turn-taking round robin (an explicit rotating cursor over eligible
queues, not an age comparison) would not have this failure mode, but is
also not what "round robin" naturally compiles to as a single PostgreSQL
query, and was not what either this pass or the review pass built. **If
the ADR wants to credit round-robin fairness, it needs to specify and
re-measure a genuine turn-taking variant — the age-proxy version measured
here and in the review pass is now shown, reproducibly, to fail under
sustained flooding, which is the exact scenario TF-INV-019 exists to
bound.**

**Under capacity limiting (§D6), the gap between the two mechanisms
narrows sharply** — p95 waits are within 1–15% of each other at both
queue counts once a per-queue cap (3) and a short lease (300ms) are also
in play. This makes sense: once a queue's own concurrency cap is the
binding constraint (not global claim-query fairness), the *ordering*
algorithm matters less, because a flooded queue literally cannot claim
faster than its own cap regardless of how "eligible" its backlog looks —
capacity limiting itself provides a form of isolation that dampens (but,
per §D6's still-nonzero `no-progress` counts at 20 queues, does not
fully eliminate) the age-dominance failure mode. This is itself useful
evidence: **the concurrency-limit mechanism and the fairness mechanism
are not fully independent levers** — a tight-enough cap partially
compensates for a weak fairness algorithm, which the ADR should weigh
when deciding how much fairness-algorithm sophistication is actually
load-bearing given Phase 13 ships per-queue caps regardless.

---

## G. Queue-isolation — now a clean, reproducible signal, reversing v1's "inconclusive" verdict

v1 reported overlapping, noisy ranges (13.7–21.3% for advisory lock,
1.0–19.5% for slot-table) and explicitly declined to draw a conclusion.
With the `ANALYZE`/indexing fixes (§A6) removing a large source of
plan-choice variance, and five repetitions instead of three, §D7's numbers
are tight and non-overlapping: **advisory lock shows ~0% isolation cost;
slot-table shows a consistent 5–13%.**

A plausible, but not yet directly verified, explanation: advisory lock's
isolation is a single hashed integer key per queue name, with zero shared
state between two different queues' locks. Slot-table's isolation depends
on every claim touching `bench_queue_slots`, a physically separate but
*structurally similar* table for every queue — plausibly subject to
shared buffer-cache pressure, WAL volume, or autovacuum scheduling effects
that a pure in-memory lock ID never touches. This explanation is
consistent with the direction and rough magnitude of the effect but has
not been isolated the way §F's fairness root-cause was (no targeted
micro-benchmark was built to confirm it) — flagged honestly in §L as the
one finding in this pass not fully mechanistically explained, even though
its *existence* is now solidly measured.

---

## H. Performance tradeoffs (updated)

| | Advisory lock (naive) | Advisory lock (optimized) | Slot-table | `SERIALIZABLE` |
|---|---|---|---|---|
| Peak throughput | ~735/s | ~839/s | **~1,974/s** | ~448/s (at 5 workers; falls thereafter) |
| Plateau/scaling | Hard plateau ~600–740/s past 5 workers | Hard plateau ~700–840/s past 10 workers | Scales to 25 workers, degrades gracefully after | Degrades from the start |
| True corrected `SERIALIZABLE` abort rate | n/a | n/a | n/a | **62–97%**, rising with concurrency (corrected via M3 — v1's 47–82% undercounted) |
| Queue isolation | **~0% measured cost** | **~0% measured cost** | ~5–13% measured cost | not separately measured |
| Round-trip sensitivity | Baseline (5 RT) | ~15–20% faster plateau (2 RT) | n/a (already 1 atomic statement) | n/a (dominated by abort rate, not RT count) |
| Reclaim/capacity interaction | Correctly gates only fresh claims; reclaim bypasses the cap (§A) | Same | Correctly branches: fresh claims acquire a slot, reclaims don't touch slots at all (§A7) | Correctly gates only fresh claims |

Slot-table remains the clear concurrency-limit throughput and
lock-contention leader (§D2/§D3, unchanged conclusion from v1, now on a
corrected baseline). It is no longer clearly the isolation leader — that
distinction now goes to advisory lock, a genuinely new, higher-confidence
finding this pass produced.

---

## I. Compatibility implications (updated)

All of v1's compatibility findings (§I there) are reconfirmed against the
v2 schema — the correctness-check suite (§D6 of v1, §7 of this pass's
harness) passes identically, now with one addition: **the production
baseline's own reclaim branch was directly tested and confirmed
correct** (`prod_baseline_reclaim_branch_fires`, §D of this document) —
something v1 could not check at all, since it never built a faithful
production reconstruction.

**New from A7**: the slot-table mechanism's reclaim/slot-ownership
semantics are now a concrete design question for the ADR, not an
implicit assumption. `phase-13-plan.md` §7's own text ("every
terminalization call site... gains a slot-release step") does not
currently distinguish a terminalization from a *reclaim* (a job
returning to `RUNNING` under a new lease, not terminalizing at all) — this
pass's finding is that they must be handled differently: a reclaim must
**not** trigger any slot-table write at all, or it risks either double-
counting a slot against one job or spuriously failing to reclaim a job
whose slot appears (incorrectly) unavailable. This should be added to
`phase-13-plan.md` §17's file/call-site list before implementation, not
discovered during it.

---

## J. Recommendation for OD-1 (updated)

**Concurrency-limit mechanism: slot-table remains the recommendation,
now on materially stronger footing.** Every axis that favored it in v1
(exactness, throughput, lock contention) reconfirms on the corrected
baseline, with the round-trip-count confound (M1) explicitly controlled
for via the optimized advisory-lock variant — slot-table's throughput
lead (1,974/s vs. 839/s) is not an implementation-chatter artifact, it
survives against advisory lock's *best* measured implementation. The one
new consideration is isolation (§G): slot-table's 5–13% measured
degradation under concurrent hot-queue load is a real, now-reproducible
cost advisory lock does not share. Whether this outweighs slot-table's
3× throughput and near-zero lock-contention advantages is a judgment call
the ADR should make explicitly — this evidence package's own reading is
that it does not (a 5–13% throughput cost under adversarial concurrent
flooding is a smaller risk than a mechanism whose realized concurrency is
capped by lock serialization regardless of the configured limit — v1's
and this pass's shared, unretracted objection to advisory lock as the
general-purpose choice), but the isolation mechanism itself is not fully
explained (§L), and the ADR may reasonably weigh it differently.

**Fairness mechanism: `last_claimed_at` is now the clear,
evidence-backed recommendation — not a close call the way v1 and the
review pass both (differently) suggested.** §F's corrected, reproducible
measurement shows round-robin (as specified and implementable in one
PostgreSQL-native query) failing under sustained flooding at both tested
queue counts, with the failure *worsening* as queue count grows — the
opposite of what a fairness mechanism should do as the system scales.
`last_claimed_at` costs one column, one query-shape change, and (per this
pass's own §D5/§F numbers) delivers a 600–800× better tail-wait outcome
at 2 queues and near-total stall prevention at 60, where round-robin
stalls nearly everything. If the ADR wants round-robin fairness
specifically (e.g., to match Hatchet's own behavior more closely), it
needs to specify a genuine turn-taking mechanism with persistent
state — which is a materially different, more expensive design than
either measured implementation, and has not been built or tested here.

**Combined recommendation, updated**: slot-table concurrency limiting +
`last_claimed_at` fairness — the same pairing v1 proposed, now actually
measured together (§D6), not asserted from two separate half-experiments.
Zero limit violations across every integrated run, reclaim genuinely
exercised (17–118 reclaims per run depending on shape), and the two
fairness candidates converge in the capacity-limited regime (§F) — meaning
the ADR can adopt this combination with confidence that combining the two
mechanisms does not introduce an interaction neither half-experiment could
see, which was v1's single largest disclosed gap (§L there).

---

## K. TF-INV-019 bound semantics — now materially more defensible, still not final

§D5/§D6 give the ADR real numbers to build a bound from, at two queue
counts, under both uncapped and capacity-limited conditions — a much
stronger evidentiary base than v1's single 2-queue data point.

> **Proposed bound, `last_claimed_at` mechanism, uncapped**: a queue with
> pending, capacity-eligible work is claimed within one full fairness
> cycle — bounded, in this pass's measurements, by p95 waits of 6.6–9.5ms
> at 2 concurrently-eligible queues and 338–401ms at 60, under sustained
> adversarial flooding of every other queue. The bound scales with queue
> count (§D5's own numbers show p95 growing roughly 40–60× from 2 to 60
> queues, not linearly with queue count alone — the flooded queue's own
> claim rate and worker count both factor in and were not independently
> varied in this pass, §L).
>
> **Proposed bound, integrated (capacity-limited, reclaim active)**: p95
> waits of ~1.2s at both 2 and 20 queues (§D6) — dominated by the 300ms
> lease duration and per-queue cap of 3 in this specific experiment
> configuration, not by the fairness algorithm (§F) — meaning this bound
> is really a statement about lease-duration and cap-size choices, which
> are operator-configured, not a property of `last_claimed_at` itself.
> The ADR should state the bound as a function of those operator knobs,
> not a fixed number.

**Round-robin is not recommended for TF-INV-019's mechanism**, so no
bound is proposed for it — §F's finding is that its worst-case behavior
(stalling nearly all sparse queues simultaneously under sustained
flooding) does not admit a useful bounded-wait statement at all with the
measured implementation.

This remains **not a final bound** — §L names the specific follow-up
experiments (queue-count sweep beyond 2 and 60, worker-count sweep,
production-scale run duration) needed before the ADR should commit to an
exact numeric bound rather than the qualitative/parametric form above.

---

## L. Remaining uncertainties / experiments still needed (updated)

Gaps v1/the review named, and their status after this pass:

1. **Fairness tested at only 2 queues** — **partially resolved.** Now
   tested at 2 and 60 (specifically chosen to exceed the old `LIMIT 50`).
   Still not tested at intermediate points (5, 10, 20 non-integrated;
   20 was only tested in the *integrated*, capacity-limited condition) or
   beyond 60 — the ADR should not assume linear interpolation between 2
   and 60 queues given §K's note that the scaling isn't obviously linear.
2. **Noisy queue-isolation results** — **resolved.** §D7/§G now show a
   clean, reproducible, non-overlapping signal across 5 runs per
   candidate. The *mechanism* behind slot-table's specific cost is still
   not isolated (no targeted micro-benchmark built, unlike §F's fairness
   root-cause) — a genuine remaining gap, smaller than v1's, but real.
3. **Combined mechanism never tested together** — **resolved.** §D6
   measures slot-table + each fairness candidate together, with capacity
   limits and reclaim both active, at two queue shapes, three repeats
   each.
4. **New gaps this pass surfaces**:
   - Round-robin's failure mode (§F) was characterized qualitatively and
     its *magnitude* measured, but its *scaling law* (how badly does it
     degrade as a function of flood-to-sparse insert-rate ratio,
     specifically, independent of queue count) was not isolated —
     useful if any future design still wants to attempt a stateless
     round-robin approximation.
   - The isolation mechanism's root cause (§G) is asserted as plausible,
     not confirmed via a targeted experiment the way §F's fairness root
     cause was.
   - This pass's integrated experiment used one fixed capacity
     configuration (limit 3, 300ms lease, ~10% simulated crash rate) —
     not varied, so §K's integrated bound is only defensible for
     configurations resembling this one, not as a general statement about
     how capacity limits and fairness interact across the configuration
     space.
   - No experiment in this pass or its predecessors has run at production
     scale (hundreds of workers, minutes-to-hours duration, realistic
     execution times rather than 3–15ms simulated holds).
   - The retention-sweeper interaction (`phase-13-plan.md` §13's own
     named failure scenario) remains untested across all three passes.

None of these block OD-1 from proceeding with the slot-table +
`last_claimed_at` recommendation in §J for the *qualitative* mechanism
choice — the evidence for that choice's core properties (exactness,
throughput ordering, fairness effectiveness, and now their combination)
is consistent and reproducible across every run in this pass. They do
mean the *exact numeric bound* in §K should be stated conditionally
(queue count, worker count, capacity configuration), not as a single
constant, until the queue-count and worker-count sweeps above are run.

---

## Answers to the eleven verification questions (A–K, per this pass's brief)

**A. Corrected methodology** — §A above: six review findings fixed
(H1, H2, M1, M2, M3, plus one review-adjacent defect the review didn't
name, A6), each verified via `EXPLAIN (ANALYZE, BUFFERS)` before being
accepted; one rejected design path (the two-step pattern applied to
single-queue candidates) disclosed rather than hidden (§A8).

**B. Exact benchmark/harness changes** — §B's file-by-file table; full
diff is `tools/phase13bench/*.go` against the frozen `v1/main.go`.

**C. Corrected results** — §D, all eight experiment groups, 90
sub-experiments, one clean run, no cherry-picking.

**D. EXPLAIN/locking evidence** — §E, four `EXPLAIN (ANALYZE, BUFFERS)`
comparisons (combined index, missing-index-found-and-fixed, per-queue
LATERAL, and `pg_locks` sampling for the advisory-lock convoy).

**E. Fairness results at multiple queue counts** — §D5 (2 and 60 queues,
uncapped) and §D6 (2 and 20 queues, capacity-limited + reclaim active).

**F. Integrated-combination results** — §D6/§F: zero limit violations
across 6 runs in 2 shapes, reclaim genuinely exercised, fairness gap
between mechanisms narrows once capacity limiting dominates.

**G. Is slot-table still the concurrency recommendation** — **Yes**, on
stronger footing than v1 (round-trip confound controlled for), with one
new, real cost (isolation, §G) the ADR should weigh explicitly rather
than assume away.

**H. Which fairness mechanism is now recommended** — **`last_claimed_at`,
clearly**, reversing the ambiguous/round-robin-leaning readings of both
v1 and the review pass. This is the single most important change in this
document relative to its two predecessors.

**I. Is a TF-INV-019 bound semantics now defensible** — **More
defensible than before, still not final.** §K proposes a parametric
(queue-count- and capacity-configuration-dependent) bound rather than a
single constant, and names the exact sweeps (§L items 1 and the new
capacity-configuration point) still needed before the ADR should commit
to specific numbers.

**J. Is the OD-1 ADR ready to write** — **Closer than before, not yet.**
The qualitative mechanism recommendation (slot-table + `last_claimed_at`)
is now backed by reproducible, cross-checked, EXPLAIN-verified evidence
including a real integrated test — a materially stronger position than
either v1 or the review pass reached. What's still missing before the
numeric bound in §K can be stated as more than a parametric sketch: the
queue-count sweep beyond {2, 60} and a capacity-configuration sweep for
the integrated case (§L). The ADR could reasonably be drafted now with
the mechanism recommendation stated firmly and the bound stated in the
parametric form §K already gives — whether that is sufficient is the
ADR author's call to make explicitly, not this evidence package's.

**K. Remaining blocker** — **None that blocks a mechanism
recommendation.** The remaining blocker is scope-limited to the *exact
numeric* TF-INV-019 bound: a queue-count sweep (at minimum one
intermediate point, e.g. 10–20 queues, non-integrated) and a
capacity-configuration sweep for the integrated case, both cheap
extensions of `runFairnessMultiQueue`'s existing parameterization, not new
design work.

---

## Repo hygiene note

Nothing in this pass has been staged, committed, or pushed, per
instruction. `docs/phase-13-concurrency-evidence-v2.md` (this file),
`tools/phase13bench/*.go` (the corrected harness), and
`tools/phase13bench/results-v2.json` are new/modified, currently-untracked
files in the working tree. `tools/phase13bench/v1/` remains exactly as it
was when the review was written against it. The harness's own database
setup/teardown (`taskforge_phase13_bench_v2`, a separate database from
v1's `taskforge_phase13_bench`) is the only mutation made to the local
PostgreSQL instance.
