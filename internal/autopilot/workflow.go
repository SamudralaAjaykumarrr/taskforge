package autopilot

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Workflow is Autopilot's orchestrator: it drives State through the bounded
// step graph in stage.go, calling out to Git/GitHub/Claude, and stopping
// (never looping unboundedly, never running as a daemon) at the first
// human gate, unrecoverable error, or completion.
type Workflow struct {
	Cfg    Config
	Git    *Git
	GH     *GitHub
	Claude *Claude
	Out    io.Writer
}

func (w *Workflow) log(format string, a ...any) {
	fmt.Fprintf(w.Out, format+"\n", a...)
}

// Start begins a new run for phase/stage. It refuses to start if an
// unfinished run already exists, unless force is set.
func (w *Workflow) Start(ctx context.Context, phase int, stage Stage, force bool) error {
	if !stage.Valid() {
		return fmt.Errorf("invalid --stage %q (must be %q or %q)", stage, StagePlanning, StageImplementation)
	}
	if existing, err := LoadState(w.Cfg); err == nil {
		if existing.Status != StatusCompleted && !force {
			return fmt.Errorf("an unfinished run already exists (phase %d, stage %s, step %s, status %s) -- use 'resume' to continue it, 'pause'/inspect it first, or pass a force option to abandon it", existing.Phase, existing.Stage, existing.Step, existing.Status)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for existing state: %w", err)
	}

	base, err := w.Git.DefaultBranch(ctx)
	if err != nil {
		return fmt.Errorf("resolving base branch: %w", err)
	}
	var branch string
	switch stage {
	case StagePlanning:
		branch = fmt.Sprintf("phase-%d-planning", phase)
	case StageImplementation:
		branch = fmt.Sprintf("phase-%d-implementation", phase)
	}
	if err := ValidateBranchName(branch); err != nil {
		return err
	}

	s := NewState(phase, stage, branch, base)
	if err := s.Save(w.Cfg); err != nil {
		return err
	}
	w.log("[CHECK] repository state")
	return w.run(ctx, s)
}

// Resume re-verifies real git/GitHub state against the saved state, then
// continues the workflow from wherever it left off.
func (w *Workflow) Resume(ctx context.Context) error {
	s, err := LoadState(w.Cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved state found -- nothing to resume; use 'start'")
		}
		return err
	}
	if s.Status == StatusCompleted {
		w.log("[INFO] phase %d (%s) is already complete", s.Phase, s.Stage)
		return nil
	}
	if s.Status == StatusFailed {
		return fmt.Errorf("run is in a failed state (%s) -- resume refuses to continue automatically; investigate and edit/remove state.json deliberately", s.PausedDetail)
	}
	for _, warn := range Reconcile(ctx, w.Git, w.GH, s) {
		w.log("[WARN] %s", warn)
	}
	if err := s.Save(w.Cfg); err != nil {
		return err
	}
	if s.Status == StatusFailed {
		return fmt.Errorf("reconciliation found real state cannot be trusted: %s", s.PausedDetail)
	}
	if s.Status == StatusAwaitingApproval || s.Status == StatusPaused {
		w.log("[PAUSE] still paused: %s (%s) -- run 'approve' once resolved", s.PausedReason, s.PausedDetail)
		return nil
	}
	return w.run(ctx, s)
}

// Approve clears the current human-gate pause and continues the run. It is
// the only way past a pause -- there is no flag or config that skips one.
func (w *Workflow) Approve(ctx context.Context) error {
	s, err := LoadState(w.Cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved state found -- nothing to approve")
		}
		return err
	}
	if s.Status != StatusAwaitingApproval && s.Status != StatusPaused {
		return fmt.Errorf("nothing is awaiting approval (status=%s)", s.Status)
	}
	w.log("[APPROVE] clearing pause: %s", s.PausedReason)
	reason := s.PausedReason
	s.Status = StatusRunning
	s.PausedReason = ""
	s.PausedDetail = ""

	// Approving a pause that occurred mid-step (an ordinary blocker
	// classification, a CI-repair exhaustion, a manual pause) resumes at
	// the same step; approving the dedicated merge-confirmation gate
	// advances into Merge.
	if reason == PauseMergeConfirmation {
		if err := s.Advance(StepMerge); err != nil {
			return err
		}
	}
	if reason == PauseCIRepairExhausted {
		// A human approved continuing despite the bound; reset the counter
		// for a fresh, explicitly-authorized bounded window rather than
		// silently making it unlimited.
		s.CIRepairCount = 0
	}
	if reason == PauseExternalValidation && s.PendingHumanProof != "" {
		// The human's explicit "approve" here IS the durable record that
		// this specific manifest proof obligation was satisfied --
		// Autopilot never infers this from anything else.
		s.ApprovedHumanProofs = append(s.ApprovedHumanProofs, s.PendingHumanProof)
		s.PendingHumanProof = ""
	}
	if err := s.Save(w.Cfg); err != nil {
		return err
	}
	return w.run(ctx, s)
}

