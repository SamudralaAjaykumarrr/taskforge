package autopilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateSchemaVersion is bumped whenever State's on-disk shape changes
// incompatibly. Resume/status must refuse (not guess at) a state file from
// a newer, unknown schema version.
const StateSchemaVersion = 1

// State is Autopilot's complete, persisted workflow state. Every field
// listed in the V1 spec's "STATE / RESUMABILITY" section has a home here.
type State struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         int    `json:"phase"`
	Stage         Stage  `json:"stage"`
	Step          Step   `json:"step"`
	Status        Status `json:"status"`

	Branch               string `json:"branch"`
	BaseBranch           string `json:"base_branch"`
	PlanPath             string `json:"plan_path,omitempty"`
	ImplementationBranch string `json:"implementation_branch,omitempty"`
	CommitSHA            string `json:"commit_sha,omitempty"`
	PRNumber             int    `json:"pr_number,omitempty"`

	CIRunIDs []string `json:"ci_run_ids,omitempty"`

	// MergeCommitSHA is the real, gh-resolved merge commit for PRNumber,
	// set once PRNumber is actually merged -- the durable anchor
	// post-merge verification checks against. It is never the PR's own
	// pre-merge head SHA.
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	// PostMergeRuns records, per required post-merge workflow, the most
	// recently observed run against MergeCommitSHA -- persisted so a crash
	// mid-watch, or a resume, never has to re-derive (or falsely assume)
	// what was already observed.
	PostMergeRuns []PostMergeRun `json:"post_merge_runs,omitempty"`

	ReviewCount   int `json:"review_count"`
	RecheckCount  int `json:"recheck_count"`
	CIRepairCount int `json:"ci_repair_count"`

	LastSuccessfulGate string `json:"last_successful_gate,omitempty"`

	PausedReason PauseReason `json:"paused_reason,omitempty"`
	PausedDetail string      `json:"paused_detail,omitempty"`

	PendingBlockerType BlockerType `json:"pending_blocker_type,omitempty"`
	PendingBlockerText string      `json:"pending_blocker_text,omitempty"`

	// PendingHumanProof names the phase-manifest human proof obligation
	// (see manifest.go) currently blocking local_gates, if any.
	PendingHumanProof string `json:"pending_human_proof,omitempty"`
	// ApprovedHumanProofs lists the names of human proof obligations an
	// operator has explicitly approved for this run, via "approve" while
	// paused on PendingHumanProof. A proof's name appearing here is the
	// only thing that satisfies it -- Autopilot never infers approval from
	// anything else.
	ApprovedHumanProofs []string `json:"approved_human_proofs,omitempty"`

	LogPaths []string `json:"log_paths,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PostMergeRun records one required post-merge workflow's observed status
// for the actual merged commit on main -- never the pre-merge PR checks.
type PostMergeRun struct {
	Workflow   string `json:"workflow"`
	RunID      string `json:"run_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

// StatePath returns the path state.json is read from / written to.
func StatePath(cfg Config) string {
	return filepath.Join(cfg.RepoRoot, cfg.StateDir, "state.json")
}

// LoadState reads and validates the persisted state. A missing file returns
// (nil, os.ErrNotExist) via errors.Is, distinguishing "never started" from
// a real read/parse error.
func LoadState(cfg Config) (*State, error) {
	path := StatePath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state file %s is not valid JSON (refusing to trust it): %w", path, err)
	}
	if s.SchemaVersion > StateSchemaVersion {
		return nil, fmt.Errorf("state file %s has schema_version %d, newer than this binary understands (%d) -- refusing to guess at its meaning", path, s.SchemaVersion, StateSchemaVersion)
	}
	return &s, nil
}

// Save persists state atomically (write to a temp file, then rename) so a
// crash mid-write never leaves a truncated/corrupt state.json behind.
func (s *State) Save(cfg Config) error {
	s.SchemaVersion = StateSchemaVersion
	s.UpdatedAt = time.Now().UTC()
	dir := filepath.Join(cfg.RepoRoot, cfg.StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	path := StatePath(cfg)
	tmp, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file into place: %w", err)
	}
	return nil
}

// NewState constructs the initial state for a fresh "start".
func NewState(phase int, stage Stage, branch, baseBranch string) *State {
	now := time.Now().UTC()
	return &State{
		SchemaVersion: StateSchemaVersion,
		Phase:         phase,
		Stage:         stage,
		Step:          StepNotStarted,
		Status:        StatusRunning,
		Branch:        branch,
		BaseBranch:    baseBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// Advance validates and applies a step transition in place. It never
// bypasses ValidateTransition -- this is the single choke point every
// caller (workflow.go) must use to move the state machine forward.
func (s *State) Advance(to Step) error {
	if err := ValidateTransition(s.Step, to); err != nil {
		return err
	}
	s.Step = to
	return nil
}

// IncrementReview enforces the "exactly one independent review" bound.
func (s *State) IncrementReview(max int) error {
	if s.ReviewCount >= max {
		return fmt.Errorf("review bound exceeded: %d review(s) already performed (max %d) -- this is a workflow-policy violation, not a retryable condition", s.ReviewCount, max)
	}
	s.ReviewCount++
	return nil
}

// IncrementRecheck enforces the "one focused re-check" bound.
func (s *State) IncrementRecheck(max int) error {
	if s.RecheckCount >= max {
		return fmt.Errorf("recheck bound exceeded: %d recheck(s) already performed (max %d) -- this is a workflow-policy violation, not a retryable condition", s.RecheckCount, max)
	}
	s.RecheckCount++
	return nil
}

// IncrementCIRepair enforces the bounded-CI-repair-attempts human gate. When
// the bound would be exceeded, the caller must pause (PauseCIRepairExhausted)
// rather than call this again.
func (s *State) IncrementCIRepair(max int) error {
	if s.CIRepairCount >= max {
		return fmt.Errorf("CI repair bound exceeded: %d attempt(s) already made (max %d)", s.CIRepairCount, max)
	}
	s.CIRepairCount++
	return nil
}

// Pause marks the state as stopped at a human gate, preserving everything
// needed to resume later.
func (s *State) Pause(reason PauseReason, detail string) {
	s.Status = StatusAwaitingApproval
	s.PausedReason = reason
	s.PausedDetail = detail
}

// Fail marks the state as stopped due to an unrecoverable/unexplained
// error -- distinct from a human gate: this means Autopilot itself hit a
// condition its own policy says it must not paper over.
func (s *State) Fail(detail string) {
	s.Status = StatusFailed
	s.PausedDetail = detail
}
