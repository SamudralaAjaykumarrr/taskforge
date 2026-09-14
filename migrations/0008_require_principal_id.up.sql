-- Phase 12 (docs/phase-12-plan.md §5), step 4: declare principal_id
-- mandatory, without validating it yet.
--
-- LOCK PROFILE: brief ACCESS EXCLUSIVE, catalog-only, NO table scan
-- ---------------------------------------------------------------------
-- ADD CONSTRAINT ... NOT VALID records the constraint in pg_constraint and
-- returns. It takes ACCESS EXCLUSIVE -- unavoidable for any ALTER TABLE --
-- but it reads no heap pages, so the lock is held for O(1) work. From this
-- point on, every NEW row is checked; EXISTING rows are not yet proven to
-- conform, which is what VALIDATE CONSTRAINT in 0009 establishes.
--
-- Why this is a separate file from 0009's VALIDATE, and not merely a
-- separate statement:
--
--   internal/migrate applies each .up.sql inside one transaction, and
--   PostgreSQL releases locks only at commit. Putting ADD CONSTRAINT and
--   VALIDATE CONSTRAINT in the same file therefore holds THIS statement's
--   ACCESS EXCLUSIVE lock across the full-table scan the next one performs
--   -- which defeats the entire reason for choosing the two-step form over
--   a direct ALTER COLUMN ... SET NOT NULL. The first implementation of
--   this phase did exactly that and still claimed the non-blocking
--   property in README.md and docs/data-model.md; an independent review
--   measured it and found the opposite. Splitting the two steps across two
--   files is what makes the claim true, because the commit at the end of
--   this file releases the lock before 0009 begins.
--
-- Deployment note, stated rather than glossed: like all DDL, this statement
-- must WAIT to acquire ACCESS EXCLUSIVE if any transaction currently holds
-- a conflicting lock on the table, and while it waits it queues new lock
-- requests behind it. It is fast once acquired, not fast to acquire on a
-- busy table. Set lock_timeout and retry, or deploy in a quiet window.

ALTER TABLE jobs
    ADD CONSTRAINT jobs_principal_id_not_null
    CHECK (principal_id IS NOT NULL) NOT VALID;

ALTER TABLE workflow_instances
    ADD CONSTRAINT workflow_instances_principal_id_not_null
    CHECK (principal_id IS NOT NULL) NOT VALID;
