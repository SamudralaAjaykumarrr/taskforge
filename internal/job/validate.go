// Phase 11 (docs/enterprise-roadmap.md "Transactional Enqueue & API Contract
// Hardening"): ValidateSubmission is the single, shared implementation of
// TaskForge's submission-validation rules -- job_type presence/length,
// payload JSON validity, max_attempts/execution_timeout_seconds bounds and
// defaults, and Idempotency-Key normalization/length. Before this phase,
// internal/api/handlers.go (validateCreateJobRequest) and
// internal/api/workflow_handlers.go (validateWorkflowNodeRequest) each
// carried their own copy of these same rules, and the new pgx.Tx-based
// enqueue path (see the txenqueue package) needed a third. Factoring the
// rules here means all three entry points (HTTP job submission, HTTP
// workflow-node submission, and direct-Go transactional enqueue) can never
// silently diverge on what "a valid submission" means.
package job

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Default values applied when a submission omits max_attempts/
// execution_timeout_seconds. docs/data-model.md gives max_attempts a
// schema default of 5; execution_timeout_seconds has no schema default (it
// is NOT NULL with no DEFAULT), so the application layer supplies one.
const (
	DefaultMaxAttempts             = 5
	DefaultExecutionTimeoutSeconds = 30

	// MaxJobTypeLength bounds job_type. Not a documented schema
	// constraint (the column is plain TEXT) -- an application-layer
	// choice made in Phase 1 and preserved here unchanged.
	MaxJobTypeLength = 255

	// MaxIdempotencyKeyLength bounds the optional Idempotency-Key. Not a
	// documented schema constraint (the column is plain TEXT) -- a
	// Phase 4 application-layer choice, matching MaxJobTypeLength, kept
	// here unchanged. See docs/idempotency.md "Implementation Notes."
	MaxIdempotencyKeyLength = 255

	// DefaultQueueName is the queue every pre-Phase-13 row carries
	// (migration 0011's column default) and every submission that omits
	// queue_name is assigned, per docs/phase-13-plan.md §14's
	// compatibility requirement.
	DefaultQueueName = "default"

	// MaxQueueNameLength bounds queue_name. Not a documented schema
	// constraint (OD-8: free-form TEXT, no registry table, mirroring the
	// job_type precedent exactly) -- an application-layer choice matching
	// MaxJobTypeLength.
	MaxQueueNameLength = 255

	// MaxRepresentableMaxAttempts is the largest caller-supplied
	// max_attempts value that PostgreSQL's jobs.max_attempts column
	// (INTEGER, i.e. a 32-bit signed integer -- migrations/0001_create_jobs_table.up.sql)
	// can actually store. Go's int is 64-bit on every platform TaskForge
	// ships for, so a caller-supplied value can be a valid Go int (and
	// pass a naive ">= 1" check) while being outside PostgreSQL's INTEGER
	// range -- rejecting such a value here, before any INSERT is
	// attempted, turns what would otherwise be a database-level
	// out-of-range error (an internal 500, per Phase 11's public-API
	// error-leakage requirement) into an ordinary, documented 400/invalid-
	// request response at every submission entry point (POST /jobs,
	// POST /workflows, txenqueue). This is a storage-representability
	// bound, not a smaller operational/product retry-budget cap -- see
	// the no-upper-bound policy decision below.
	MaxRepresentableMaxAttempts = math.MaxInt32
)

// Deliberately absent: an enforced *operational* upper bound on
// caller-supplied max_attempts, beyond MaxRepresentableMaxAttempts's
// storage-representability bound above. TaskForge documents this as an
// explicit, no-arbitrary-product-cap policy decision rather than an
// enforced business limit -- see docs/retry-semantics.md "Open Questions"
// and docs/security-model.md's "Abusive retry workload" row for the
// reasoning (Phase 11, docs/enterprise-roadmap.md, closes this gap by
// documenting the policy, not by inventing an arbitrary number). This is
// not a claim that a very large max_attempts is free: retries consume
// shared worker, database, and scheduling resources, so an operator-facing
// governance control over aggregate retry budget remains an accepted,
// deferred risk for Phase 13 (docs/security-model.md), not something this
// phase claims is harmless.

