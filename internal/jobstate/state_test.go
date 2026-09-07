package jobstate_test

import (
	"testing"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// legalPairs is a hand-transcribed copy of docs/execution-semantics.md's
// "Transition Table", independent of internal/jobstate's implementation,
// so this test actually checks the implementation against the documented
// contract rather than against itself.
var legalPairs = map[[2]jobstate.State]bool{
	{jobstate.Queued, jobstate.Running}:       true,
	{jobstate.Queued, jobstate.Cancelled}:     true,
	{jobstate.RetryWait, jobstate.Running}:    true,
	{jobstate.RetryWait, jobstate.Cancelled}:  true,
	{jobstate.Running, jobstate.Succeeded}:    true,
	{jobstate.Running, jobstate.RetryWait}:    true,
	{jobstate.Running, jobstate.DeadLettered}: true,
	{jobstate.Running, jobstate.Running}:      true, // lease-expiry reclaim, new generation
	{jobstate.Running, jobstate.Cancelled}:    true,
}

// TestTransitionTable_FullMatrix enumerates every (from, to) pair across
// all six states (36 pairs) and asserts IsValidTransition matches
// legalPairs exactly — i.e. every pair not documented as legal is
// rejected. This is the table-driven test required by
// docs/testing-strategy.md's "state-machine table tests" category and
// proves TF-INV-005 ("terminal states never become non-terminal") as a
// side effect: every pair with a terminal `from` is asserted forbidden.
func TestTransitionTable_FullMatrix(t *testing.T) {
	for _, from := range jobstate.All() {
		for _, to := range jobstate.All() {
			want := legalPairs[[2]jobstate.State{from, to}]
			got := jobstate.IsValidTransition(from, to)
			if got != want {
				t.Errorf("IsValidTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestTerminalStatesHaveNoOutboundTransition is an explicit, adversarial
// restatement of TF-INV-005: for each terminal state, every possible
// destination (including itself) must be rejected. Not merely a
// consequence of the matrix test above — this test would fail loudly on
// its own even if legalPairs itself were wrong about a terminal state.
func TestTerminalStatesHaveNoOutboundTransition(t *testing.T) {
	terminal := []jobstate.State{jobstate.Succeeded, jobstate.Cancelled, jobstate.DeadLettered}
	for _, from := range terminal {
		if !jobstate.IsTerminal(from) {
			t.Fatalf("%s should be reported terminal", from)
		}
		for _, to := range jobstate.All() {
			if jobstate.IsValidTransition(from, to) {
				t.Errorf("terminal state %s must have no outbound transition, but %s -> %s was accepted", from, from, to)
			}
		}
	}
}

// TestNonTerminalStatesAreNotTerminal guards against a future edit
// accidentally marking a non-terminal state terminal (which would make
// IsValidTransition vacuously "correct" by having no outbound edges to
// check).
func TestNonTerminalStatesAreNotTerminal(t *testing.T) {
	for _, s := range []jobstate.State{jobstate.Queued, jobstate.RetryWait, jobstate.Running} {
		if jobstate.IsTerminal(s) {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

// TestExplicitlyForbiddenTransitions asserts the specific named-forbidden
// transitions from docs/execution-semantics.md's "Explicitly Forbidden
// Transitions" section, so a regression here fails with a message that
// names the exact documented rule broken, not just "matrix mismatch".
func TestExplicitlyForbiddenTransitions(t *testing.T) {
	cases := []struct {
		name string
		from jobstate.State
		to   jobstate.State
	}{
		{"SUCCEEDED -> RUNNING forbidden: a completed job never re-executes", jobstate.Succeeded, jobstate.Running},
		{"CANCELLED -> RUNNING forbidden: a cancelled row can never be claimed", jobstate.Cancelled, jobstate.Running},
		{"DEAD_LETTERED -> RETRY_WAIT forbidden: dead-lettering is a terminal boundary", jobstate.DeadLettered, jobstate.RetryWait},
		{"RUNNING -> QUEUED forbidden: no shortcut back to immediate eligibility", jobstate.Running, jobstate.Queued},
		{"QUEUED -> SUCCEEDED forbidden: cannot complete without being claimed", jobstate.Queued, jobstate.Succeeded},
		{"QUEUED -> DEAD_LETTERED forbidden: cannot dead-letter without being claimed", jobstate.Queued, jobstate.DeadLettered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if jobstate.IsValidTransition(tc.from, tc.to) {
				t.Errorf("%s: expected %s -> %s to be rejected, but it was accepted", tc.name, tc.from, tc.to)
			}
		})
	}
}

func TestIsValidTransition_UnknownStateIsAlwaysRejected(t *testing.T) {
	if jobstate.IsValidTransition("BOGUS", jobstate.Queued) {
		t.Error("an unrecognized source state must never be reported as having a valid transition")
	}
}
