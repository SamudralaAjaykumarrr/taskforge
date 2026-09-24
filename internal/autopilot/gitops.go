package autopilot

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Git wraps a Runner with the specific, safety-checked git operations
// Autopilot is allowed to perform. There is no method on this type for any
// of the operations GIT SAFETY forbids (git reset --hard, git clean -fd,
// force-push, direct writes to main) -- they simply do not exist here, and
// Run rejects any argument list that looks like one, as defense in depth
// against a future caller trying to construct one from a variable.
type Git struct {
	Runner Runner
	Dir    string
}

// bannedArgPatterns are argument shapes Git.run refuses to execute, no
// matter which method assembled them. This is a second, mechanical layer
// behind "no method exists for this" -- see docs/autopilot.md "Git Safety."
var bannedArgPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^--force$`),
	regexp.MustCompile(`^-f$`),
	regexp.MustCompile(`^--force-with-lease.*$`),
	regexp.MustCompile(`^--hard$`),
	regexp.MustCompile(`^-fd$`),
	regexp.MustCompile(`^-df$`),
}

func (g *Git) run(ctx context.Context, args ...string) (Result, error) {
	if len(args) >= 1 && args[0] == "push" {
		for _, a := range args[1:] {
			for _, pat := range bannedArgPatterns {
				if pat.MatchString(a) {
					return Result{}, fmt.Errorf("refusing git push with banned flag %q (force-push is never automated; see docs/autopilot.md Git Safety)", a)
				}
			}
		}
	}
	if len(args) >= 1 && args[0] == "reset" {
		for _, a := range args[1:] {
			if a == "--hard" {
				return Result{}, fmt.Errorf("refusing git reset --hard (destructive git cleanup is never automated)")
			}
		}
	}
	if len(args) >= 1 && args[0] == "clean" {
		return Result{}, fmt.Errorf("refusing git clean (destructive git cleanup is never automated)")
	}
	return g.Runner.Run(ctx, g.Dir, "git", args...)
}

// ProtectedBranches lists branch names Autopilot must never commit to or
// push directly.
var ProtectedBranches = map[string]bool{"main": true, "master": true}

// ValidateBranchName rejects empty names, protected-branch names, and
// anything that isn't a plausible git ref (no whitespace, no leading dash,
// no "..", matching git's own ref-naming discipline closely enough to catch
// obvious mistakes before they reach a git subprocess).
func ValidateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("branch name must not be empty")
	}
	if ProtectedBranches[name] {
		return fmt.Errorf("refusing to use protected branch %q as a working branch", name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("branch name %q must not start with '-'", name)
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, " \t\n~^:?*[\\") {
		return fmt.Errorf("branch name %q is not a valid git ref", name)
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("branch name %q has an invalid suffix", name)
	}
	return nil
}

func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	res, err := g.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// IsClean reports whether the working tree has no staged, unstaged, or
// untracked changes.
func (g *Git) IsClean(ctx context.Context) (bool, string, error) {
	res, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, "", err
	}
	out := strings.TrimSpace(res.Stdout)
	return out == "", out, nil
}

func (g *Git) FetchOrigin(ctx context.Context) error {
	_, err := g.run(ctx, "fetch", "origin", "--prune")
	return err
}

// DefaultBranch resolves origin's HEAD (falling back to "main" if the
// symbolic ref isn't set locally, matching this repository's actual default
// branch).
func (g *Git) DefaultBranch(ctx context.Context) (string, error) {
	res, err := g.run(ctx, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		ref := strings.TrimSpace(res.Stdout)
		if idx := strings.LastIndex(ref, "/"); idx >= 0 {
			return ref[idx+1:], nil
		}
	}
	if _, err := g.run(ctx, "rev-parse", "--verify", "origin/main"); err == nil {
		return "main", nil
	}
	return "", fmt.Errorf("could not resolve origin's default branch")
}

func (g *Git) RevParse(ctx context.Context, ref string) (string, error) {
	res, err := g.run(ctx, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (g *Git) OriginRemoteExists(ctx context.Context) bool {
	_, err := g.run(ctx, "remote", "get-url", "origin")
	return err == nil
}

// CreateBranch creates and checks out a new branch from base. It refuses to
// create a protected-branch name or to branch while the tree is dirty.
func (g *Git) CreateBranch(ctx context.Context, name, base string) error {
	if err := ValidateBranchName(name); err != nil {
		return err
	}
	clean, dirty, err := g.IsClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("refusing to create branch %q: working tree is not clean:\n%s", name, dirty)
	}
	if _, err := g.run(ctx, "checkout", "-b", name, base); err != nil {
		return fmt.Errorf("creating branch %s from %s: %w", name, base, err)
	}
	return nil
}

// Checkout switches to an existing branch, refusing to do so with a dirty
// tree (never discards uncommitted work silently).
func (g *Git) Checkout(ctx context.Context, name string) error {
	clean, dirty, err := g.IsClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("refusing to checkout %q: working tree is not clean:\n%s", name, dirty)
	}
	_, err = g.run(ctx, "checkout", name)
	return err
}

// FastForwardPull fast-forwards the current branch from origin, failing
// (not falling back to a merge) if a fast-forward isn't possible.
func (g *Git) FastForwardPull(ctx context.Context) error {
	_, err := g.run(ctx, "pull", "--ff-only", "origin")
	return err
}

func (g *Git) AddAll(ctx context.Context, paths ...string) error {
	args := append([]string{"add"}, paths...)
	_, err := g.run(ctx, args...)
	return err
}

// Commit commits currently-staged changes with the given message, read from
// a message file rather than an inline -m argument, avoiding the same class
// of shell/quoting corruption this project has hit with PR bodies (see
// docs/autopilot.md "PR Creation").
func (g *Git) Commit(ctx context.Context, messageFilePath string) error {
	_, err := g.run(ctx, "commit", "--file", messageFilePath)
	return err
}

// Push pushes the named branch to origin, setting upstream on first push.
// It never accepts a force flag -- see the banned-pattern guard in run().
func (g *Git) Push(ctx context.Context, branch string) error {
	if err := ValidateBranchName(branch); err != nil {
		return err
	}
	_, err := g.run(ctx, "push", "--set-upstream", "origin", branch)
	return err
}

// DiffCheck runs "git diff --check" (whitespace-error gate).
func (g *Git) DiffCheck(ctx context.Context) (Result, error) {
	return g.run(ctx, "diff", "--check")
}

// IsAncestor reports whether ancestor is an ancestor of (or identical to)
// descendant. Used to verify that main, after a fast-forward pull, actually
// contains a specific merge commit -- never assumed from HEAD alone, since
// main may have advanced further via other, later merges.
func (g *Git) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	res, err := g.run(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if res.ExitCode == 1 {
		// git merge-base --is-ancestor's documented convention: exit 1
		// means "no", not a tool failure.
		return false, nil
	}
	return false, fmt.Errorf("checking whether %s is an ancestor of %s: %w", ancestor, descendant, err)
}

// MainAdvancedBeyond reports whether origin/<base> has moved past the
// commit this run last observed, so a push/merge step can detect that main
// unexpectedly advanced underneath it.
func (g *Git) MainAdvancedBeyond(ctx context.Context, base, knownSHA string) (bool, error) {
	if knownSHA == "" {
		return false, nil
	}
	current, err := g.RevParse(ctx, "origin/"+base)
	if err != nil {
		return false, err
	}
	return current != knownSHA, nil
}
