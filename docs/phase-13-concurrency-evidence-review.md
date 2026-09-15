# Independent Review — Phase 13 OD-1 Concurrency/Fairness Evidence Package

Status: **REVIEW ONLY.** No production code touched. No file the review
covers (`docs/phase-13-concurrency-evidence.md`,
`tools/phase13bench/main.go`) was modified. No ADR written, nothing
staged/committed/pushed. This document is itself a new, untracked file.

Reviewer note on method: this is not a re-read-and-nod review. Every
finding below is backed by either (a) direct re-inspection of the harness
source against the report's claims, or (b) a new, targeted diagnostic run
against the same live PostgreSQL 16 instance the original evidence was
measured on, specifically designed to falsify a claim rather than confirm
it. Two throwaway diagnostic programs
(`tools/phase13bench/diag_lastclaimed.go`,
`tools/phase13bench/diag_lockwait.go`, both `//go:build ignore`, not part
of the harness) were written for this review and are left in place for
reproducibility; they are not committed.

## Verdict: **REVISE EVIDENCE**

Not because the underlying experiments were faked or the qualitative
headline conclusions are wrong — the concurrency-limit exactness results,
the advisory-lock convoy, and the existence of queue-blind starvation are
all solidly supported. It's REVISE because **one specific, load-bearing
causal claim in the report is wrong** (§F.3's attribution of
`last_claimed_at`'s throughput cost), and that claim feeds directly into
§J's recommendation and §K's proposed bound. The ADR should not cite §F.3
or the round-robin-vs-`last_claimed_at` throughput comparison in §D4/§J as
currently written.

---

## HIGH findings

### H1 — `last_claimed_at`'s measured throughput penalty is misattributed. It is not row-lock contention; it is an unindexed O(backlog) sort, and the report's own diagnostic-free narrative overstated confidence in the wrong mechanism.

The evidence document's §F.3 states: *"the mechanism is identifiable, not
a mystery: `last_claimed_at` requires an `UPDATE` to one shared row... under
a permanently flooded queue, every claim serializes briefly on that one
row's lock."* This claim was never independently verified in the original
package — it's a plausible-sounding narrative, not a measurement. I tested
it two ways and it fails both:

1. **Isolated single-row UPDATE throughput ceiling, same statement shape,
   same database, same connection pool**: 1,211–1,504 claims/s across
   worker counts 5→80 (`diag_lastclaimed.go`). This is **6–10× higher**
   than the full candidate's observed flood-queue throughput (124–189/s,
   §D4). If the shared-row `UPDATE` were the dominant bottleneck, the full
   candidate's throughput should approach this ceiling, not sit an order
   of magnitude below it.
2. **Direct `pg_locks` sampling during an actual running `last_claimed_at`
   claim workload**, broken out by relation (`bench_queue_state` vs.
   `bench_jobs`) rather than aggregated as the original E3 experiment did:
   **zero measurable waiting locks on either relation**, for the entire
   4-second run, at 10 workers (`diag_lockwait.go`). If row-lock
   contention were the mechanism, this should show non-zero waits on
   `bench_queue_state`. It shows none.

Having ruled out lock contention, I checked the query plan directly
(`EXPLAIN (ANALYZE, BUFFERS)` against a 20,001-row seeded backlog):

```
Limit (actual time=8.050..8.053 rows=1)
  -> LockRows (actual time=8.049..8.052 rows=1)
      -> Sort (actual time=8.040..8.042 rows=1)  Sort Method: quicksort  Memory: 2331kB
          Sort Key: q.last_claimed_at, j.priority DESC, j.eligible_at
          -> Hash Join
              -> Seq Scan on ej j  (rows=20001)  Filter: state='QUEUED' AND eligible_at<=now()
              -> Seq Scan on eq q  (rows=2)
```

The claim query does a **full sequential scan of the entire eligible
backlog, a hash join against the two-row `queue_state` table, and a full
in-memory sort of every matching row — for every single claim attempt** —
because `ORDER BY q.last_claimed_at` (a column on the *joined* table)
cannot be satisfied by any index on `bench_jobs`, structurally, regardless
of what index exists. At 20,000 rows this alone costs ~8ms of CPU-bound
work per claim, before any lock is even requested. Because the flooded
queue's insert rate (2,500/s) outpaces this candidate's actual claim
throughput, **the backlog grows across the run, which makes each
subsequent claim's sort more expensive, which slows throughput further —
a genuine positive-feedback spiral**, not a fixed per-claim cost. This
plausibly explains both the magnitude of the deficit and why it was
*worse* in absolute terms than the isolated single-row-update ceiling
would predict on its own.

