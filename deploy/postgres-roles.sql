-- TaskForge Phase 12 (G4, docs/phase-12-plan.md §4; docs/security-model.md
-- §2 "Least-privilege roles, and what they do and do not buy"):
-- least-privilege PostgreSQL roles for the API server and the worker fleet.
--
-- This is OPERATOR PROVISIONING, not a migration. It is deliberately not in
-- migrations/: migrations run as the application's own role and describe
-- the schema, whereas this script grants privileges and must be run once by
-- a superuser/owner when a deployment is set up (and re-run after a
-- migration adds a new table). Keeping it out of the migration runner also
-- means a developer running `go test` against a throwaway database is never
-- forced to have these roles.
--
-- ============================================================
-- WHAT THIS BUYS, AND WHAT IT DOES NOT
-- ============================================================
--
-- Buys: a compromised worker process cannot read or write the credential
-- tables, cannot DELETE any row, and cannot change the schema. A compromised
-- API server cannot claim jobs or write attempt records. That bounds the
-- blast radius of either process being compromised.
--
-- Does NOT buy: containment of HOSTILE WORKER CODE. A worker still holds
-- direct SELECT on jobs, so it can read every tenant's payload, and direct
-- UPDATE, so it can affect jobs outside its intended job_type set. The
-- containment value here comes entirely from the PRIVILEGE DIFFERENCE
-- between the two roles, not from the fact that they have different
-- passwords. Fully isolating untrusted worker code would need an
-- architectural boundary this phase does not build -- a worker-facing
-- gateway service, or a stored-function API granting EXECUTE but no table
-- access -- and TaskForge's Enterprise Deployment Profile explicitly assumes
-- a trusted first-party worker fleet instead (docs/security-model.md).
-- Stated here rather than left to be inferred.
--
-- ============================================================
-- USAGE
-- ============================================================
--
--   1. Apply TaskForge's migrations first (cmd/api or cmd/worker does this
--      automatically on startup, running as the owner role). The GRANTs
--      below name tables that must already exist.
--   2. Set real passwords -- the placeholders below will not authenticate.
--   3. psql "$OWNER_DATABASE_URL" -f deploy/postgres-roles.sql
--   4. Point cmd/api at taskforge_api and cmd/worker at taskforge_worker.
--
-- Re-run after any future migration that adds a table, or that table will
-- be unreachable by both roles.

-- ------------------------------------------------------------
-- Roles
-- ------------------------------------------------------------
-- Passwords are placeholders. Replace them, or create the roles separately
-- with your own secret-management tooling and run only the GRANTs below.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'taskforge_api') THEN
        CREATE ROLE taskforge_api LOGIN PASSWORD 'CHANGE_ME_api';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'taskforge_worker') THEN
        CREATE ROLE taskforge_worker LOGIN PASSWORD 'CHANGE_ME_worker';
    END IF;
END
$$;

-- GRANT ... ON DATABASE takes a literal identifier, not an expression, so
-- the current database's name is interpolated rather than written inline --
-- this script must work against whatever database TaskForge was deployed
-- into, not a hardcoded "taskforge".
DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO taskforge_api, taskforge_worker', current_database());
END
$$;

GRANT USAGE ON SCHEMA public TO taskforge_api, taskforge_worker;

-- Neither role may create objects in the schema. Migrations run as the
-- owner, not as either of these.
REVOKE CREATE ON SCHEMA public FROM taskforge_api, taskforge_worker;

-- ------------------------------------------------------------
-- taskforge_api: the HTTP surface
-- ------------------------------------------------------------
-- Submits jobs and workflows, reads them back, and records cancellation
-- requests. It authenticates callers, so it needs to READ api_keys and
-- principals -- but it never mints or revokes credentials (that is
-- cmd/taskforge-admin, run by an operator under the owner role), so it gets
-- SELECT only on principals and, on api_keys, SELECT plus the single UPDATE
-- it actually performs.
GRANT SELECT, INSERT, UPDATE ON jobs                TO taskforge_api;
GRANT SELECT, INSERT, UPDATE ON workflow_instances  TO taskforge_api;
GRANT SELECT, INSERT          ON workflow_nodes     TO taskforge_api;
GRANT SELECT                  ON job_attempts       TO taskforge_api;
GRANT SELECT                  ON principals         TO taskforge_api;
GRANT SELECT                  ON api_keys           TO taskforge_api;

