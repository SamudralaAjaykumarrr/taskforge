package store

import "errors"

var (
	// ErrNotFound is returned when a job row does not exist.
	ErrNotFound = errors.New("store: job not found")

	// ErrStaleTransition is returned when a fenced state-transition UPDATE
	// affects zero rows: the job is not in the expected source state,
	// and/or the supplied lease_owner/lease_generation no longer match
	// the current row. This is the mechanism behind TF-INV-003,
	// TF-INV-005, and TF-INV-014 — see docs/worker-protocol.md "The
	// Fencing Guarantee, Stated Precisely." It is never returned by
	// silently succeeding; callers must treat it as an explicit
	// rejection, not a network-style transient error.
	ErrStaleTransition = errors.New("store: stale or invalid transition rejected")

	// ErrInvalidTransition is returned when calling code asks for a
	// (from, to) pair that internal/jobstate.IsValidTransition says is
	// illegal, before any SQL is even issued. This should be unreachable
	// given the fixed queries in this package; it exists as defense in
	// depth against a future refactor that parameterizes the transition.
	ErrInvalidTransition = errors.New("store: invalid state transition")
)
