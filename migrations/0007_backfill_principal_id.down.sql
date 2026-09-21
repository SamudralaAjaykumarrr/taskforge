-- taskforge:down-migration-status: data-safe-reversible
-- The faithful inverse of the backfill: return exactly the rows this
-- migration attributed to the system principal to NULL.
--
-- This is the inverse of the only statement that ever assigns the system
-- principal, and no HTTP caller's row can carry that identity: authenticating
-- as it is impossible (migration 0005's api_keys_no_system_principal CHECK
-- plus principal.Store.CreateAPIKey's guard), and the API handlers set
-- principal_id solely from the verified credential.
--
-- One honest caveat rather than an absolute claim: in-process Go code that
-- already holds a database connection -- a txenqueue integrator, or a direct
-- internal/store call -- CAN pass principal.SystemPrincipalID explicitly. That
-- is a deliberate act by code with full database access, not a credential
-- escalation, and txenqueue is deliberately forbidden from even referencing
-- that constant (TestTxenqueuePackage_ContainsNoSystemPrincipalFallback), so
-- it cannot happen by accident. If a deployment has done it on purpose, this
-- rollback returns those rows to NULL along with the backfilled ones.
--
-- It must run after 0008's down has dropped the NOT NULL check constraints;
-- running it before them fails loudly on the constraint, which is correct.
UPDATE jobs
SET principal_id = NULL
WHERE principal_id = '00000000-0000-0000-0000-000000000001';

UPDATE workflow_instances
SET principal_id = NULL
WHERE principal_id = '00000000-0000-0000-0000-000000000001';
