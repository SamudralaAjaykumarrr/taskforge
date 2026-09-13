# Phase 12 Implementation Plan — Security & Trust Boundaries

Status: **PLANNING ONLY, Revision 2 (post independent architecture review).
Not implemented. No Go code has been written or changed for this phase.**
This document does not redefine Phase 12's scope — that scope remains
authoritative in [docs/enterprise-roadmap.md](enterprise-roadmap.md)
("Phase 12 — Security & Trust Boundaries") and
[docs/security-model.md](security-model.md). Revision 1 of this document
left eight open decisions (OD-1–OD-8) unresolved and pending review. This
revision resolves all eight per explicit reviewer direction, and an
independent audit of the resulting design against the current codebase
surfaced **one additional architecture blocker**, resolved below (§4a).
Where this document and `enterprise-roadmap.md`/`security-model.md`
disagree, those two documents still win.

---

## Resolved Open Decisions (OD-1 – OD-8)

| ID | Topic | Final decision |
|---|---|---|
| OD-1 | Legacy/backfill identity | An explicit, immutable **system principal** (`principal.SystemPrincipalID`, a fixed well-known UUID) backfills every pre-Phase-12 row. `principal_id` is **not** left nullable as a steady-state design — it becomes `NOT NULL` on both `jobs` and `workflow_instances` in the same migration set that adds it, once backfill has run. Idempotency scoping (`UNIQUE(principal_id, job_type, idempotency_key)`) is therefore never exposed to a NULL-widening loophole at any point. |
| OD-2 | API credential transport | `Authorization: Bearer <key_id>.<secret>`. No `X-API-Key` header. |
| OD-3 | Worker identity | **No application-level `worker` principal kind.** `principals.kind` is `('caller', 'admin')` only. Workers remain an infrastructure/database trust boundary (a PostgreSQL role/credential), documented explicitly in §4a as distinct from the application/API trust boundary this phase secures. Revisit only if a future phase has workers call an authenticated application API. |
| OD-4 | txenqueue identity | `txenqueue.EnqueueRequest.PrincipalID` becomes **required**; `EnqueueTx` rejects a zero-value `PrincipalID` with `ErrInvalidRequest` before touching `tx`. **No compatibility adapter is built in this phase** — confirmed by repository search that `txenqueue` has zero callers outside its own test files (Phase 11 shipped one release cycle ago; nothing depends on the old shape yet). The adapter's design is documented as a contingency (§6b) only, not committed scope. |
| OD-5 | Metrics access | `GET /metrics` requires a credential carrying a **`metrics` scope**, modeled as a new `api_keys.scopes` array column — a per-key capability list, not a new principal kind and not a policy engine. An ordinary job-submission key (`scopes = {jobs}`) does not grant metrics access, and a metrics-only key does not grant job submission. |
| OD-6 | Auth rollout | **Hard cutover.** No `observe`/`enforce` dual mode. `README.md` labels the whole project "Experimental" with no evidence of an established external caller base (confirmed: `txenqueue` — the only integration surface with any compatibility stakes — has zero real callers; the HTTP API's only consumers found in-repo are its own tests and `cmd/`'s demo handlers). Dual-mode complexity is not justified by any repository evidence and is dropped. |
| OD-7 | TLS boundary | **No native TLS termination in `cmd/api`.** TLS termination is defined as an external reverse-proxy/load-balancer responsibility, documented as a required deployment boundary. TaskForge's own HTTP server continues to speak plaintext HTTP behind that boundary and makes no transport-security claim beyond it (§4a, §9). |
| OD-8 | Invariant numbering | **No new `TF-INV-0NN` ID is allocated in this plan.** `docs/invariants.md` currently defines through `TF-INV-016`; `docs/enterprise-roadmap.md` provisionally reserves `TF-INV-017` for Phase 13's fairness property (confirmed: three citations, all in the Phase 13 section, all "tentatively"). No document reserves `018` or above, but "provisional" language in the one existing reservation means the registry itself has not been formally extended past `016`. Per the approved instruction, this plan keeps symbolic guarantee names (`G1`–`G8`, §2) and records formal ID allocation (whether Phase 12's guarantees get `TF-INV-018`+, and whether Phase 13's "tentative" `TF-INV-017` is confirmed as-is) as a **documentation-governance step**, to be resolved in its own small PR to `invariants.md` — coordinated across Phase 12 and 13 owners — before either phase's implementation PR, not decided unilaterally inside either. |

---

## Architecture blocker discovered during this review (§4a)

**Finding**: Revision 1's plan added ownership checks "after the existing
lookup, before writing the response" in the HTTP handler layer. Direct
inspection of the current code shows this is unsafe as stated:

- `CancelJob` (`internal/api/handlers.go`) calls
  `h.store.CancelQueuedOrRetryWait(ctx, id)` — a **mutating**, fenced
  `UPDATE` — as its *first* action, with no prior read of the row at all.
  If that fails with `ErrStaleTransition`, it falls through to
  `h.store.RequestCancellation(ctx, id)` — also mutating. Only if *both*
  mutating calls fail does it finally call `GetByID` to report the
  terminal state.
- `CancelWorkflow` (`internal/api/workflow_handlers.go`) is the same
  shape: `h.store.CancelWorkflow(ctx, id)` mutates first; there is no
  read-then-check step anywhere before it.

