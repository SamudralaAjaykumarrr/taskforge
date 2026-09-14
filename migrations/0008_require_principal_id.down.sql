-- Dropping a check constraint is catalog-only and always safe: it removes a
-- restriction, loses no data, and re-running 0008's up restores it (0009's
-- up then re-validates it).
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_principal_id_not_null;
ALTER TABLE workflow_instances DROP CONSTRAINT IF EXISTS workflow_instances_principal_id_not_null;
