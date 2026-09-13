// Phase 11 audit-fix regression (docs/enterprise-roadmap.md "Transactional
// Enqueue & API Contract Hardening"): max_attempts must be rejected by
// ValidateSubmission -- a 400/invalid-request outcome at every submission
// entry point -- before it ever reaches PostgreSQL's jobs.max_attempts
// INTEGER column (migrations/0001_create_jobs_table.up.sql), which cannot
// represent a value outside a 32-bit signed integer's range. Go's int is
// 64-bit on every platform this project ships for, so a caller-supplied
// value can be a valid Go int and still be unrepresentable in that column.
package job_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// TestValidateSubmission_MaxAttempts_LargestRepresentableValue_Accepted
// proves the boundary itself is inclusive: math.MaxInt32
// (job.MaxRepresentableMaxAttempts) -- the largest value PostgreSQL's
// INTEGER column can actually store -- is accepted, not rejected.
func TestValidateSubmission_MaxAttempts_LargestRepresentableValue_Accepted(t *testing.T) {
	ma := job.MaxRepresentableMaxAttempts
	require.Equal(t, math.MaxInt32, ma, "sanity: this bound must track PostgreSQL's INTEGER range")

	params, err := job.ValidateSubmission("email.send", nil, &ma, nil, nil)
	require.NoError(t, err)
	require.Equal(t, math.MaxInt32, params.MaxAttempts)
}

// TestValidateSubmission_MaxAttempts_FirstUnrepresentableValue_Rejected
// proves the very next value -- math.MaxInt32+1, still a perfectly valid
// Go int -- is rejected by ValidateSubmission itself, before any store/SQL
// call is made: a client-supplied max_attempts a naive ">= 1" check would
// have accepted must never turn into an internal 500 once it reaches
// PostgreSQL; it must be an ordinary, documented 400/invalid-request
// response instead.
func TestValidateSubmission_MaxAttempts_FirstUnrepresentableValue_Rejected(t *testing.T) {
	tooLarge := job.MaxRepresentableMaxAttempts + 1
	require.Greater(t, int64(tooLarge), int64(math.MaxInt32))

	_, err := job.ValidateSubmission("email.send", nil, &tooLarge, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_attempts must be at most")
}

// TestValidateSubmission_MaxAttempts_DefaultAndLowerBoundUnaffected is a
// narrow non-regression check that this phase's new upper bound did not
// disturb the existing default-application and lower-bound (>= 1) rules.
func TestValidateSubmission_MaxAttempts_DefaultAndLowerBoundUnaffected(t *testing.T) {
	params, err := job.ValidateSubmission("email.send", nil, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, job.DefaultMaxAttempts, params.MaxAttempts)

	zero := 0
	_, err = job.ValidateSubmission("email.send", nil, &zero, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_attempts must be at least 1")
}
