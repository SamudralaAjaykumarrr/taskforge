-- Phase 13 (docs/phase-13-plan.md §7, §11), step 2 of the expand/migrate/
-- contract sequence: build the queue-aware claimable index, in its own
-- file and its own transaction, isolated from 0011's column add and from
-- 0014's later DROP of the old index -- mirroring Phase 12's 0009/0010
-- split precedent (a new index is built and proven live before the old
-- one is retired, and a lock-bearing statement never shares a file with
-- unrelated DDL).
--
-- LOCK PROFILE: SHARE, held for the duration of the build, work
-- proportional to table size
-- ---------------------------------------------------------------------
-- CREATE INDEX (not CONCURRENTLY -- every migration here runs inside one
-- transaction, TF-INV-013, and CREATE INDEX CONCURRENTLY cannot run inside
-- a transaction block) takes SHARE for its whole duration. SHARE
-- conflicts with ROW EXCLUSIVE, which every write to jobs requires --
-- including the worker claim query (an UPDATE, not the SELECT its
-- candidate CTE resembles -- see migration 0009's comment for the exact
-- same reasoning, already measured and pinned by
-- TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery). This file
-- blocks writes to jobs, including claiming, completing, and heartbeating,
-- for its own duration -- proportional to jobs' current size, not O(1).
-- Reads (ACCESS SHARE) are unaffected.
--
-- This is NOT asserted by analogy to 0009 alone: it is measured directly
-- against this migration's own file by
-- TestPhase13Migrations_0012BlocksWritesAndTheClaimQuery, per this
-- project's own "never assert a lock property without measuring it"
-- discipline (docs/data-model.md "Phase 12 migration lock profile," the
-- two retracted claims that established this rule).
--
-- DEPLOYMENT OBLIGATIONS, stated rather than glossed, identical in kind to
-- 0009's: benchmark this migration against a realistically-sized copy of
-- your own jobs table before running it in production, set lock_timeout
-- and retry rather than queueing traffic indefinitely, and run it in a
-- quiet or maintenance window sized by that benchmark.
--
-- The OLD idx_jobs_claimable index (no queue_name column) is deliberately
-- NOT dropped here -- it remains correct, just non-optimal, until the
-- claim query is actually cut over to filter by queue_name in the same
-- release. Dropping it is migration 0014, isolated in its own file for
-- the same reason this file is isolated from 0011.
--
-- A SECOND index is built in this same file, for the same reason and under
-- the same SHARE lock: the queue-aware claim query's per-queue candidate
-- scan (internal/store/claim.go) evaluates a fresh QUEUED/RETRY_WAIT claim
-- and an expired-lease RUNNING reclaim as two SEPARATELY indexed branches,
-- combined with UNION ALL, rather than one `WHERE (...) OR (...)` spanning
-- both partial indexes. This mirrors a defect this phase's own concurrency
-- evidence pass found and fixed (docs/phase-13-concurrency-evidence-v2.md,
-- claim_concurrency.go's claimableWhere comment): an OR spanning two
-- differently-shaped partial indexes plus a shared ORDER BY defeats index
-- usage entirely and forces a sequential scan and an external sort at
-- realistic backlog size (measured there at 69.8ms at 300,000 rows vs.
-- 0.24ms for the per-branch-index fix). idx_jobs_claimable_by_queue serves
-- the fresh-claim branch; idx_jobs_reclaimable_by_queue below serves the
-- reclaim branch -- the queue-scoped analog of the existing
-- idx_jobs_expired_lease (migration 0001), which has no queue_name column
-- and is retained unchanged for any pre-Phase-13 code path.
CREATE INDEX IF NOT EXISTS idx_jobs_claimable_by_queue
    ON jobs (queue_name, priority DESC, eligible_at ASC)
    WHERE state IN ('QUEUED', 'RETRY_WAIT');

CREATE INDEX IF NOT EXISTS idx_jobs_reclaimable_by_queue
    ON jobs (queue_name, lease_expires_at)
    WHERE state = 'RUNNING';
