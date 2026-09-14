-- Data-safe reversible: the columns are wholly additive and re-running
-- 0006's up recreates them exactly (0007 then re-derives the same backfill
-- for every row). Dropping a column drops its indexes with it.
ALTER TABLE jobs DROP COLUMN IF EXISTS principal_id;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS principal_id;