// Pause manually stops a running workflow, preserving resumable state.
func (w *Workflow) Pause(ctx context.Context, detail string) error {
	s, err := LoadState(w.Cfg)
	if err != nil {
		return err
	}
	if s.Status != StatusRunning {
		return fmt.Errorf("nothing is running (status=%s)", s.Status)
	}
	s.Pause(PauseManual, detail)
	return s.Save(w.Cfg)
}

// StatusReport loads and reconciles state without advancing it, for the
// "status" command.
func (w *Workflow) StatusReport(ctx context.Context) (*State, []string, error) {
	s, err := LoadState(w.Cfg)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if s.Status != StatusCompleted && s.Status != StatusFailed {
		warnings = Reconcile(ctx, w.Git, w.GH, s)
	}
	return s, warnings, nil
}

// run is the bounded execution loop: it performs automated steps until it
// hits a human gate, an error, or completion, saving state after every
// single step transition so a crash mid-run loses at most the in-flight
// step.
func (w *Workflow) run(ctx context.Context, s *State) error {
	for {
		if s.Status != StatusRunning {
			return s.Save(w.Cfg)
		}
		prevStep, prevStatus := s.Step, s.Status
		var err error
		switch s.Step {
		case StepNotStarted:
			err = s.Advance(StepBranch)
		case StepBranch:
			err = w.doBranch(ctx, s)
		case StepDraft:
			err = w.doDraft(ctx, s)
		case StepReview:
			err = w.doReview(ctx, s)
		case StepFixBlocker:
			err = w.doFixBlocker(ctx, s)
		case StepRecheck:
			err = w.doRecheck(ctx, s)
		case StepLocalGates:
			err = w.doLocalGates(ctx, s)
		case StepCommit:
			err = w.doCommit(ctx, s)
		case StepPush:
			err = w.doPush(ctx, s)
		case StepPRCreate:
			err = w.doPRCreate(ctx, s)
		case StepCIWatch:
			err = w.doCIWatch(ctx, s)
		case StepCIRepair:
			err = w.doCIRepair(ctx, s)
		case StepMergeApproval:
			err = w.doMergeApproval(ctx, s)
		case StepMerge:
			err = w.doMerge(ctx, s)
		case StepPostMerge:
			err = w.doPostMerge(ctx, s)
		case StepComplete:
			s.Status = StatusCompleted
			w.log("[DONE ] phase %d (%s) complete", s.Phase, s.Stage)
		default:
			err = fmt.Errorf("unknown step %q", s.Step)
		}
		if err != nil {
			s.Fail(err.Error())
			w.log("[FAIL ] %v", err)
			_ = s.Save(w.Cfg)
			return err
		}
		if saveErr := s.Save(w.Cfg); saveErr != nil {
			return saveErr
		}
		if s.Step == StepComplete && s.Status == StatusCompleted {
			return nil
		}
		if s.Step == prevStep && s.Status == prevStatus {
			// No progress was made this iteration (e.g. doCIWatch exhausted
			// its bounded poll budget without a conclusive result).
			// Autopilot is not a daemon: it stops here rather than
			// busy-looping, leaving the run resumable at the same step.
			return nil
		}
	}
}

func (w *Workflow) docs(s *State) (PhaseDocs, error) {
	return DiscoverPhase(w.Cfg.RepoRoot, s.Phase)
}

