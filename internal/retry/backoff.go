// Package retry implements docs/retry-semantics.md's backoff formula:
// exponential backoff with equal jitter, capped at a configured maximum.
// It is intentionally pure/DB-free -- computing a delay never needs
// PostgreSQL -- so it can be unit-tested deterministically. The caller
// (internal/worker) is responsible for turning the returned time.Duration
// into a durably-persisted eligible_at via internal/store, which computes
// that timestamp using PostgreSQL's own clock (now() + interval), not this
// package's or the worker's local clock, per docs/failure-model.md's Clock
// Model: only the *duration* is computed here, and a duration (unlike an
// absolute timestamp) is not subject to cross-machine clock skew.
package retry

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Defaults, per docs/retry-semantics.md "Backoff Policy (v1 default)".
const (
	DefaultBaseDelay  = 1 * time.Second
	DefaultMaxBackoff = 300 * time.Second
)

// Config holds the two backoff parameters docs/retry-semantics.md defines.
// Per-job-type configuration is an explicitly deferred open question in
// that document (and docs/roadmap.md Phase 3 non-goals) -- v1 has exactly
// one, global Config.
type Config struct {
	BaseDelay  time.Duration
	MaxBackoff time.Duration
}

// DefaultConfig returns docs/retry-semantics.md's v1 defaults: 1s base
// delay, 300s (5 minute) cap.
func DefaultConfig() Config {
	return Config{BaseDelay: DefaultBaseDelay, MaxBackoff: DefaultMaxBackoff}
}

// Validate rejects a Config that RawDelay cannot safely or meaningfully
// compute against: a non-positive delay/cap, or a base delay that already
// exceeds the cap it's supposed to be bounded by.
func (c Config) Validate() error {
	if c.BaseDelay <= 0 {
		return fmt.Errorf("retry: base delay must be positive, got %s", c.BaseDelay)
	}
	if c.MaxBackoff <= 0 {
		return fmt.Errorf("retry: max backoff must be positive, got %s", c.MaxBackoff)
	}
	if c.BaseDelay > c.MaxBackoff {
		return fmt.Errorf("retry: base delay %s must not exceed max backoff %s", c.BaseDelay, c.MaxBackoff)
	}
	return nil
}

// RawDelay computes docs/retry-semantics.md's pre-jitter backoff:
//
//	raw_delay = min(max_backoff, base_delay * 2^(attempt-1))
//
// attempt is 1-based (the attempt that just failed, i.e. jobs.attempt_count
// at the time of that failure); values below 1 are treated as 1, matching
// attempt 1's raw delay of exactly base_delay.
//
// The computation is overflow-safe: instead of computing
// base_delay * 2^(attempt-1) directly (which overflows time.Duration's
// int64 nanosecond range for a large attempt count and could wrap around to
// a bogus, possibly negative, duration), it first checks -- via division,
// which cannot itself overflow -- whether that product would exceed
// max_backoff, and returns max_backoff directly without ever forming the
// overflowing value. cfg is assumed valid (see Validate); RawDelay does not
// itself validate cfg.
func RawDelay(attempt int, cfg Config) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1

	// A duration fits in 63 bits (int64, always non-negative here). Once
	// the shift alone would require more of those bits than base_delay
	// could possibly leave room for, the product cannot stay in range --
	// short-circuit to the cap without attempting the shift/multiply at
	// all (avoids even constructing a too-large shift amount).
	const maxShift = 62
	if shift >= maxShift {
		return cfg.MaxBackoff
	}

	// factor = 2^shift, computed before comparing against how many
	// multiples of base_delay fit inside max_backoff (floor division,
	// which never overflows since both operands are already valid,
	// positive durations).
	factor := int64(1) << uint(shift)
	maxFactor := int64(cfg.MaxBackoff) / int64(cfg.BaseDelay)
	if factor > maxFactor {
		return cfg.MaxBackoff
	}

	raw := cfg.BaseDelay * time.Duration(factor)
	if raw > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}
	return raw
}

// RandSource is the minimal randomness contract EqualJitter/Delay need.
// *rand.Rand and *Rand both satisfy it. Injectable so backoff jitter is
// deterministically testable (docs/testing-strategy.md: "no flaky timing
// tests") -- a test can supply a stub that always returns a fixed value to
// assert an exact boundary (e.g. zero jitter, or maximum jitter).
type RandSource interface {
	Int63n(n int64) int64
}

// Rand is a mutex-guarded RandSource. math/rand.Rand is not safe for
// concurrent use by multiple goroutines, and a worker pool may compute
// backoff for several jobs concurrently against one shared source.
type Rand struct {
	mu  sync.Mutex
	src *rand.Rand
}

// NewRand returns a Rand seeded deterministically from seed. Production
// callers should seed from a real entropy source (e.g.
// time.Now().UnixNano()); tests seed with a fixed value for
// reproducibility.
func NewRand(seed int64) *Rand {
	return &Rand{src: rand.New(rand.NewSource(seed))} //nolint:gosec // jitter, not security-sensitive
}

// Int63n implements RandSource.
func (r *Rand) Int63n(n int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.src.Int63n(n)
}

// EqualJitter applies docs/retry-semantics.md's "equal jitter" formula to a
// pre-jitter raw delay:
//
//	delay = (raw_delay / 2) + random_uniform(0, raw_delay / 2)
//
// so the result is always within [raw/2, raw] -- never negative, never
// exceeding the raw (and therefore capped) delay it was derived from. A
// non-positive raw returns 0.
func EqualJitter(raw time.Duration, src RandSource) time.Duration {
	if raw <= 0 {
		return 0
	}
	half := raw / 2
	if half <= 0 {
		// raw is 1ns: too small to meaningfully split; no jitter to add.
		return raw
	}
	jitter := time.Duration(src.Int63n(int64(half) + 1))
	return half + jitter
}

// Delay computes the full backoff delay for attempt (the attempt that just
// failed, 1-based) per docs/retry-semantics.md's complete Backoff Policy:
// RawDelay's exponential-with-cap value, with EqualJitter applied.
func Delay(attempt int, cfg Config, src RandSource) time.Duration {
	return EqualJitter(RawDelay(attempt, cfg), src)
}
