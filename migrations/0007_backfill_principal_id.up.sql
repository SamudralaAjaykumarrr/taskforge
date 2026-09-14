-- Phase 12 (docs/phase-12-plan.md §5), step 3: attribute every
-- pre-Phase-12 row to the immutable system principal (OD-1).
--
-- LOCK PROFILE: ROW EXCLUSIVE only -- the whole point of this file
-- ---------------------------------------------------------------------
-- This file and 0009 are the two Phase 12 migrations whose duration scales
-- with the size of the jobs table. This one runs without blocking writes.
-- 0009 is the opposite case: it blocks every write to jobs, worker claims
-- included, for its whole duration. See that file's header before scheduling
-- either.
--
-- A plain UPDATE takes ROW EXCLUSIVE on the table, which conflicts only with
-- SHARE, SHARE UPDATE EXCLUSIVE, SHARE ROW EXCLUSIVE, EXCLUSIVE and ACCESS
-- EXCLUSIVE. It therefore does NOT block:
--
--   * concurrent reads (ACCESS SHARE),
--   * the worker claim query -- which is an UPDATE, not a SELECT
--     (internal/store/claim.go: "WITH candidate AS (SELECT ... FOR UPDATE
--     SKIP LOCKED) UPDATE jobs ...") and so takes ROW EXCLUSIVE, which does
--     not conflict with itself,
--   * other concurrent INSERTs/UPDATEs (ROW EXCLUSIVE),
--
-- and it takes no lock on any row it does not itself touch. On a large
-- table this migration can take a long time; it can do so without taking
-- the deployment offline, which is exactly why it is isolated here instead
-- of sharing a transaction with the ADD COLUMN in 0006 or the ADD
-- CONSTRAINT in 0008, both of which hold ACCESS EXCLUSIVE until commit.
--
-- What this does NOT claim: that it never waits at all. It writes every row,
-- so it takes an ordinary row-level lock on each, and it will wait behind a
-- worker that currently holds one of those rows under SELECT ... FOR UPDATE
-- -- normal MVCC contention, bounded by that one worker transaction, and
-- affecting only that row. The guarantee is about the TABLE lock: readers,
-- the claim query, and every other writer keep running throughout. Nor does
-- it claim a bounded duration: that is table-size and environment dependent,
-- so benchmark it against a realistically-sized copy of your own jobs table
-- and set lock_timeout. See
-- TestPhase12Migrations_BackfillDoesNotBlockReadsOrTheClaimPath.
--
-- This is the ONLY place in TaskForge where a row is attributed to the
-- system principal. No application code path resolves an unset or zero
-- principal to it, and no credential can authenticate as it: migration 0005
-- forbids an api_keys row for it at the schema level and
-- principal.Store.CreateAPIKey refuses to mint one. See
-- txenqueue's TestEnqueueTx_NoDefaultSystemPrincipalPath and
-- internal/principal's TestCreateAPIKey_RefusesTheSystemPrincipal.

UPDATE jobs
SET principal_id = '00000000-0000-0000-0000-000000000001'
WHERE principal_id IS NULL;

UPDATE workflow_instances
SET principal_id = '00000000-0000-0000-0000-000000000001'
WHERE principal_id IS NULL;
