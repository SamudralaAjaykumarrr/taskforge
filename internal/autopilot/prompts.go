package autopilot

import (
	"fmt"
	"strings"
)

// PromptInput bundles what a prompt template needs. Prompt generation never
// duplicates a phase's scope/proof-obligations text into Go source (see the
// spec's "CLAUDE INTEGRATION" section) -- it points Claude at the
// authoritative repository documents and tells it to treat them as binding.
type PromptInput struct {
	Phase  int
	Stage  Stage
	Docs   PhaseDocs
	Fixed  []string // AuthoritativeDocs(repoRoot)
	Branch string
	// Manifest is this phase's checked-in proof manifest (manifest.go),
	// when one could be loaded -- included so the implementer/reviewer
	// know up front exactly which phase-specific proof obligations
	// local_gates will enforce, rather than discovering it only after
	// local_gates runs. Left nil if no manifest exists yet or it failed to
	// load; prompt generation itself never fails over that (doLocalGates
	// is the sole enforcement point -- see workflow.go).
	Manifest *PhaseManifest
}

const resultContractInstructions = `
Before finishing, print exactly one machine-readable result block, as the
very last thing in your output, in exactly this shape (no extra
whitespace inside it, no additional keys unless specified below):

AUTOPILOT_RESULT_BEGIN
VALIDATION=PASS|FAIL
READY_FOR_REVIEW=true|false
BLOCKER_COUNT=<integer>
BLOCKERS=<semicolon-separated one-line summaries, empty if BLOCKER_COUNT=0>
AUTOPILOT_RESULT_END

Do not omit this block. Do not print it more than once. Do not wrap it in a
code fence.`

const reviewContractInstructions = `
Before finishing, print exactly one machine-readable result block, as the
very last thing in your output, in exactly this shape:

AUTOPILOT_RESULT_BEGIN
VERDICT=APPROVE|BLOCKED
READY=true|false
BLOCKER_COUNT=<integer>
BLOCKER_TYPE=ordinary|architecture|migration|security|invariant|proof_weakening
BLOCKERS=<semicolon-separated one-line summaries, empty if VERDICT=APPROVE>
AUTOPILOT_RESULT_END

VERDICT=APPROVE requires BLOCKER_COUNT=0 and READY=true. VERDICT=BLOCKED
requires BLOCKER_COUNT>=1 and a BLOCKER_TYPE classifying the single most
severe blocker found:
  - architecture:     a new architectural decision not already resolved by
                       an existing ADR or authoritative doc
  - migration:        a destructive/non-additive schema migration
  - security:         a security/trust-boundary change not already approved
  - invariant:         a change to an existing TF-INV-* invariant
  - proof_weakening:  weakening or removing a test or proof obligation
  - ordinary:         any other genuine blocker (a bug, a missing test, a
                       scope deviation) that a bounded, focused fix can
                       plausibly resolve without a human decision
Only use BLOCKER_TYPE values other than "ordinary" for something that
actually requires a human decision this reviewer cannot make -- do not
default to "ordinary" to avoid escalating, and do not over-escalate a
routine implementation defect to avoid doing the work of describing it
precisely.
Do not wrap the block in a code fence. Do not print it more than once.`