func (w *Workflow) promptInput(s *State) (PromptInput, error) {
	docs, err := w.docs(s)
	if err != nil {
		return PromptInput{}, err
	}
	// Best-effort only: a missing/invalid manifest must not block prompt
	// generation (and therefore the draft/review/fix step it feeds) --
	// doLocalGates is the sole enforcement point for manifest validity
	// (see its own doc comment). This just gives Claude visibility into
	// what will be enforced, when a valid manifest happens to exist.
	var manifest *PhaseManifest
	if s.Stage == StageImplementation && docs.PlanExists {
		manifest, _ = LoadManifest(w.Cfg.RepoRoot, s.Phase, docs.PlanPath)
	}
	return PromptInput{
		Phase:    s.Phase,
		Stage:    s.Stage,
		Docs:     docs,
		Fixed:    AuthoritativeDocs(w.Cfg.RepoRoot),
		Branch:   s.Branch,
		Manifest: manifest,
	}, nil
}

func (w *Workflow) doBranch(ctx context.Context, s *State) error {
	w.log("[RUN ] create branch %s", s.Branch)
	if err := w.Git.CreateBranch(ctx, s.Branch, s.BaseBranch); err != nil {
		return err
	}
	w.log("[PASS] branch %s created from %s", s.Branch, s.BaseBranch)
	return s.Advance(StepDraft)
}

func (w *Workflow) doDraft(ctx context.Context, s *State) error {
	in, err := w.promptInput(s)
	if err != nil {
		return err
	}
	w.log("[RUN ] invoke claude (implementer) for %s stage of phase %d", s.Stage, s.Phase)
	prompt := GenerateImplementationPrompt(in)
	out, err := w.Claude.Invoke(ctx, RoleImplementer, prompt, "acceptEdits")
	if err != nil {
		return fmt.Errorf("draft invocation failed: %w", err)
	}
	logPath, _ := WriteLog(w.Cfg, "claude", "draft", out)
	s.LogPaths = append(s.LogPaths, logPath)
	res, err := ParseImplementationResult(out)
	if err != nil {
		return fmt.Errorf("draft result contract invalid, failing closed (see %s): %w", logPath, err)
	}
	if res.Validation != "PASS" || !res.ReadyForReview {
		return fmt.Errorf("implementer reported VALIDATION=%s READY_FOR_REVIEW=%v, blockers=%q -- not proceeding to review", res.Validation, res.ReadyForReview, res.Blockers)
	}
	w.log("[PASS] draft complete, ready for review")
	if s.Stage == StagePlanning {
		s.PlanPath = in.Docs.PlanPath
	}
	return s.Advance(StepReview)
}

func (w *Workflow) doReview(ctx context.Context, s *State) error {
	if err := s.IncrementReview(w.Cfg.MaxReviewCount); err != nil {
		return err
	}
	in, err := w.promptInput(s)
	if err != nil {
		return err
	}
	w.log("[RUN ] independent review (fresh session)")
	prompt := GenerateReviewPrompt(in)
	out, err := w.Claude.Invoke(ctx, RoleReviewer, prompt, "acceptEdits")
	if err != nil {
		return fmt.Errorf("review invocation failed: %w", err)
	}
	logPath, _ := WriteLog(w.Cfg, "claude", "review", out)
	s.LogPaths = append(s.LogPaths, logPath)
	res, err := ParseReviewResult(out)
	if err != nil {
		return fmt.Errorf("review result contract invalid, failing closed (see %s): %w", logPath, err)
	}
	if res.Verdict == "APPROVE" {
		w.log("[PASS] independent review: APPROVE")
		return s.Advance(StepLocalGates)
	}
	w.log("[BLOCK] independent review found a blocker (type=%s): %s", res.BlockerType, res.Blockers)
	if pauseReason, isHuman := res.BlockerType.PauseReason(); isHuman {
		s.Pause(pauseReason, res.Blockers)
		w.log("[PAUSE] %s requires human approval", pauseReason)
		return nil
	}
	s.PendingBlockerType = res.BlockerType
	s.PendingBlockerText = res.Blockers
	return s.Advance(StepFixBlocker)
}

