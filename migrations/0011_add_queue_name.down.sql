-- taskforge:down-migration-status: data-safe-reversible
-- Data-safe-reversible (docs/compatibility-policy.md): dropping an
-- additive column with a fixed default loses no information a rollback
-- would need to preserve -- every row's queue_name is 'default' or a
-- caller-chosen value that this rollback intentionally discards, exactly
-- like rolling back any other additive Phase 13 column.

ALTER TABLE workflow_instances
    DROP COLUMN IF EXISTS queue_name;

ALTER TABLE jobs
    DROP COLUMN IF EXISTS queue_name;
