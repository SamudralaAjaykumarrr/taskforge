package retry_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/retry"
)

// fixedSource is a RandSource stub that always returns a fixed value,
// clamped to (0, n), for deterministic boundary assertions.
type fixedSource struct {
	value int64
}

func (f fixedSource) Int63n(n int64) int64 {
	if f.value >= n {
		return n - 1
	}
	return f.value
}

// TestRawDelay_MatchesDocumentedFormula proves RawDelay computes exactly
// docs/retry-semantics.md's "raw_delay = min(max_backoff, base_delay *
// 2^(attempt-1))" for a range of concrete attempts against the v1 defaults
// (base=1s, max=300s).
func TestRawDelay_MatchesDocumentedFormula(t *testing.T) {
	cfg := retry.DefaultConfig()

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 1 * time.Second},   // 1 * 2^0
		{attempt: 2, want: 2 * time.Second},   // 1 * 2^1
		{attempt: 3, want: 4 * time.Second},   // 1 * 2^2
		{attempt: 4, want: 8 * time.Second},   // 1 * 2^3
		{attempt: 9, want: 256 * time.Second}, // 1 * 2^8, still under the 300s cap
		{attempt: 10, want: 300 * time.Second},
		{attempt: 11, want: 300 * time.Second},
	}
	for _, c := range cases {
		got := retry.RawDelay(c.attempt, cfg)
		require.Equal(t, c.want, got, "attempt %d", c.attempt)
	}
}

// TestRawDelay_AttemptBelowOneTreatedAsOne proves the documented 1-based
// convention holds even for a defensively-passed 0 or negative attempt.
func TestRawDelay_AttemptBelowOneTreatedAsOne(t *testing.T) {
	cfg := retry.DefaultConfig()
	want := retry.RawDelay(1, cfg)
	require.Equal(t, want, retry.RawDelay(0, cfg))
	require.Equal(t, want, retry.RawDelay(-5, cfg))
}

// TestRawDelay_CappedAtMaxBackoff proves the cap is enforced across a wide
// sweep of attempt numbers, never exceeding MaxBackoff.
func TestRawDelay_CappedAtMaxBackoff(t *testing.T) {
	cfg := retry.DefaultConfig()
	for attempt := 1; attempt <= 200; attempt++ {
		got := retry.RawDelay(attempt, cfg)
		require.LessOrEqual(t, got, cfg.MaxBackoff, "attempt %d exceeded max backoff", attempt)
		require.GreaterOrEqual(t, got, time.Duration(0), "attempt %d produced a negative delay", attempt)
	}
}

// TestRawDelay_OverflowSafeAtExtremeAttempts is adversarial case #12:
// "backoff multiplication approaches overflow." A naive
// base_delay << (attempt-1) computation would overflow int64 and could wrap
// to a negative or nonsensical duration well before attempt reaches
// math.MaxInt. RawDelay must return exactly MaxBackoff, never panic, and
// never go negative.
func TestRawDelay_OverflowSafeAtExtremeAttempts(t *testing.T) {
	cfg := retry.DefaultConfig()
	for _, attempt := range []int{62, 63, 64, 100, 1000, 1 << 20, 1 << 30} {
		require.NotPanics(t, func() {
			got := retry.RawDelay(attempt, cfg)
			require.Equal(t, cfg.MaxBackoff, got, "attempt %d", attempt)
		})
	}
}

