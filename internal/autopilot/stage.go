package autopilot

import "fmt"

// Stage is the coarse phase of work Autopilot is carrying out. Both stages
// share the same Step sequence (below) -- what "Draft" and "Review" mean
// differs (a plan doc vs. an implementation), but the shape of the
// bounded workflow is identical, matching this repository's own real
// planning-PR-then-implementation-PR history.
type Stage string

const (
	StagePlanning       Stage = "planning"
	StageImplementation Stage = "implementation"
)

func (s Stage) Valid() bool {
	return s == StagePlanning || s == StageImplementation
}

// Step is one node in Autopilot's bounded state machine.
type Step string

const (
	StepNotStarted    Step = "not_started"
	StepBranch        Step = "branch"
	StepDraft         Step = "draft"
	StepReview        Step = "review"
	StepFixBlocker    Step = "fix_blocker"
	StepRecheck       Step = "recheck"
	StepLocalGates    Step = "local_gates"
	StepCommit        Step = "commit"
	StepPush          Step = "push"
	StepPRCreate      Step = "pr_create"
	StepCIWatch       Step = "ci_watch"
	StepCIRepair      Step = "ci_repair"
	StepMergeApproval Step = "merge_approval"
	StepMerge         Step = "merge"
	StepPostMerge     Step = "post_merge"
	StepComplete      Step = "complete"
)

// Status is the run-level disposition of the current state.
type Status string

const (
	StatusRunning          Status = "running"
	StatusPaused           Status = "paused"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusFailed           Status = "failed"
	StatusCompleted        Status = "completed"
)

// PauseReason names exactly which human gate Autopilot stopped at. These
// correspond 1:1 to the nine human gates in docs/autopilot.md "Human
// Gates."
type PauseReason string

const (
	PauseArchitecturalDecision PauseReason = "architectural_decision"
	PauseDestructiveMigration  PauseReason = "destructive_migration"
	PauseSecurityTrustBoundary PauseReason = "security_trust_boundary_change"
	PauseInvariantChange       PauseReason = "invariant_change"
	PauseProofWeakening        PauseReason = "test_or_proof_obligation_weakening"
	PauseForcePushOrBypass     PauseReason = "force_push_or_main_protection_bypass"
	PauseCIRepairExhausted     PauseReason = "ci_repair_attempts_exhausted"
	PauseReleaseApproval       PauseReason = "release_candidate_or_v1_approval"
	PauseExternalValidation    PauseReason = "external_human_validation_required"
	PauseMergeConfirmation     PauseReason = "merge_confirmation_required"
	PauseManual                PauseReason = "manual_pause"
)

// BlockerType classifies what an independent review (or the implementation
// agent itself) found. Autopilot only attempts a bounded, automated fix for
// BlockerOrdinary; every other category is, by this project's own
// discipline, a human decision (see docs/autopilot.md "Human Gates") and
// Autopilot pauses immediately instead of trying to resolve it.
type BlockerType string

const (
	BlockerNone         BlockerType = "none"
	BlockerOrdinary     BlockerType = "ordinary"
	BlockerArchitecture BlockerType = "architecture"
	BlockerMigration    BlockerType = "migration"
	BlockerSecurity     BlockerType = "security"
	BlockerInvariant    BlockerType = "invariant"
	BlockerProofWeaken  BlockerType = "proof_weakening"
)

func (b BlockerType) PauseReason() (PauseReason, bool) {
	switch b {
	case BlockerArchitecture:
		return PauseArchitecturalDecision, true
	case BlockerMigration:
		return PauseDestructiveMigration, true
	case BlockerSecurity:
		return PauseSecurityTrustBoundary, true
	case BlockerInvariant:
		return PauseInvariantChange, true
	case BlockerProofWeaken:
		return PauseProofWeakening, true
	default:
		return "", false
	}
}

// stepOrder is the canonical linear sequence every run walks through,
// modulo the conditional branches encoded in nextSteps below (skipping
// FixBlocker/Recheck on a clean review approval, looping CIRepair back to
// LocalGates on a bounded CI-repair iteration).
var stepOrder = []Step{
	StepBranch, StepDraft, StepReview, StepFixBlocker, StepRecheck,
	StepLocalGates, StepCommit, StepPush, StepPRCreate,
	StepCIWatch, StepCIRepair, StepMergeApproval, StepMerge,
	StepPostMerge, StepComplete,
}

// nextSteps enumerates, for each step, every step that step is allowed to
// transition to. This is the single source of truth ValidateTransition
// checks against -- it deliberately does not permit skipping the review
// step, re-entering review, or reaching Merge without MergeApproval.
var nextSteps = map[Step][]Step{
	StepNotStarted:    {StepBranch},
	StepBranch:        {StepDraft},
	StepDraft:         {StepReview},
	StepReview:        {StepLocalGates, StepFixBlocker},
	StepFixBlocker:    {StepRecheck},
	StepRecheck:       {StepLocalGates},
	StepLocalGates:    {StepCommit},
	StepCommit:        {StepPush},
	StepPush:          {StepPRCreate},
	StepPRCreate:      {StepCIWatch},
	StepCIWatch:       {StepMergeApproval, StepCIRepair, StepCIWatch},
	StepCIRepair:      {StepLocalGates},
	StepMergeApproval: {StepMerge},
	StepMerge:         {StepPostMerge},
	StepPostMerge:     {StepComplete},
	StepComplete:      {},
}

// ValidateTransition reports whether moving from `from` to `to` is a
// structurally allowed edge in Autopilot's bounded workflow graph.
func ValidateTransition(from, to Step) error {
	allowed, ok := nextSteps[from]
	if !ok {
		return fmt.Errorf("unknown step %q", from)
	}
	for _, s := range allowed {
		if s == to {
			return nil
		}
	}
	return fmt.Errorf("invalid transition: %s -> %s is not permitted", from, to)
}

// StepSequence returns the canonical ordered step list, for display
// purposes (status/dry-run output).
func StepSequence() []Step {
	out := make([]Step, len(stepOrder))
	copy(out, stepOrder)
	return out
}
