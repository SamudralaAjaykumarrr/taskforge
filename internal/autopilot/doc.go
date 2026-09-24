// Package autopilot implements TaskForge Autopilot: repository/developer
// tooling that carries a roadmap phase (docs/enterprise-roadmap.md) through
// this project's existing planning -> implementation -> review -> CI ->
// merge workflow, with minimal manual copy/paste.
//
// Autopilot is NOT part of the TaskForge job-processing runtime (cmd/api,
// cmd/worker, internal/store, ...). It is a separate, bounded, resume-safe
// CLI that automates the repetitive mechanics of that workflow (branch
// creation, prompt generation, invoking the "claude" CLI, local quality
// gates, git/gh operations) while pausing at explicit human-approval
// boundaries for every decision this project's own discipline reserves for
// a human: new architectural decisions, destructive migrations,
// security/trust-boundary changes, invariant changes, weakened proof
// obligations, force-push/branch-protection bypass, repeated unexplained CI
// failure, and release-candidate/v1.0.0 approval.
//
// Autopilot never runs as a background daemon. It is a plain CLI: each
// invocation performs a bounded amount of work, persists its state to
// .taskforge-autopilot/state.json, and exits. "taskforge-autopilot resume"
// picks the workflow back up, always re-verifying real git/GitHub state
// before trusting what was last saved.
//
// See docs/autopilot.md for the full design, safety model, and examples.
package autopilot