func (w *Workflow) doFixBlocker(ctx context.Context, s *State) error {
	in, err := w.promptInput(s)
	if err != nil {
		return err
	}
	w.log("[RUN ] bounded fix for blocker: %s", s.PendingBlockerText)
	prompt := GenerateFixPrompt(in, s.PendingBlockerText)
	out, err := w.Claude.Invoke(ctx, RoleFixer, prompt, "acceptEdits")
	if err != nil {
		return fmt.Errorf("fix invocation failed: %w", err)
	}
	logPath, _ := WriteLog(w.Cfg, "claude", "fix-blocker", out)
	s.LogPaths = append(s.LogPaths, logPath)
	res, err := ParseImplementationResult(out)
	if err != nil {
		return fmt.Errorf("fix result contract invalid, failing closed (see %s): %w", logPath, err)
	}
	if res.Validation != "PASS" {
		return fmt.Errorf("bounded fix did not report VALIDATION=PASS (blockers=%q) -- not proceeding automatically", res.Blockers)
	}
	w.log("[PASS] fix applied")
	return s.Advance(StepRecheck)
}

func (w *Workflow) doRecheck(ctx context.Context, s *State) error {
	if err := s.IncrementRecheck(w.Cfg.MaxRecheckCount); err != nil {
		return err
	}
	in, err := w.promptInput(s)
	if err != nil {
		return err
	}
	w.log("[RUN ] focused re-check of the fixed blocker")
	prompt := GenerateReviewPrompt(in)
	out, err := w.Claude.Invoke(ctx, RoleReviewer, prompt, "acceptEdits")
	if err != nil {
		return fmt.Errorf("recheck invocation failed: %w", err)
	}
	logPath, _ := WriteLog(w.Cfg, "claude", "recheck", out)
	s.LogPaths = append(s.LogPaths, logPath)
	res, err := ParseReviewResult(out)
	if err != nil {
		return fmt.Errorf("recheck result contract invalid, failing closed (see %s): %w", logPath, err)
	}
	if res.Verdict != "APPROVE" {
		if pauseReason, isHuman := res.BlockerType.PauseReason(); isHuman {
			s.Pause(pauseReason, res.Blockers)
			w.log("[PAUSE] %s requires human approval", pauseReason)
			return nil
		}
		s.Pause(PauseManual, fmt.Sprintf("focused recheck still found a blocker after the one bounded fix attempt: %s -- this workflow policy never loops full reviews, so it stops here for a human", res.Blockers))
		return nil
	}
	w.log("[PASS] recheck: blocker resolved")
	return s.Advance(StepLocalGates)
}

// doLocalGates runs the fixed core gates plus, for an implementation-stage
// run, this phase's checked-in proof manifest (manifest.go): every required
// human proof must already be explicitly approved (else this pauses,
// PauseExternalValidation, rather than running anything), and every
// required command proof is appended to the gate sequence so it must pass
// alongside gofmt/vet/build/test. A missing/malformed/stale manifest fails
// closed -- it is never treated as "no phase-specific proofs."
//
// Planning-stage runs do not check a manifest: a phase's proof manifest is
// authored *from* its ratified plan, so it cannot meaningfully exist yet
// while that plan is still being drafted/reviewed.
func (w *Workflow) doLocalGates(ctx context.Context, s *State) error {
	var extra []gateSpec
	if s.Stage == StageImplementation {
		docs, err := w.docs(s)
		if err != nil {
			return err
		}
		manifest, err := LoadManifest(w.Cfg.RepoRoot, s.Phase, docs.PlanPath)
		if err != nil {
			return fmt.Errorf("phase %d proof manifest invalid, failing closed: %w", s.Phase, err)
		}

		for _, p := range manifest.RequiredHumanProofs() {
			if containsString(s.ApprovedHumanProofs, p.Name) {
				continue
			}
			s.PendingHumanProof = p.Name
			s.Pause(PauseExternalValidation, fmt.Sprintf("required human proof obligation %q (phase %d manifest) is not yet approved: %s", p.Name, s.Phase, p.Description))
			w.log("[PAUSE] human proof required: %s", p.Name)
			return nil
		}
		s.PendingHumanProof = ""
		extra = manifest.RequiredCommandProofs()
	}

	results, err := RunLocalGates(ctx, w.Cfg, w.Claude.Runner, extra, func(msg string) { w.log("%s", msg) })
	for _, r := range results {
		if r.LogPath != "" {
			s.LogPaths = append(s.LogPaths, r.LogPath)
		}
	}
	if err != nil {
		return err
	}
	s.LastSuccessfulGate = "local_gates"
	return s.Advance(StepCommit)
}