**Consequence for the report**: §F.3, §D4's "flood share" framing, §H's
tradeoff table row, and §J's "last_claimed_at measurably taxes the
flooded queue's own throughput... a specific mechanism (the shared
per-queue counter row)" sentence are all built on the wrong causal story.
The *qualitative* fact that `last_claimed_at`, as implemented, costs the
flooded queue meaningfully more throughput than round-robin is still
true and reproducible. But the report tells the ADR author this is an
*inherent, hard-to-avoid* property of the ordering concept — it is not.
A restructured implementation (e.g., a cheap separate lookup —
`SELECT queue_name FROM queue_state WHERE queue_name = ANY($subscribed)
ORDER BY last_claimed_at ASC LIMIT 1` — to pick the target queue first,
*then* an ordinary single-queue-scoped `FOR UPDATE SKIP LOCKED` claim
using the existing `(queue_name, priority DESC, eligible_at ASC)` index,
falling back to the next-least-recently-served queue only if the first is
empty) would plausibly close most of this gap, since it avoids the
cross-queue sort entirely. This was never built or tested. Citing the
current 5–8× figure in an ADR as a property of "`last_claimed_at` fairness"
rather than "this particular unoptimized implementation of it" would be a
real error.

**Severity justification**: this directly undermines the comparative
half of §J's recommendation (which candidate's fairness mechanism to
prefer) and the empirical basis some of §K's proposed bound. It does not
undermine the recommendation's other half (slot-table for concurrency
limiting), which rests on single-queue experiments unaffected by this
issue (see H2).

### H2 — The entire fairness comparison (baseline vs. round-robin vs. `last_claimed_at`, §D4/§F/§J) ran against a schema whose only index cannot serve *any* of the three candidates' cross-queue query shapes, including the "baseline = today's production behavior" one — so throughput numbers for this experiment are not representative of production, in either direction.

`tools/phase13bench/main.go`'s `schemaSQL` defines exactly one index on
`bench_jobs`: `idx_bench_claimable ON bench_jobs (queue_name, priority
DESC, eligible_at ASC) WHERE state = 'QUEUED'`. This is the **Phase-13
future-state index** — it matches `phase-13-plan.md` §7's proposed
`idx_jobs_claimable_by_queue`, not today's actual production index.
Today's real index (`migrations/0001_create_jobs_table.up.sql`) is
`idx_jobs_claimable ON jobs (priority DESC, eligible_at ASC) WHERE state
IN ('QUEUED','RETRY_WAIT')` — **no `queue_name` column at all**, because
`queue_name` doesn't exist in production yet.

This matters because the *fairness baseline* candidate (`FairBaseline`,
meant to represent "today's actual queue-blind behavior") issues exactly
the query shape production's *real, current* index would serve perfectly
via a plain index scan — but against the harness's queue_name-prefixed
index, that ordering can't be satisfied, and Postgres falls back to a full
sequential scan + sort (confirmed via `EXPLAIN ANALYZE`: 11.4ms at 20,001
rows, `Seq Scan on ej` under a `Sort`, not an `Index Scan`). **The
"baseline" experiment was measured against a strictly worse index than
production actually has for that exact query today**, meaning its
reported throughput (~960–1,075/s) likely *understates* real baseline
throughput, not overstates it.

The same `EXPLAIN ANALYZE` check on the round-robin candidate's ranking
query shows the identical class of problem (`WindowAgg` over a full
`Index Scan` of the entire eligible set, ~7.8ms at 20,001 rows) — it is
somewhat mitigated by the `LIMIT 50` (top-N heapsort instead of a full
sort) but is not free of the same structural issue.

I confirmed, by contrast, that the **single-queue-scoped queries used by
all three concurrency-limit candidates (E1/E2/E3) do use the index
correctly** — `EXPLAIN ANALYZE` on `claimAdvisory`/`claimSlot`/
`claimSerializable`'s job-selection query (with `queue_name = $1` as an
equality predicate) shows a clean `Index Scan`, 0.070ms at 20,000 rows.
**So this finding does not touch §D1/§D2/§D3 or the concurrency-mechanism
recommendation** — it is scoped specifically to the fairness experiment
(§D4) and its dependents (§F, most of §J, §K).

