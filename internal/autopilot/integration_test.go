package autopilot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegration_GitOpsAgainstRealTemporaryRepo exercises Git against a
// real, disposable git repository (real "git" binary via ExecRunner, no
// FakeRunner) -- the one integration-style test the spec asks for,
// covering branch creation, dirty-tree refusal, commit, and diff --check
// against an actual working tree rather than a scripted double.
func TestIntegration_GitOpsAgainstRealTemporaryRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial commit")

	g := &Git{Runner: ExecRunner{}, Dir: dir}

	branch, err := g.CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("expected main, got %s", branch)
	}

	clean, _, err := g.IsClean(ctx)
	if err != nil || !clean {
		t.Fatalf("expected a clean tree right after commit, got clean=%v err=%v", clean, err)
	}

	if err := g.CreateBranch(ctx, "phase-1-implementation", "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err = g.CurrentBranch(ctx)
	if err != nil || branch != "phase-1-implementation" {
		t.Fatalf("expected to be on the new branch, got %s (err=%v)", branch, err)
	}

	// Dirty the tree, then prove commit works end-to-end via a real
	// message file (never an inline -m string, per docs/autopilot.md "PR
	// Creation"/"Git Safety").
	if err := os.WriteFile(filepath.Join(dir, "new-file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, _, err = g.IsClean(ctx)
	if err != nil || clean {
		t.Fatalf("expected a dirty tree after adding a file, got clean=%v err=%v", clean, err)
	}
	if err := g.CreateBranch(ctx, "phase-2-implementation", "main"); err == nil {
		t.Fatal("expected CreateBranch to refuse to branch from a dirty tree")
	}

	if err := g.AddAll(ctx, "."); err != nil {
		t.Fatalf("AddAll: %v", err)
	}
	// The message file lives outside the repo entirely (a sibling temp
	// dir) -- if it were inside dir, it would itself be an untracked file
	// after commit, defeating the "clean tree after commit" assertion
	// below.
	msgFile := filepath.Join(t.TempDir(), "commit-msg.txt")
	if err := os.WriteFile(msgFile, []byte("feat: implement Phase 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.Commit(ctx, msgFile); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	clean, _, err = g.IsClean(ctx)
	if err != nil || !clean {
		t.Fatalf("expected a clean tree after commit, got clean=%v err=%v", clean, err)
	}

	if res, err := g.DiffCheck(ctx); err != nil {
		t.Fatalf("DiffCheck on a clean, whitespace-clean tree should pass: %v (%q)", err, res.Stdout+res.Stderr)
	}

	sha, err := g.RevParse(ctx, "HEAD")
	if err != nil || sha == "" {
		t.Fatalf("RevParse HEAD: %q, %v", sha, err)
	}
}
