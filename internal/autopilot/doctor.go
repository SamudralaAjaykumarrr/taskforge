package autopilot

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// claudeDoctorTimeout bounds each of the "claude --version"/"claude --help"
// capability probes doctor runs -- long enough for a cold CLI start, short
// enough that a hung/misbehaving binary fails doctor promptly rather than
// hanging it.
const claudeDoctorTimeout = 15 * time.Second

// lookPath is exec.LookPath by default; tests override it (like the
// existing "sleep" var in workflow.go) to deterministically simulate a
// missing binary without depending on the host machine's real PATH.
var lookPath = exec.LookPath

// CheckSeverity classifies a doctor finding.
type CheckSeverity string

const (
	SeverityPass CheckSeverity = "PASS"
	SeverityWarn CheckSeverity = "WARN"
	SeverityFail CheckSeverity = "FAIL"
)

// DoctorCheck is one finding.
type DoctorCheck struct {
	Name     string
	Severity CheckSeverity
	Detail   string
}

// RunDoctor performs every check the spec's DOCTOR section requires,
// mutating nothing: it never writes a file, never creates a branch, never
// invokes claude. Read-only git/gh calls (rev-parse, remote get-url, repo
// view, auth status) are permitted -- "do not mutate anything" governs
// repository/GitHub state, not local inspection.
func RunDoctor(ctx context.Context, cfg Config, git *Git, gh *GitHub, claude *Claude) []DoctorCheck {
	var checks []DoctorCheck

	add := func(name string, sev CheckSeverity, detail string) {
		checks = append(checks, DoctorCheck{Name: name, Severity: sev, Detail: detail})
	}

	if path, err := lookPath("git"); err == nil {
		add("git available", SeverityPass, path)
	} else {
		add("git available", SeverityFail, "git not found on PATH")
	}

	ghPath, ghErr := lookPath("gh")
	if ghErr == nil {
		add("gh available", SeverityPass, ghPath)
	} else {
		add("gh available", SeverityFail, "gh not found on PATH")
	}

	// "claude available" only proves the binary resolves; it does NOT prove
	// it supports the non-interactive invocation contract Invoke (in
	// claudeops.go) actually depends on. That is what "claude CLI
	// capability" verifies below, and a capability gap is a hard FAIL, not
	// a WARN -- an installed-but-incompatible CLI would otherwise make
	// every subsequent draft/review/fix invocation fail mid-run instead of
	// being caught up front.
	claudePath, claudeLookErr := lookPath("claude")
	if claudeLookErr == nil {
		add("claude available", SeverityPass, claudePath)
	} else {
		add("claude available", SeverityFail, "claude not found on PATH")
	}

	switch {
	case claudeLookErr != nil:
		add("claude CLI capability", SeverityFail, "skipped: claude not found on PATH")
	default:
		if claude == nil {
			claude = &Claude{Runner: ExecRunner{}, Dir: cfg.RepoRoot}
		}
		check := claude.CheckCapabilities(ctx, claudeDoctorTimeout)
		switch {
		case check.Err != nil:
			add("claude CLI capability", SeverityFail, fmt.Sprintf("path=%s: %v", claudePath, check.Err))
		case len(check.MissingFlags) > 0:
			add("claude CLI capability", SeverityFail, fmt.Sprintf("%s at %s does not support required flag(s): %s", orUnknown(check.Version), claudePath, strings.Join(check.MissingFlags, ", ")))
		default:
			add("claude CLI capability", SeverityPass, fmt.Sprintf("%s (%s) supports %s", orUnknown(check.Version), claudePath, strings.Join(requiredClaudeFlags, ", ")))
		}
	}

	if cfg.RepoRoot == "" {
		add("repository root found", SeverityFail, "could not resolve repository root")
	} else {
		add("repository root found", SeverityPass, cfg.RepoRoot)
	}

	if git != nil {
		if git.OriginRemoteExists(ctx) {
			add("origin remote exists", SeverityPass, "")
		} else {
			add("origin remote exists", SeverityFail, "no 'origin' remote configured")
		}

		if branch, err := git.DefaultBranch(ctx); err == nil {
			add("main resolvable", SeverityPass, branch)
		} else {
			add("main resolvable", SeverityWarn, fmt.Sprintf("could not resolve default branch: %v", err))
		}

		if clean, dirty, err := git.IsClean(ctx); err == nil {
			if clean {
				add("working tree status", SeverityPass, "clean")
			} else {
				add("working tree status", SeverityWarn, "dirty:\n"+dirty)
			}
		} else {
			add("working tree status", SeverityWarn, fmt.Sprintf("could not determine: %v", err))
		}
	}

	if ghErr == nil && gh != nil {
		if ok, detail, _ := gh.AuthStatus(ctx); ok {
			add("gh authenticated", SeverityPass, "")
		} else {
			add("gh authenticated", SeverityWarn, strings.TrimSpace(detail))
		}
		if owner, name, err := gh.RepoView(ctx); err == nil {
			add("GitHub repository resolvable", SeverityPass, owner+"/"+name)
		} else {
			add("GitHub repository resolvable", SeverityWarn, err.Error())
		}
	} else {
		add("gh authenticated", SeverityWarn, "skipped: gh not available")
		add("GitHub repository resolvable", SeverityWarn, "skipped: gh not available")
	}

	names, err := WorkflowFileNames(cfg.RepoRoot)
	if err != nil || len(names) == 0 {
		add("required workflow files discoverable", SeverityFail, "no .github/workflows/*.yml files found")
	} else {
		var found []string
		for _, n := range names {
			if display, err := WorkflowDisplayName(cfg.RepoRoot, n); err == nil {
				found = append(found, fmt.Sprintf("%s (%s)", n, display))
			} else {
				found = append(found, n)
			}
		}
		add("required workflow files discoverable", SeverityPass, strings.Join(found, ", "))
	}

	docs := AuthoritativeDocs(cfg.RepoRoot)
	roadmapFound := false
	for _, d := range docs {
		if d == "docs/enterprise-roadmap.md" {
			roadmapFound = true
		}
	}
	if roadmapFound {
		add("authoritative roadmap exists", SeverityPass, "docs/enterprise-roadmap.md")
	} else {
		add("authoritative roadmap exists", SeverityFail, "docs/enterprise-roadmap.md not found")
	}

	return checks
}

func orUnknown(s string) string {
	if s == "" {
		return "(version unknown)"
	}
	return s
}

// DoctorFailed reports whether any check is a hard FAIL.
func DoctorFailed(checks []DoctorCheck) bool {
	for _, c := range checks {
		if c.Severity == SeverityFail {
			return true
		}
	}
	return false
}
