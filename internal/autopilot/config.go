package autopilot

// Config holds Autopilot's tunable bounds and paths. Every bound here exists
// to keep the automation bounded, per docs/autopilot.md "Safety Model" --
// none of these are meant to be relaxed to "unlimited" by a caller.
type Config struct {
	// RepoRoot is the absolute path to the repository's working tree root.
	RepoRoot string

	// StateDir is where state.json and logs/ live, relative to RepoRoot.
	// Always ".taskforge-autopilot" in normal operation; overridable only
	// for tests.
	StateDir string

	// MaxReviewCount bounds independent reviews per run stage. The workflow
	// policy is "exactly one independent review"; this exists so the bound
	// is enforced in code, not only in prose.
	MaxReviewCount int

	// MaxRecheckCount bounds focused blocker re-checks per run stage.
	MaxRecheckCount int

	// MaxCIRepairAttempts bounds evidence-based CI repair iterations before
	// Autopilot pauses and requires human approval (human gate 7).
	MaxCIRepairAttempts int

	// AutoMergeAfterGreen, when false (the default), means Autopilot always
	// pauses for explicit human confirmation immediately before merging,
	// even when every required check is green. It is provided as a config
	// option for a future, more-trusted run; V1 must default it to false
	// and nothing in this package may flip it silently.
	AutoMergeAfterGreen bool

	// CIWatchPollInterval and CIWatchMaxPolls bound one round of "watch
	// required checks" before Autopilot gives up for this invocation and
	// leaves the workflow resumable at the same step. Autopilot is not a
	// daemon: it does not poll indefinitely.
	CIWatchPollIntervalSeconds int
	CIWatchMaxPolls            int

	// Verbose enables printing full subprocess output as it runs, rather
	// than only on failure.
	Verbose bool
}

// DefaultConfig returns Autopilot's V1 defaults. Every bound is deliberately
// conservative; see docs/autopilot.md "Why bounded, not fully autonomous."
func DefaultConfig(repoRoot string) Config {
	return Config{
		RepoRoot:                   repoRoot,
		StateDir:                   ".taskforge-autopilot",
		MaxReviewCount:             1,
		MaxRecheckCount:            1,
		MaxCIRepairAttempts:        3,
		AutoMergeAfterGreen:        false,
		CIWatchPollIntervalSeconds: 20,
		CIWatchMaxPolls:            9,
		Verbose:                    false,
	}
}