-- last_used_at is best-effort telemetry written on a successful
-- verification (internal/principal.Store.touchLastUsed). Column-scoped, so
-- a compromised API server cannot un-revoke a key, extend an expiry, or
-- rewrite a secret_hash.
GRANT UPDATE (last_used_at) ON api_keys TO taskforge_api;

-- ------------------------------------------------------------
-- taskforge_worker: the execution fleet
-- ------------------------------------------------------------
-- Claims jobs, heartbeats, records attempt outcomes, and drives workflow
-- dependency propagation. It has NO access to the credential tables at all:
-- worker identity is the database role itself (OD-3), so a worker never
-- needs to look a principal or an API key up, and must not be able to.
GRANT SELECT, INSERT, UPDATE ON jobs               TO taskforge_worker;
GRANT SELECT, INSERT, UPDATE ON job_attempts       TO taskforge_worker;
GRANT SELECT, UPDATE          ON workflow_instances TO taskforge_worker;
GRANT SELECT                  ON workflow_nodes     TO taskforge_worker;

-- Explicitly denied, and named rather than merely omitted, so a reviewer
-- can see the decision: a worker cannot read or write credentials.
REVOKE ALL ON principals, api_keys FROM taskforge_worker;

-- ------------------------------------------------------------
-- Neither role may DELETE anything
-- ------------------------------------------------------------
-- No TaskForge code path issues a DELETE against any of these tables
-- (TF-INV-005: terminal states are never reopened, and rows are never
-- removed). Retention/cleanup is a Phase 13 concern and will get its own,
-- narrower role when it exists -- it is not granted here in advance.
REVOKE DELETE ON jobs, job_attempts, workflow_instances, workflow_nodes, principals, api_keys
    FROM taskforge_api, taskforge_worker;

-- ------------------------------------------------------------
-- schema_migrations
-- ------------------------------------------------------------
-- cmd/api and cmd/worker both call migrate.Up on startup. Applying a
-- migration needs owner privileges, which these roles deliberately do not
-- have -- but STARTING does not, and must not.
--
-- migrate.Up therefore reads this ledger before it writes anything: it
-- probes pg_catalog for schema_migrations rather than issuing an
-- unconditional CREATE TABLE IF NOT EXISTS, because PostgreSQL performs the
-- schema-level CREATE privilege check BEFORE the IF NOT EXISTS
-- short-circuit. Without that probe, a role correctly denied CREATE could
-- not start even against a fully migrated database with nothing to do --
-- which is exactly the defect an independent review found here. SELECT on
-- this ledger is all a least-privilege process needs to confirm the schema
-- is current and continue.
--
-- Deployment consequence, stated plainly and now actually true:
--
--   * Against an ALREADY-MIGRATED database, starting under taskforge_api or
--     taskforge_worker works: migrate.Up finds every version recorded,
--     applies nothing, and returns. This is the steady-state deployment.
--   * Against a database with a PENDING migration, a process started under
--     either role fails loudly at startup with a PostgreSQL
--     insufficient_privilege error (SQLSTATE 42501) and applies nothing.
--     That is the correct outcome, not something to work around by widening
--     these grants: schema changes are a separate, owner-privileged
--     deployment step (run either binary once under the owner role, or
--     apply migrations/ with psql) and must happen BEFORE the least-
--     privilege processes start. Failing to start beats coming up against a
--     schema the binary was not built for.
--
-- Both behaviours are covered by TestMigrateUp_RunsUnderLeastPrivilegeRoles
-- and TestMigrateUp_UnderLeastPrivilegeRole_CannotApplyAPendingMigration,
-- which connect as these roles for real rather than only asking the catalog
-- what they are allowed to do.
GRANT SELECT ON schema_migrations TO taskforge_api, taskforge_worker;
