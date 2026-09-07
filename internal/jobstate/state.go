// Package jobstate is the authoritative, pure-function representation of
// the job state machine documented in docs/execution-semantics.md. It
// exists so that "is this transition legal" is answered in exactly one
// place in the codebase (docs/execution-semantics.md's Transition Table),
// rather than being re-derived ad hoc wherever a state assignment happens.
//
// This package documents the FULL state machine, including transitions
// (RETRY_WAIT, CANCELLED, RUNNING->RUNNING reclaim) that Phase 1 does not
// yet drive end-to-end. Phase 1's actual database operations (see
// internal/store) only ever execute a subset of the legal transitions
// below (insert->QUEUED, QUEUED->RUNNING, RUNNING->SUCCEEDED,
// RUNNING->DEAD_LETTERED); the remaining legal transitions are recorded
// here now, per docs/execution-semantics.md, so that (a) this table can be
// tested exhaustively against the documented contract today, and (b) later
// phases implement new Store methods against an already-reviewed rulebook
// instead of inventing transition legality alongside new SQL.
package jobstate

// State is a job-level state, one of the six values in
// docs/execution-semantics.md.
type State string

const (
	Queued       State = "QUEUED"
	RetryWait    State = "RETRY_WAIT"
	Running      State = "RUNNING"
	Succeeded    State = "SUCCEEDED"
	Cancelled    State = "CANCELLED"
	DeadLettered State = "DEAD_LETTERED"
)

// All returns every job-level state, in a stable order. Used by table
// tests that need to enumerate all (from, to) pairs.
func All() []State {
	return []State{Queued, RetryWait, Running, Succeeded, Cancelled, DeadLettered}
}

// IsTerminal reports whether s has no legal outbound transition, per
// ADR-0008 and TF-INV-005.
func IsTerminal(s State) bool {
	switch s {
	case Succeeded, Cancelled, DeadLettered:
		return true
	default:
		return false
	}
}

// transitions enumerates every legal (from, to) edge in
// docs/execution-semantics.md's Transition Table. A (from, to) pair absent
// from this table is forbidden by default, per that document's explicit
// statement: "anything not in the table above is forbidden by default."
//
// Terminal states are present as keys with an empty edge set, making the
// "no outbound transition" property explicit rather than implicit in a
// missing map entry.
var transitions = map[State]map[State]bool{
	Queued: {
		Running:   true, // claim
		Cancelled: true, // cancel before claim
	},
	RetryWait: {
		Running:   true, // claim (reclaim path in Phase 2+)
		Cancelled: true, // cancel before claim
	},
	Running: {
		Succeeded:    true, // worker reports success
		RetryWait:    true, // worker reports retryable failure, attempts remain (Phase 3+)
		DeadLettered: true, // worker reports permanent failure, or retries exhausted, or lease-expiry sweep
		Running:      true, // lease-expiry reclaim: new generation, same state (Phase 2+)
		Cancelled:    true, // worker acknowledges a pending cancellation request (Phase 6+)
	},
	Succeeded:    {}, // terminal: no outbound transition
	Cancelled:    {}, // terminal: no outbound transition
	DeadLettered: {}, // terminal: no outbound transition
}

// IsValidTransition reports whether from -> to is a legal job-level state
// transition per docs/execution-semantics.md. It is the single source of
// truth other packages must consult before performing (or asserting) a
// state change; it does not, by itself, check ownership/lease fencing —
// see internal/store for the database-enforced guard clauses that make a
// transition attempt fail closed (affect zero rows) even when the Go-level
// table would allow it in the abstract (e.g. a stale lease_generation).
func IsValidTransition(from, to State) bool {
	edges, ok := transitions[from]
	if !ok {
		return false
	}
	return edges[to]
}
