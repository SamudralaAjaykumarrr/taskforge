// Package governance is Phase 13's queue/tenant governance configuration
// layer (docs/phase-13-plan.md §7, §8, §17; ADR-0009): the queue_limits
// (static, operator-set concurrency/rate configuration) and queue_slots
// (ADR-0009's slot-table provisioning) tables, plus rate_limit_buckets'
// durable token-bucket runtime state. Kept HTTP-agnostic and store-package-
// agnostic -- like internal/job/internal/principal, it depends on nothing
// in internal/api or cmd/, so cmd/taskforge-admin (operator tooling, run
// under the database owner role) and internal/api (the admission check,
// run under taskforge_api) can both depend on it without an import cycle.
//
// Configuration writes here (SetConcurrencyLimit, SetRateLimit) are
// owner-privileged DML, never a runtime code path -- deploy/postgres-roles.sql
// grants neither taskforge_api nor taskforge_worker INSERT/UPDATE/DELETE
// on queue_limits or queue_slots. The runtime rate-limit check
// (CheckAndConsumeRateLimit) is the one method here taskforge_api actually
// calls on the request path; it needs only SELECT/INSERT/UPDATE on
// rate_limit_buckets, never queue_limits or queue_slots directly (the
// caller passes the already-configured rate/burst values in).
package governance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrZeroConcurrencyLimit is returned by SetConcurrencyLimit when asked to
// set a queue's concurrency_limit to exactly 0. docs/phase-13-plan.md §13:
// "the admin tool to reject 0 explicitly with a distinct error from
// unset/unlimited (NULL) -- the two are easy to conflate in the schema and
// must not be conflated in code." Zero concurrency is not a supported
// configuration in this phase; an operator who wants to stop admitting new
// work to a queue has no code path here that does that silently.
var ErrZeroConcurrencyLimit = errors.New("governance: concurrency_limit must not be 0 -- use a positive value, or omit it entirely for unlimited")

// ErrQueueNameRequired is returned by any method taking a queue_name when
// it is empty after trimming.
var ErrQueueNameRequired = errors.New("governance: queue_name is required")

// QueueLimit is one queue_limits row -- the queue-wide (principal_id IS
// NULL) configuration this phase's admission/concurrency code paths
// actually read. Per-principal rows (OD-2, still open) are representable
// in the schema but not surfaced or enforced by this package.
type QueueLimit struct {
	ID               uuid.UUID
	QueueName        string
	ConcurrencyLimit *int // nil = unlimited
	RateLimitPerSec  *float64
	RateLimitBurst   *int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// QueueState is a point-in-time operator-visible snapshot of one queue's
// configuration and live state -- the CLI-side complement to
// docs/observability.md's queue metrics (taskforge_queue_running,
// taskforge_queue_concurrency_limit), for cmd/taskforge-admin's
// show-queue-state.
type QueueState struct {
	QueueName        string
	ConcurrencyLimit *int
	SlotsTotal       int
	SlotsHeld        int
	RateLimitPerSec  *float64
	RateLimitBurst   *int
	RateTokens       *float64 // nil if no rate_limit_buckets row exists yet
	// LastClaimedAt is nil for a queue that has never been served (the
	// schema's own '-infinity' default, per ADR-0009's "newly eligible
	// queue" rule) -- Go's time.Time has no infinite value to represent
	// that sentinel directly, so it is normalized to nil here rather than
	// failing to scan.
	LastClaimedAt *time.Time
}

// Store is governance configuration's persistence layer.
type Store struct {
	db *sql.DB
}

// New wraps an already-open *sql.DB. The caller owns the DB's lifecycle.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetConcurrencyLimit sets queueName's queue-wide concurrency_limit and
// reconciles queue_slots to match, in one transaction. limit == nil means
// unlimited (deletes the queue-wide row's limit and every currently-free
// slot -- held slots drain naturally as their jobs terminalize). limit
// pointing at 0 is rejected (ErrZeroConcurrencyLimit) before any write.
//
// Idempotent: re-running with the same limit converges to the same slot
// count, never accumulating or duplicating slots
// (docs/phase-13-plan.md §11's "rollback concern specific to this phase").
// Shrinking below the currently-held count does not evict a running job:
// slot_index values at or above the new limit that are still held are
// left in place (draining naturally as their jobs terminalize); only
// currently-FREE out-of-range slots are removed immediately. A later call
// to this same method (e.g. after those jobs finish) finishes the
// convergence.
func (s *Store) SetConcurrencyLimit(ctx context.Context, queueName string, limit *int) error {
	queueName, err := normalizeQueueName(queueName)
	if err != nil {
		return err
	}
	if limit != nil && *limit == 0 {
		return ErrZeroConcurrencyLimit
	}
	if limit != nil && *limit < 0 {
		return fmt.Errorf("governance: concurrency_limit must not be negative")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("governance: set concurrency limit: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	_, err = tx.ExecContext(ctx, `
		INSERT INTO queue_limits (id, queue_name, principal_id, concurrency_limit)
		VALUES ($1, $2, NULL, $3)
		ON CONFLICT (queue_name) WHERE principal_id IS NULL
		DO UPDATE SET concurrency_limit = $3, updated_at = now()`,
		uuid.New(), queueName, limit)
	if err != nil {
		return fmt.Errorf("governance: set concurrency limit: upsert queue_limits: %w", err)
	}

	if limit == nil {
		// Unlimited: remove every currently-free slot; held ones drain
		// naturally as their jobs terminalize (see doc comment above).
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM queue_slots WHERE queue_name = $1 AND held_by_job_id IS NULL`, queueName,
		); err != nil {
			return fmt.Errorf("governance: set concurrency limit: deprovision slots: %w", err)
		}
		return tx.Commit()
	}

	// Add any missing slot_index in [0, *limit).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO queue_slots (queue_name, slot_index)
		SELECT $1, s FROM generate_series(0, $2::int - 1) AS s
		ON CONFLICT (queue_name, slot_index) DO NOTHING`,
		queueName, *limit,
	); err != nil {
		return fmt.Errorf("governance: set concurrency limit: provision slots: %w", err)
	}
	// Remove currently-free slot_index values at or beyond the new limit.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM queue_slots
		WHERE queue_name = $1 AND slot_index >= $2::int AND held_by_job_id IS NULL`,
		queueName, *limit,
	); err != nil {
		return fmt.Errorf("governance: set concurrency limit: deprovision excess slots: %w", err)
	}

	return tx.Commit()
}

