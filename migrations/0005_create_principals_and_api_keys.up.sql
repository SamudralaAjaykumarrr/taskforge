-- Phase 12 (docs/phase-12-plan.md §5, docs/enterprise-roadmap.md "Phase 12
-- -- Security & Trust Boundaries", docs/security-model.md §1): the two
-- tables that carry TaskForge's application/API trust boundary --
-- principals (who is calling) and api_keys (how they prove it).
--
-- This migration adds no column to, and no constraint on, any existing
-- table: it is purely additive, so applying it to a running deployment
-- cannot affect any claim/complete/cancel path. The columns that bind
-- existing rows to a principal arrive in 0006, the backfill in 0007, the
-- NOT NULL constraints in 0008 (declared NOT VALID) and 0009 (validated,
-- alongside the index builds), and the retirement of the old global
-- idempotency index in 0010 -- six files rather than three, deliberately,
-- because internal/migrate holds every lock a file takes until that file
-- commits, so no ACCESS EXCLUSIVE statement may share a file with a
-- full-table scan or write. See docs/phase-12-plan.md §5 "Why six files,
-- not three" and §5a for the measured locking semantics.
--
-- The whole file runs inside one transaction (internal/migrate's applyOne
-- wraps every .up.sql in its own transaction, TF-INV-013), so either both
-- tables and the system-principal seed row exist afterwards, or none of
-- them do.

CREATE TABLE IF NOT EXISTS principals (
    id            UUID PRIMARY KEY,
    kind          TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ NULL,

    -- OD-3 (docs/phase-12-plan.md): there is deliberately NO 'worker'
    -- kind. A worker process's identity is its PostgreSQL role
    -- credential, verified by PostgreSQL itself at connection time --
    -- an infrastructure/database trust boundary, in a different layer,
    -- checked by a different system than this table's application/API
    -- trust boundary (G3). Adding a 'worker' principal kind here "for
    -- symmetry" would create exactly the conflation OD-3 rejects: an
    -- application credential that looks like it could authenticate a
    -- worker, when nothing in the worker path ever consults this table.
    CONSTRAINT principals_kind_check CHECK (kind IN ('caller', 'admin'))
);

-- The system principal (OD-1). A fixed, well-known, documented UUID --
-- exported as principal.SystemPrincipalID in Go, so no code path ever
-- needs a runtime lookup to find it -- seeded by this migration so that
-- 0007's backfill has a valid foreign-key target the instant it runs.
--
-- This row is never deleted and never revoked -- principal.Store's
-- RevokePrincipal refuses it explicitly. It has no api_keys row and cannot
-- get one: it is not a credential-holding caller, it is the durable
-- identity every pre-Phase-12 row is attributed to.
--
-- "Nothing can authenticate AS the system principal" is enforced, not just
-- asserted, in two independent places: the api_keys_no_system_principal
-- CHECK constraint below (schema level, effective even against a direct
-- INSERT) and principal.Store.CreateAPIKey's guard (application level).
-- Authentication additionally requires an api_keys row that cannot exist,
-- so backfilled rows are unreachable by any credential a caller could
-- present. Before those guards existed this paragraph was aspirational --
-- an independent review demonstrated a working credential for this exact
-- principal -- which is why the property is now checked twice and tested.
--
-- ON CONFLICT DO NOTHING keeps this statement safe if a future operator
-- ever seeds the same row by hand before migrating.
INSERT INTO principals (id, kind, display_name)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'caller',
    'system (pre-Phase-12 backfill; not a real caller)'
)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS api_keys (
    id            UUID PRIMARY KEY,
    principal_id  UUID NOT NULL REFERENCES principals(id),

    -- key_id is the NON-SECRET half of the Bearer credential
    -- (docs/phase-12-plan.md §6a: "Authorization: Bearer
    -- <key_id>.<secret>"). It exists so a specific key can be looked up,
    -- rotated, or revoked without ever comparing raw secrets, and so the
    -- verification path's database lookup is keyed on a non-secret value
    -- -- no raw secret is ever used as a lookup key, which would be a
    -- timing/observability channel of its own
    -- (docs/security-model.md "API Key Design Requirements").
    key_id        TEXT NOT NULL UNIQUE,

    -- secret_hash is HMAC-SHA256(pepper, raw secret) -- never the raw
    -- secret, and never a bare digest of it. The pepper is held only in
    -- the running process's environment (TASKFORGE_API_KEY_PEPPER), never
    -- in this database, so a stolen dump of this table alone is not
    -- sufficient to forge a credential.
    secret_hash   BYTEA NOT NULL,

    -- scopes is the per-key capability list (OD-5): 'jobs', 'metrics',
    -- 'admin'. Deliberately NO database CHECK on the array's contents --
    -- the same precedent migration 0004 sets for terminal_attempt_count:
    -- where an invariant is fully owned by trusted application code, a
    -- hard CHECK buys nothing and costs the ability to write the row
    -- shapes the application-level validation tests need. Validation
    -- lives exactly once, centrally, in internal/principal.ValidateScopes.
    scopes        TEXT[] NOT NULL DEFAULT '{}',

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NULL,

    -- revoked_at takes effect on the very next verification -- there is
    -- no cache and no TTL anywhere in internal/principal's verification
    -- path, so revocation never requires a redeploy or a restart
    -- (docs/security-model.md's revocation requirement). Multiple live,
    -- non-revoked rows per principal are ordinary, which is what makes
    -- rotation-with-overlap fall out of this schema with no extra
    -- mechanism.
    revoked_at    TIMESTAMPTZ NULL,

    -- last_used_at is best-effort telemetry only: written outside the
    -- request's critical path, errors ignored, and never read as a
    -- correctness signal by anything.
    last_used_at  TIMESTAMPTZ NULL,

    -- The system principal (OD-1) must never hold a credential. It is the
    -- identity migration 0007 backfills every pre-Phase-12 row to, so a key
    -- minted against it would authenticate a caller straight into every
    -- legacy row in the database -- the one identity in the system with no
    -- legitimate owner and the widest reach.
    --
    -- This constraint exists because an independent review found that the
    -- property was asserted in this file's own comments and in
    -- principal.SystemPrincipalID's doc comment, but enforced NOWHERE:
    -- principal.Store.CreateAPIKey checked only the scopes, so
    -- `taskforge-admin create-key -principal=00000000-...-0001` minted a
    -- working credential. internal/principal.Store.CreateAPIKey now refuses
    -- it too (with a clearer error, and without a round trip); this CHECK is
    -- the backstop that makes the claim true even against a direct INSERT by
    -- an operator or a future code path that forgets.
    CONSTRAINT api_keys_no_system_principal
        CHECK (principal_id <> '00000000-0000-0000-0000-000000000001')
);

CREATE INDEX IF NOT EXISTS idx_api_keys_principal_id
    ON api_keys (principal_id);
