// Package txenqueue is TaskForge's Phase 11 public, in-process Go
// integration surface (docs/enterprise-roadmap.md "Transactional Enqueue &
// API Contract Hardening"): it lets a caller whose business data lives in
// the *same* PostgreSQL database as TaskForge enqueue a job using a
// transaction the caller already owns, so the caller's own business write
// and the job insert commit together, or neither does.
//
// This is deliberately narrow. It is not a replacement for the HTTP API
// (internal/api) or a general TaskForge client library -- it is one
// additional entry point, for one specific integration shape: a caller
// that already has an open pgx.Tx (github.com/jackc/pgx/v5) against the
// same database TaskForge's own migrations (migrations/) have been applied
// to, and wants "enqueue this job if and only if my own write commits."
//
// # Same-database atomicity only
//
// The guarantee this package provides is exactly: if EnqueueTx returns
// success and the caller's tx later commits, the job is durable and
// claimable; if the caller's tx rolls back (for any reason, at any later
// point, regardless of whether other statements ran first or after), the
// job does not exist. This holds because the job insert is one more
// statement inside the caller's own PostgreSQL transaction -- it is
// PostgreSQL's own atomicity guarantee, not anything TaskForge invents or
// coordinates itself.
//
// This package provides NO guarantee whatsoever across two different
// databases, two different PostgreSQL instances, or any non-PostgreSQL
// system (MySQL, DynamoDB, an external SaaS/HTTP API, etc.). TaskForge does
// not implement distributed transactions, two-phase commit, or any other
// cross-database coordination mechanism, and nothing in this package
// should be read as claiming otherwise. If the caller's business data
// lives in a different database, the correct integration pattern is a
// transactional outbox: the caller writes its business row and an outbox
// row in its own database's own transaction, and a separate relay process
// (the caller's own, not TaskForge's) reads the outbox and calls
// TaskForge's canonical HTTP POST /v1/jobs endpoint (idempotently, via the
// existing Idempotency-Key mechanism) to enqueue the job. See
// docs/transactional-enqueue.md for the full pattern and reasoning, and
// docs/compatibility-policy.md for how this fits TaskForge's broader
// compatibility posture.
//
// # What this package does not change
//
// A job enqueued through EnqueueTx, once its caller's transaction commits,
// is an ordinary row in TaskForge's one jobs table (docs/data-model.md) --
// it enters the exact same durable state machine, is claimed by the exact
// same worker claim query, and is subject to the exact same leases,
// fencing, retries, dead-lettering, scheduling, and workflow mechanisms as
// a job submitted via POST /jobs. There is no second queue table and no
// parallel execution path. Submission-idempotency (docs/idempotency.md,
// TF-INV-008/TF-INV-016) is preserved unchanged, including inside the
// caller's transaction -- see EnqueueTx's doc comment.
package txenqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

// Job is the durable identity EnqueueTx returns for the row it (or an
// earlier, already-committed idempotent submission) inserted. It is a
// small, package-owned projection -- deliberately NOT a type alias onto
// internal/job.Job (Phase 11 correction: an earlier draft of this package
// aliased the internal type directly, which would have made TaskForge's
// entire internal durable job representation part of this package's public
// compatibility surface, coupling a public API to an internal
// implementation detail that is free to change shape as later phases add
// columns). For Phase 11, the caller's durable submission parameters are
// already known to the caller (it supplied them in EnqueueRequest); the one
// piece of new information EnqueueTx needs to hand back is the row's
// TaskForge-assigned identity, so that is all this type exposes. A caller
// that needs the job's full current state (state, attempt_count, etc.)
// fetches it the same way any other caller does: GET /jobs/{id}, using
// ID.String() -- internal/api's response shape can evolve independently of
// this package's public surface.
type Job struct {
	// ID is the durable jobs.id PostgreSQL assigned this submission (or,
	// for an idempotent duplicate, the id of the existing row this call's
	// own INSERT lost the race to -- see EnqueueTx's created return
	// value).
	ID uuid.UUID
}

// toPublicJob converts an internal/job.Job (as returned by
// internal/store.InsertTx) into this package's own public Job projection.
// internal/job.Job's type identity itself is never part of this package's
// return signature -- see Job's doc comment.
func toPublicJob(j *job.Job) *Job {
	if j == nil {
		return nil
	}
	return &Job{ID: j.ID}
}