// TestRawDelay_LargeBaseDelayStillOverflowSafe proves the overflow guard
// holds even with a much larger-than-default base delay, where a smaller
// attempt count is enough to approach overflow.
func TestRawDelay_LargeBaseDelayStillOverflowSafe(t *testing.T) {
	cfg := retry.Config{BaseDelay: 1 * time.Hour, MaxBackoff: 24 * time.Hour}
	for attempt := 1; attempt <= 128; attempt++ {
		got := retry.RawDelay(attempt, cfg)
		require.LessOrEqual(t, got, cfg.MaxBackoff)
		require.GreaterOrEqual(t, got, time.Duration(0))
	}
	require.Equal(t, cfg.MaxBackoff, retry.RawDelay(10, cfg))
}

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     retry.Config
		wantErr bool
	}{
		{"defaults ok", retry.DefaultConfig(), false},
		{"zero base delay", retry.Config{BaseDelay: 0, MaxBackoff: time.Second}, true},
		{"negative base delay", retry.Config{BaseDelay: -time.Second, MaxBackoff: time.Second}, true},
		{"zero max backoff", retry.Config{BaseDelay: time.Second, MaxBackoff: 0}, true},
		{"negative max backoff", retry.Config{BaseDelay: time.Second, MaxBackoff: -time.Second}, true},
		{"base exceeds max", retry.Config{BaseDelay: 10 * time.Second, MaxBackoff: time.Second}, true},
		{"base equals max ok", retry.Config{BaseDelay: time.Second, MaxBackoff: time.Second}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestEqualJitter_BoundsAndDeterminism proves docs/retry-semantics.md's
// "delay = (raw/2) + random_uniform(0, raw/2)" formula, using a stub
// RandSource for exact, deterministic boundary assertions rather than
// asserting only a range across real randomness.
func TestEqualJitter_BoundsAndDeterminism(t *testing.T) {
	raw := 100 * time.Second
	half := raw / 2

	// Zero jitter: result must equal exactly half.
	require.Equal(t, half, retry.EqualJitter(raw, fixedSource{value: 0}))

	// Maximum jitter: result must equal exactly raw (never exceed it).
	require.Equal(t, raw, retry.EqualJitter(raw, fixedSource{value: int64(half)}))

	// Mid jitter.
	got := retry.EqualJitter(raw, fixedSource{value: int64(half) / 2})
	require.Equal(t, half+half/2, got)
}

func TestEqualJitter_NonPositiveRawReturnsZero(t *testing.T) {
	require.Equal(t, time.Duration(0), retry.EqualJitter(0, fixedSource{}))
	require.Equal(t, time.Duration(0), retry.EqualJitter(-5*time.Second, fixedSource{}))
}

// TestEqualJitter_NeverNegativeNeverExceedsRaw sweeps a real (seeded,
// reproducible) rand.Rand source across many raw values and iterations,
// proving the invariant holds broadly, not just at the two stub-tested
// exact boundaries.
func TestEqualJitter_NeverNegativeNeverExceedsRaw(t *testing.T) {
	src := retry.NewRand(12345)
	for _, raw := range []time.Duration{time.Second, 7 * time.Second, 300 * time.Second, 1} {
		for i := 0; i < 1000; i++ {
			got := retry.EqualJitter(raw, src)
			require.GreaterOrEqual(t, got, time.Duration(0))
			require.LessOrEqual(t, got, raw)
		}
	}
}

// TestDelay_CapAndDeterminism proves the composed Delay function (RawDelay
// + EqualJitter) never exceeds cfg.MaxBackoff even at extreme attempt
// counts, and is exactly reproducible given a fixed source.
func TestDelay_CapAndDeterminism(t *testing.T) {
	cfg := retry.DefaultConfig()
	for _, attempt := range []int{1, 5, 10, 50, 1000} {
		got := retry.Delay(attempt, cfg, fixedSource{value: 0})
		require.LessOrEqual(t, got, cfg.MaxBackoff, "attempt %d", attempt)
		require.GreaterOrEqual(t, got, time.Duration(0), "attempt %d", attempt)

		// Deterministic: same attempt, same fixed source -> same delay.
		got2 := retry.Delay(attempt, cfg, fixedSource{value: 0})
		require.Equal(t, got, got2)
	}
}

// TestRand_ConcurrentUseIsRaceFree proves Rand is safe under concurrent
// use (run with -race); math/rand.Rand alone is not.
func TestRand_ConcurrentUseIsRaceFree(t *testing.T) {
	r := retry.NewRand(1)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Int63n(1000)
			}
		}()
	}
	wg.Wait()
}
