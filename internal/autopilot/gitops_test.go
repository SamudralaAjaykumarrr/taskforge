package autopilot

import (
	"context"
	"testing"
)

func TestValidateBranchName(t *testing.T) {
	valid := []string{"phase-16-implementation", "phase-16-planning", "fix-sf053-slot-deadlock"}
	for _, v := range valid {
		if err := ValidateBranchName(v); err != nil {
			t.Errorf("expected %q to be valid, got %v", v, err)
		}
	}
	invalid := []string{"", "main", "master", "-leading-dash", "has space", "has..dotdot", "trailing/", "bad.lock"}
	for _, v := range invalid {
		if err := ValidateBranchName(v); err == nil {
			t.Errorf("expected %q to be invalid, got no error", v)
		}
	}
}

func TestGit_Push_RefusesForceFlags(t *testing.T) {
	fr := NewFakeRunner()
	g := &Git{Runner: fr, Dir: "/repo"}
	for _, forceArgs := range [][]string{
		{"push", "--set-upstream", "origin", "b", "--force"},
		{"push", "origin", "b", "-f"},
		{"push", "origin", "b", "--force-with-lease"},
	} {
		if _, err := g.run(context.Background(), forceArgs...); err == nil {
			t.Errorf("expected git %v to be refused", forceArgs)
		}
	}
	if fr.CallCount() != 0 {
		t.Fatalf("no force-push variant should have reached the runner, got %d calls", fr.CallCount())
	}
}

func TestGit_Run_RefusesResetHardAndClean(t *testing.T) {
	fr := NewFakeRunner()
	g := &Git{Runner: fr, Dir: "/repo"}
	if _, err := g.run(context.Background(), "reset", "--hard", "HEAD~1"); err == nil {
		t.Error("expected git reset --hard to be refused")
	}
	if _, err := g.run(context.Background(), "clean", "-fd"); err == nil {
		t.Error("expected git clean to be refused")
	}
	if fr.CallCount() != 0 {
		t.Fatalf("destructive git operations must never reach the runner, got %d calls", fr.CallCount())
	}
}

func TestGit_Push_ValidatesBranchName(t *testing.T) {
	fr := NewFakeRunner()
	g := &Git{Runner: fr, Dir: "/repo"}
	if err := g.Push(context.Background(), "main"); err == nil {
		t.Fatal("expected Push to main to be refused")
	}
	if fr.CallCount() != 0 {
		t.Fatalf("expected zero calls, got %d", fr.CallCount())
	}
}

func TestGit_CreateBranch_RefusesWhenDirty(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"status", "--porcelain"}, func(c FakeCall) (Result, error) {
		return resultOK(" M dirty-file.go\n"), nil
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	if err := g.CreateBranch(context.Background(), "phase-16-implementation", "main"); err == nil {
		t.Fatal("expected CreateBranch to refuse a dirty tree")
	}
	if len(fr.CallsMatching("git")) != 1 {
		t.Fatalf("expected only the status check to run, got %v", fr.Calls)
	}
}

func TestGit_CreateBranch_RefusesProtectedName(t *testing.T) {
	fr := NewFakeRunner()
	g := &Git{Runner: fr, Dir: "/repo"}
	if err := g.CreateBranch(context.Background(), "main", "main"); err == nil {
		t.Fatal("expected CreateBranch(\"main\", ...) to be refused")
	}
	if fr.CallCount() != 0 {
		t.Fatalf("expected zero calls for a protected branch name, got %d", fr.CallCount())
	}
}

func TestGit_CreateBranch_HappyPath(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"status", "--porcelain"}, func(c FakeCall) (Result, error) {
		return resultOK(""), nil
	})
	fr.On("git", []string{"checkout", "-b", "phase-16-implementation", "main"}, func(c FakeCall) (Result, error) {
		return resultOK(""), nil
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	if err := g.CreateBranch(context.Background(), "phase-16-implementation", "main"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGit_IsAncestor_True(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 0}, nil
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	ok, err := g.IsAncestor(context.Background(), "abc123", "main")
	if err != nil || !ok {
		t.Fatalf("expected true, nil; got %v, %v", ok, err)
	}
}

func TestGit_IsAncestor_False(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 1}, &exitErrStub{}
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	ok, err := g.IsAncestor(context.Background(), "abc123", "main")
	if err != nil {
		t.Fatalf("exit code 1 (\"not an ancestor\") must not be reported as a Go error: %v", err)
	}
	if ok {
		t.Fatal("expected false")
	}
}

func TestGit_IsAncestor_RealError(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 128}, &exitErrStub{}
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	if _, err := g.IsAncestor(context.Background(), "not-a-real-commit", "main"); err == nil {
		t.Fatal("expected an error for a genuine failure (e.g. unknown revision), distinct from a plain \"not an ancestor\"")
	}
}

func TestGit_Checkout_RefusesWhenDirty(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("git", []string{"status", "--porcelain"}, func(c FakeCall) (Result, error) {
		return resultOK(" M dirty.go\n"), nil
	})
	g := &Git{Runner: fr, Dir: "/repo"}
	if err := g.Checkout(context.Background(), "main"); err == nil {
		t.Fatal("expected Checkout to refuse a dirty tree")
	}
}