func (w *Workflow) doCommit(ctx context.Context, s *State) error {
	if clean, _, err := w.Git.IsClean(ctx); err != nil {
		return err
	} else if clean {
		return fmt.Errorf("nothing to commit -- working tree is clean, which is unexpected at this step")
	}
	if err := w.Git.AddAll(ctx, "."); err != nil {
		return err
	}
	msg := commitMessage(s)
	msgFile, err := PRBodyFile(w.Cfg, msg)
	if err != nil {
		return err
	}
	if err := w.Git.Commit(ctx, msgFile); err != nil {
		return err
	}
	sha, err := w.Git.RevParse(ctx, "HEAD")
	if err != nil {
		return err
	}
	s.CommitSHA = sha
	w.log("[PASS] committed %s", sha)
	return s.Advance(StepPush)
}

func (w *Workflow) doPush(ctx context.Context, s *State) error {
	if err := w.Git.Push(ctx, s.Branch); err != nil {
		return err
	}
	w.log("[PASS] pushed %s", s.Branch)
	return s.Advance(StepPRCreate)
}

func (w *Workflow) doPRCreate(ctx context.Context, s *State) error {
	// Idempotent: a CI-repair iteration loops back through LocalGates ->
	// Commit -> Push -> PRCreate with new commits on the SAME branch/PR,
	// it must never open a second PR for the same run.
	if s.PRNumber != 0 {
		w.log("[SKIP] PR #%d already exists for this run", s.PRNumber)
		return s.Advance(StepCIWatch)
	}
	title := prTitle(s)
	body := prBody(s)
	bodyFile, err := PRBodyFile(w.Cfg, body)
	if err != nil {
		return err
	}
	num, err := w.GH.CreatePR(ctx, title, s.BaseBranch, s.Branch, bodyFile)
	if err != nil {
		return err
	}
	s.PRNumber = num
	w.log("[PASS] opened PR #%d", num)
	return s.Advance(StepCIWatch)
}

func (w *Workflow) doCIWatch(ctx context.Context, s *State) error {
	w.log("[RUN ] watch required checks for PR #%d", s.PRNumber)
	for i := 0; i < w.Cfg.CIWatchMaxPolls; i++ {
		checks, err := w.GH.PRChecks(ctx, s.PRNumber)
		if err != nil {
			return err
		}
		summary := SummarizeChecks(checks)
		if summary.AllPass {
			w.log("[PASS] all required checks green")
			return s.Advance(StepMergeApproval)
		}
		if len(summary.Failed) > 0 {
			w.log("[FAIL] %d check(s) failed", len(summary.Failed))
			return s.Advance(StepCIRepair)
		}
		w.log("[WAIT] %d check(s) still pending (%d/%d polls)", len(summary.Pending), i+1, w.Cfg.CIWatchMaxPolls)
		if i < w.Cfg.CIWatchMaxPolls-1 {
			sleep(ctx, time.Duration(w.Cfg.CIWatchPollIntervalSeconds)*time.Second)
		}
	}
	w.log("[WAIT] checks still pending after this invocation's poll budget -- run 'resume' again later")
	return nil // stays at StepCIWatch; not an error, not a pause -- just incomplete this invocation
}

