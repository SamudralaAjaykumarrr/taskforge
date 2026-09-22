package governance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CheckAndConsumeRateLimit attempts to debit one token from scopeKey's
// durable token bucket, refilling it first based on elapsed wall-clock
// time since its last refill (PostgreSQL's own now(), per
// docs/failure-model.md's Clock Model -- never this process's local
// clock). ratePerSec and burst are the caller's already-resolved queue
// configuration (this method does not itself read queue_limits -- the
// caller, internal/api's admission check, already has them from its own
// governance lookup, and passing them in keeps this the one method
// taskforge_api actually needs at request time, with no dependency on
// queue_limits access).
//
// Refill-and-debit is ONE atomic UPDATE, matching this codebase's
// check-then-act-free idiom (internal/store/idempotency.go's comment):
// the refreshed token count is computed in both the SET and WHERE clauses
// of a single statement, so the row's own lock (implicitly taken by the
// UPDATE) serializes concurrent requests for the same scopeKey -- there
// is no separate "read tokens, decide, write" sequence for two concurrent
// callers to race on.
//
// allowed is true iff a token was available and consumed. If false,
// retryAfter is the minimum wait (rounded up to a whole second, the unit
// Retry-After is specified in) before a token will next be available,
// computed from the bucket's own post-refill (but not post-consumption)
// state -- never a guess.
//
// A missing bucket row (a queue that has a rate limit configured but
// whose bucket was never seeded -- should not happen via SetRateLimit,
// but defended against here rather than assumed) is treated as "not yet
// provisioned" and seeded at a full burst, then immediately debited by
// this same call -- so a first request against a freshly configured
// queue is never spuriously rejected for a provisioning gap that is this
// package's own responsibility, not the caller's.
func (s *Store) CheckAndConsumeRateLimit(ctx context.Context, scopeKey string, ratePerSec float64, burst int) (allowed bool, retryAfter time.Duration, err error) {
	if ratePerSec <= 0 || burst <= 0 {
		return false, 0, fmt.Errorf("governance: check rate limit: ratePerSec and burst must be positive")
	}

	if err := s.ensureBucketExists(ctx, scopeKey, burst); err != nil {
		return false, 0, err
	}

	// GREATEST(0, ...) floors the elapsed interval at zero: PostgreSQL's
	// now() is wall-clock time, not monotonic (docs/failure-model.md's
	// Clock Model), so an NTP or VM-host clock correction can make
	// now() - last_refill_at briefly negative. Without the floor, that
	// negative elapsed time gets multiplied by ratePerSec and ADDED
	// (i.e. subtracts real tokens) below, so a backward clock jump could
	// erroneously deny a request the bucket already had a token for. A
	// forward jump needs no equivalent ceiling: LEAST($2, ...) already
	// caps the refilled amount at burst.
	var remaining sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `
		UPDATE rate_limit_buckets
		SET tokens = LEAST($2::numeric, tokens + GREATEST(0, EXTRACT(EPOCH FROM (now() - last_refill_at))) * $3::numeric) - 1,
			last_refill_at = now()
		WHERE scope_key = $1
		  AND LEAST($2::numeric, tokens + GREATEST(0, EXTRACT(EPOCH FROM (now() - last_refill_at))) * $3::numeric) >= 1
		RETURNING tokens`,
		scopeKey, burst, ratePerSec,
	).Scan(&remaining)
	if errors.Is(err, sql.ErrNoRows) {
		// Not enough tokens. Read the bucket's own (already-refilled-as-
		// of-now, not-yet-consumed) state to compute an exact wait,
		// rather than guessing.
		var tokens float64
		if rerr := s.db.QueryRowContext(ctx, `
			SELECT LEAST($2::numeric, tokens + GREATEST(0, EXTRACT(EPOCH FROM (now() - last_refill_at))) * $3::numeric)
			FROM rate_limit_buckets WHERE scope_key = $1`,
			scopeKey, burst, ratePerSec,
		).Scan(&tokens); rerr != nil {
			return false, 0, fmt.Errorf("governance: check rate limit: read tokens after rejection: %w", rerr)
		}
		deficit := 1 - tokens
		if deficit < 0 {
			deficit = 0
		}
		wait := time.Duration(deficit/ratePerSec*float64(time.Second)) + time.Second - 1
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait.Round(time.Second), nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("governance: check rate limit: %w", err)
	}
	return true, 0, nil
}

func (s *Store) ensureBucketExists(ctx context.Context, scopeKey string, burst int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rate_limit_buckets (scope_key, tokens, last_refill_at)
		VALUES ($1, $2, now())
		ON CONFLICT (scope_key) DO NOTHING`, scopeKey, float64(burst))
	if err != nil {
		return fmt.Errorf("governance: ensure rate limit bucket: %w", err)
	}
	return nil
}