// manifestSummary renders this phase's checked-in proof manifest, when one
// was loaded, so Claude knows up front exactly which phase-specific proof
// obligations local_gates will enforce (see manifest.go / doLocalGates),
// rather than discovering it only after local_gates runs. A nil Manifest
// (no manifest yet, or one that failed to load) renders nothing -- prompt
// generation is never the enforcement point for this.
func manifestSummary(m *PhaseManifest) string {
	if m == nil || len(m.Proofs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nThis phase's checked-in proof manifest additionally requires (beyond gofmt/vet/build/test/test-race/mod-verify/diff-check):\n\n")
	for _, p := range m.Proofs {
		if !p.Required {
			continue
		}
		switch p.Type {
		case ProofCommand:
			fmt.Fprintf(&b, "- %s (automated): `%s` must pass\n", p.Name, strings.Join(p.Command, " "))
		case ProofHuman:
			fmt.Fprintf(&b, "- %s (human-approved, not automatable): %s\n", p.Name, p.Description)
		}
	}
	return b.String()
}

func docList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

func noOrchestrationInstructions() string {
	return `
You do not control git or GitHub in this session. Do not run "git add",
"git commit", "git push", "gh pr create", "gh pr merge", or any other
staging/commit/push/PR/merge action. The orchestrator (Autopilot) performs
every one of those steps explicitly, after this session ends, and only
after independent review and local quality gates pass. Edit files in the
working tree as needed; leave them uncommitted.`
}

// GenerateImplementationPrompt builds the compact prompt for the
// implementer/planning-drafter role, referencing authoritative documents
// rather than duplicating their content.
func GenerateImplementationPrompt(in PromptInput) string {
	var b strings.Builder
	if in.Stage == StagePlanning {
		fmt.Fprintf(&b, "You are producing the pre-implementation plan for Phase %d of the TaskForge enterprise roadmap, as docs/phase-%d-plan.md, following the exact discipline of docs/phase-12-plan.md through docs/phase-15-plan.md (already in this repository -- read at least one for the expected shape and rigor before writing).\n\n", in.Phase, in.Phase)
	} else {
		fmt.Fprintf(&b, "You are implementing Phase %d of the TaskForge enterprise roadmap on branch %s.\n\n", in.Phase, in.Branch)
	}

	b.WriteString("Treat the following repository documents as authoritative. Where they disagree with anything else (including your own prior assumptions), they win:\n\n")
	if in.Docs.PlanExists {
		fmt.Fprintf(&b, "- %s (this phase's own ratified plan -- the primary scope document)\n", in.Docs.PlanPath)
	} else {
		fmt.Fprintf(&b, "- %s does not exist yet -- produce it as this task's own output; do not invent scope beyond docs/enterprise-roadmap.md's Phase %d section\n", in.Docs.PlanPath, in.Phase)
	}
	for _, adr := range in.Docs.ADRPaths {
		fmt.Fprintf(&b, "- %s\n", adr)
	}
	b.WriteString(docList(in.Fixed))
	b.WriteString(manifestSummary(in.Manifest))

	b.WriteString(`
Requirements, non-negotiable:
- Do not deviate from this phase's scope as defined by the documents above.
  Do not implement, plan, or scope-creep into a later phase.
- Every proof obligation / acceptance criterion those documents state for
  this phase must be met, with a real, passing, executable test -- not
  described as future work.
- Do not weaken, remove, or skip any existing test, invariant, or proof
  obligation to make this phase's own work pass.
- Follow this repository's existing engineering conventions exactly (see
  Makefile, .github/workflows/ci.yml, and the existing internal/ package
  layout) -- do not introduce a new build system, linter config, or
  directory convention.
- If you encounter a genuine, unresolved architectural decision, a
  destructive/non-additive migration, a security/trust-boundary change, an
  invariant change, or a reason you believe a test/proof obligation should
  be weakened: STOP and report it as a blocker in your result block rather
  than deciding it yourself.
`)
	b.WriteString(noOrchestrationInstructions())
	b.WriteString("\n")
	b.WriteString(resultContractInstructions)
	return b.String()
}

// GenerateReviewPrompt builds the compact prompt for the independent
// reviewer role. The caller (Claude wrapper) must invoke this in a fresh
// session -- this prompt does not, and cannot, carry the implementer
// session's context.
func GenerateReviewPrompt(in PromptInput) string {
	var b strings.Builder
	if in.Stage == StagePlanning {
		fmt.Fprintf(&b, "You are independently reviewing the just-drafted plan %s for Phase %d of the TaskForge enterprise roadmap. You did not write it and have no memory of writing it -- review it exactly as a second, independent engineer would.\n\n", in.Docs.PlanPath, in.Phase)
	} else {
		fmt.Fprintf(&b, "You are independently reviewing the just-completed implementation of Phase %d of the TaskForge enterprise roadmap, on branch %s. You did not write it and have no memory of writing it -- review it exactly as a second, independent engineer would.\n\n", in.Phase, in.Branch)
	}
	b.WriteString("Verify the work against these authoritative documents (not against your own judgment of what would be nice to have):\n\n")
	if in.Docs.PlanExists {
		fmt.Fprintf(&b, "- %s\n", in.Docs.PlanPath)
	}
	for _, adr := range in.Docs.ADRPaths {
		fmt.Fprintf(&b, "- %s\n", adr)
	}
	b.WriteString(docList(in.Fixed))
	b.WriteString(manifestSummary(in.Manifest))
	b.WriteString(`
Check specifically:
- Every proof obligation / acceptance criterion the plan/roadmap states for
  this phase is actually met, with a real passing test -- not merely
  claimed.
- No existing test, invariant, or proof obligation was weakened, removed,
  or skipped.
- No scope creep into a later phase, and no silent narrowing of this
  phase's own required scope.
- Existing engineering conventions (Makefile targets, CI gates, package
  layout) were followed, not reinvented.
- Any genuine architectural/security/invariant/migration decision was
  either already resolved by an existing ADR/doc, or is correctly flagged
  as a blocker rather than decided silently.
`)
	b.WriteString(noOrchestrationInstructions())
	b.WriteString("\n")
	b.WriteString(reviewContractInstructions)
	return b.String()
}

// GenerateFixPrompt builds a focused correction prompt limited to exactly
// one previously-identified blocker -- never a request to redo the whole
// review or restart planning/implementation from scratch.
func GenerateFixPrompt(in PromptInput, blockerText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent review of Phase %d found exactly one blocker that requires a bounded, focused fix (not a broader rewrite). Fix only this:\n\n%s\n\n", in.Phase, blockerText)
	b.WriteString("Do not make any other change. Do not re-scope, refactor, or \"improve\" anything the reviewer did not flag. Re-read the specific authoritative document(s) the blocker concerns before fixing it:\n\n")
	if in.Docs.PlanExists {
		fmt.Fprintf(&b, "- %s\n", in.Docs.PlanPath)
	}
	for _, adr := range in.Docs.ADRPaths {
		fmt.Fprintf(&b, "- %s\n", adr)
	}
	b.WriteString(noOrchestrationInstructions())
	b.WriteString("\n")
	b.WriteString(resultContractInstructions)
	return b.String()
}

// GenerateCIRepairPrompt builds a focused correction prompt limited to a
// specific, evidence-backed CI failure -- never a speculative "make CI
// green" instruction.
func GenerateCIRepairPrompt(in PromptInput, failedCheckNames []string, evidence string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI failed on Phase %d's PR for the following required check(s): %s.\n\nHere is the actual failure evidence (captured log output) -- do not guess at the cause, and do not assume a failure is unrelated to this change without evidence in this log:\n\n%s\n\n", in.Phase, strings.Join(failedCheckNames, ", "), evidence)
	b.WriteString(`
Fix only the root cause this evidence supports. Do not weaken, remove, or
skip a test to make CI pass. If the evidence does not clearly support a
specific root cause, or if fixing it would require an architectural,
security, invariant, or migration decision, report that as a blocker
instead of guessing.
`)
	b.WriteString(noOrchestrationInstructions())
	b.WriteString("\n")
	b.WriteString(resultContractInstructions)
	return b.String()
}
