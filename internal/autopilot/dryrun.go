package autopilot

import (
	"fmt"
	"strings"
)

// DryRunPlan is the printable, exact-intended-workflow description dry-run
// produces. Building it touches only the local filesystem (os.ReadFile /
// os.Stat, via DiscoverPhase/AuthoritativeDocs) -- it deliberately never
// holds a Runner/Git/GitHub/Claude reference, so it is structurally
// incapable of creating a branch, invoking claude, committing, pushing,
// opening a PR, or merging. That guarantee is enforced by this function's
// own signature, not by a runtime flag a future change could flip.
type DryRunPlan struct {
	Phase      int
	Stage      Stage
	Branch     string
	BaseBranch string
	Docs       PhaseDocs
	FixedDocs  []string
	Steps      []DryRunStep

	// Manifest/ManifestErr describe this phase's proof manifest
	// (manifest.go), loaded read-only for display only -- dry-run shows
	// what a real run would require and enforce; it never executes a
	// single proof command. Only populated for an implementation-stage
	// dry-run against a phase whose plan already exists (a manifest binds
	// to a ratified plan's digest, so checking it against a not-yet-
	// written plan is meaningless).
	ManifestChecked bool
	Manifest        *PhaseManifest
	ManifestErr     error
}

type DryRunStep struct {
	Step        Step
	Description string
}

// BuildDryRunPlan resolves phase documents from disk and describes, step by
// step, exactly what "taskforge-autopilot start --phase N --stage S" would
// do -- without doing any of it.
func BuildDryRunPlan(repoRoot string, phase int, stage Stage) (DryRunPlan, error) {
	if !stage.Valid() {
		return DryRunPlan{}, fmt.Errorf("invalid stage %q", stage)
	}
	docs, err := DiscoverPhase(repoRoot, phase)
	if err != nil {
		return DryRunPlan{}, err
	}
	fixed := AuthoritativeDocs(repoRoot)

	var branch string
	switch stage {
	case StagePlanning:
		branch = fmt.Sprintf("phase-%d-planning", phase)
	case StageImplementation:
		branch = fmt.Sprintf("phase-%d-implementation", phase)
	}

	plan := DryRunPlan{
		Phase:      phase,
		Stage:      stage,
		Branch:     branch,
		BaseBranch: "main",
		Docs:       docs,
		FixedDocs:  fixed,
	}

	if stage == StageImplementation && docs.PlanExists {
		plan.ManifestChecked = true
		plan.Manifest, plan.ManifestErr = LoadManifest(repoRoot, phase, docs.PlanPath)
	}

	draftDesc := fmt.Sprintf("invoke claude (role=implementer) to draft %s, referencing %s and %d authoritative ADR(s)", docs.PlanPath, docs.PlanPath, len(docs.ADRPaths))
	if stage == StageImplementation {
		draftDesc = fmt.Sprintf("invoke claude (role=implementer) to implement Phase %d per %s, referencing %d authoritative ADR(s)", phase, docs.PlanPath, len(docs.ADRPaths))
	}

	plan.Steps = []DryRunStep{
		{StepBranch, fmt.Sprintf("create branch %q from %q (refuses if tree is dirty or name is protected)", branch, plan.BaseBranch)},
		{StepDraft, draftDesc},
		{StepReview, "invoke claude (role=reviewer) in a FRESH session (no --resume/--continue) for exactly one independent review; parse its AUTOPILOT_RESULT_BEGIN/END verdict, failing closed if absent/malformed"},
		{StepFixBlocker, "ONLY if the review found a BLOCKER_TYPE=ordinary blocker: one bounded, focused fix prompt limited to that finding. Any other BLOCKER_TYPE (architecture/migration/security/invariant/proof_weakening) pauses immediately for human approval instead"},
		{StepRecheck, "ONLY after a fix: one focused re-check of that specific blocker, never a repeated full review"},
		{StepLocalGates, "run gofmt, go vet, go build, go test -p 1, go test -race -p 1, go mod verify, git diff --check, PLUS this phase's checked-in proof manifest's required command proofs, stopping at the first failure; any required human proof not yet explicitly approved pauses here instead"},
		{StepCommit, "git commit (message from a file, not an inline -m string)"},
		{StepPush, "git push --set-upstream origin " + branch + " (never --force)"},
		{StepPRCreate, "gh pr create --body-file <generated file> --base " + plan.BaseBranch + " --head " + branch},
		{StepCIWatch, "poll gh pr checks (bounded: a fixed number of polls at a fixed interval per invocation, never an unbounded/background loop) until required checks conclude or the poll budget is exhausted"},
		{StepCIRepair, fmt.Sprintf("on failure: collect real failed-check logs via gh, generate one evidence-based repair prompt, rerun local gates, push again -- bounded to a fixed number of attempts, then pause (PauseCIRepairExhausted)")},
		{StepMergeApproval, "PAUSE: require explicit human confirmation before merge, even if every required check is already green (V1 default; see docs/autopilot.md)"},
		{StepMerge, "gh pr merge --merge (ordinary merge workflow; never --admin, never with a failing required check)"},
		{StepPostMerge, "resolve the REAL gh-reported merge commit SHA for the PR; checkout+fast-forward main; verify main actually contains that commit; discover and bounded-poll the real GitHub Actions runs for CI+CodeQL against that exact commit (never the PR's pre-merge checks); on failure, capture logs and fail closed; on persistent pending, stay resumable at this step"},
		{StepComplete, "phase stage marked complete only after post-merge required checks pass"},
	}
	return plan, nil
}

