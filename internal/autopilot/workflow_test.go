package autopilot

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

const approveResult = "AUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=0\nAUTOPILOT_RESULT_END\n"
const passResult = "AUTOPILOT_RESULT_BEGIN\nVALIDATION=PASS\nREADY_FOR_REVIEW=true\nBLOCKER_COUNT=0\nAUTOPILOT_RESULT_END\n"

func ordinaryBlockedResult(text string) string {
	return "AUTOPILOT_RESULT_BEGIN\nVERDICT=BLOCKED\nREADY=false\nBLOCKER_COUNT=1\nBLOCKER_TYPE=ordinary\nBLOCKERS=" + text + "\nAUTOPILOT_RESULT_END\n"
}

func architectureBlockedResult(text string) string {
	return "AUTOPILOT_RESULT_BEGIN\nVERDICT=BLOCKED\nREADY=false\nBLOCKER_COUNT=1\nBLOCKER_TYPE=architecture\nBLOCKERS=" + text + "\nAUTOPILOT_RESULT_END\n"
}

// approveThenPass is the common script: review-shaped prompts approve,
// everything else (draft/fix) passes.
func approveThenPass(prompt string) (string, error) {
	if strings.Contains(prompt, "independently reviewing") {
		return approveResult, nil
	}
	return passResult, nil
}

// wireClaude is assigned per-test by newTestWorkflow; declared here so
// every test in this file can register its implementer/reviewer script
// without threading a dirty-tracking flag through every call site (see
// newTestWorkflow's doc comment).
var wireClaude func(fn func(prompt string) (string, error))

// newTestWorkflow wires a Workflow against a FakeRunner with the standard
// git/gh scaffolding a run needs (branch creation, clean-tree checks,
// commit, push) pre-wired to succeed, and no sleeping between CI polls. It
// also tracks a simple "is the working tree dirty" model matching what
// actually happens across a real run: clean at branch creation, dirtied by
// any file-editing Claude call (draft/fix/ci-repair -- never review, which
// makes no edits), and cleaned again by commit. Tests register their
// implementer/reviewer responses via wireClaude, which keeps this model in
// sync automatically.
func newTestWorkflow(t *testing.T) (*Workflow, *FakeRunner, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig(t)
	cfg.CIWatchPollIntervalSeconds = 0
	cfg.CIWatchMaxPolls = 2
	fr := NewFakeRunner()
	dirty := false

	fr.On("git", []string{"symbolic-ref", "refs/remotes/origin/HEAD"}, func(c FakeCall) (Result, error) {
		return resultOK("refs/remotes/origin/main\n"), nil
	})
	fr.On("git", []string{"status", "--porcelain"}, func(c FakeCall) (Result, error) {
		if dirty {
			return resultOK(" M some-file.go\n"), nil
		}
		return resultOK(""), nil
	})
	fr.On("git", []string{"checkout", "-b"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"add"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"commit", "--file"}, func(c FakeCall) (Result, error) {
		dirty = false
		return resultOK(""), nil
	})
	fr.On("git", []string{"rev-parse", "HEAD"}, func(c FakeCall) (Result, error) { return resultOK("deadbeef\n"), nil })
	fr.On("git", []string{"push", "--set-upstream"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"diff", "--check"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })

	fr.On("gh", []string{"pr", "create"}, func(c FakeCall) (Result, error) {
		return resultOK("https://github.com/o/r/pull/1\n"), nil
	})

	// doLocalGates requires a valid phase proof manifest for every
	// implementation-stage run (manifest.go); write a minimal, explicitly-
	// reviewed-empty one for phase 16 (the phase every test in this file
	// uses) so ordinary workflow tests aren't about manifest content.
	// Tests that ARE about manifest behavior overwrite this file with
	// their own content before calling Start/Resume.
	writeFile(t, cfg.RepoRoot+"/docs/phase-16-plan.md", "test fixture plan content\n")
	writeEmptyReviewedManifest(t, cfg.RepoRoot, 16, "docs/phase-16-plan.md")

	out := &bytes.Buffer{}
	w := &Workflow{
		Cfg:    cfg,
		Git:    &Git{Runner: fr, Dir: cfg.RepoRoot},
		GH:     &GitHub{Runner: fr, Dir: cfg.RepoRoot},
		Claude: &Claude{Runner: fr, Dir: cfg.RepoRoot},
		Out:    out,
	}
	wireClaude = func(fn func(prompt string) (string, error)) {
		fr.On("claude", nil, func(c FakeCall) (Result, error) {
			prompt := c.Args[1]
			response, err := fn(prompt)
			if err == nil && !strings.Contains(prompt, "independently reviewing") {
				dirty = true
			}
			return resultOK(response), err
		})
	}
	return w, fr, out
}

