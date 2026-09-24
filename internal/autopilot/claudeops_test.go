package autopilot

import (
	"context"
	"strings"
	"testing"
)

func TestClaude_Invoke_NeverPassesResumeOrContinue(t *testing.T) {
	fr := NewFakeRunner()
	var seenArgs []string
	fr.On("claude", nil, func(c FakeCall) (Result, error) {
		seenArgs = c.Args
		return resultOK("AUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=0\nAUTOPILOT_RESULT_END\n"), nil
	})
	c := &Claude{Runner: fr, Dir: "/repo"}
	out, err := c.Invoke(context.Background(), RoleReviewer, "review this", "acceptEdits")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "AUTOPILOT_RESULT_BEGIN") {
		t.Fatal("expected raw output to be returned unmodified")
	}
	for _, a := range seenArgs {
		if a == "--resume" || a == "-r" || a == "--continue" || a == "-c" {
			t.Fatalf("reviewer invocation must never resume/continue a prior session, found arg %q in %v", a, seenArgs)
		}
	}
	if !containsFlag(seenArgs, "-p") && !containsFlag(seenArgs, "--print") {
		t.Fatalf("expected non-interactive -p/--print invocation, got %v", seenArgs)
	}
}

func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestClaude_Invoke_PropagatesFailureWhenNoOutput(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("claude", nil, func(c FakeCall) (Result, error) {
		return Result{}, &exitErrStub{}
	})
	c := &Claude{Runner: fr, Dir: "/repo"}
	if _, err := c.Invoke(context.Background(), RoleImplementer, "do work", "acceptEdits"); err == nil {
		t.Fatal("expected error when claude fails with no output")
	}
}