// Render formats the plan as the concise, structured status lines
// docs/autopilot.md documents.
func (p DryRunPlan) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[DRY-RUN] Phase %d, stage=%s\n", p.Phase, p.Stage)
	fmt.Fprintf(&b, "[DRY-RUN] branch: %s (base: %s)\n", p.Branch, p.BaseBranch)
	if p.Docs.PlanExists {
		fmt.Fprintf(&b, "[DRY-RUN] plan doc: %s (found)\n", p.Docs.PlanPath)
	} else {
		fmt.Fprintf(&b, "[DRY-RUN] plan doc: %s (does not exist yet -- this stage would create it)\n", p.Docs.PlanPath)
	}
	for _, adr := range p.Docs.ADRPaths {
		fmt.Fprintf(&b, "[DRY-RUN] discovered ADR: %s\n", adr)
	}
	for _, d := range p.FixedDocs {
		fmt.Fprintf(&b, "[DRY-RUN] authoritative doc: %s\n", d)
	}
	switch {
	case !p.ManifestChecked:
		fmt.Fprintf(&b, "[DRY-RUN] phase proof manifest: not checked (planning stage, or plan doc not yet drafted)\n")
	case p.ManifestErr != nil:
		fmt.Fprintf(&b, "[DRY-RUN] phase proof manifest: INVALID -- %v\n", p.ManifestErr)
		fmt.Fprintf(&b, "[DRY-RUN]   a real run's local_gates step would fail closed here, before ever committing/pushing\n")
	default:
		fmt.Fprintf(&b, "[DRY-RUN] phase proof manifest: %s (plan_sha256 verified against %s)\n", ManifestPath("", p.Phase), p.Docs.PlanPath)
		if len(p.Manifest.Proofs) == 0 {
			fmt.Fprintf(&b, "[DRY-RUN]   no phase-specific proofs beyond the fixed core gates (explicitly reviewed: no_extra_proofs_reviewed=true)\n")
		}
		for _, proof := range p.Manifest.Proofs {
			req := "optional"
			if proof.Required {
				req = "required"
			}
			switch proof.Type {
			case ProofCommand:
				fmt.Fprintf(&b, "[DRY-RUN]   proof (%s, command, NOT executed): %s -- %s\n", req, proof.Name, strings.Join(proof.Command, " "))
			case ProofHuman:
				fmt.Fprintf(&b, "[DRY-RUN]   proof (%s, human gate): %s -- %s\n", req, proof.Name, proof.Description)
			}
		}
	}
	b.WriteString("[DRY-RUN] intended workflow (nothing below is executed):\n")
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "  %2d. [%s] %s\n", i+1, s.Step, s.Description)
	}
	b.WriteString("[DRY-RUN] zero repository/GitHub/Claude mutations performed.\n")
	return b.String()
}