func (w *Workflow) doCIRepair(ctx context.Context, s *State) error {
	if err := s.IncrementCIRepair(w.Cfg.MaxCIRepairAttempts); err != nil {
		s.Pause(PauseCIRepairExhausted, err.Error())
		w.log("[PAUSE] %s", PauseCIRepairExhausted)
		return nil
	}
	checks, err := w.GH.PRChecks(ctx, s.PRNumber)
	if err != nil {
		return err
	}
	summary := SummarizeChecks(checks)
	var names []string
	var evidence string
	for _, c := range summary.Failed {
		names = append(names, c.Name)
		if runID, ok := ActionsRunIDFromLink(c.Link); ok {
			logText, logErr := w.GH.FailedCheckLog(ctx, runID)
			if logErr == nil {
				evidence += fmt.Sprintf("=== %s (run %s) ===\n%s\n", c.Name, runID, logText)
				if path, werr := WriteLog(w.Cfg, "ci", c.Name, logText); werr == nil {
					s.LogPaths = append(s.LogPaths, path)
				}
			}
		}
	}
	if evidence == "" {
		s.Pause(PauseManual, "CI reported failed checks but Autopilot could not collect any failed-check log evidence -- refusing to guess at a fix")
		return nil
	}
	in, err := w.promptInput(s)
	if err != nil {
		return err
	}
	prompt := GenerateCIRepairPrompt(in, names, evidence)
	out, err := w.Claude.Invoke(ctx, RoleFixer, prompt, "acceptEdits")
	if err != nil {
		return fmt.Errorf("CI repair invocation failed: %w", err)
	}
	logPath, _ := WriteLog(w.Cfg, "claude", "ci-repair", out)
	s.LogPaths = append(s.LogPaths, logPath)
	res, err := ParseImplementationResult(out)
	if err != nil {
		return fmt.Errorf("CI repair result contract invalid, failing closed (see %s): %w", logPath, err)
	}
	if res.Validation != "PASS" {
		s.Pause(PauseManual, fmt.Sprintf("CI repair attempt did not report VALIDATION=PASS: %s", res.Blockers))
		return nil
	}
	w.log("[PASS] evidence-based CI repair applied (attempt %d/%d)", s.CIRepairCount, w.Cfg.MaxCIRepairAttempts)
	return s.Advance(StepLocalGates)
}

func (w *Workflow) doMergeApproval(ctx context.Context, s *State) error {
	if w.Cfg.AutoMergeAfterGreen {
		return s.Advance(StepMerge)
	}
	s.Pause(PauseMergeConfirmation, fmt.Sprintf("PR #%d has all required checks green; explicit confirmation is required before merge (V1 default)", s.PRNumber))
	w.log("[PAUSE] merge confirmation required for PR #%d", s.PRNumber)
	return nil
}

func (w *Workflow) doMerge(ctx context.Context, s *State) error {
	checks, err := w.GH.PRChecks(ctx, s.PRNumber)
	if err != nil {
		return err
	}
	if !SummarizeChecks(checks).AllPass {
		return fmt.Errorf("refusing to merge PR #%d: required checks are not all green (state may have gone stale since the merge gate; run resume)", s.PRNumber)
	}
	if err := w.GH.MergePR(ctx, s.PRNumber); err != nil {
		return err
	}
	w.log("[PASS] merged PR #%d", s.PRNumber)
	return s.Advance(StepPostMerge)
}

