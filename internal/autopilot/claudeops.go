package autopilot

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Claude wraps a Runner with the two sanctioned ways Autopilot invokes the
// "claude" CLI: as an implementation/planning agent, and as an independent
// reviewer. Both are non-interactive ("-p"/--print) and both write full
// output to LogsDir for later inspection.
//
// The reviewer role deliberately never passes --resume/--continue, so every
// review call starts a genuinely fresh session with no access to the
// implementation conversation's context -- this is what "independent"
// means here, not merely a different prompt in the same session.
type Claude struct {
	Runner Runner
	Dir    string
}

// Role selects which system framing/permission posture a Claude invocation
// runs under.
type Role string

const (
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
	RoleFixer       Role = "fixer"
)

// Invoke runs claude non-interactively with the given prompt text and
// returns its raw stdout. permissionMode governs how much it may do without
// prompting a human (the orchestrator, not Claude, still controls
// stage/commit/push/PR/merge -- prompts.go instructs Claude never to run
// those itself).
func (c *Claude) Invoke(ctx context.Context, role Role, prompt string, permissionMode string) (string, error) {
	args := []string{
		"-p", prompt,
		"--output-format", "text",
		"--permission-mode", permissionMode,
		"--no-session-persistence",
	}
	res, err := c.Runner.Run(ctx, c.Dir, "claude", args...)
	if err != nil && res.Stdout == "" {
		return "", fmt.Errorf("invoking claude (%s role): %w", role, err)
	}
	return res.Stdout, nil
}

// requiredClaudeFlags lists exactly the CLI flags Invoke (above) depends
// on. Doctor's capability check derives its verification from this same
// list -- kept beside Invoke's own argument-building code so the two can
// never silently drift apart. "--print" is checked rather than the short
// "-p" alias Invoke actually passes, because --help documents the long
// form even where a short alias also exists.
var requiredClaudeFlags = []string{"--print", "--output-format", "--permission-mode", "--no-session-persistence"}

// ClaudeCapabilityCheck is the result of probing an installed "claude"
// binary for the non-interactive invocation contract Invoke requires,
// without ever sending it a real model prompt.
type ClaudeCapabilityCheck struct {
	Version      string
	MissingFlags []string
	Err          error // a hard failure running --version/--help itself
}

// OK reports whether the installed CLI satisfies every capability Invoke
// depends on.
func (c ClaudeCapabilityCheck) OK() bool {
	return c.Err == nil && len(c.MissingFlags) == 0
}

// CheckCapabilities runs a bounded "claude --version" followed by a
// bounded "claude --help" (never a real prompt -- both are non-mutating,
// no-API-call capability queries) and verifies every flag Invoke depends
// on is documented as supported.
func (c *Claude) CheckCapabilities(ctx context.Context, timeout time.Duration) ClaudeCapabilityCheck {
	verCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	verRes, err := c.Runner.Run(verCtx, c.Dir, "claude", "--version")
	if err != nil {
		return ClaudeCapabilityCheck{Err: fmt.Errorf("claude --version failed: %w", err)}
	}
	version := strings.TrimSpace(verRes.Stdout)
	if version == "" {
		version = strings.TrimSpace(verRes.Stderr)
	}

	helpCtx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	helpRes, err := c.Runner.Run(helpCtx, c.Dir, "claude", "--help")
	if err != nil {
		return ClaudeCapabilityCheck{Version: version, Err: fmt.Errorf("claude --help failed: %w", err)}
	}
	helpText := helpRes.Stdout + helpRes.Stderr

	var missing []string
	for _, flag := range requiredClaudeFlags {
		if !strings.Contains(helpText, flag) {
			missing = append(missing, flag)
		}
	}
	return ClaudeCapabilityCheck{Version: version, MissingFlags: missing}
}