// ValidateSubmission applies TaskForge's job-field submission rules to
// already-decoded Go values (not JSON) so it can be shared by an HTTP
// handler (which decodes JSON first) and a direct-Go caller (which never
// touches JSON at all -- see the txenqueue package). maxAttempts and
// executionTimeoutSeconds are pointers so "not supplied" (nil) is
// distinguishable from "explicitly zero": nil applies the documented
// default above; a non-nil value less than 1 is rejected. idempotencyKey
// is a pointer to the caller-supplied value (already extracted from
// whatever transport-specific location it came from -- an HTTP header, or
// a direct field); nil, or a value that is empty/all-whitespace after
// trimming, means "no key supplied," per docs/idempotency.md.
//
// Error strings are stable text intended to be surfaced directly to
// callers (an HTTP 400 body, or a returned Go error) -- see
// docs/compatibility-policy.md on not silently changing established
// wording.
func ValidateSubmission(jobType string, payload json.RawMessage, maxAttempts, executionTimeoutSeconds *int, idempotencyKey *string) (NewParams, error) {
	jt := strings.TrimSpace(jobType)
	if jt == "" {
		return NewParams{}, fmt.Errorf("job_type is required")
	}
	if len(jt) > MaxJobTypeLength {
		return NewParams{}, fmt.Errorf("job_type must be at most %d characters", MaxJobTypeLength)
	}

	p := payload
	if len(p) == 0 {
		p = json.RawMessage(`{}`)
	} else if !json.Valid(p) {
		return NewParams{}, fmt.Errorf("payload must be valid JSON")
	}

	ma := DefaultMaxAttempts
	if maxAttempts != nil {
		if *maxAttempts < 1 {
			return NewParams{}, fmt.Errorf("max_attempts must be at least 1")
		}
		if *maxAttempts > MaxRepresentableMaxAttempts {
			return NewParams{}, fmt.Errorf("max_attempts must be at most %d", MaxRepresentableMaxAttempts)
		}
		ma = *maxAttempts
	}

	et := DefaultExecutionTimeoutSeconds
	if executionTimeoutSeconds != nil {
		if *executionTimeoutSeconds < 1 {
			return NewParams{}, fmt.Errorf("execution_timeout_seconds must be at least 1")
		}
		et = *executionTimeoutSeconds
	}

	key, err := ValidateIdempotencyKey(idempotencyKey)
	if err != nil {
		return NewParams{}, err
	}

	return NewParams{
		JobType:                 jt,
		Payload:                 p,
		MaxAttempts:             ma,
		ExecutionTimeoutSeconds: et,
		IdempotencyKey:          key,
		// Defaulted here (not left empty) so every caller of
		// ValidateSubmission gets a NewParams that is already safe to
		// INSERT even if it never separately calls ValidateQueueName --
		// callers that do supply a request-level queue_name overwrite
		// this afterward, exactly as they already do for ScheduledAt.
		QueueName: DefaultQueueName,
	}, nil
}

// ValidateQueueName normalizes and bounds-checks an optional,
// already-extracted queue_name value (docs/phase-13-plan.md §8: "POST
// /jobs, POST /workflows: new optional request field queue_name ...
// omitted/empty -> 'default'"). nil, or a value that is empty or
// all-whitespace after trimming, defaults to DefaultQueueName -- unlike
// ValidateIdempotencyKey, "not supplied" here is never represented as a Go
// nil/empty value flowing further, because jobs.queue_name is NOT NULL and
// every INSERT this package drives supplies an explicit value rather than
// relying on the schema's own column default.
//
// Called separately from ValidateSubmission (like ScheduledAt in
// internal/api/handlers.go), not folded into it, so every existing caller
// of ValidateSubmission continues to compile and behave identically; a
// caller that cares about queue_name calls this too and assigns the result
// to NewParams.QueueName, exactly as it already does for ScheduledAt.
func ValidateQueueName(raw *string) (string, error) {
	if raw == nil {
		return DefaultQueueName, nil
	}
	name := strings.TrimSpace(*raw)
	if name == "" {
		return DefaultQueueName, nil
	}
	if len(name) > MaxQueueNameLength {
		return "", fmt.Errorf("queue_name must be at most %d characters", MaxQueueNameLength)
	}
	return name, nil
}

// ValidateIdempotencyKey normalizes and bounds-checks an optional,
// already-extracted Idempotency-Key value. nil, or a value that is empty
// or all-whitespace after trimming, is treated as "no key supplied" -- not
// a validation error -- per docs/idempotency.md "Implementation Notes":
// operationally indistinguishable from a caller who did not mean to send
// one.
func ValidateIdempotencyKey(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	key := strings.TrimSpace(*raw)
	if key == "" {
		return nil, nil
	}
	if len(key) > MaxIdempotencyKeyLength {
		return nil, fmt.Errorf("idempotency_key must be at most %d characters", MaxIdempotencyKeyLength)
	}
	return &key, nil
}