**Consequence**: none of the three fairness candidates' throughput numbers
in §D4 can be trusted as representative of a properly-indexed production
implementation. The *qualitative* starvation finding survives (§F.1 — see
"strongly supported" below), because which row wins a claim is a pure
function of the `ORDER BY` semantics and is identical regardless of
physical plan. The *quantitative* comparison between candidates does not
survive, because it's dominated by an implementation artifact common to
all three cross-queue query shapes, to different (and differently
compounding, per H1) degrees.

---

## MEDIUM findings

### M1 — The advisory-lock candidate's implementation uses 5 round trips per claim where a reasonably careful implementation would need ~2, inflating its measured latency/throughput disadvantage beyond what the lock-serialization mechanism alone causes.

Re-reading `claimAdvisory` (`main.go`): per claim, it issues
`pg_advisory_xact_lock`, then a separate `SELECT concurrency_limit`, then a
separate `SELECT count(*) ... WHERE state='RUNNING'`, then the candidate
`SELECT ... FOR UPDATE SKIP LOCKED`, then the `UPDATE` — **5 sequential
round trips inside the held lock**, each one extending how long every
other worker waits behind that lock. `claimSlot`, by contrast, does the
equivalent decision in a single combined CTE statement plus one release
statement — **2 round trips**. This is not a fair apples-to-apples
implementation-quality comparison: this codebase's own idiom
(`internal/store/claim.go`'s CTE) already shows the natural pattern is to
combine reads/writes into one statement where possible, and a
straightforward rewrite (e.g., `SELECT concurrency_limit,
(SELECT count(*) ...) FROM queue_state WHERE queue_name=$1`, folded into
the same `WITH ... UPDATE` as the candidate selection) would plausibly cut
advisory-lock's critical-section time by more than half.

**This does not undermine the convoy finding itself** — 35.4 avg
waiting connections out of 37.7 active, directly measured via `pg_locks`,
is a fact about the lock being held for whatever duration the critical
section takes, and *some* convoy would exist under any implementation,
since the mechanism is fundamentally "one claim at a time per queue." But
the *magnitude* comparison in §A1/§D2 ("slot-table's peak throughput is
~3× advisory-lock's") should not be read as a clean measurement of "the
advisory-lock mechanism vs. the slot-table mechanism" — part of that gap
is "this specific 5-round-trip implementation vs. this specific
2-round-trip implementation." A re-run with an optimized (1–2 round trip)
advisory-lock claim statement would very likely narrow, though not
eliminate, the gap.

### M2 — Round-robin's hardcoded `LIMIT 50` candidate window was never stress-tested at a queue cardinality where it could matter, and it is a concrete, identifiable mechanism by which round-robin fairness could silently fail at scale — this sharpens, rather than just restates, the already-disclosed "only tested at 2 queues" gap.

`claimFairness`'s `FairRoundRobin` branch fetches at most 50 ranked
candidate ids before restricting the locking query to that set
(`LIMIT 50`). At 2 queues this is never binding (confirmed: with only 2
partitions, the top 50 globally-ranked rows span roughly 25 rounds of both
queues). At higher queue cardinality — the plan's own scope explicitly
allows arbitrarily many named queues — if more than 50 distinct queues
have simultaneously-eligible work, some queues' rank-1 row would not even
appear in a given claim attempt's candidate set, and could be starved by
this specific, fixable-but-currently-real limit rather than by anything
inherent to the round-robin *concept*. Should be listed as a named risk,
not folded into the generic "more queues should be tested" caveat already
in §L.

### M3 — `SERIALIZABLE`'s reported abort/retry rate is real and directionally correct, but the number itself undercounts the true rate; the report cites an exact percentage without disclosing this.

`claimSerializable`'s retry loop (`main.go`) returns a single boolean
(`attempt > 0`) to its caller on success, not the actual number of aborted
attempts that preceded that success (0 to 7, given `maxRetries = 8`). The
caller increments its `serialization_failures` counter by at most 1 per
*successful* claim, regardless of whether that claim needed 1 retry or 7.
This means every `ser_fail_rate_pct` figure in §D1/§D2 (47.0%–82.1%)
**understates** the true fraction of wasted transaction attempts — the
real number is higher, plausibly meaningfully so under the 70–82%-failure
regime where multi-retry successes are common.

This does **not** change the qualitative conclusion (reject
`SERIALIZABLE` — §J correctly does not recommend it) — if anything, the
true picture is worse for this candidate than reported, so the
recommendation is, if anything, *more* justified than the current numbers
suggest. But an ADR should not cite "82.1% serialization failure rate" as
a precise, defensible figure; it should be re-measured with an actual
retry-attempt counter (increment a counter inside the retry loop itself,
not a boolean at the end) before being quoted verbatim.

### M4 — E5 (queue/tenant isolation) noise: correctly disclosed by the original document; re-confirmed, no new issue found.

I re-checked the three-run spread reported in §D5/§L (13.7–21.3% for
advisory lock, 1.0–19.5% for slot-table) against the raw JSON files
(`results.json`, `results-run2.json`, and the console log from the first
run). The ranges are accurately transcribed, and the document's own §L
already flags this as inconclusive and recommends a dedicated re-run on
quieter hardware. I have no correction here — this is the model of how the
other two HIGH findings' underlying issues *should* have been handled
(measured, found noisy/unexplained, disclosed as provisional) rather than
narrated with unverified confidence, which is exactly what happened with
H1.

---

## LOW findings

### L1 — Exactness claims (§A1/§D1: "Exactness: exact") were validated only under a *static* `concurrency_limit`/slot-count configuration; no experiment tested an operator changing the limit mid-run.

`phase-13-plan.md` §11 already names this as a real production risk for
the slot-table mechanism specifically ("provisioning/deprovisioning
`queue_slots` rows... must be idempotent"). The evidence package inherits
this risk correctly by not claiming to have tested it, but §A1's table
cells read as unconditional ("Exact") without a footnote scoping the claim
to static configuration. A one-line caveat would remove any risk of the
claim being over-read later.

### L2 — The slot-table exactness monitor checks only `count(RUNNING jobs) ≤ limit`; it never independently cross-checks `count(RUNNING jobs) == count(held slots)`.

Both counts are expected to agree by construction (the same atomic CTE
binds a job and a slot together), and I found no evidence of disagreement
in any run. But the monitor as built could not have detected a bug where,
say, two jobs both ended up bound to the same `slot_index` in a way that
still kept the RUNNING count within bounds — a stronger monitor would
assert both invariants, not just one. Suggested strengthening for any
follow-up run, not a finding that the measured exactness result is wrong.

### L3 — Client-side latency measurements (§D2) include Go driver + network round-trip time on top of server-side execution time.

Fine for the relative comparisons the report actually makes (same
measurement method applied identically to all three candidates, and for
E2 specifically, all three use index-correct single-queue queries per
H2's finding), but should not be read as isolated server-side query cost.
Already implicitly true of any client-measured benchmark; worth a one-line
explicit note next to the latency tables.

### L4 — Short run windows (4–6s) on a shared, noisy single-node VM (already disclosed generally in §C) apply with more force to E4 specifically, given H1/H2's findings about backlog-growth feedback loops.

Since H1 identified that `last_claimed_at`'s cost compounds with backlog
growth over the course of a run, a 6-second window is short enough that
the *reported* numbers partly reflect "how bad did the spiral get in 6
seconds" rather than a steady-state throughput figure. Any re-run should
either run substantially longer or explicitly report throughput as a
time series to distinguish steady-state cost from spiral onset.

---

## Point-by-point on the 12 verification items requested

1. **Harness measures what the document claims** — Mostly yes for E1,
   E2, E3, E5, E6. **No** for E4's causal interpretation (§F.3): the raw
   numbers are accurately reported, but the document's explanation of
   *why* those numbers came out that way is incorrect (H1).
2. **No benchmark bug/timing artifact/sampling method invalidates the
   conclusions** — **No.** Two real issues found: the `SERIALIZABLE`
   retry-undercounting bug (M3) and the index/schema mismatch underlying
   H1/H2. Neither invalidates the *qualitative* headline conclusions, but
   both invalidate specific quantitative claims the report makes with
   unwarranted confidence.
3. **Three concurrency mechanisms compared fairly** — **Partially.**
   Exactness comparison: yes, fair, identical methodology, confirmed
   equal index usage. Throughput comparison: advisory lock's
   implementation carries avoidable round-trip overhead (M1) that
   inflates its measured disadvantage relative to slot-table; serializable
   vs. slot-table remains a fair comparison since serializable's cost is
   dominated by genuine abort overhead, not round-trip count.
4. **Two fairness mechanisms compared fairly** — **No.** Both suffer the
   same class of query-plan defect (H2), to different, compounding degrees
   (H1), against a baseline that itself was measured against a
   non-representative index. This is the review's central finding.
5. **Advisory-lock convoy conclusion supported by measured `pg_locks`
   evidence** — **Yes, solidly.** 35.4 average waiting connections out of
   37.7 active (locktype `advisory`), independently reproducible, and this
   finding is not affected by M1's round-trip-count critique (that critique
   affects the throughput *magnitude*, not the fact that a convoy exists).
6. **`SERIALIZABLE` abort/retry measurements correctly interpreted** —
   **Directionally yes, numerically no.** The conclusion (severe,
   worsening-with-concurrency abort overhead, reject this candidate) is
   correct and, once corrected, likely understated rather than overstated.
   The specific percentages (47.0–82.1%) undercount the true rate (M3) and
   should be re-measured before being cited precisely.
7. **Slot-table throughput not benefiting from a weaker correctness
   contract** — **Correct, no weaker contract found.** Its exactness is
   structurally guaranteed the same way TF-INV-002's own mechanism is
   (`FOR UPDATE SKIP LOCKED` binding a job and a capacity unit atomically),
   confirmed via the same 0-violation monitor as the other two candidates,
   using the same partial-index-correct query shape as the other two
   single-queue candidates (H2 confirmed this class of query uses the
   index properly for all three). L1/L2 note minor unstrengthened edges,
   not a weaker contract.
8. **Queue-starvation baseline genuinely representative of production** —
   **The ordering behavior is representative** (identical `ORDER BY
   priority DESC, eligible_at ASC`, no queue-awareness — this *is* what
   production does today) **but the measured throughput is not** (H2: run
   against a worse index than production's actual current one). The
   *existence* of starvation is a valid finding; the *cost* of the baseline
   query, and therefore any throughput comparison involving it, is not.
9. **`last_claimed_at` throughput penalty correctly attributed** —
   **No** (H1). This is the review's single most important correction.
10. **Round-robin PostgreSQL/window-function constraint real and
    accurately described** — **Yes, confirmed independently.** I
    reproduced the exact error (`ERROR: FOR UPDATE is not allowed with
    window functions`) directly against this project's PostgreSQL 16
    instance with a minimal repro, matching the report's claim exactly.
    This is the one piece of the fairness analysis I can fully endorse as
    stated.
11. **Recommendation (slot-table + `last_claimed_at`) justified by
    available evidence** — **The slot-table half: yes**, solidly, from
    experiments unaffected by H1/H2. **The `last_claimed_at` half: not
    yet** — it was chosen over round-robin partly on a throughput
    comparison now known to be dominated by an implementation artifact
    (H1/H2), not a property of the mechanism. Once re-measured with a
    query shape that avoids the cross-queue sort, `last_claimed_at` may
    still turn out to be the better choice — or may not. The current
    evidence doesn't actually settle it.
12. **Can this honestly support TF-INV-019 yet** — **No.** The qualitative
    half of TF-INV-019 (a bound exists and is achievable) is supported.
    The specific bound proposed in §K leans on the `last_claimed_at`
    measurement that H1 shows is not a reliable characterization of the
    mechanism's true cost. TF-INV-019's text should stay exactly as
    reserved/algorithm-independent as it already is — this package cannot
    yet responsibly narrow it further.

---

## Conclusions: strongly supported vs. provisional

**Strongly supported, safe to cite in the ADR as-is:**
- All three concurrency-limit candidates enforce the cap exactly under
  load (§D1) — zero violations, ~14,000 samples, three independent runs.
- The advisory-lock candidate creates a genuine, measured lock convoy
  (§D3/§G) — directly observed via `pg_locks`, not inferred.
- `SERIALIZABLE`'s abort rate rises with concurrency and its throughput
  *falls* as workers increase (§D2) — qualitatively correct, and if
  anything the true magnitude is worse than reported (M3).
- Today's actual queue-blind claim ordering genuinely starves a
  low-volume queue under sustained flood (§F.1) — the ordering mechanism
  is faithfully reproduced, independent of query cost.
- Reclaim respects the same queue-subscription boundary as a fresh claim,
  in both directions (§D6/§E6) — four correctness checks, no ambiguity.
- The round-robin candidate genuinely cannot be expressed as PostgreSQL
  `FOR UPDATE` + a window function in one statement (§F.4) — independently
  reproduced.
- Slot-table's concurrency-limit exactness and its single-queue throughput/
  latency/lock-contention advantage over the other two mechanisms (§D2/§D3)
  — unaffected by either HIGH finding, since single-queue queries use the
  index correctly across all three candidates.

**Provisional — do not cite as settled without the follow-up work below:**
- The specific throughput multiplier between advisory-lock and slot-table
  (M1: round-trip-count asymmetry inflates the gap).
- The exact `ser_fail_rate_pct` percentages for `SERIALIZABLE` (M3:
  undercounted).
- Every number in §D4 (fairness throughput/flood-share figures) and every
  conclusion in §F that compares round-robin to `last_claimed_at`
  quantitatively (H1/H2).
- §J's specific preference ordering between round-robin and
  `last_claimed_at`, and the combined recommendation's fairness half.
- §K's proposed bound (built directly on the now-suspect §D4 numbers).
- §D5's isolation comparison (already self-disclosed as noisy — M4
  confirms this disclosure was accurate and sufficient).

---

## Benchmark methodology corrections needed

1. **Fix the `SERIALIZABLE` retry counter** to increment on every actual
   aborted attempt inside `claimSerializable`'s loop, not once per
   successful outer call. Trivial code change (`return id, ok, attempt,
   nil` and sum attempts, not a boolean).
2. **Rebuild the fairness experiment's schema/queries** so all three
   candidates (baseline, round-robin, `last_claimed_at`) can actually use
   an index for their respective access patterns:
   - Add production's *actual current* index shape
     (`(priority DESC, eligible_at ASC)`, no `queue_name`) for the
     baseline candidate specifically, so it is genuinely measured against
     today's real production query cost, not a hypothetical future index
     that happens to be the wrong shape for it.
   - Reimplement `last_claimed_at` as "pick the least-recently-served
     eligible queue first via a small lookup, then claim from just that
     queue using the existing per-queue index," and re-measure.
   - Consider a `LATERAL`-join-based round-robin implementation that pulls
     the top-ranked row per queue via the existing per-queue index rather
     than materializing a windowed scan of the full cross-queue backlog,
     and re-measure against the current `LIMIT 50` approach.
3. **Optimize (or explicitly justify not optimizing) the advisory-lock
   candidate's round-trip count** before quoting an exact throughput
   multiple against slot-table.
4. **Extend E4 to more than 2 queues**, specifically including a queue
   count exceeding round-robin's `LIMIT 50` window, to test M2 directly.
5. **Run the combined slot-table + (corrected) fairness-layer mechanism
   as one integrated experiment** — never done, already disclosed in the
   original package's §L, still not done.
6. **Re-run E5 on quieter hardware or with longer windows** and report a
   confidence interval, per the original package's own §L recommendation
   (unchanged by this review — it was already correctly scoped).

## Exact additional experiments required before the OD-1 ADR can be written

1. `SERIALIZABLE` retry-counter fix + re-run of E1/E2 (cheap, mechanical).
2. Corrected-index baseline re-run of E4 (adds production's real current
   index; re-measure baseline throughput).
3. Reimplemented `last_claimed_at` (queue-first-lookup pattern) re-run of
   E4, compared against round-robin under the *same* corrected baseline.
4. E4 extended to ≥3 queues, including a case exceeding round-robin's
   `LIMIT 50`, to directly test M2.
5. One combined experiment: slot-table concurrency limiting +
   (corrected) preferred fairness ordering, measured together, not
   separately.
6. E5 isolation re-run with either a longer window or repeated trials
   sufficient to report a confidence interval, per the original package's
   own already-correct §L recommendation.

Items 1–4 are the ones this review adds; item 5 was already flagged by the
original package and is reaffirmed here as still outstanding; item 6 was
already flagged and is reaffirmed as still outstanding and unchanged.

## Whether the ADR can be written now

**Not yet.** The concurrency-limit mechanism recommendation (slot-table)
is on solid enough ground to state with confidence today. The fairness
mechanism recommendation is not — it currently rests on a comparison this
review found to be measuring an implementation artifact rather than the
mechanisms themselves, and the proposed TF-INV-019 bound in §K inherits
that weakness. Writing the ADR now would either have to (a) omit a
fairness recommendation and defer OD-1's fairness half pending corrected
evidence, or (b) cite numbers this review has shown are not reliable.
Recommend completing experiments 1–3 above (cheap, mechanical, likely
completable in under an hour of harness work) before drafting the ADR;
items 4–6 strengthen confidence further but are less immediately
blocking than 1–3, which directly falsify the current §F.3/§J/§K text as
written.
