// Unit tests for request validation only (docs/testing-strategy.md "Unit
// tests: Pure logic with no database — ... request validation."). These
// deliberately do not touch a database and must not be read as proof of
// persistence correctness — see handlers_integration_test.go for that.
package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCreateJobRequest_ValidMinimal(t *testing.T) {
	params, verr := validateCreateJobRequest(createJobRequest{JobType: "email.send"})
	require.Empty(t, verr)
	require.Equal(t, "email.send", params.JobType)
	require.Equal(t, DefaultMaxAttempts, params.MaxAttempts)
	require.Equal(t, DefaultExecutionTimeoutSeconds, params.ExecutionTimeoutSeconds)
	require.JSONEq(t, `{}`, string(params.Payload))
}

func TestValidateCreateJobRequest_TrimsJobType(t *testing.T) {
	params, verr := validateCreateJobRequest(createJobRequest{JobType: "  email.send  "})
	require.Empty(t, verr)
	require.Equal(t, "email.send", params.JobType)
}

func TestValidateCreateJobRequest_EmptyJobTypeRejected(t *testing.T) {
	_, verr := validateCreateJobRequest(createJobRequest{JobType: "   "})
	require.NotEmpty(t, verr)
}

func TestValidateCreateJobRequest_OversizedJobTypeRejected(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	_, verr := validateCreateJobRequest(createJobRequest{JobType: string(long)})
	require.NotEmpty(t, verr)
}

func TestValidateCreateJobRequest_InvalidPayloadJSONRejected(t *testing.T) {
	_, verr := validateCreateJobRequest(createJobRequest{
		JobType: "email.send",
		Payload: json.RawMessage(`{not valid json`),
	})
	require.NotEmpty(t, verr)
}

func TestValidateCreateJobRequest_CustomMaxAttempts(t *testing.T) {
	ma := 3
	params, verr := validateCreateJobRequest(createJobRequest{JobType: "x", MaxAttempts: &ma})
	require.Empty(t, verr)
	require.Equal(t, 3, params.MaxAttempts)
}

func TestValidateCreateJobRequest_ZeroMaxAttemptsRejected(t *testing.T) {
	ma := 0
	_, verr := validateCreateJobRequest(createJobRequest{JobType: "x", MaxAttempts: &ma})
	require.NotEmpty(t, verr)
}

func TestValidateCreateJobRequest_NegativeExecutionTimeoutRejected(t *testing.T) {
	timeout := -1
	_, verr := validateCreateJobRequest(createJobRequest{JobType: "x", ExecutionTimeoutSeconds: &timeout})
	require.NotEmpty(t, verr)
}

func TestValidateCreateJobRequest_CustomExecutionTimeout(t *testing.T) {
	timeout := 120
	params, verr := validateCreateJobRequest(createJobRequest{JobType: "x", ExecutionTimeoutSeconds: &timeout})
	require.Empty(t, verr)
	require.Equal(t, 120, params.ExecutionTimeoutSeconds)
}
