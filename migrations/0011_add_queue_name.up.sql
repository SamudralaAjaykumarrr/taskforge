-- Phase 13 (docs/phase-13-plan.md §7, §11): named queues, step 1 of the
-- expand/migrate/contract sequence. Adds queue_name to jobs and
-- workflow_instances only -- nothing else in this file.
--
-- LOCK PROFILE: ACCESS EXCLUSIVE, catalog-only, NO table rewrite, NO scan
-- ---------------------------------------------------------------------
-- Unlike Phase 12's principal_id (a nullable column with no default, later
-- backfilled row-by-row across four more migrations because each row's
-- correct value differed), every existing row's correct queue_name is the
-- SAME literal, 'default'. PostgreSQL 11+ implements
-- `ADD COLUMN ... NOT NULL DEFAULT '<literal>'` as a catalog-only
-- operation when the default is a non-volatile constant: the new column's
-- value is recorded once in the catalog (pg_attribute.atthasmissing /
-- attmissingval), not written into every existing row, so there is no
-- table rewrite and no full-table scan -- the same optimization
-- docs/data-model.md's "Phase 12 migration lock profile" section already
-- relies on for migration 0006's reasoning. The ACCESS EXCLUSIVE lock this
-- statement takes is real (any DDL against jobs/workflow_instances takes
-- it), but it is held for O(1) work, not work proportional to table size --
-- see TestPhase13Migrations_0011IsCatalogOnly for the measured proof
-- against a non-trivially-sized table, not an assumption.
--
-- This is called out explicitly because a future reviewer familiar with
-- Phase 12's six-file principal_id story might otherwise assume queue_name
-- needs the same NOT VALID/VALIDATE/backfill split. It does not: there is
-- no backfill because there is nothing to compute per row.
--
-- Deployment note, stated rather than glossed, exactly as 0006's did: like
-- all DDL this statement must WAIT to acquire ACCESS EXCLUSIVE if any
-- transaction currently holds a conflicting lock, and while it waits
-- PostgreSQL queues new lock requests behind it. Fast once acquired, not
-- necessarily fast to acquire on a busy table. Set lock_timeout and retry,
-- or deploy in a quiet window.

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS queue_name TEXT NOT NULL DEFAULT 'default';

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS queue_name TEXT NOT NULL DEFAULT 'default';

-- OD-8 (docs/phase-13-plan.md §18): queue_name is free-form TEXT, no
-- registry/allow-list table -- mirrors the existing job_type precedent
-- exactly (no job_types table exists either). Application-layer length
-- bounds (mirroring job_type's MaxJobTypeLength) live in internal/job.