A handler-layer "check ownership, then call the existing mutating store
method" design — as Revision 1 implicitly assumed — would therefore let
an unauthorized principal's cancel request actually mutate another
principal's job/workflow (e.g., flip `cancel_requested = true` on a
`RUNNING` job) *before* any ownership check had a chance to run, unless a
new read-then-act step is inserted ahead of the existing cascade — which
would itself introduce exactly the check-then-act TOCTOU shape this
codebase deliberately avoids elsewhere (see `internal/store/idempotency.go`'s
comment: the idempotency guarantee is real specifically *because* it is
enforced by "the INSERT attempted directly... never a check-then-act
read").

**Resolution — push principal scoping into the store layer's existing
fencing pattern, not a handler-layer pre-check.** Every mutating/reading
store method already guards its `WHERE` clause on exactly the fields that
must match for the operation to be valid (`id`, `state`,
`lease_owner`/`lease_generation` for worker-side calls) — this is the
same idiom `ADR-0002` and TF-INV-003/010/014 already establish: push the
authorization-relevant condition into the SQL, not into a separate
application-level check. Phase 12 extends this idiom one column further:

- `Store.GetByID(ctx, id, authz)`,
  `Store.CancelQueuedOrRetryWait(ctx, id, authz)`,
  `Store.RequestCancellation(ctx, id, authz)`,
  `Store.GetWorkflow(ctx, id, authz)`,
  `Store.CancelWorkflow(ctx, id, authz)` all gain an `authz` parameter
  (concrete shape: `principal.AccessContext{ PrincipalID uuid.UUID;
  IsAdmin bool }`, package `internal/principal`).
- Every one of these queries' `WHERE id = $1` becomes
  `WHERE id = $1 AND ($2::boolean OR principal_id = $3)` (admin-bypass
  literal `TRUE`/authenticated principal's ID bound as parameters) — a
  single additional predicate on the *same* statement, not a second
  query. A row that exists but belongs to a different, non-admin
  principal simply fails to match, exactly like a row that does not exist
  at all or is in the wrong state — it is `ErrNotFound`/`ErrStaleTransition`
  either way, with **no new error type and no new code branch** to keep
  in sync.
- This closes the blocker structurally: there is no window in which a
  mutating statement can affect a row before an authorization check runs,
  because the authorization check *is part of* the only statement that
  can affect the row. It also gives verification-point 9 (no
  existence-disclosure) for free: the handler code does not need to
  distinguish "not found" from "not yours" at all — the store layer
  already made them indistinguishable at the SQL level.
- `internal/api` handlers change only in that they now build a
  `principal.AccessContext` from `r.Context()` (set by the new auth
  middleware, §6) and pass it through to the otherwise-unchanged call
  sites — no new handler-level branching logic, no new response-shape
  code.

This finding **replaces** Revision 1's §4/§6 handler-level
`authorizeJobAccess`/`authorizeWorkflowAccess` helper design. See §5, §6,
§7 below for the updated, concrete signatures and SQL.

---

## 1. Problem statement and why Phase 12 is the correct next step

Unchanged from Revision 1 and re-confirmed during this review: direct
inspection of `internal/` and `cmd/` still shows zero authentication,
authorization, or transport-security code anywhere; `security-model.md`'s
Summary table still rates Application/Worker/Transport/Operational
security `Absent`; `enterprise-roadmap.md`'s sequencing diagram still
places Phase 12 immediately after the now-merged Phase 11 with no phase
interleaved; and Phase 11's own exit criteria/README explicitly defer
authentication to "Phase 12." Nothing in this review changes that
conclusion.

---

## 2. Exact system guarantees Phase 12 must add

Unchanged in substance from Revision 1; restated with the OD resolutions
folded in and symbolic names retained per OD-8:

- **G1 — No unauthenticated write, and no unauthenticated read.** Every
  route — `POST /jobs`, `POST /jobs/{id}/cancel`, `POST /workflows`,
  `POST /workflows/{id}/cancel`, `GET /jobs/{id}`, `GET /workflows/{id}`,
  and `GET /metrics` — rejects a request with no or invalid credential
  with `401`. This is enforced **structurally** (§6a: one router-wiring
  point every route passes through), not per-handler, so it is
  deny-by-default rather than opt-in (verification point 5).
- **G2 — Ownership-scoped access, enforced in SQL (§4a).** A principal
  can read/cancel only jobs/workflows it submitted, unless it is an
  admin principal. "Not found" and "found but not yours" are the same
  response by construction, not by a matching-strings convention.
- **G3 — Distinct trust boundaries, not distinct application principal
  types (OD-3).** API-caller identity (`principals`/`api_keys`,
  application-level) and worker identity (a PostgreSQL role credential,
  infrastructure-level) are different mechanisms in different layers;
  neither can be presented as the other because they are checked by
  different systems (the HTTP auth middleware vs. PostgreSQL's own
  connection authentication) — see §4a's trust-boundary diagram.
- **G4 — Least-privilege PostgreSQL roles** for `taskforge_api` and
  `taskforge_worker`, bounding blast radius, explicitly not claiming
  hostile-worker-code containment.
- **G5 — Transport security via a documented, required external
  boundary (OD-7).** TaskForge does not terminate TLS itself; the
  deployment guide states the proxy/LB boundary is mandatory for any
  deployment reachable by an untrusted network, and the database
  connection requires `sslmode=verify-full` for the enterprise reference
  deployment.
- **G6 — Audit trail**: every submit/cancel log line carries an `actor`
  (principal ID), never a raw credential.
- **G7 — Tenant-scoped idempotency**, landed atomically with the
  principal concept: `UNIQUE(principal_id, job_type, idempotency_key)`,
  with `principal_id` `NOT NULL` (OD-1) so there is no NULL-widening
  collision window.
- **G8 — Credential hygiene**: never store raw secrets; non-secret
  `key_id`; rotation-with-overlap (a natural consequence of multiple live
  `api_keys` rows per principal, §6a); revocation with no redeploy;
  constant-time comparison (verification point 4, §6a); never logged.

---

## 3. Explicit non-goals

Unchanged from Revision 1, with OD resolutions making several of these
sharper rather than adding new ones:

- No full RBAC / permission-graph system — ownership check (G2) plus a
  flat per-key scope list (OD-5) only, not a policy engine.
- No OIDC/SSO integration.
- No per-job-type or per-tenant authorization policy engine.
- No mTLS between workers and the API server (workers never call the
  HTTP API at all — confirmed unchanged by `architecture.md`'s data-flow
  diagram).
- **No application-level worker principal (OD-3, new in this revision)**
  — worker identity stays a PostgreSQL-role/infrastructure concern, not
  an `internal/principal` concept, unless a future phase gives workers an
  authenticated HTTP surface to call.
- No change to `lease_owner`/`lease_generation` fencing semantics.
- **No support for hostile/fully-untrusted worker code.**
- No named queues, rate limiting, or per-tenant concurrency limits
  (Phase 13).
- **No automated rate limiting or brute-force lockout for API-key
  verification (verification point 12, explicit non-goal with
  rationale — new in this revision).** `enterprise-roadmap.md` assigns
  rate limiting to Phase 13 by name ("Rate limiting: static, per-queue...
  submission rate limits... staged *after* [governance primitives],
  not simultaneously"), and this phase's credential is a high-entropy
  random secret (≥256 bits, §6a), not a guessable password — the
  brute-force threat model for a random bearer token is qualitatively
  different from a login form, and an online-guessing attack against it
  is not the P0/P1 risk `security-model.md` names for this phase.
  Mitigation in this phase is **observability only**:
  `taskforge_auth_failures_total{reason}` (§10) gives an operator the
  signal to notice a credential-stuffing pattern and react manually or
  via their own edge/WAF tooling; no automatic lockout, IP-based
  throttling, or exponential backoff is built here. This must not be
  read as "solved" — it is a deliberate, documented deferral to Phase 13,
  consistent with `enterprise-roadmap.md`'s own explicit sequencing.
- **No self-service HTTP API for API-key creation/rotation/revocation
  (new in this revision, resolving an implementation question OD-5/OD-8
  surfaced).** Key lifecycle management (§6a) is an operator-tool
  operation (a small admin CLI or script against `internal/principal`),
  not a public or authenticated HTTP endpoint — consistent with "no full
  RBAC system" and keeping this phase's new HTTP surface area at zero
  (it adds middleware and scoping to *existing* routes; it adds no new
  route). Revisit only if a real self-service requirement is identified.
- No workflow-definition versioning or rolling-upgrade proof (Phase 14).
- No PostgreSQL HA/backup/DR work (Phase 15).
- **No `observe`/`enforce` dual-mode rollout (OD-6, supersedes Revision
  1's tentative recommendation of the same).**

---

## 4. Architecture and component boundaries

```
   [ any HTTP client ] ----(Bearer key_id.secret)---> [ reverse proxy / LB ]
                                                          | terminates TLS
                                                          | (G5, OD-7 — NOT
                                                          |  TaskForge's job)
                                                          v
                                              +----------------------+
                                              |  cmd/api (plaintext   |
                                              |  HTTP behind the      |
                                              |  proxy boundary)      |
                                              |  +------------------+ |
                                              |  | auth middleware  | |  <- G1, deny-by-
                                              |  | (internal/api)   | |     default (§6a)
                                              |  +------------------+ |
                                              |  | handlers (unchanged| |
                                              |  |  control flow;     | |
                                              |  |  pass AccessContext| |
                                              |  |  through, §4a)     | |
                                              +----------------------+
                                                          |
                                              sslmode=verify-full (G5)
                                                          v
                     +--------------------------------------------------+
                     |                    PostgreSQL                     |
                     |  taskforge_api role  |  taskforge_worker role      |  <- G4, distinct
                     |  jobs, job_attempts,  |  (same grants scoped        |     roles
                     |  workflow_*, principals,|  differently per §7)      |
                     |  api_keys (new)        |                            |
                     +--------------------------------------------------+
                                                          ^
                     +-------------------+                |
                     |   Worker Pool     |--- plain PostgreSQL connection,
                     | (infra/DB trust   |    authenticated by the DB role
                     |  boundary, OD-3 — |    itself, NOT by internal/principal
                     |  no app principal)|    (G3 — different mechanism,
                     +-------------------+     different layer)
```

**Two distinct trust boundaries, explicitly not conflated (G3/OD-3):**

1. **Application/API trust boundary** — this phase's actual subject.
   Multi-principal, potentially-mutually-distrusting HTTP callers,
   authenticated by `internal/principal`'s API-key mechanism, checked in
   `internal/api` middleware and scoped in `internal/store` SQL (§4a).
2. **Infrastructure/database trust boundary** — unchanged by this phase,
   and *not* modeled as an application principal. A worker process's
   identity is its PostgreSQL role credential, verified by PostgreSQL
   itself at connection time, per the Enterprise Deployment Profile's
   "trusted first-party worker fleet" assumption
   (`security-model.md` "Enterprise Deployment Profile"). `lease_owner`
   (an arbitrary string worker-supplied identifier, unchanged) continues
   to answer "which worker process" for fencing purposes (TF-INV-002/003/
   014); it is orthogonal to, and not replaced by, anything this phase
   adds. If a future phase gives workers an authenticated call into the
   HTTP API, *that* phase is where an application-level worker principal
   would be introduced — not this one, and not for symmetry alone (OD-3).

- **No new network-facing component** — same conclusion as Revision 1;
  authentication is middleware inside the existing `internal/api` server,
  consistent with `architecture.md`'s "smallest architecture" philosophy.
- **New package: `internal/principal`** — `Principal`, `APIKey`,
  `AccessContext` types; PostgreSQL-backed store (create/rotate/revoke/
  verify); constant-time secret verification. HTTP-agnostic, matching the
  existing `internal/job`/`internal/store` split.
- **New file: `internal/api/auth.go`** — middleware only; depends on
  `internal/principal`, not vice versa.
- **`internal/store` extended in place**, per §4a's signature changes —
  no second store, no second query path, no new response-shape code in
  handlers.

---

## 5. Data model / schema changes

Two new tables, plus additive-then-tightened columns on two existing
tables (OD-1: nullable only transiently, mid-migration-set, never as a
steady state).

### New table: `principals`

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `kind` | `text` NOT NULL | `CHECK (kind IN ('caller', 'admin'))` — **no `'worker'` value (OD-3)**. |
| `display_name` | `text` NOT NULL | Operator-assigned; never used for lookup. |
| `created_at` | `timestamptz` NOT NULL DEFAULT now() | |
| `revoked_at` | `timestamptz` NULL | A revoked principal's keys are all treated as revoked regardless of their own `revoked_at`. |

One row is seeded by migration `0005` itself: the **system principal**,
`id = '00000000-0000-0000-0000-000000000001'` (a fixed, documented UUID —
exported as `principal.SystemPrincipalID` in Go, so code never needs a
runtime lookup to find it), `kind = 'caller'`, `display_name =
'system (pre-Phase-12 backfill; not a real caller)'`. This row is never
deleted, never revoked, and has no `api_keys` row — it exists purely as a
foreign-key target for backfilled `principal_id` columns (OD-1) and, per
§6b, as the resolution target for a future compatibility adapter that
does not exist yet.

### New table: `api_keys`

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `principal_id` | `uuid` NOT NULL REFERENCES `principals(id)` | |
| `key_id` | `text` NOT NULL UNIQUE | Non-secret identifier; the `<key_id>` half of the `Bearer <key_id>.<secret>` credential (OD-2, §6a). |
| `secret_hash` | `bytea` NOT NULL | HMAC-SHA256 of the raw secret under a server-held pepper. Never the raw secret (verification point 3). |
| `scopes` | `text[]` NOT NULL DEFAULT `'{}'` | **New vs. Revision 1 (OD-5).** Application-validated set, e.g. `{jobs}`, `{metrics}`, `{jobs,metrics}`, `{admin}`. No DB `CHECK` on the array's contents (mirrors this codebase's existing "no CHECK where the invariant is fully owned by trusted application code" precedent from migration 0004's `terminal_attempt_count`) — validated once, centrally, in `internal/principal`. |
| `created_at` | `timestamptz` NOT NULL DEFAULT now() | |
| `expires_at` | `timestamptz` NULL | Optional caller-set expiry. |
| `revoked_at` | `timestamptz` NULL | Revocation takes effect on the next lookup (G8, verification point ~revocation). |
| `last_used_at` | `timestamptz` NULL | Best-effort only, never on the fencing-critical path; never used as a correctness signal. |

Indexes: `UNIQUE (key_id)`; `CREATE INDEX idx_api_keys_principal_id ON api_keys (principal_id);`.

### `jobs` and `workflow_instances` — `principal_id` column

Both tables get `principal_id uuid REFERENCES principals(id)`, ending
`NOT NULL` (OD-1) once backfilled. Index on both:
`CREATE INDEX idx_<table>_principal_id ON <table> (principal_id);`.

### Idempotency constraint change (G7)

```sql
-- was (migration 0001): idx_jobs_idempotency_key, UNIQUE (job_type, idempotency_key)
--   WHERE idempotency_key IS NOT NULL -- confirmed exact name/shape by inspection
CREATE UNIQUE INDEX idx_jobs_idempotency_scoped
  ON jobs (principal_id, job_type, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
DROP INDEX idx_jobs_idempotency_key;
```

Because `principal_id` is `NOT NULL` by the time this index is created
(OD-1's resolution — see migration ordering below), there is **no**
NULL-widening loophole: every row, backfilled or new, has a real
`principal_id` value, so the composite unique index behaves exactly like
an ordinary tenant-scoped constraint with no special-cased "unscoped" rows
(verification point 2).

`internal/store/idempotency.go`'s `isIdempotencyKeyViolation` helper
currently hardcodes the index name `idx_jobs_idempotency_key` and checks
`pgErr.ConstraintName` against it exactly — this constant **must** be
updated to `idx_jobs_idempotency_scoped` in the same change that ships
migration `0007` below, or the Phase 8-era conflict-recovery path in
`InsertIdempotent` silently stops recognizing idempotency-key conflicts
(falls through to the generic `ErrEnqueueFailed`-equivalent path instead
of the documented re-read-and-return-existing-row behavior) — flagged
explicitly because it is exactly the kind of single-hardcoded-string
coupling that is easy to miss when only reading the schema, not the code
that depends on its exact name.

### Migration file plan (final, next available number: `0005`)

1. **`0005_create_principals_and_api_keys.{up,down}.sql`** — both new
   tables (`api_keys` includes `scopes`), plus the system-principal seed
   row, all in one transaction (the existing `internal/migrate` runner
   wraps each `.up.sql` in its own transaction — confirmed by
   `internal/migrate/migrate.go`'s `applyOne`). `down`: drop `api_keys`
   then `principals` (FK order).
2. **`0006_add_principal_id_to_jobs_and_workflows.{up,down}.sql`** — adds
   both columns as **nullable** `REFERENCES principals(id)` + indexes;
   backfills every existing row to `SystemPrincipalID` in the same
   transaction (`UPDATE jobs SET principal_id = '000...001' WHERE
   principal_id IS NULL`, same for `workflow_instances`). Still nullable
   at the end of this migration — intentionally, to isolate the
   (data-volume-dependent, potentially slower) backfill step from the
   constraint-tightening step that follows. `down`: `DROP COLUMN
   principal_id` on both tables (data-safe: the column and its backfilled
   values are wholly additive and recoverable by re-running `0006`'s
   `up` if ever needed).
3. **`0007_scope_idempotency_and_require_principal.{up,down}.sql`** —
   the constraint-tightening step, all in one migration per
   `security-model.md` §7's explicit "must land together, not as a
   follow-up" requirement:
   - `ALTER TABLE jobs ADD CONSTRAINT jobs_principal_id_not_null CHECK
     (principal_id IS NOT NULL) NOT VALID;` followed by `ALTER TABLE jobs
     VALIDATE CONSTRAINT jobs_principal_id_not_null;` — the `NOT VALID` +
     `VALIDATE CONSTRAINT` two-step is used deliberately instead of a
     direct `ALTER COLUMN principal_id SET NOT NULL`: the former takes
     only a `SHARE UPDATE EXCLUSIVE` lock during validation (does not
     block concurrent reads/the claim query), the latter takes `ACCESS
     EXCLUSIVE` for the full table scan — this is exactly the "no
     migration may lock claim-critical tables for an incompatible
     duration" discipline `compatibility-policy.md` already states
     informally (formalized only in Phase 14, but sound practice to
     follow now, verification point 13). Same treatment for
     `workflow_instances.principal_id`.
   - Create `idx_jobs_idempotency_scoped`, drop `idx_jobs_idempotency_key`
     (shown above).
   - **`down` is honest, not blanket-safe**: it drops the `NOT NULL`
     check constraints (cheap, always safe) and *attempts* to recreate
     `idx_jobs_idempotency_key` in its original global shape. If any
     post-`0007` data now has two different principals sharing a
     `(job_type, idempotency_key)` pair — the entire point of this
     migration having shipped — that `CREATE UNIQUE INDEX` fails with a
     PostgreSQL uniqueness-violation error, **on purpose**: this down
     migration is documented as forward-fix-preferred (the same category
     `compatibility-policy.md`/Phase 14 will formalize as "not
     data-safe-reversible after real divergent data exists"), and a
     failing rollback attempt here is correct, not a bug to work around
     (verification point 13).

Both `internal/job.NewParams`/`internal/job.Job` (jobs) and
`internal/workflow.GraphSpec`/`internal/workflow.Instance` (workflows)
gain a `PrincipalID uuid.UUID` field alongside this schema work, threaded
through `Store.InsertIdempotent`/`Store.CreateWorkflow`.

---

## 6a. Final public authentication contract

**Transport**: `Authorization: Bearer <key_id>.<secret>` (OD-2), on every
one of the six job/workflow routes (both `/v1` and legacy-unprefixed) and
`GET /metrics`, enforced by one middleware wired once, in
`registerJobRoutes`/`NewRouter`, so no future route can be added to the
router without passing through it (structural deny-by-default,
verification point 5).

**Key format**:
- `key_id`: 16 random bytes from `crypto/rand`, hex-encoded (32 chars),
  non-secret, used only for `O(1)` lookup — collisions are practically
  impossible and are additionally caught by the `UNIQUE (key_id)` index
  (retry generation on the rare conflict).
- `secret`: 32 random bytes from `crypto/rand` (256 bits of entropy),
  base64url-encoded (no padding), shown to the caller **exactly once**,
  at creation time, by the operator tooling (§3's "no self-service HTTP
  API" non-goal) — never re-derivable or re-displayable afterward.
- Wire format: `key_id + "." + secret`, e.g.
  `Authorization: Bearer 3f9a...c1.YmFzZTY0dXJsc2VjcmV0...`.

**Storage**: `secret_hash = HMAC-SHA256(pepper, secret)`, `pepper` read
once at process start from `TASKFORGE_API_KEY_PEPPER` (new required env
var when auth is enabled), never persisted anywhere, never logged. Using
an HMAC pepper rather than a bare `SHA-256(secret)` means a stolen
`api_keys` table alone is insufficient to forge a credential without also
compromising the running process's environment (verification point 3).

**Verification** (`internal/principal.Store.Verify`, called by the
middleware on every request):
1. Parse `key_id`/`secret` from the header; malformed → `401`
   (`reason=malformed`).
2. `SELECT principal_id, secret_hash, scopes, revoked_at, expires_at FROM
   api_keys WHERE key_id = $1` — not found → `401`
   (`reason=unknown_key`).
3. `revoked_at IS NOT NULL` or `expires_at < now()` → `401`
   (`reason=revoked`/`reason=expired`).
4. Compute `HMAC-SHA256(pepper, provided secret)`; compare against the
   stored `secret_hash` with `crypto/subtle.ConstantTimeCompare`
   (verification point 4 — the comparison itself is constant-time
   regardless of where the two byte strings first differ; the *lookup* in
   step 2 is by the non-secret `key_id`, so no raw secret is ever used as
   a database lookup key, avoiding a separate timing channel there too).
   Mismatch → `401` (`reason=bad_secret`).
5. Look up the owning `principal`; `revoked_at IS NOT NULL` on the
   principal itself → `401` (`reason=principal_revoked`).
6. Success: attach `principal.AccessContext{PrincipalID, IsAdmin:
   principal.Kind == "admin", Scopes}` to `r.Context()`; best-effort,
   non-blocking `UPDATE api_keys SET last_used_at = now() WHERE id = $1`
   (errors ignored, never on the request's critical path).

Every `401` response body is the **same generic shape**
(`{"error": "unauthorized"}`) regardless of which of steps 1–5 failed —
the `reason` value exists only in the internal
`taskforge_auth_failures_total{reason}` metric label and structured log
field, never in the HTTP response, so a caller cannot use response
differences to enumerate valid `key_id`s (an extension of verification
point 9's "don't disclose existence" principle to credentials, not just
jobs).

**Scopes (OD-5)**: `jobs` (submit/read/cancel jobs+workflows — the
ordinary caller capability), `metrics` (read `GET /metrics` only),
`admin` (bypasses ownership checks per §4a's `AccessContext.IsAdmin`,
implies `jobs`). A key with only `{metrics}` gets `403` (not `401` — the
credential itself is valid, it simply lacks the scope) on any job/
workflow route; a key with only `{jobs}` gets `403` on `GET /metrics`.
`403` is used here, deliberately different from G2's job-ownership `404`
rule: a scope mismatch on an operational endpoint discloses nothing
about any specific tenant's data (there is no per-resource ID at stake),
so there is no existence-disclosure reason to mask it as `404`/`401`, and
a clear `403` is more operable for a caller misconfigured with the wrong
key type.

**Identity cannot be overridden by the request body (verification point
6)**: confirmed by inspection that `createJobRequest`,
`createWorkflowNodeRequest`, and `txenqueue.EnqueueRequest`'s Phase-11
shape contain no `principal_id`/`tenant_id`-shaped field today. This plan
adds the rule explicitly: **no request DTO may ever gain such a field**;
`PrincipalID` is populated exactly once, in the handler, directly from
`r.Context()`'s `AccessContext` — never parsed from the JSON body. A test
(§11) asserts that a request body containing an extra
`"principal_id": "<attacker-controlled-uuid>"` field is silently ignored
(Phase 11's unknown-field tolerance) and the resulting job is still
attributed to the authenticated caller, not the attacker-supplied value.

**Rotation-with-overlap / revocation (G8)** falls out of the schema
directly: two live, non-revoked `api_keys` rows for one principal during
a rotation window; `revoked_at` on the old row takes effect on its very
next lookup (step 3 above) — no cache, no TTL.

---

## 6b. Final txenqueue principal contract (OD-4)

- `txenqueue.EnqueueRequest` gains `PrincipalID uuid.UUID` as a
  **required** field.
- `EnqueueTx` validates `req.PrincipalID != uuid.Nil` as part of its
  existing ordinary-submission-validation step (alongside
  `internal/job.ValidateSubmission`), before `tx` is touched at all —
  consistent with the existing documented contract ("`ErrInvalidRequest`
  — `req` failed ordinary submission validation... `tx` was never
  touched"). No new error sentinel; this is reported via the existing
  `ErrInvalidRequest`.
- **No compatibility/default-to-system-principal path exists in the core
  `EnqueueTx` function.** This is a deliberate, reviewed hard break in
  `txenqueue`'s barely-one-release-old public Go API, justified by two
  confirmed facts: (1) a repository-wide search for `txenqueue\.` outside
  `txenqueue/`'s own test files returns zero results — there are no real
  callers to break; (2) `README.md`/`docs/roadmap.md` both still label
  the whole project "Experimental." The same reasoning OD-6 applies to
  the HTTP surface applies here.
- **Contingency, not committed scope**: if a real `txenqueue` integrator
  is identified before or during implementation who genuinely cannot
  supply a `PrincipalID` yet, OD-4's approved direction is to add a
  **separately, explicitly named** function — e.g.
  `txenqueue.EnqueueTxUnscoped` (or a distinct
  `txenqueue/txenqueuecompat` sub-package) — that internally resolves to
  `principal.SystemPrincipalID`, logs a warning on every call
  (`event: "txenqueue_unscoped_compat_used"`), and is documented as
  narrow, temporary, and named for removal — never the default, never
  silently reachable through the ordinary `EnqueueTx` call. This plan
  does **not** build that function now (verification point 14: the
  adapter, if it is ever built, is narrow, named, testable, and
  structurally distinct from the core path by construction — a caller
  cannot reach the system-principal behavior by accident, only by calling
  a function whose name says what it does).

---

## 7. Final authorization rules for every HTTP route

All rows below assume the request already passed §6a's authentication
step (valid, non-revoked, non-expired credential); "Access rule" is what
happens next.

| Route | AuthN | Required scope | Access rule |
|---|---|---|---|
| `POST /jobs` (+ `/v1`) | Required | `jobs` | `PrincipalID` from `AccessContext` attached to the new row; no ownership check needed (nothing to own yet). |
| `GET /jobs/{id}` (+ `/v1`) | Required | `jobs` | `Store.GetByID(ctx, id, authz)` — `WHERE id=$1 AND (authz.IsAdmin OR principal_id=$2)` (§4a). Non-match → `404 job not found`, identical to a genuinely nonexistent ID. |
| `POST /jobs/{id}/cancel` (+ `/v1`) | Required | `jobs` | All three cascade steps (`CancelQueuedOrRetryWait`, `RequestCancellation`, the terminal-state `GetByID` fallback) take `authz` and apply the same scoped `WHERE` (§4a) — a non-owned row never matches any step, so it is never mutated and is reported as `404`, identical to nonexistent. |
| `POST /workflows` (+ `/v1`) | Required | `jobs` | `PrincipalID` attached at creation, same as `POST /jobs`. |
| `GET /workflows/{id}` (+ `/v1`) | Required | `jobs` | `Store.GetWorkflow(ctx, id, authz)`, same scoped-`WHERE` pattern. `404` collapse as above. |
| `POST /workflows/{id}/cancel` (+ `/v1`) | Required | `jobs` | `Store.CancelWorkflow(ctx, id, authz)` — the workflow-level check gates before any of its per-node job cancellations run (its own internal fenced `UPDATE`s), so a non-owned workflow's nodes are never touched, not just its top-level row. `404` collapse as above. |
| `GET /metrics` | Required | `metrics` | No per-resource ownership concept; scope mismatch → `403` (§6a), missing/invalid credential → `401`. |

Legacy unprefixed routes get identical treatment to their `/v1`
counterparts — Phase 11's "deprecated but fully functional" stance is
unaffected; a deprecated route is not an unauthenticated one.

---

## 8. Concurrency and idempotency requirements

Unchanged in substance from Revision 1, with the store-layer scoping from
§4a folded in:

- Auth verification's hot-path read (`api_keys` `SELECT` by `key_id`) is
  a single indexed lookup with no locking; `last_used_at` updates are
  fully best-effort/non-blocking (§6a) so they never add contention.
- Revocation checked once per request at authentication time, not
  re-validated mid-request — an explicit, documented, bounded-risk
  choice (same category as TF-INV-010's "first commit wins" framing).
- **Two-principal idempotency concurrency** (G7): N/M concurrent
  submissions from principals A and B, identical `job_type`/
  `idempotency_key`, must yield exactly one row per principal — direct
  extension of Phase 4's `TestConcurrent*Idempotency` pattern and Phase
  11's `TestEnqueueTx_IdempotencyKey_ConcurrentTransactions_ExactlyOneCreates`
  against the new `idx_jobs_idempotency_scoped` index.
- **No new lease/fencing concurrency surface** — TF-INV-002/003/014 are
  unaffected; a full regression run is the proof, not a new test.
- **§4a's store-layer `WHERE`-clause scoping is itself race-free by
  construction**: because the authorization check and the mutation are
  the same statement, there is no window between "check ownership" and
  "mutate" for a concurrent cancel/complete/claim to land in — this was
  the entire reason §4a rejected the handler-level pre-check design.

---

## 9. Backward-compatibility requirements

- Still, by construction, a breaking API-contract change: every existing
  unauthenticated caller starts receiving `401` (verification: this is
  the intended, approved outcome per OD-6, not a regression to mitigate).
- **OD-6 supersedes Revision 1's tentative grace-period recommendation.**
  No `TASKFORGE_AUTH_ENFORCEMENT_MODE` config, no dual-mode logging path.
  This is documented here as a **new, one-off compatibility event**
  distinct from `compatibility-policy.md`'s existing rules (which cover
  additive schema/field changes, not "a previously-open endpoint now
  requires a credential") — `compatibility-policy.md` itself is not
  edited by this plan (no authoritative inconsistency requires it: that
  document's scope is schema/API-shape evolution, not authentication
  policy, and Phase 12 does not contradict anything it currently states).
- **`txenqueue`'s break is likewise a hard cutover (§6b)**, for the same
  evidence-based reason (OD-4).
- Migrations `0005`–`0007` are additive with respect to every existing
  column; no existing column is dropped, renamed, or narrowed.
- `sslmode=verify-full` remains scoped to "the enterprise reference
  deployment" only — local/dev profiles are unaffected, matching
  `security-model.md`'s existing framing exactly.

---

## 10. Security and observability implications

- **New operator secret**: `TASKFORGE_API_KEY_PEPPER` — same handling
  discipline as `DatabaseURL` today (env var, not committed, not logged);
  losing it invalidates every stored `secret_hash`'s verifiability
  (all keys must be reissued), a risk this plan accepts as proportionate
  to the "not expected at this stage" framing `security-model.md` §3
  already gives to secrets-manager integration.
- **Observability additions**:
  - `actor` (the authenticated `PrincipalID`, never the raw credential)
    added to every existing submit/cancel structured log line
    (verification point 10 — a log-output test asserts no log line ever
    contains the raw `secret` substring, on both success and every
    failure path in §6a's verification steps).
  - `taskforge_auth_failures_total{reason}` — `reason` is the fixed,
    small enum from §6a step 1–5 (`malformed`, `unknown_key`, `revoked`,
    `expired`, `bad_secret`, `principal_revoked`) — cardinality-safe,
    never a raw key/principal value, matching `observability.md`'s
    existing discipline.
  - `GET /metrics` becoming authenticated closes `security-model.md` §5's
    "operational volume disclosure" finding.
- Unchanged non-claims from Revision 1: no payload/`result_metadata`
  classification/encryption; no worker-code isolation; no cancel-replay
  nonce/freshness mechanism (accepted low-severity risk, per
  `security-model.md` §2, unchanged by this revision).
- **Proxy trust boundary, made explicit (verification point 15, new in
  this revision)**: because TLS terminates externally (OD-7), TaskForge
  necessarily receives plaintext HTTP from the proxy. This plan states
  explicitly: **`cmd/api` does not parse or trust any proxy-injected
  header** (`X-Forwarded-For`, `X-Forwarded-Proto`, `X-Real-IP`, or
  equivalent) **for any authorization or security decision in Phase
  12** — authentication is entirely credential-based (§6a), not
  IP-based, and this phase adds no IP-based logic of any kind (rate
  limiting is explicitly Phase 13, §3). The only trust placed in the
  proxy is that the network segment between the proxy and `cmd/api` is
  itself private/operator-controlled — stated here as a documentation
  requirement for the eventual deployment guide, not verified or
  enforced by TaskForge's own code (it cannot be, from inside the
  process). If a future phase ever wants to trust `X-Forwarded-For` for
  IP-based rate limiting (Phase 13) or audit logging, that trust decision
  must be made and documented explicitly then, including which specific
  proxy layer is allowed to set that header and how spoofing from a
  client-supplied copy of the same header name is prevented — it is not
  granted implicitly by this phase's proxy-boundary requirement.

---

## 11. Required tests / proof matrix

Mapped directly to the review's 16 verification points; file location
follows the existing `internal/api/handlers_phaseN_test.go` /
`internal/store/*_test.go` conventions.

| # | Verification point | Test(s) |
|---|---|---|
| 1 | `principal_id` placement/migration ordering safe for existing data | Migration test: seed pre-Phase-12 rows (no `principal_id`), run `0005`–`0007`, assert every row resolves to `SystemPrincipalID`, `NOT NULL` constraints hold, old index gone, new index enforced. |
| 2 | Principal-scoped idempotency cannot be bypassed via NULL/legacy rows | Schema test asserting `jobs.principal_id` is `NOT NULL` post-migration (no row can ever reach the NULL-widening case); two-principal concurrency test (§8) proving the composite index scopes correctly with real data. |
| 3 | API keys never stored plaintext; generation/hashing/verification/revocation/lookup precise | `internal/principal` unit tests: `Create` never returns/logs a value equal to the stored `secret_hash`'s preimage in any accessible form beyond the one-time creation response; `Verify` round-trips a freshly created key; a tampered secret against a real `key_id` fails; a revoked/expired key fails post-revocation/expiry. |
| 4 | Credential comparison avoids timing leaks | Code-level test asserting `Verify`'s secret comparison calls `crypto/subtle.ConstantTimeCompare` (mechanism-level proof, same pattern as TF-INV-016's "schema test asserting the constraint exists" — a live timing measurement in CI is not attempted, as it would be flaky by nature). |
| 5 | Authorization is deny-by-default | Table-driven test over all six routes + `/metrics`: no header → `401`; also a structural test asserting `NewRouter`'s route-registration helper is the *only* place handlers are mounted (grep-style/reflection check that no handler is reachable outside it). |
| 6 | Authenticated identity cannot be overridden by payload | `TestCreateJob_RequestBodyPrincipalIDFieldIgnored` — a request body with an extra `principal_id` field pointing at a different, real principal; asserts the created job's `principal_id` is the *authenticated caller's*, not the body's. |
| 7 | `txenqueue` cannot cross principal boundaries | `TestEnqueueTx_PrincipalID_Required_RejectsZeroValue`; two-`PrincipalID` concurrent-race variant of the existing Phase 11 idempotency test (§8), proving rows are correctly attributed and never share the scoped index across principals. |
| 8 | Existing job lookup/mutation/list routes cannot expose another principal's jobs | Two-principal test per route in §7's table: principal B's `GET`/`cancel` against principal A's job/workflow ID — asserts `404`, and asserts (via a direct DB read in the test, bypassing the API) that a `cancel` attempt performed **zero row mutations** on A's row (proving §4a's blocker is actually closed, not just that the HTTP response looks right). |
| 9 | Error responses don't reveal existence of another principal/job/key | Byte-for-byte response-body comparison: B's `GET`/`cancel` on A's real job ID vs. B's `GET`/`cancel` on a random nonexistent UUID — bodies must be identical. Same pattern for `401` on `api_keys` lookup failures (§6a: identical generic body across all five failure reasons). |
| 10 | Logging never emits raw API keys | Log-output test (extends `internal/api/observability_test.go`'s pattern): no log line, across every §6a success/failure path, contains the raw `secret` substring; `actor` field present on submit/cancel lines. |
| 11 | Metrics access separated from mutation access | `{jobs}`-scoped key → `403` on `GET /metrics`; `{metrics}`-scoped key → `403` on `POST /jobs`; `{admin}` → both succeed. |
| 12 | Rate-limiting/brute-force behavior specified or classified as non-goal | No test required by this phase (§3's explicit non-goal with rationale) — the one test this phase does add is that `taskforge_auth_failures_total{reason}` increments correctly per failure reason (observability proof, not a rate-limit proof). |
| 13 | Migrations support rollback/forward deployment safely | Up-then-down-then-up round-trip test for `0005`/`0006` (data-safe-reversible); a deliberate test proving `0007`'s `down` **fails loudly** (not silently) when post-migration cross-principal idempotency-key reuse exists, per §5's "honest, not blanket-safe" design; a `NOT VALID`/`VALIDATE CONSTRAINT` lock-duration smoke test (assert no `ACCESS EXCLUSIVE`-only-achievable blocking occurs against a concurrent read during validation, in the spirit of `compatibility-policy.md`'s discipline). |
| 14 | System-principal compatibility behavior narrow/named/testable/non-bypassable | Since no compatibility adapter is built in this phase (§6b), the test here is negative: assert `txenqueue.EnqueueTx` has no code path that resolves an unset/zero `PrincipalID` to `SystemPrincipalID` — i.e., assert the *absence* of an accidental default, directly contradicting Revision 1's original (rejected) design. If OD-4's contingency function is later added, it requires its own equivalent "always logs, never the default path" test at that time. |
| 15 | Proxy/TLS trust assumptions explicit, including which forwarded headers are trusted/ignored | Code-level test asserting no production code path in `internal/api` reads `X-Forwarded-For`/`X-Forwarded-Proto`/`X-Real-IP` (or any header) for any authorization decision — an absence-of-behavior test, mirroring #14's shape. |
| 16 | Tests prove authN, authZ, isolation, revocation, idempotency, cross-principal denial — not just happy paths | Satisfied collectively by #1–#11, #13–#14 above; every one of those is specifically an adversarial/negative case (wrong principal, revoked key, expired key, wrong scope, missing header, tampered secret, migration data divergence), not a success-path-only suite. A single aggregate "regression: all Phase 1–11 tests still pass unchanged" run is the complementary happy-path proof that nothing existing broke. |

All new tests run against real PostgreSQL, per this project's unbroken
existing discipline — no mocked-database shortcut is introduced.

---

## 12. Acceptance criteria and proof obligations

Unchanged: this plan adopts `enterprise-roadmap.md` Phase 12's own
"Enterprise exit criteria" checklist verbatim as the acceptance bar (see
Revision 1's §12, reproduced there in full — not restated here to avoid
drift between two copies; the checklist itself was not touched by this
review). Added by this revision:

- [ ] The §4a store-layer scoping change (not a handler-level check) is
      what implements every ownership-related exit criterion — verified
      by test #8's direct-DB-read proof that an unauthorized cancel
      mutates zero rows, not merely that the HTTP response is a `404`.
- [ ] All eight OD items above are reflected in the actual merged code,
      not just this document (a straightforward diff-against-plan check
      at PR review time).
- [ ] `docs/invariants.md` is **not** modified by the Phase 12
      implementation PR itself (OD-8) — any `TF-INV-0NN` allocation is a
      separate, subsequent documentation-governance PR.

---

## 13. Files / packages likely to change

Unchanged list from Revision 1 (see prior version's §13 for the full
enumeration: `internal/principal/*`, `internal/api/auth.go`,
`internal/api/handlers_phase12_test.go`, migrations `0005`–`0007`,
`internal/config`, `internal/store/*`, `internal/job`, `internal/workflow`,
`txenqueue/*`, `cmd/api`, `cmd/worker`, doc updates), with these
corrections from this review:

- **`internal/store/idempotency.go`** — now explicitly called out (§5):
  the `idempotencyKeyIndexName` constant must change to
  `idx_jobs_idempotency_scoped`, or `InsertIdempotent`'s conflict-recovery
  path silently breaks.
- **`internal/store/store.go`, `cancellation.go`, `workflow.go`** — the
  change is now precisely specified as adding an `authz
  principal.AccessContext` parameter and an `AND ($n::boolean OR
  principal_id = $n+1)` clause to `GetByID`, `CancelQueuedOrRetryWait`,
  `RequestCancellation`, `GetWorkflow`, `CancelWorkflow` — not a
  handler-level helper as Revision 1 stated.
- **`cmd/api/main.go`** — no TLS listener wiring (OD-7 removes that item
  from Revision 1's list); still needs `taskforge_api`-role DB connection
  wiring and `TASKFORGE_API_KEY_PEPPER` plumbing into `internal/principal`.
- **No new HTTP route added anywhere** (§3's "no self-service key
  management API" non-goal) — `internal/api/server.go` changes are
  limited to wiring the auth middleware, not adding routes.
- **New: an operator-facing key-management tool** (location TBD — a
  small `cmd/taskforge-admin` binary, or a `Makefile` target wrapping a
  short Go program against `internal/principal` directly) for
  create/rotate/revoke — out of the HTTP surface entirely per §3.
- `docs/data-model.md`, `docs/observability.md`, `README.md` — same as
  Revision 1. `docs/compatibility-policy.md` — **not** changed by this
  revision (§9: no authoritative inconsistency found; OD-6 removed the
  one item that would have required a new compatibility-policy category).
  `docs/security-model.md` — **not** changed by this plan itself (its
  "Absent" rows get updated once the phase is actually implemented, as
  Phase 10/11 did for their own sections; this planning pass does not
  pre-claim completion).

---

## 14. Risks, ambiguous decisions, and alternatives considered

OD-1 through OD-8 are resolved (top of document) and removed from this
section. Remaining, narrower items surfaced by this review:

- **Risk — `TASKFORGE_API_KEY_PEPPER` is a single point of failure**
  (unchanged from Revision 1, restated: losing it invalidates every
  stored key; leaking it plus the `api_keys` table together are
  sufficient to forge credentials). Accepted for this phase per
  `security-model.md` §3's existing framing; no secrets-manager
  integration in scope.
- **Risk — `0007`'s `NOT VALID`/`VALIDATE CONSTRAINT` technique still
  requires a full-table read during validation**, just without the
  exclusive lock — on a `jobs` table grown large from Phase 9 chaos/load
  runs, this could still run for a meaningful duration. Mitigated by
  running it via `SHARE UPDATE EXCLUSIVE` (non-blocking to reads/writes)
  rather than eliminated; flagged for the implementer to actually time
  against a realistically-sized table before shipping, not assumed safe
  by construction.
- **Open, narrower question (new)**: should `internal/principal.Store`
  return a distinguishable Go error for each of the five `401` reasons
  in §6a even though the HTTP layer collapses them to one generic body?
  This plan says yes (needed for the `reason` metric label / log field,
  §10) — noted here only because it means `internal/principal`'s error
  contract has *more* granularity than `txenqueue`'s deliberately
  leak-nothing error contract (`transactional-enqueue.md`'s "Error
  contract") — a deliberate, reviewed asymmetry (an internal Go error
  used for an internal metric label is not the same exposure surface as
  a public package's returned error), not an inconsistency to fix.
- Alternatives considered and rejected: unchanged from Revision 1 (JWT/
  OIDC, mTLS for API callers) — this review found no new evidence to
  revisit either rejection.

---

## 15. Staged implementation sequence

Unchanged in shape from Revision 1, with stages re-ordered slightly to
reflect that native TLS (OD-7) is no longer a stage at all, and OD
resolution is no longer "stage 1: decide" but already done:

1. **Schema stage**: migrations `0005`–`0007` (§5), plus the
   backfill-correctness and honest-down-migration tests (§11 #1, #2,
   #13). No application code changes yet.
2. **`internal/principal` package**: types, store, `Verify` (§6a), unit
   tests (§11 #3, #4) — no HTTP wiring yet.
3. **Authentication middleware, direct hard cutover (OD-6)**:
   `internal/api/auth.go` wired into `NewRouter`; tests §11 #5, #9
   (generic-body proof), #10 (no-secret-in-logs).
4. **Store-layer authorization scoping (§4a)**: `AccessContext` threaded
   through `GetByID`/`CancelQueuedOrRetryWait`/`RequestCancellation`/
   `GetWorkflow`/`CancelWorkflow`; tests §11 #8 (including the
   direct-DB-read zero-mutation proof), #9.
5. **`txenqueue` principal requirement (§6b)**: required `PrincipalID`,
   tests §11 #7, #14 (absence-of-default proof).
6. **PostgreSQL least-privilege roles (§7 of Revision 1, unchanged)**:
   provisioning script, `cmd/api`/`cmd/worker` role wiring, privilege
   audit — independent of stages 2–5, parallelizable.
7. **`sslmode=verify-full` enforcement for the enterprise profile**
   (§9 of Revision 1, unchanged in substance, now without any native-TLS
   listener work per OD-7) — independent, parallelizable with 6.
8. **Metrics scope (OD-5)**: `scopes` enforcement on `GET /metrics`;
   test §11 #11.
9. **Audit logging**: `actor` field on existing submit/cancel log call
   sites; test §11 #10 (shared with stage 3's proof, extended to cover
   these call sites specifically).
10. **Proxy-trust documentation (§10)**: deployment-guide language on the
    required external TLS boundary and the explicit "no forwarded-header
    trust" statement; test §11 #15 (absence-of-behavior proof).
11. **Documentation sync**: `security-model.md`, `data-model.md`,
    `observability.md`, `README.md` "Phase 12: What's Implemented" —
    `compatibility-policy.md` and `invariants.md` deliberately **not**
    touched by this implementation PR (§9, OD-8).
12. **Full regression pass**: entire existing Phase 1–11 suite plus
    `internal/chaos` campaigns, unchanged, before exit criteria (§12) are
    declared met.

Stages 6 and 7 have no ordering dependency on 2–5 or each other. Stage 1
must precede everything else. Stages 3→4→5 are strictly sequential
(auth must exist before scoping can use it; `txenqueue`'s requirement is
independent of the HTTP stages but shares the same underlying
`internal/principal` package from stage 2).

---

## Cross-references

Unchanged from Revision 1 — see that section for the full list
(`enterprise-roadmap.md`, `security-model.md`, `data-model.md`,
`architecture.md`, `invariants.md`, `transactional-enqueue.md`,
`compatibility-policy.md`).