// EnqueueRequest is the caller-supplied shape for a transactional job
// submission. It intentionally mirrors POST /jobs's request contract
// (docs/worker-protocol.md) field-for-field: the same job, submitted
// in-process instead of over HTTP, is validated by exactly the same rules
// (internal/job.ValidateSubmission) and produces exactly the same durable
// row shape.
//
// MaxAttempts and ExecutionTimeoutSeconds are pointers so "not supplied"
// (nil) applies TaskForge's documented defaults, exactly as an omitted JSON
// field does over HTTP -- a non-nil value less than 1 is rejected.
type EnqueueRequest struct {
	// PrincipalID is the TaskForge principal this job is submitted on
	// behalf of. REQUIRED as of Phase 12 (docs/phase-12-plan.md §6b,
	// OD-4): a zero value is rejected with ErrInvalidRequest before tx is
	// touched at all.
	//
	// There is deliberately no default. TaskForge does NOT resolve an
	// unset PrincipalID to a system principal, to the first principal it
	// finds, or to anything else -- a caller that cannot say who it is
	// submitting for cannot enqueue. The system principal exists solely
	// as the backfill identity for rows that predate Phase 12 (migration
	// 0006); nothing on this path, or on the HTTP path, ever assigns it.
	// TestEnqueueTx_NoDefaultSystemPrincipalPath asserts the absence of
	// such a fallback directly, against real database state.
	//
	// This is a deliberate, reviewed hard break in this package's public
	// Go API, justified by evidence rather than convenience: a
	// repository-wide search for callers of this package outside its own
	// tests returns zero results, and README.md/docs/roadmap.md both
	// still label the project Experimental. Per OD-4, an
	// EnqueueTxUnscoped-style compatibility adapter is a documented
	// CONTINGENCY, not committed scope -- it is not built here, and would
	// only ever be built as a separately named function that logs on
	// every call, never as a silent fallback reachable through EnqueueTx.
	//
	// The caller decides where this value comes from. A service handling
	// its own authenticated HTTP traffic should pass the principal it
	// authenticated, not a hardcoded constant.
	PrincipalID uuid.UUID

	// JobType identifies which handler executes this job. Required,
	// non-empty after trimming, at most 255 characters.
	JobType string

	// Payload is opaque to TaskForge (docs/data-model.md); nil or empty
	// is stored as `{}`. Must be valid JSON if supplied.
	Payload json.RawMessage

	// MaxAttempts defaults to internal/job.DefaultMaxAttempts (5) when
	// nil. A supplied value must be at least 1 and at most
	// internal/job.MaxRepresentableMaxAttempts (the largest value
	// PostgreSQL's jobs.max_attempts INTEGER column can store) -- TaskForge
	// enforces no smaller, arbitrary operational upper bound
	// (docs/retry-semantics.md "Open Questions").
	MaxAttempts *int

	// ExecutionTimeoutSeconds defaults to
	// internal/job.DefaultExecutionTimeoutSeconds (30) when nil. A
	// supplied value must be at least 1.
	ExecutionTimeoutSeconds *int

	// IdempotencyKey, if non-nil and non-empty after trimming, scopes
	// submission-idempotency exactly as the Idempotency-Key HTTP header
	// does for POST /jobs (docs/idempotency.md): at most one job is ever
	// created for a given (JobType, IdempotencyKey) pair, enforced by the
	// same database unique constraint -- including when two concurrent
	// transactions (through this package, through POST /jobs, or a mix of
	// both) race on the same key. At most 255 characters after trimming.
	IdempotencyKey *string

	// ScheduledAt, if non-nil, is the caller's requested execution time
	// (docs/scheduling.md); nil means "eligible as soon as claimed."
	ScheduledAt *time.Time

	// QueueName is Phase 13's optional named-queue field
	// (docs/phase-13-plan.md §8), mirroring POST /jobs's queue_name field
	// exactly. Nil, or empty/whitespace-only after trimming, defaults to
	// internal/job.DefaultQueueName ("default") -- every pre-Phase-13
	// caller of this package (there is a nil value here, since queue_name
	// did not exist before) gets identical behavior to before.
	QueueName *string
}

// Store is TaskForge's transactional-enqueue integration surface. It holds
// no database connection or other state of its own -- every EnqueueTx call
// operates entirely within the caller-supplied pgx.Tx -- so a *Store is
// safe to share across goroutines and cheap to construct. It is a type
// (rather than a bare package-level function) to mirror internal/store's
// existing Store shape and leave room for future options without an
// API-breaking change.
type Store struct{}

// New constructs a Store. It never opens a database connection: it has
// nothing to open a connection to, since it operates entirely through
// whatever pgx.Tx a caller supplies to EnqueueTx.
func New() *Store {
	return &Store{}
}