// doPostMerge verifies the ACTUAL merged commit on main, not the PR's own
// pre-merge checks. It never advances to StepComplete on merge alone: the
// phase is complete only once every required post-merge workflow (CI,
// CodeQL) has a real, gh-observed, successful run against the real merge
// commit SHA. Like doCIWatch, this polls a bounded number of times per
// invocation and, if still pending, leaves the run resumable at the same
// step rather than looping unboundedly -- Autopilot is not a daemon.
func (w *Workflow) doPostMerge(ctx context.Context, s *State) error {
	if s.MergeCommitSHA == "" {
		sha, err := w.GH.MergeCommitSHA(ctx, s.PRNumber)
		if err != nil {
			return fmt.Errorf("resolving merge commit for PR #%d: %w", s.PRNumber, err)
		}
		s.MergeCommitSHA = sha
		w.log("[PASS] resolved merge commit %s for PR #%d", sha, s.PRNumber)
		if err := s.Save(w.Cfg); err != nil {
			return err
		}
	}

	if err := w.Git.Checkout(ctx, s.BaseBranch); err != nil {
		return err
	}
	if err := w.Git.FastForwardPull(ctx); err != nil {
		return err
	}
	clean, dirty, err := w.Git.IsClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("post-merge working tree is not clean:\n%s", dirty)
	}
	isAncestor, err := w.Git.IsAncestor(ctx, s.MergeCommitSHA, s.BaseBranch)
	if err != nil {
		return err
	}
	if !isAncestor {
		return fmt.Errorf("local %s does not (yet) contain merge commit %s after a fast-forward pull -- refusing to verify post-merge CI against a commit main doesn't actually have", s.BaseBranch, s.MergeCommitSHA)
	}

	required, err := RequiredPostMergeWorkflows(w.Cfg.RepoRoot)
	if err != nil {
		return err
	}

	w.log("[RUN ] watch post-merge %s for commit %s", strings.Join(required, "+"), s.MergeCommitSHA)
	for i := 0; i < w.Cfg.CIWatchMaxPolls; i++ {
		runs, err := w.GH.RunsForCommit(ctx, s.MergeCommitSHA)
		if err != nil {
			return err
		}
		outcome := EvaluatePostMergeRuns(required, runs)
		s.PostMergeRuns = outcome.Runs
		if err := s.Save(w.Cfg); err != nil {
			return err
		}
		if outcome.AllSuccess {
			w.log("[PASS] post-merge %s all green for commit %s", strings.Join(required, "+"), s.MergeCommitSHA)
			s.LastSuccessfulGate = fmt.Sprintf("post_merge (%s)", strings.Join(required, "+"))
			return s.Advance(StepComplete)
		}
		if len(outcome.Failed) > 0 {
			for _, f := range outcome.Failed {
				logText, logErr := w.GH.FailedCheckLog(ctx, f.RunID)
				if logErr == nil {
					if path, werr := WriteLog(w.Cfg, "post-merge", f.Workflow, logText); werr == nil {
						s.LogPaths = append(s.LogPaths, path)
					}
				}
			}
			s.Fail(fmt.Sprintf("required post-merge run(s) failed for merge commit %s: %s", s.MergeCommitSHA, postMergeFailedSummary(outcome.Failed)))
			w.log("[FAIL ] post-merge verification failed for commit %s", s.MergeCommitSHA)
			return nil
		}
		w.log("[WAIT] post-merge checks for %s still pending (%d/%d polls)", s.MergeCommitSHA, i+1, w.Cfg.CIWatchMaxPolls)
		if i < w.Cfg.CIWatchMaxPolls-1 {
			sleep(ctx, time.Duration(w.Cfg.CIWatchPollIntervalSeconds)*time.Second)
		}
	}
	w.log("[WAIT] post-merge checks still pending after this invocation's poll budget -- run 'resume' again later")
	return nil // stays at StepPostMerge, resumable; absence is never success
}

func postMergeFailedSummary(failed []PostMergeRun) string {
	var parts []string
	for _, f := range failed {
		parts = append(parts, fmt.Sprintf("%s(run=%s,conclusion=%s)", f.Workflow, f.RunID, f.Conclusion))
	}
	return strings.Join(parts, ", ")
}

func commitMessage(s *State) string {
	if s.Stage == StagePlanning {
		return fmt.Sprintf("docs: plan Phase %d\n", s.Phase)
	}
	return fmt.Sprintf("feat: implement Phase %d\n", s.Phase)
}

func prTitle(s *State) string {
	if s.Stage == StagePlanning {
		return fmt.Sprintf("docs: plan Phase %d", s.Phase)
	}
	return fmt.Sprintf("feat: implement Phase %d", s.Phase)
}

func prBody(s *State) string {
	if s.Stage == StagePlanning {
		return fmt.Sprintf("## Summary\n\nPre-implementation plan for Phase %d, produced and independently reviewed by TaskForge Autopilot per docs/autopilot.md.\n\nSee %s.\n", s.Phase, s.PlanPath)
	}
	return fmt.Sprintf("## Summary\n\nImplementation of Phase %d, produced and independently reviewed by TaskForge Autopilot per docs/autopilot.md.\n\nSee %s.\n", s.Phase, s.PlanPath)
}

// sleep is a var so tests can stub it out (no real waiting in unit tests).
var sleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
