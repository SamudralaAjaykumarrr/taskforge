// Phase 12 (docs/phase-12-plan.md §6a, OD-6): the API server's pepper is a
// hard startup requirement, not a soft default.
//
// This matters because of what the alternative would look like: a process
// that starts without a pepper and then fails every request is a confusing
// outage, and a process that starts with a silently-defaulted pepper is a
// security hole (every deployment would share the same HMAC key). Failing
// at startup is the only acceptable behaviour, and OD-6's hard cutover
// means there is no "authentication disabled" mode to fall back to either.
package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
)

const (
	envDatabaseURL = "TASKFORGE_DATABASE_URL"
	envPepper      = "TASKFORGE_API_KEY_PEPPER"
	testDBURL      = "postgres://user:pass@localhost:5432/taskforge?sslmode=disable"
)

func TestFromEnvRequiringPepper_FailsWhenPepperAbsent(t *testing.T) {
	t.Setenv(envDatabaseURL, testDBURL)
	t.Setenv(envPepper, "")

	_, err := config.FromEnvRequiringPepper()
	require.Error(t, err, "cmd/api must refuse to start without a pepper")
	require.Contains(t, err.Error(), envPepper)
}

func TestFromEnvRequiringPepper_FailsWhenPepperTooShort(t *testing.T) {
	t.Setenv(envDatabaseURL, testDBURL)
	t.Setenv(envPepper, "short")

	_, err := config.FromEnvRequiringPepper()
	require.Error(t, err, "an obviously-too-weak pepper must be rejected at startup, not accepted")
	require.Contains(t, err.Error(), envPepper)
}

func TestFromEnvRequiringPepper_NeverEchoesThePepperValue(t *testing.T) {
	t.Setenv(envDatabaseURL, testDBURL)
	const secret = "this-pepper-value-must-never-be-echoed"
	t.Setenv(envPepper, secret)

	// Long enough to pass length validation? No -- deliberately shorter
	// than the minimum, so this exercises the error path that reports a
	// length. Even then, the value itself must not appear.
	_, err := config.FromEnvRequiringPepper()
	if err != nil {
		require.NotContains(t, err.Error(), secret,
			"a configuration error must report the pepper's length, never its value")
	}
}

func TestFromEnvRequiringPepper_AcceptsASufficientPepper(t *testing.T) {
	t.Setenv(envDatabaseURL, testDBURL)
	pepper := strings.Repeat("p", config.MinAPIKeyPepperLength)
	t.Setenv(envPepper, pepper)

	cfg, err := config.FromEnvRequiringPepper()
	require.NoError(t, err)
	require.Equal(t, []byte(pepper), cfg.APIKeyPepper)
}

// TestFromEnv_DoesNotRequireThePepper pins the deliberate asymmetry:
// cmd/worker and the chaos harness have no HTTP surface and no credentials
// to verify, so requiring the pepper of them would be a startup failure
// with no security benefit.
func TestFromEnv_DoesNotRequireThePepper(t *testing.T) {
	t.Setenv(envDatabaseURL, testDBURL)
	t.Setenv(envPepper, "")

	cfg, err := config.FromEnv()
	require.NoError(t, err, "cmd/worker must still start without a pepper")
	require.Empty(t, cfg.APIKeyPepper)
}
