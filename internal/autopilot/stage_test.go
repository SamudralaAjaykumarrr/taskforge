package autopilot

import "testing"

func TestValidateTransition_ValidEdges(t *testing.T) {
	cases := []struct{ from, to Step }{
		{StepNotStarted, StepBranch},
		{StepBranch, StepDraft},
		{StepDraft, StepReview},
		{StepReview, StepLocalGates},
		{StepReview, StepFixBlocker},
		{StepFixBlocker, StepRecheck},
		{StepRecheck, StepLocalGates},
		{StepLocalGates, StepCommit},
		{StepCommit, StepPush},
		{StepPush, StepPRCreate},
		{StepPRCreate, StepCIWatch},
		{StepCIWatch, StepMergeApproval},
		{StepCIWatch, StepCIRepair},
		{StepCIRepair, StepLocalGates},
		{StepMergeApproval, StepMerge},
		{StepMerge, StepPostMerge},
		{StepPostMerge, StepComplete},
	}
	for _, c := range cases {
		if err := ValidateTransition(c.from, c.to); err != nil {
			t.Errorf("expected %s -> %s to be valid, got error: %v", c.from, c.to, err)
		}
	}
}

func TestValidateTransition_InvalidEdges(t *testing.T) {
	cases := []struct{ from, to Step }{
		{StepBranch, StepMerge},
		{StepDraft, StepCommit},
		{StepReview, StepReview},   // no self-loop review -- never repeat a full review
		{StepReview, StepMerge},    // cannot skip local gates / CI / PR
		{StepCommit, StepPRCreate}, // must push first
		{StepComplete, StepBranch}, // terminal
		{StepMergeApproval, StepPostMerge},
		{StepFixBlocker, StepLocalGates}, // must recheck first
	}
	for _, c := range cases {
		if err := ValidateTransition(c.from, c.to); err == nil {
			t.Errorf("expected %s -> %s to be invalid, got no error", c.from, c.to)
		}
	}
}

func TestValidateTransition_UnknownFromStep(t *testing.T) {
	if err := ValidateTransition(Step("bogus"), StepBranch); err == nil {
		t.Fatal("expected error for unknown from-step")
	}
}

func TestBlockerType_PauseReasonMapping(t *testing.T) {
	humanGated := []BlockerType{BlockerArchitecture, BlockerMigration, BlockerSecurity, BlockerInvariant, BlockerProofWeaken}
	for _, bt := range humanGated {
		if _, ok := bt.PauseReason(); !ok {
			t.Errorf("expected BlockerType %q to map to a human pause reason", bt)
		}
	}
	if _, ok := BlockerOrdinary.PauseReason(); ok {
		t.Error("BlockerOrdinary must not map to a human pause reason -- it is the one category Autopilot may attempt a bounded fix for")
	}
}

func TestStepSequence_MatchesGraphOrder(t *testing.T) {
	seq := StepSequence()
	if len(seq) == 0 {
		t.Fatal("StepSequence returned nothing")
	}
	if seq[0] != StepBranch || seq[len(seq)-1] != StepComplete {
		t.Fatalf("unexpected sequence bounds: first=%s last=%s", seq[0], seq[len(seq)-1])
	}
}