// EnqueueTx validates req exactly as POST /jobs validates a request body
// (internal/job.ValidateSubmission -- the same shared logic, so the two
// entry points cannot silently diverge), then inserts the resulting job
// row using tx.
//
// # Transaction ownership
//
// tx is the caller's own, already-open transaction. EnqueueTx:
//   - uses tx for the insert (and, only for an idempotency-key conflict,
//     a fallback re-read -- see below);
//   - never calls tx.Commit or tx.Rollback;
//   - opens no second connection and no independent top-level
//     transaction -- the only Begin/Commit/Rollback calls this makes
//     internally are against a PostgreSQL SAVEPOINT-backed pseudo-nested
//     transaction (what tx.Begin returns when tx is already open -- see
//     pgx/v5's Tx.Begin doc comment), used solely to recover from an
//     idempotency-key conflict without aborting tx itself;
//     see internal/store.InsertTx's doc comment for the exact mechanism;
//   - reports every error to the caller rather than silently retrying or
//     swallowing it -- a non-nil error here means tx may be unusable
//     (PostgreSQL may have put it in an aborted state) and the caller is
//     responsible for deciding whether to roll it back.
//
// # Principal
//
// req.PrincipalID is required (Phase 12). A zero value is rejected with
// ErrInvalidRequest before tx is touched, and is never silently resolved
// to a default or system principal -- see EnqueueRequest.PrincipalID.
//
// # Error contract
//
// Every non-nil error EnqueueTx returns is classifiable via errors.Is as
// exactly one of ErrInvalidRequest, ErrInvalidTransaction,
// ErrMustRetryTransaction, or ErrEnqueueFailed (see errors.go), or is
// context.Canceled/context.DeadlineExceeded passed through unchanged. The
// underlying internal/store or PostgreSQL error (SQL text, SQLSTATE,
// constraint names, driver-internal wording) is never included in the
// returned error's Error() text -- this package's error strings are safe
// to log, return to a caller's own API response, or otherwise surface
// without leaking TaskForge's internal persistence details. tx being nil,
// specifically, is detected before any statement is issued and reported as
// ErrInvalidTransaction -- it never panics.
//
// A successful return means the job has been INSERTed within tx -- NOT
// that it is durable yet. If the caller later rolls back tx (for this
// reason or any other), the job does not exist, exactly as if EnqueueTx
// had never been called (TF-INV-013, docs/invariants.md). Only once the
// caller commits tx does the job become a normal, durable, claimable row,
// indistinguishable from one submitted via POST /jobs.
//
// Because success here does not mean "durably committed," EnqueueTx itself
// never logs a "job submitted"-style message -- doing so before the
// caller's own commit would misrepresent an uncertain, in-flight write as
// a completed one. Log the caller's own successful commit instead, using
// the returned Job's ID.
//
// # Idempotency
//
// If req.IdempotencyKey is nil (or empty/whitespace-only after trimming),
// this always creates a new job. If it is set, created reports whether
// this call's own INSERT created the row (true) or whether an existing,
// already-committed row for (JobType, IdempotencyKey) was found instead
// (false, following docs/idempotency.md's "first-write-wins" rule
// unchanged) -- created must never be used to choose a status code or
// otherwise distinguish "success" from "success"; both are a fully valid
// submission outcome. This holds even under concurrent transactions racing
// on the same key (docs/invariants.md TF-INV-008/TF-INV-016): the
// uniqueness guarantee is the same database constraint POST /jobs relies
// on, not a second, weaker application-level check.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, req EnqueueRequest) (*Job, bool, error) {
	params, err := job.ValidateSubmission(req.JobType, req.Payload, req.MaxAttempts, req.ExecutionTimeoutSeconds, req.IdempotencyKey)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrInvalidRequest, err.Error())
	}

	// Phase 12 (docs/phase-12-plan.md §6b): PrincipalID is part of
	// ordinary submission validation, checked here -- alongside
	// internal/job.ValidateSubmission and BEFORE the tx nil-check below,
	// so tx is provably never touched when this fails, exactly as this
	// method's documented ErrInvalidRequest contract already promises
	// ("req failed ordinary submission validation ... tx was never
	// touched"). No new error sentinel is introduced: this is an invalid
	// request, and ErrInvalidRequest already means that.
	//
	// Note what does NOT happen here: there is no "if PrincipalID is
	// zero, use the system principal" branch, no lookup of a default
	// principal, and no environment-derived fallback. Rejecting is the
	// only behaviour.
	if req.PrincipalID == uuid.Nil {
		return nil, false, fmt.Errorf("%w: principal_id is required", ErrInvalidRequest)
	}
	params.PrincipalID = req.PrincipalID
	params.ScheduledAt = req.ScheduledAt

	queueName, qerr := job.ValidateQueueName(req.QueueName)
	if qerr != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrInvalidRequest, qerr.Error())
	}
	params.QueueName = queueName

	// A caller passing a nil pgx.Tx (the ordinary nil-interface case --
	// tx == nil is a well-defined, correct check for it) must never reach
	// store.InsertTx, which would panic dereferencing it. This is checked
	// after validation (so an invalid request against a nil tx is still
	// reported as ErrInvalidRequest, consistent with every other
	// validation-first path in this method) and before any statement is
	// issued.
	if tx == nil {
		return nil, false, ErrInvalidTransaction
	}

	j, created, err := store.InsertTx(ctx, tx, params)
	if err != nil {
		return nil, false, classifyStoreErr(err)
	}
	return toPublicJob(j), created, nil
}
