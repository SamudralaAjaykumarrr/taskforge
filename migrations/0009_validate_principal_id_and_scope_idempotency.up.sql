-- Phase 12 (docs/phase-12-plan.md §5), step 5: prove every existing row
-- conforms, and build every index this phase needs.
--
-- LOCK PROFILE: no ACCESS EXCLUSIVE -- but this file DOES BLOCK ALL WRITES,
-- INCLUDING THE WORKER CLAIM QUERY, for its entire duration.
-- ---------------------------------------------------------------------
-- Read that heading before scheduling this migration. An earlier revision of
-- this comment claimed the opposite, and an independent review measured the
-- real behaviour against this exact file: on a 3M-row / 426 MB jobs table the
-- real worker claim query hit lock_timeout (SQLSTATE 55P03) rather than
-- claiming a job. The claim below is the corrected one.
--
--   VALIDATE CONSTRAINT  -> SHARE UPDATE EXCLUSIVE for a full table scan.
--        Taken on its own this statement blocks neither reads nor writes.
--        That is the property the NOT VALID/VALIDATE two-step exists to buy,
--        and it is real here only because 0008 committed and released its
--        ACCESS EXCLUSIVE lock before this file began. It is a property of
--        this STATEMENT, not of this FILE -- see below.
--
--   CREATE INDEX / CREATE UNIQUE INDEX -> SHARE for each build. SHARE
--        conflicts with ROW EXCLUSIVE, which EVERY write to jobs requires.
--        CREATE INDEX CONCURRENTLY would avoid that, but cannot run inside a
--        transaction block, and every migration here runs inside one
--        (TF-INV-013).
--
-- WHY THE FILE IS WORSE THAN ITS FIRST STATEMENT: internal/migrate runs this
-- whole file in one transaction and PostgreSQL releases locks only at commit,
-- so the SHARE taken by the first CREATE INDEX is held until the file
-- commits. The write stall is therefore NOT "per index build" -- it runs from
-- the first CREATE INDEX to the end of the migration.
--
-- AND THE WORKER CLAIM QUERY IS A WRITE. internal/store/claim.go's claimQuery
-- reads
--        WITH candidate AS (SELECT ... FOR UPDATE SKIP LOCKED) UPDATE jobs ...
-- The CTE's SELECT takes ROW SHARE, which IS compatible with SHARE -- and
-- that compatibility is exactly what misled the previous version of this
-- comment. The statement as a whole is an UPDATE and takes ROW EXCLUSIVE,
-- which is NOT. While this file runs, workers cannot claim, complete, or
-- heartbeat, and cmd/api cannot submit. Only reads (ACCESS SHARE) get
-- through.
--
-- DEPLOYMENT OBLIGATIONS, stated rather than glossed. Duration here is
-- proportional to table size and entirely data/environment dependent; no test
-- can establish it for your data:
--   * Benchmark this migration against a realistically-sized copy of your own
--     jobs table before running it against production.
--   * Set lock_timeout and retry rather than queueing traffic indefinitely.
--   * Run it in a quiet or maintenance window sized by that benchmark, and
--     expect worker claiming to stop for its duration.
--
-- TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery asserts this
-- blocking behaviour through the real migrate.Up, so a future change that
-- quietly reintroduces the false "does not block the claim query" claim fails
-- the suite instead of shipping.
--
-- The DROP of the old global idempotency index is deliberately NOT here: it
-- needs ACCESS EXCLUSIVE, and putting it at the end of this file would make
-- the entire transaction -- including the scan work already done -- queue
-- for that lock, blocking every new reader behind it too. It is 0010.

ALTER TABLE jobs VALIDATE CONSTRAINT jobs_principal_id_not_null;
ALTER TABLE workflow_instances VALIDATE CONSTRAINT workflow_instances_principal_id_not_null;

-- Support the principal-scoped WHERE predicate every read and every
-- cancellation path gains in this phase (docs/phase-12-plan.md §4a).
CREATE INDEX IF NOT EXISTS idx_jobs_principal_id
    ON jobs (principal_id);

CREATE INDEX IF NOT EXISTS idx_workflow_instances_principal_id
    ON workflow_instances (principal_id);

-- Tenant-scoped submission idempotency (G7, TF-INV-008/TF-INV-016's
-- mechanism, re-scoped). Created only now, once principal_id is provably
-- NOT NULL above, so every row -- backfilled or new -- carries a real
-- principal and the composite index behaves as an ordinary tenant-scoped
-- constraint with no special-cased "unscoped" rows and no NULL-widening
-- loophole.
--
-- internal/store/idempotency.go's idempotencyKeyIndexName constant is
-- matched against pgErr.ConstraintName by name, exactly: it names
-- 'idx_jobs_idempotency_scoped'. If the two ever drift apart,
-- InsertIdempotent's conflict-recovery path stops recognizing idempotency
-- conflicts and falls through to a generic insert failure instead of
-- returning the existing row -- see
-- TestInsertIdempotent_ConflictRecoveryStillWorksAfterIndexRename.
--
-- Both this index and migration 0001's global one are live between this
-- migration and 0010. That window is closed within a single migrate.Up run,
-- and the stricter (global) constraint is the one still enforcing during it,
-- so no cross-tenant collision can slip through the gap.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_idempotency_scoped
    ON jobs (principal_id, job_type, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
