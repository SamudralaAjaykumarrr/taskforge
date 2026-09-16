-- Phase 13 (docs/phase-13-plan.md §7, ADR-0009 "Concurrency mechanism:
-- slot-table" / "Fairness mechanism: last_claimed_at ascending"): the four
-- new governance tables. All brand new -- creating each one takes ACCESS
-- EXCLUSIVE only on itself, which nothing else can be contending for since
-- the table does not exist until that statement runs.
--
-- LOCK PROFILE ON PRE-EXISTING TABLES: NOT ZERO -- measured, not assumed
-- ---------------------------------------------------------------------
-- An earlier draft of this comment claimed this file takes "no lock at
-- all" on jobs. That claim is FALSE and was caught by this file's own
-- lock test (TestPhase13Migration0013_TakesNoLockOnJobsOrWorkflowInstances),
-- exactly the "never assert a lock property without measuring it"
-- discipline docs/data-model.md's "Phase 12 migration lock profile"
-- section exists to enforce. `queue_slots.held_by_job_id ... REFERENCES
-- jobs(id)` and `queue_limits.principal_id ... REFERENCES principals(id)`
-- are foreign keys FROM a brand-new table TO an existing one -- and
-- creating a foreign-key constraint takes SHARE ROW EXCLUSIVE on the
-- REFERENCED table (jobs, principals), not merely on the new table, so
-- PostgreSQL can protect the constraint's validity against concurrent
-- modification while it is created. SHARE ROW EXCLUSIVE conflicts with the
-- ROW EXCLUSIVE every write to jobs needs, including the real claim
-- query's UPDATE -- so this file DOES block writes to jobs (and, via the
-- queue_limits FK, to principals) for its own duration. Reads (ACCESS
-- SHARE) are unaffected: SHARE ROW EXCLUSIVE does not conflict with it.
--
-- This is still the cheapest possible instance of that lock: both new
-- tables are empty at creation time, so validating the (vacuously
-- satisfied) constraint against zero existing queue_slots/queue_limits
-- rows is O(1) -- there is nothing to scan in the new table, and the
-- referenced table (jobs/principals) is not scanned at all for a freshly
-- created FK on an empty child table. The lock is real but brief -- proved
-- by measuring actual duration against a non-trivially-sized jobs table,
-- not asserted by analogy.
--
-- queue_state: ADR-0009's last_claimed_at fairness column, one row per
-- known queue_name. "Known" means: every queue_name that has ever had a
-- job or workflow submitted to it (internal/store upserts a row here, in
-- the SAME transaction as the job INSERT, the instant a new queue_name is
-- first used -- see internal/store/idempotency.go, tx.go, workflow.go).
-- This table is also the fairness roster's domain: the claim query's
-- per-queue candidate scan iterates queue_state rows (optionally filtered
-- by a worker's queue subscription), never an expensive
-- SELECT DISTINCT queue_name FROM jobs.
--
-- last_claimed_at defaults to '-infinity', per ADR-0009's "newly eligible
-- queue" rule: a queue that has never been served is immediately the
-- most-stale roster member the instant it becomes pending and
-- capacity-eligible, so its very first turn costs it zero E-1 wait.
CREATE TABLE IF NOT EXISTS queue_state (
    queue_name      TEXT PRIMARY KEY,
    last_claimed_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity'
);

-- Seed the 'default' queue's row now, so a freshly migrated deployment
-- has a non-empty fairness roster even before any Phase-13-aware code
-- path has inserted a job -- every pre-Phase-13 row already carries
-- queue_name = 'default' (migration 0011), and this is the row that lets
-- the claim query discover them without a first insert-time upsert ever
-- having run.
INSERT INTO queue_state (queue_name) VALUES ('default')
ON CONFLICT (queue_name) DO NOTHING;

-- queue_limits: static, operator-set configuration (docs/phase-13-plan.md
-- §7's strawman, adopted as final -- ADR-0009 selected the slot-table
-- mechanism this table's concurrency_limit column feeds).
--
-- id is application-supplied (uuid.New() in Go), matching this codebase's
-- existing convention (internal/store/claim.go, idempotency.go) rather
-- than a DB-side default.
--
-- principal_id NULL means "queue-wide default" -- deliberately NOT part
-- of a composite PRIMARY KEY (PostgreSQL forces every PRIMARY KEY column
-- NOT NULL, so a NULL-containing key cannot represent the queue-wide row
-- this table needs). Two partial unique indexes give the same guarantee
-- (at most one queue-wide row, at most one row per (queue_name,
-- principal_id) pair) without a NULL-containing key -- docs/phase-13-plan.md
-- §7's own correction of an earlier, broken draft.
--
-- Per-principal concurrency/rate-limit enforcement is an explicit Phase 13
-- non-goal (OD-2, still open) -- this table's schema supports it from day
-- one, but only the queue-wide (principal_id IS NULL) row is read by any
-- Phase 13 code path. A non-NULL principal_id row may be written (the
-- schema allows it) but nothing enforces it yet.
CREATE TABLE IF NOT EXISTS queue_limits (
    id                 UUID PRIMARY KEY,
    queue_name         TEXT NOT NULL,
    principal_id       UUID NULL REFERENCES principals(id),
    concurrency_limit  INTEGER NULL,
    rate_limit_per_sec NUMERIC NULL,
    rate_limit_burst   INTEGER NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Fails closed on misconfiguration (docs/phase-13-plan.md §13):
    -- concurrency_limit = 0 is rejected at the application layer
    -- (internal/governance) with a distinct error from "unset/unlimited"
    -- (NULL) before this row is ever written; this CHECK is the schema
    -- backstop so a direct INSERT bypassing that validation still cannot
    -- silently create a "zero concurrency" row that is indistinguishable
    -- from "no limit at all."
    CONSTRAINT queue_limits_concurrency_limit_not_zero
        CHECK (concurrency_limit IS NULL OR concurrency_limit > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_limits_scoped
    ON queue_limits (queue_name, principal_id) WHERE principal_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_limits_default
    ON queue_limits (queue_name) WHERE principal_id IS NULL;

-- queue_slots: ADR-0009's slot-table concurrency semaphore. Exactly one
-- row per unit of a queue's configured concurrency_limit, provisioned/
-- deprovisioned by cmd/taskforge-admin's set-queue-limit (idempotent DML,
-- not a migration -- see docs/phase-13-plan.md §11's "rollback concern
-- specific to this phase"). A queue with no rows here has no configured
-- limit and is unconditionally capacity-eligible -- the claim query never
-- queries queue_limits at all; "does this queue have any provisioned
-- slots" is answered entirely from this table, which is also why a
-- pre-Phase-13 ('default', unconfigured) queue's claim behavior is
-- unaffected: zero queue_slots rows for 'default' means the slot
-- mechanism is never consulted for it.
--
-- held_by_job_id REFERENCES jobs(id): a slot is either free (NULL) or
-- held by exactly one currently-RUNNING job -- SF-053's per-row
-- consistency proof is checkable directly against this column.
CREATE TABLE IF NOT EXISTS queue_slots (
    queue_name    TEXT NOT NULL,
    slot_index    INTEGER NOT NULL,
    held_by_job_id UUID NULL REFERENCES jobs(id),

    PRIMARY KEY (queue_name, slot_index)
);

-- Supports the claim query's targeted, per-job slot release (completion/
-- cancellation/dead-letter paths release "the slot this job_id holds,"
-- not "some slot in this queue") and SF-053's per-row proof query.
CREATE INDEX IF NOT EXISTS idx_queue_slots_held_by_job_id
    ON queue_slots (held_by_job_id) WHERE held_by_job_id IS NOT NULL;

-- rate_limit_buckets: durable token-bucket state for Phase 13's rate
-- limiting, deliberately a SEPARATE table from queue_limits (static,
-- operator-set configuration) because this one is continuously-mutated
-- runtime state -- the same table-role separation this codebase already
-- draws between principals (identity, low-churn) and api_keys (also
-- low-churn, but distinct) in Phase 12. Durable across a process restart
-- (docs/phase-13-plan.md §7: "must survive a worker/API-server restart,"
-- consistent with ADR-0001 -- PostgreSQL is the only datastore).
--
-- scope_key is a free-form text key (e.g. "queue:emails" or
-- "principal:<uuid>:queue:emails") so OD-9's rate-limit scope-key
-- composition question does not require a schema change either way it is
-- eventually answered.
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    scope_key      TEXT PRIMARY KEY,
    tokens         NUMERIC NOT NULL,
    last_refill_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
