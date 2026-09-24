package autopilot

import (
	"context"
	"fmt"
)

// BranchExistsLocally reports whether a local branch ref exists.
func (g *Git) BranchExistsLocally(ctx context.Context, name string) bool {
	_, err := g.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// Reconcile inspects real git/GitHub state and corrects `s` in place where
// reality has diverged from what was last saved -- "resume" must never
// blindly trust a stale state.json. It returns human-readable warnings for
// everything it found or corrected; it only sets Status=Failed for a
// divergence severe enough that continuing automatically would be unsafe
// (e.g. the PR was closed without merging).
func Reconcile(ctx context.Context, git *Git, gh *GitHub, s *State) []string {
	var warnings []string

	if s.Status == StatusCompleted {
		return warnings
	}

	pastNeedingLocalBranch := s.Step == StepPostMerge || s.Step == StepComplete
	if s.Branch != "" && git != nil && !pastNeedingLocalBranch {
		if !git.BranchExistsLocally(ctx, s.Branch) {
			warnings = append(warnings, fmt.Sprintf("local branch %q not found (state.json expected it) -- it may only exist on origin, or may have been deleted; verify before continuing", s.Branch))
		}
	}

	if s.PRNumber != 0 && gh != nil {
		state, err := gh.PRState(ctx, s.PRNumber)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not verify PR #%d's real state via gh: %v -- proceeding on saved state only, with reduced confidence", s.PRNumber, err))
		} else {
			switch state {
			case "MERGED":
				if s.Step != StepPostMerge && s.Step != StepComplete {
					warnings = append(warnings, fmt.Sprintf("PR #%d is already MERGED on GitHub, but state.json still shows step=%s -- advancing to post_merge", s.PRNumber, s.Step))
					s.Step = StepPostMerge
					s.Status = StatusRunning
					s.PausedReason = ""
					s.PausedDetail = ""
				}
			case "CLOSED":
				warnings = append(warnings, fmt.Sprintf("PR #%d was CLOSED without merging -- saved state can no longer be trusted to resume automatically", s.PRNumber))
				s.Fail(fmt.Sprintf("PR #%d closed without merging; requires manual investigation", s.PRNumber))
			case "OPEN":
				// consistent with an in-progress run; nothing to correct.
			default:
				warnings = append(warnings, fmt.Sprintf("PR #%d reported unexpected state %q from gh", s.PRNumber, state))
			}
		}
	}

	return warnings
}
