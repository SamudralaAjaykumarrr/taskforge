-- Phase 12 (docs/phase-12-plan.md §5), step 2 of the migration file plan:
-- add the principal_id columns. Nothing else.
--
-- LOCK PROFILE: ACCESS EXCLUSIVE, catalog-only, NO table scan
-- ---------------------------------------------------------------------
-- ADD COLUMN of a nullable column with no DEFAULT is a catalog-only
-- operation in PostgreSQL 11+: it takes ACCESS EXCLUSIVE but performs no
-- table rewrite and no scan, so the lock is held for O(1) work. The foreign
-- key is likewise not validated against existing rows, because every value
-- in a newly added column is NULL.
--
-- This file contains NOTHING else, and that is the point. internal/migrate
-- wraps each .up.sql in one transaction and PostgreSQL releases locks only
-- at commit, so anything sharing this file runs under the ACCESS EXCLUSIVE
-- lock taken above. The first implementation of this phase put the
-- data-volume-dependent backfill here, which meant the entire backfill --
-- and therefore an outage for every read and for the worker claim query --
-- ran under that lock, while README.md claimed the opposite. An independent
-- review measured it. The backfill is now 0007 and the index builds are now
-- 0009, each with its own transaction and its own, weaker, lock. "Weaker"
-- means "not ACCESS EXCLUSIVE" and nothing more: 0009's SHARE still blocks
-- every write to jobs, the worker claim query included, for that file's
-- whole duration. See its header.
--
-- Deployment note, stated rather than glossed: like all DDL this statement
-- must WAIT to acquire ACCESS EXCLUSIVE if any transaction currently holds a
-- conflicting lock on the table, and while it waits PostgreSQL queues new
-- lock requests behind it. It is fast once acquired, not necessarily fast to
-- acquire on a busy table. Set lock_timeout and retry, or deploy in a quiet
-- window.

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS principal_id UUID REFERENCES principals(id);

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS principal_id UUID REFERENCES principals(id);