func wireLocalGatesToPass(fr *FakeRunner) {
	fr.On("gofmt", nil, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("go", []string{"vet"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("go", []string{"build"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("go", []string{"test", "-p", "1"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("go", []string{"test", "-race", "-p", "1"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("go", []string{"mod", "verify"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
}

func wireAllChecksGreen(fr *FakeRunner, prNum string) {
	fr.On("gh", []string{"pr", "checks", prNum}, func(c FakeCall) (Result, error) {
		return resultOK(`[{"name":"CI/test","state":"SUCCESS","bucket":"pass"}]`), nil
	})
}

const testMergeSHA = "cafef00dcafef00dcafef00dcafef00dcafef00d"

// wirePostMergeGreen wires everything doPostMerge needs to resolve the real
// merge commit, verify main contains it, and observe both required
// post-merge workflows (CI, CodeQL) as green for that exact commit.
func wirePostMergeGreen(t *testing.T, fr *FakeRunner, repoRoot, prNum string) {
	t.Helper()
	writeRequiredWorkflowFiles(t, repoRoot)
	fr.On("gh", []string{"pr", "view", prNum}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"MERGED","mergeCommit":{"oid":"` + testMergeSHA + `"}}`), nil
	})
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 0}, nil
	})
	fr.On("gh", []string{"run", "list"}, func(c FakeCall) (Result, error) {
		return resultOK(`[{"databaseId":501,"workflowName":"CI","status":"completed","conclusion":"success","headSha":"` + testMergeSHA + `"},{"databaseId":502,"workflowName":"CodeQL","status":"completed","conclusion":"success","headSha":"` + testMergeSHA + `"}]`), nil
	})
}

func TestWorkflow_Start_HappyPath_PausesAtMergeConfirmation(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusAwaitingApproval || s.PausedReason != PauseMergeConfirmation {
		t.Fatalf("expected pause at merge confirmation, got status=%s reason=%s step=%s", s.Status, s.PausedReason, s.Step)
	}
	if s.PRNumber != 1 {
		t.Fatalf("expected PR #1, got %d", s.PRNumber)
	}
	if s.ReviewCount != 1 {
		t.Fatalf("expected exactly one review to have run, got %d", s.ReviewCount)
	}
}

func TestWorkflow_Approve_AdvancesPastMergeConfirmation(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatal(err)
	}

	fr.On("gh", []string{"pr", "merge", "1"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	wirePostMergeGreen(t, fr, w.Cfg.RepoRoot, "1")

	if err := w.Approve(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusCompleted || s.Step != StepComplete {
		t.Fatalf("expected completion after approve+merge+post-merge, got status=%s step=%s", s.Status, s.Step)
	}
	if s.MergeCommitSHA != testMergeSHA {
		t.Fatalf("expected the real merge commit SHA to be persisted, got %q", s.MergeCommitSHA)
	}
	if len(s.PostMergeRuns) != 2 {
		t.Fatalf("expected two post-merge run records (CI, CodeQL), got %+v", s.PostMergeRuns)
	}
}

func TestWorkflow_ReviewCount_NeverExceedsOne(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	reviewCalls := 0
	wireClaude(func(prompt string) (string, error) {
		if strings.Contains(prompt, "independently reviewing") {
			reviewCalls++
			return ordinaryBlockedResult("missing test"), nil
		}
		return passResult, nil
	})
	wireLocalGatesToPass(fr)

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Review -> BLOCKED(ordinary) -> FixBlocker -> Recheck (a review-shaped
	// call, but structurally distinct: RecheckCount, not ReviewCount) ->
	// still blocked -> manual pause. The *review* step itself must only
	// have incremented ReviewCount once.
	if s.ReviewCount != 1 {
		t.Fatalf("expected ReviewCount to stay at 1 (review is never repeated), got %d", s.ReviewCount)
	}
	if s.RecheckCount != 1 {
		t.Fatalf("expected exactly one recheck attempt, got %d", s.RecheckCount)
	}
	if s.Status != StatusAwaitingApproval && s.Status != StatusPaused {
		t.Fatalf("expected a pause after the bounded fix+recheck still found a blocker, got status=%s", s.Status)
	}
	if reviewCalls != 2 {
		t.Fatalf("expected exactly 2 review-shaped calls (1 review + 1 recheck), got %d", reviewCalls)
	}
}

func TestWorkflow_ArchitectureBlocker_PausesImmediately_NoFixAttempted(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	fixCalls := 0
	wireClaude(func(prompt string) (string, error) {
		// Check the review marker first: the review prompt's own blocker-
		// classification text also mentions "a bounded, focused fix" (in
		// its description of the "ordinary" category), so a fix-prompt
		// marker must be checked only after ruling out "this is a review
		// prompt" -- "not a broader rewrite" appears only in the actual
		// fix prompt (see GenerateFixPrompt).
		if strings.Contains(prompt, "independently reviewing") {
			return architectureBlockedResult("new caching layer decision"), nil
		}
		if strings.Contains(prompt, "not a broader rewrite") {
			fixCalls++
			return passResult, nil
		}
		return passResult, nil
	})

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.PausedReason != PauseArchitecturalDecision {
		t.Fatalf("expected PauseArchitecturalDecision, got %s", s.PausedReason)
	}
	if fixCalls != 0 {
		t.Fatalf("an architecture blocker must never be auto-fixed, got %d fix attempts", fixCalls)
	}
	if s.ReviewCount != 1 {
		t.Fatalf("expected exactly one review, got %d", s.ReviewCount)
	}
}

func TestWorkflow_CIRepair_BoundedThenPauses(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	cfg := w.Cfg
	cfg.MaxCIRepairAttempts = 2
	w.Cfg = cfg

	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)

	fr.On("gh", []string{"pr", "checks", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`[{"name":"CI/test","state":"FAILURE","bucket":"fail","link":"https://github.com/o/r/actions/runs/1/job/1"}]`), nil
	})
	fr.On("gh", []string{"run", "view"}, func(c FakeCall) (Result, error) {
		return resultOK("some failure log output"), nil
	})

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.PausedReason != PauseCIRepairExhausted {
		t.Fatalf("expected CI repair to exhaust its bound and pause, got status=%s reason=%s ci_repair_count=%d", s.Status, s.PausedReason, s.CIRepairCount)
	}
	if s.CIRepairCount != cfg.MaxCIRepairAttempts {
		t.Fatalf("expected CIRepairCount to equal the configured max (%d), got %d", cfg.MaxCIRepairAttempts, s.CIRepairCount)
	}
}

func TestWorkflow_MissingResultContract_FailsClosed(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	wireClaude(func(prompt string) (string, error) {
		return "Claude said some things but never printed a result block.", nil
	})

	err := w.Start(context.Background(), 16, StageImplementation, false)
	if err == nil {
		t.Fatal("expected an error when Claude's output has no result contract")
	}
	s, loadErr := LoadState(w.Cfg)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if s.Status != StatusFailed {
		t.Fatalf("expected Status=Failed on a missing result contract, got %s", s.Status)
	}
}

func TestWorkflow_Start_RefusesWhenUnfinishedRunExists(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	wireClaude(func(prompt string) (string, error) {
		if strings.Contains(prompt, "independently reviewing") {
			return architectureBlockedResult("x"), nil
		}
		return passResult, nil
	})
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background(), 17, StageImplementation, false); err == nil {
		t.Fatal("expected Start to refuse starting a new run over an unfinished one without --force")
	}
}

func TestWorkflow_Resume_ReconcilesMergedPR_AdvancesToPostMerge(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	s := NewState(16, StageImplementation, "phase-16-implementation", "main")
	s.Step = StepCIWatch
	s.Status = StatusRunning
	s.PRNumber = 1
	if err := s.Save(w.Cfg); err != nil {
		t.Fatal(err)
	}

	fr.On("git", []string{"show-ref", "--verify", "--quiet", "refs/heads/phase-16-implementation"}, func(c FakeCall) (Result, error) {
		return resultOK(""), nil
	})
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	// A single "gh pr view 1" handler serves both Reconcile's PRState
	// check (--json state) and doPostMerge's MergeCommitSHA resolution
	// (--json state,mergeCommit) -- proving reconciliation and real
	// post-merge verification compose correctly, not just one or the
	// other.
	wirePostMergeGreen(t, fr, w.Cfg.RepoRoot, "1")

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	loaded, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusCompleted {
		t.Fatalf("expected reconciliation to notice the already-merged PR, resolve the real merge commit, verify main contains it, and observe both required post-merge workflows green, got status=%s step=%s", loaded.Status, loaded.Step)
	}
	if loaded.MergeCommitSHA != testMergeSHA {
		t.Fatalf("expected the real (gh-resolved) merge commit to be persisted, got %q", loaded.MergeCommitSHA)
	}
}

func TestWorkflow_Resume_NothingToResume_Errors(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	if err := w.Resume(context.Background()); err == nil {
		t.Fatal("expected an error resuming with no saved state")
	}
}

func TestWorkflow_Pause_OnlyWorksWhileRunning(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	wireClaude(func(prompt string) (string, error) {
		if strings.Contains(prompt, "independently reviewing") {
			return architectureBlockedResult("x"), nil
		}
		return passResult, nil
	})
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatal(err)
	}
	// The run is now paused (awaiting approval), not running.
	if err := w.Pause(context.Background(), "operator says stop"); err == nil {
		t.Fatal("expected Pause to refuse when the run is not in StatusRunning")
	}
}

func TestWorkflow_CIWatch_PendingChecks_StaysAtStepForNextResume(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	fr.On("gh", []string{"pr", "checks", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`[{"name":"CI/test","state":"PENDING","bucket":"pending"}]`), nil
	})

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Step != StepCIWatch || s.Status != StatusRunning {
		t.Fatalf("expected the run to remain resumable at ci_watch (not paused, not failed), got step=%s status=%s", s.Step, s.Status)
	}
}

// wirePostMergePending wires everything doPostMerge needs to resolve the
// real merge commit and verify main contains it, but reports that NEITHER
// required post-merge workflow's run has appeared yet for that commit --
// the "expected runs have not appeared" case, which must never be
// interpreted as success.
func wirePostMergePending(t *testing.T, fr *FakeRunner, repoRoot, prNum string) {
	t.Helper()
	writeRequiredWorkflowFiles(t, repoRoot)
	fr.On("gh", []string{"pr", "view", prNum}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"MERGED","mergeCommit":{"oid":"` + testMergeSHA + `"}}`), nil
	})
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 0}, nil
	})
	fr.On("gh", []string{"run", "list"}, func(c FakeCall) (Result, error) {
		return resultOK(`[]`), nil
	})
}

func postMergeReadyState(prNum int) *State {
	s := NewState(16, StageImplementation, "phase-16-implementation", "main")
	s.Step = StepPostMerge
	s.Status = StatusRunning
	s.PRNumber = prNum
	return s
}

func TestWorkflow_Merge_DoesNotSynchronouslyCompletePhase(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatal(err)
	}

	fr.On("gh", []string{"pr", "merge", "1"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	// PR checks (pre-merge) were already green -- that is what got us to
	// the merge-confirmation gate. Post-merge, NO run has appeared yet for
	// the real merge commit.
	wirePostMergePending(t, fr, w.Cfg.RepoRoot, "1")

	if err := w.Approve(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status == StatusCompleted {
		t.Fatal("merging the PR must never synchronously imply the phase is complete -- post-merge CI/CodeQL for the real merge commit have not been observed yet")
	}
	if s.Step != StepPostMerge || s.Status != StatusRunning {
		t.Fatalf("expected the run to remain resumable at post_merge, got step=%s status=%s", s.Step, s.Status)
	}
	if s.MergeCommitSHA != testMergeSHA {
		t.Fatalf("expected the merge commit to already be resolved and persisted, got %q", s.MergeCommitSHA)
	}
}

func TestWorkflow_PostMerge_PreMergePRChecksCannotSatisfyVerification(t *testing.T) {
	// Even though wireAllChecksGreen made "gh pr checks" report every
	// pre-merge check green (which is what let this run reach the merge
	// gate at all), doPostMerge must never consult "gh pr checks" as
	// evidence -- only "gh run list --commit <real merge sha>" counts.
	// wirePostMergePending intentionally provides zero runs for the real
	// commit; if doPostMerge (incorrectly) fell back to the PR-checks
	// result, this run would complete. It must not.
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatal(err)
	}
	fr.On("gh", []string{"pr", "merge", "1"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	wirePostMergePending(t, fr, w.Cfg.RepoRoot, "1")

	if err := w.Approve(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status == StatusCompleted {
		t.Fatal("pre-merge PR checks being green must never, by themselves, satisfy post-merge verification")
	}
}

func TestWorkflow_PostMerge_PendingRunsSurviveResume_ThenCompleteOnceGreen(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	writeRequiredWorkflowFiles(t, w.Cfg.RepoRoot)
	s := postMergeReadyState(1)
	// Simulate a prior invocation having already resolved the merge
	// commit -- resume must watch this SAME commit, never re-merge or
	// re-resolve it.
	s.MergeCommitSHA = testMergeSHA
	if err := s.Save(w.Cfg); err != nil {
		t.Fatal(err)
	}

	fr.On("git", []string{"show-ref", "--verify", "--quiet", "refs/heads/phase-16-implementation"}, func(c FakeCall) (Result, error) {
		return resultOK(""), nil
	})
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 0}, nil
	})
	// PRState (Reconcile) reports still-open (no-op) so reconciliation
	// itself does not short-circuit this test's own post-merge polling.
	fr.On("gh", []string{"pr", "view", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"MERGED","mergeCommit":{"oid":"` + testMergeSHA + `"}}`), nil
	})

	pending := true
	fr.On("gh", []string{"run", "list"}, func(c FakeCall) (Result, error) {
		if pending {
			return resultOK(`[]`), nil
		}
		return resultOK(`[{"databaseId":501,"workflowName":"CI","status":"completed","conclusion":"success","headSha":"` + testMergeSHA + `"},{"databaseId":502,"workflowName":"CodeQL","status":"completed","conclusion":"success","headSha":"` + testMergeSHA + `"}]`), nil
	})

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("unexpected error on first resume: %v", err)
	}
	afterFirst, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if afterFirst.Status != StatusRunning || afterFirst.Step != StepPostMerge {
		t.Fatalf("expected the pending post-merge watch to survive as a resumable state, got status=%s step=%s", afterFirst.Status, afterFirst.Step)
	}
	if afterFirst.MergeCommitSHA != testMergeSHA {
		t.Fatalf("expected the merge commit to remain the same across the pending resume, got %q", afterFirst.MergeCommitSHA)
	}
	for _, r := range afterFirst.PostMergeRuns {
		if r.RunID != "" {
			t.Fatalf("expected no run IDs to be recorded yet (nothing has appeared), got %+v", afterFirst.PostMergeRuns)
		}
	}

	pending = false
	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("unexpected error on second resume: %v", err)
	}
	final, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusCompleted || final.Step != StepComplete {
		t.Fatalf("expected completion once both required runs are green, got status=%s step=%s", final.Status, final.Step)
	}
	if final.MergeCommitSHA != testMergeSHA {
		t.Fatalf("expected resume to keep watching the SAME merged commit, got %q", final.MergeCommitSHA)
	}
	var gotCI, gotCodeQL bool
	for _, r := range final.PostMergeRuns {
		if r.Workflow == "CI" && r.RunID == "501" {
			gotCI = true
		}
		if r.Workflow == "CodeQL" && r.RunID == "502" {
			gotCodeQL = true
		}
	}
	if !gotCI || !gotCodeQL {
		t.Fatalf("expected both real run IDs to be persisted in state, got %+v", final.PostMergeRuns)
	}
}

func TestWorkflow_PostMerge_FailedRequiredRun_PreventsCompletion(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	writeRequiredWorkflowFiles(t, w.Cfg.RepoRoot)
	s := postMergeReadyState(1)
	s.MergeCommitSHA = testMergeSHA
	if err := s.Save(w.Cfg); err != nil {
		t.Fatal(err)
	}

	fr.On("git", []string{"show-ref", "--verify", "--quiet", "refs/heads/phase-16-implementation"}, func(c FakeCall) (Result, error) {
		return resultOK(""), nil
	})
	fr.On("git", []string{"checkout", "main"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"pull", "--ff-only"}, func(c FakeCall) (Result, error) { return resultOK(""), nil })
	fr.On("git", []string{"merge-base", "--is-ancestor"}, func(c FakeCall) (Result, error) {
		return Result{ExitCode: 0}, nil
	})
	fr.On("gh", []string{"pr", "view", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"MERGED","mergeCommit":{"oid":"` + testMergeSHA + `"}}`), nil
	})
	fr.On("gh", []string{"run", "list"}, func(c FakeCall) (Result, error) {
		return resultOK(`[{"databaseId":501,"workflowName":"CI","status":"completed","conclusion":"success","headSha":"` + testMergeSHA + `"},{"databaseId":502,"workflowName":"CodeQL","status":"completed","conclusion":"failure","headSha":"` + testMergeSHA + `"}]`), nil
	})
	fr.On("gh", []string{"run", "view", "502"}, func(c FakeCall) (Result, error) {
		return resultOK("CodeQL analysis failed: some real finding\n"), nil
	})

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	final, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status == StatusCompleted {
		t.Fatal("a failed required post-merge run must prevent the phase from ever being marked complete")
	}
	if final.Status != StatusFailed {
		t.Fatalf("expected Status=Failed with evidence, got %s (%s)", final.Status, final.PausedDetail)
	}
	if !strings.Contains(final.PausedDetail, "CodeQL") {
		t.Fatalf("expected the failure detail to name CodeQL, got %q", final.PausedDetail)
	}
	foundLog := false
	for _, p := range final.LogPaths {
		if strings.Contains(p, "post-merge") {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatalf("expected a captured post-merge failure log path, got %v", final.LogPaths)
	}
}

func TestWorkflow_LocalGates_MissingManifest_FailsClosed(t *testing.T) {
	w, _, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	if err := os.Remove(ManifestPath(w.Cfg.RepoRoot, 16)); err != nil {
		t.Fatal(err)
	}
	err := w.Start(context.Background(), 16, StageImplementation, false)
	if err == nil {
		t.Fatal("expected an error: local_gates must fail closed when this phase's proof manifest is missing")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("expected the error to mention the manifest, got %v", err)
	}
	s, loadErr := LoadState(w.Cfg)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if s.Status != StatusFailed {
		t.Fatalf("expected Status=Failed, got %s", s.Status)
	}
}

func TestWorkflow_LocalGates_RequiredCommandProof_MustPass(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	digest := digestOf(t, w.Cfg.RepoRoot, "docs/phase-16-plan.md")
	writeManifest(t, w.Cfg.RepoRoot, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"extra-check","type":"command","command":["go","version"],"required":true}]}`)

	extraRan := false
	fr.On("go", []string{"version"}, func(c FakeCall) (Result, error) {
		extraRan = true
		return resultOK("go version go1.25.0\n"), nil
	})

	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !extraRan {
		t.Fatal("expected the manifest's required command proof to actually run as part of local_gates")
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusAwaitingApproval {
		t.Fatalf("expected the run to proceed past local_gates to the merge-confirmation gate, got status=%s step=%s", s.Status, s.Step)
	}
}

func TestWorkflow_LocalGates_RequiredCommandProof_FailurePreventsCompletion(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	digest := digestOf(t, w.Cfg.RepoRoot, "docs/phase-16-plan.md")
	writeManifest(t, w.Cfg.RepoRoot, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"extra-check","type":"command","command":["go","version"],"required":true}]}`)
	fr.On("go", []string{"version"}, func(c FakeCall) (Result, error) {
		return Result{Stderr: "boom"}, &exitErrStub{}
	})

	err := w.Start(context.Background(), 16, StageImplementation, false)
	if err == nil {
		t.Fatal("expected an error: a failing required phase-specific proof must fail local_gates")
	}
	s, loadErr := LoadState(w.Cfg)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if s.Status != StatusFailed {
		t.Fatalf("expected Status=Failed, got %s", s.Status)
	}
	if s.Step == StepCommit || s.Step == StepPush || s.Step == StepPRCreate {
		t.Fatalf("must never have advanced past local_gates on a failed phase-specific proof, got step=%s", s.Step)
	}
}

func TestWorkflow_LocalGates_RequiredHumanProof_PausesThenApproveContinues(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	digest := digestOf(t, w.Cfg.RepoRoot, "docs/phase-16-plan.md")
	writeManifest(t, w.Cfg.RepoRoot, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"benchmark-evidence","type":"human","description":"run the benchmark and confirm evidence is committed","required":true}]}`)

	// Deliberately NOT wiring wireLocalGatesToPass: if doLocalGates ran any
	// gate before checking the human proof, the FakeRunner's
	// FailUnmatched default would turn that into an unrelated error,
	// itself a regression signal.
	if err := w.Start(context.Background(), 16, StageImplementation, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusAwaitingApproval || s.PausedReason != PauseExternalValidation {
		t.Fatalf("expected a pause for the required human proof, got status=%s reason=%s", s.Status, s.PausedReason)
	}
	if s.PendingHumanProof != "benchmark-evidence" {
		t.Fatalf("expected PendingHumanProof=benchmark-evidence, got %q", s.PendingHumanProof)
	}

	// Now the human approves it, and the run must proceed through the
	// (now-wired) gates and on to the merge-confirmation gate.
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	if err := w.Approve(context.Background()); err != nil {
		t.Fatalf("unexpected error on approve: %v", err)
	}
	final, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(final.ApprovedHumanProofs, "benchmark-evidence") {
		t.Fatalf("expected the approval to be durably recorded, got %v", final.ApprovedHumanProofs)
	}
	if final.PendingHumanProof != "" {
		t.Fatalf("expected PendingHumanProof to be cleared after approval, got %q", final.PendingHumanProof)
	}
	if final.Status != StatusAwaitingApproval || final.PausedReason != PauseMergeConfirmation {
		t.Fatalf("expected the run to proceed all the way to the merge-confirmation gate, got status=%s reason=%s step=%s", final.Status, final.PausedReason, final.Step)
	}
}

func TestWorkflow_LocalGates_PlanningStage_SkipsManifestCheck(t *testing.T) {
	w, fr, _ := newTestWorkflow(t)
	wireClaude(approveThenPass)
	wireLocalGatesToPass(fr)
	wireAllChecksGreen(fr, "1")
	// No autopilot/phases/16.json at all for this run -- planning-stage
	// local_gates must not care, since a manifest binds to a ratified
	// plan that a planning-stage run is still producing.
	if err := os.Remove(ManifestPath(w.Cfg.RepoRoot, 16)); err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background(), 16, StagePlanning, false); err != nil {
		t.Fatalf("unexpected error: planning-stage local_gates must not require a phase proof manifest: %v", err)
	}
	s, err := LoadState(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusAwaitingApproval {
		t.Fatalf("expected the planning run to proceed to the merge-confirmation gate, got status=%s step=%s", s.Status, s.Step)
	}
}
