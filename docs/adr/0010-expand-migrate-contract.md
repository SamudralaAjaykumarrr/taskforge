# ADR-0010: Expand/Migrate/Contract as the Primary Schema-Compatibility Model

Status: Accepted

## Context

[compatibility-policy.md](../compatibility-policy.md) has, since its
creation, carried a `PROPOSED` section proposing expand/migrate/contract
as TaskForge's schema-evolution model, explicitly as a replacement for a
universal "every migration must have a tested, working down migration"
rule. Until [Phase 14](../enterprise-roadmap.md) ("Upgrade & Compatibility
Proof"), this was undecided — TaskForge had shipped only one
schema-evolution event of real consequence (migration `0003`) with no
in-flight production traffic to test either model against.

That is no longer true. By Phase 14 planning time, `migrations/` holds
fourteen real migrations (`0001`–`0014`), including two consequential,
already-shipped sequences that make this decision concrete rather than
hypothetical:

- The Phase 12 `principal_id` sequence (`0006`–`0010`): a four-file
  expand/backfill/validate/contract dance, specifically because a single
  combined migration would have held an `ACCESS EXCLUSIVE` lock across a
  full-table scan/write ([data-model.md](../data-model.md) "Phase 12
  migration lock profile"). Migration `0010`
  (`drop_global_idempotency_index`) is, by its own file's documented
  design, a down migration that **can legitimately fail**
  ([phase-12-plan.md](../phase-12-plan.md) §5, verification point 13) —
  it is not, and cannot honestly be, an unconditional rollback guarantee.
- The Phase 13 `queue_name` sequence (`0011`–`0014`): an ordinary
  additive-column-plus-index-swap expand/migrate/contract sequence,
  entirely reversible without data loss.

Both sequences already exist in this repository's history, already
labeled (in prose, in their own `.down.sql` files) as
data-safe-reversible or not. This ADR is the formal decision behind that
existing practice, and behind [docs/phase-14-plan.md](../phase-14-plan.md)
§8's CI-enforced version of it — not a new practice invented for Phase 14,
but the first point at which this project commits to it as *policy*
rather than as thirteen independent, ad hoc judgment calls.

## Decision

TaskForge adopts **expand / migrate / contract** as its sole,
primary production schema-compatibility model:

1. **Expand** — ship an additive schema change (new nullable/defaulted
   column, new table, new index) that both the currently-running old
   binary and the new binary can coexist against.
2. **Migrate** — backfill/migrate data if required, with old and new
   binaries both still running against the expanded schema.
3. **Contract** — only in a later, separate migration, once every
   reader/writer of the old shape is confirmed stopped, remove or alter
   the old shape.

A migration's `.down.sql` is required to actually work — and to be
exercised in CI — **only** for migrations classified
`data-safe-reversible`: those where reversing the migration cannot
destroy information already written under the new shape. A migration
that is **not** genuinely safe to reverse (dropping a column/index/table
that new rows may already depend on) is classified `forward-fix-only`:
its remediation path is a new, later migration that fixes forward, never
a down migration pretending to honestly undo it. Every migration file
carries this classification as a machine-checkable marker
([docs/phase-14-plan.md](../phase-14-plan.md) §8), and CI fails a
migration file that carries neither label. This formally **replaces** any
universal "every migration needs a tested down migration" rule — no such
rule is adopted, anywhere, for this project.

Migration ordering is fixed as a required deployment sequence: schema
migration first (confirmed applied), worker/server binary deployment
second — never the reverse, since a new binary's queries may reference
columns/tables that do not yet exist under the old schema.

## Alternatives Considered

- **Universal "every migration must have a tested, working down
  migration."** Rejected. Migration `0010` is the concrete,
  already-shipped counterexample: its down migration recreates a global
  uniqueness constraint that, if tenant-scoped duplicate data has been
  written in the meantime, **cannot** be honestly restored without
  either data loss or a conflict the down migration must surface as a
  failure ([phase-12-plan.md](../phase-12-plan.md) §5). A universal rule
  would force a choice between (a) silently violating the rule in
  practice for migrations like this one, which is a worse outcome than
  not having the rule, since it teaches operators to trust a "tested down
  migration" claim that is sometimes false, or (b) refusing to ever ship
  a migration that cannot honestly satisfy it, which would have blocked
  Phase 12's own idempotency-scoping fix — a change this project needed
  to make. Neither is acceptable.
- **Blue/green schema (fully parallel old and new schema, dual-written,
  cut over atomically).** Rejected as disproportionate to this project's
  actual migration history: fourteen migrations to date, the large
  majority purely additive, with exactly one (`0010`) needing an honest
  "may fail" down path. Blue/green's operational cost (parallel schema
  maintenance, dual-write consistency machinery) has no demonstrated need
  here and would be exactly the kind of "solve today's problem twice"
  this project's own architecture.md "smallest architecture" discipline
  argues against.
- **Dual-write / shadow-table migration pattern for every schema
  change.** Rejected for the same reason as blue/green, at smaller scale:
  TaskForge's write pattern is a single PostgreSQL primary with already
  measured, well-understood lock behavior
  ([data-model.md](../data-model.md) "Phase 12 migration lock profile");
  expand/migrate/contract already delivers the safety this pattern would,
  without its added consistency-window complexity.
- **No formal policy — continue deciding each migration's
  reversibility ad hoc, as has been done through migration `0014`.**
  Rejected. This is [compatibility-policy.md](../compatibility-policy.md)'s
  own stated reason Phase 14 exists at all: "so the policy is decided
  deliberately rather than under incident pressure." An ad hoc practice
  that happens to have been followed correctly fourteen times is not the
  same guarantee as a documented, CI-enforced policy — the next migration
  author has no enforced signal that a `.down.sql` must exist and be
  correctly classified, only a convention they must independently
  discover and choose to follow.

## Consequences

- Positive: every migration now has an unambiguous, CI-checked
  classification at merge time, rather than a reviewer's judgment call
  that leaves no durable trace of *why* a given migration's down path was
  or was not trusted.
- Positive: destructive migrations (the `0010` shape) have an honest,
  named remediation path — a forward-fix migration — instead of an
  implied-but-undeliverable rollback promise. This is a truthful
  guarantee instead of a stronger-sounding but sometimes-false one.
- Positive: this decision is retroactively consistent with all fourteen
  existing migrations without requiring any of them to be rewritten —
  the policy formalizes what `0001`–`0014` already, independently, did
  correctly.
- Negative: rolling back a `forward-fix-only` migration in production
  requires authoring and shipping a new migration under incident
  pressure, not running one pre-written command. This is an accepted
  cost: the alternative (a down migration that claims to reverse
  something it cannot honestly reverse) is a worse outcome under the same
  incident pressure, not a better one.
- Negative: this policy depends on migration authors correctly
  classifying each new migration at authoring time. A migration
  mis-labeled `data-safe-reversible` when it is not is a real residual
  risk this ADR does not eliminate — CI's up→down→up cycle
  ([docs/phase-14-plan.md](../phase-14-plan.md) §8) catches many, not
  all, such mistakes (it proves the down migration *runs* and produces an
  identical schema; it cannot prove no meaningful data was lost in a
  case where the "before" state was itself already populated in a way
  the test fixture does not reproduce).

## Failure Implications

This ADR is the formal decision behind
[docs/phase-14-plan.md](../phase-14-plan.md) §8's CI mechanism (the
`taskforge:down-migration-status` marker and the up→down→up CI job) and
§12's migration-requirements framing. A future migration that appears to
need reversal in production must be evaluated against this ADR's model:
if it is genuinely data-safe-reversible, its `.down.sql` is the
mechanism, proven in CI before it ever reaches production; if it is not,
a new forward-fix migration is the required, designed remediation path —
treating that as friction to be engineered away (rather than the
intended, deliberate behavior this ADR chooses) is a misreading of this
decision, not a gap in it. Any future desire for a universal,
unconditional down-migration guarantee should be treated as a request to
supersede this ADR explicitly, with its consequences re-evaluated in the
open — not implemented as a quiet exception to it.