// SetRateLimit sets queueName's static rate-limit configuration
// (docs/phase-13-plan.md §7/§8) and ensures a durable rate_limit_buckets
// row exists for it, seeded at a full burst of tokens. ratePerSec and
// burst must both be positive.
func (s *Store) SetRateLimit(ctx context.Context, queueName string, ratePerSec float64, burst int) error {
	queueName, err := normalizeQueueName(queueName)
	if err != nil {
		return err
	}
	if ratePerSec <= 0 {
		return fmt.Errorf("governance: rate_limit_per_sec must be positive")
	}
	if burst <= 0 {
		return fmt.Errorf("governance: rate_limit_burst must be positive")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("governance: set rate limit: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, `
		INSERT INTO queue_limits (id, queue_name, principal_id, rate_limit_per_sec, rate_limit_burst)
		VALUES ($1, $2, NULL, $3, $4)
		ON CONFLICT (queue_name) WHERE principal_id IS NULL
		DO UPDATE SET rate_limit_per_sec = $3, rate_limit_burst = $4, updated_at = now()`,
		uuid.New(), queueName, ratePerSec, burst)
	if err != nil {
		return fmt.Errorf("governance: set rate limit: upsert queue_limits: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rate_limit_buckets (scope_key, tokens, last_refill_at)
		VALUES ($1, $2, now())
		ON CONFLICT (scope_key) DO NOTHING`,
		RateLimitScopeKey(queueName), float64(burst),
	); err != nil {
		return fmt.Errorf("governance: set rate limit: seed bucket: %w", err)
	}

	return tx.Commit()
}

// ClearRateLimit removes queueName's rate-limit configuration (an
// unset/nil value means "no rate limit," matching concurrency_limit's own
// NULL-is-unlimited convention). The rate_limit_buckets row is left in
// place (harmless, unread once queue_limits no longer names a rate) --
// removing it is not necessary for correctness and this method does not
// bother.
func (s *Store) ClearRateLimit(ctx context.Context, queueName string) error {
	queueName, err := normalizeQueueName(queueName)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE queue_limits SET rate_limit_per_sec = NULL, rate_limit_burst = NULL, updated_at = now()
		WHERE queue_name = $1 AND principal_id IS NULL`, queueName)
	if err != nil {
		return fmt.Errorf("governance: clear rate limit: %w", err)
	}
	return nil
}

// GetQueueLimit returns queueName's queue-wide configuration, or
// (nil, nil) if none has ever been set (unlimited, no rate limit -- the
// pre-Phase-13 compatibility default for any queue an operator has not
// configured).
func (s *Store) GetQueueLimit(ctx context.Context, queueName string) (*QueueLimit, error) {
	queueName, err := normalizeQueueName(queueName)
	if err != nil {
		return nil, err
	}
	var ql QueueLimit
	err = s.db.QueryRowContext(ctx, `
		SELECT id, queue_name, concurrency_limit, rate_limit_per_sec, rate_limit_burst, created_at, updated_at
		FROM queue_limits WHERE queue_name = $1 AND principal_id IS NULL`, queueName,
	).Scan(&ql.ID, &ql.QueueName, &ql.ConcurrencyLimit, &ql.RateLimitPerSec, &ql.RateLimitBurst, &ql.CreatedAt, &ql.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("governance: get queue limit: %w", err)
	}
	return &ql, nil
}

// ListQueueState returns a snapshot of every queue that has either a
// queue_limits row or a queue_state row (i.e. every queue an operator has
// configured, or that has ever had a job submitted to it), for
// cmd/taskforge-admin's show-queue-state.
func (s *Store) ListQueueState(ctx context.Context) ([]QueueState, error) {
	// known_queues is the UNION of queue_state (every queue that has ever
	// had a job/workflow submitted -- the fairness roster's own domain)
	// and queue_limits (every queue an operator has configured), not
	// queue_state alone: an operator who configures a concurrency/rate
	// limit for a queue BEFORE any job has ever been submitted to it must
	// still see it here, not just after traffic arrives.
	rows, err := s.db.QueryContext(ctx, `
		WITH known_queues AS (
			SELECT queue_name FROM queue_state
			UNION
			SELECT queue_name FROM queue_limits WHERE principal_id IS NULL
		)
		SELECT
			kq.queue_name,
			ql.concurrency_limit,
			COALESCE(slots.total, 0),
			COALESCE(slots.held, 0),
			ql.rate_limit_per_sec,
			ql.rate_limit_burst,
			rlb.tokens,
			CASE WHEN qs.last_claimed_at = '-infinity'::timestamptz THEN NULL ELSE qs.last_claimed_at END
		FROM known_queues kq
		LEFT JOIN queue_state qs ON qs.queue_name = kq.queue_name
		LEFT JOIN queue_limits ql ON ql.queue_name = kq.queue_name AND ql.principal_id IS NULL
		LEFT JOIN rate_limit_buckets rlb ON rlb.scope_key = 'queue:' || kq.queue_name
		LEFT JOIN (
			SELECT queue_name, count(*) AS total, count(*) FILTER (WHERE held_by_job_id IS NOT NULL) AS held
			FROM queue_slots GROUP BY queue_name
		) slots ON slots.queue_name = kq.queue_name
		ORDER BY kq.queue_name`)
	if err != nil {
		return nil, fmt.Errorf("governance: list queue state: %w", err)
	}
	defer rows.Close()

	var out []QueueState
	for rows.Next() {
		var st QueueState
		if err := rows.Scan(&st.QueueName, &st.ConcurrencyLimit, &st.SlotsTotal, &st.SlotsHeld,
			&st.RateLimitPerSec, &st.RateLimitBurst, &st.RateTokens, &st.LastClaimedAt); err != nil {
			return nil, fmt.Errorf("governance: list queue state: scan: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ConcurrencyLimits returns every queue-wide configured concurrency_limit
// (queue_name -> limit), for docs/phase-13-plan.md §12's
// taskforge_queue_concurrency_limit gauge -- a queue with no row here
// (unlimited) simply has no series, matching the observability discipline
// of never fabricating a value for an unconfigured queue.
func (s *Store) ConcurrencyLimits(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT queue_name, concurrency_limit FROM queue_limits
		WHERE principal_id IS NULL AND concurrency_limit IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("governance: concurrency limits: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var queueName string
		var limit int
		if err := rows.Scan(&queueName, &limit); err != nil {
			return nil, fmt.Errorf("governance: concurrency limits: scan: %w", err)
		}
		out[queueName] = limit
	}
	return out, rows.Err()
}

func normalizeQueueName(queueName string) (string, error) {
	if queueName == "" {
		return "", ErrQueueNameRequired
	}
	return queueName, nil
}

// RateLimitScopeKey renders queueName as a rate_limit_buckets.scope_key
// (OD-9, resolved for this phase's own scope: queue-only, no principal
// dimension -- per-principal and combined queue+principal rate-limit keys
// are explicit non-goals). Exported so internal/api's admission check
// derives the identical key this package's own writes use.
func RateLimitScopeKey(queueName string) string {
	return "queue:" + queueName
}
