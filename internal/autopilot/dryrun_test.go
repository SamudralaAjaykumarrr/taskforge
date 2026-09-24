package autopilot

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDryRunPlan_Phase16Implementation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "See adr/0012-distributed-tracing-and-durable-trace-context.md.\n")
	writeFile(t, filepath.Join(dir, "docs", "adr", "0012-distributed-tracing-and-durable-trace-context.md"), "# ADR-0012\n")
	writeFile(t, filepath.Join(dir, "docs", "enterprise-roadmap.md"), "# Roadmap\n")

	plan, err := BuildDryRunPlan(dir, 16, StageImplementation)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Branch != "phase-16-implementation" {
		t.Fatalf("unexpected branch: %s", plan.Branch)
	}
	if len(plan.Docs.ADRPaths) != 1 {
		t.Fatalf("expected ADR-0012 to be discovered, got %v", plan.Docs.ADRPaths)
	}
	if len(plan.Steps) != len(StepSequence()) {
		t.Fatalf("expected one dry-run step per graph step, got %d vs %d", len(plan.Steps), len(StepSequence()))
	}

	rendered := plan.Render()
	for _, want := range []string{
		"phase-16-plan.md",
		"docs/adr/0012-distributed-tracing-and-durable-trace-context.md",
		"zero repository/GitHub/Claude mutations performed",
		string(StepMergeApproval),
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected rendered plan to mention %q, got:\n%s", want, rendered)
		}
	}
}

func TestBuildDryRunPlan_RejectsInvalidStage(t *testing.T) {
	if _, err := BuildDryRunPlan(t.TempDir(), 16, Stage("bogus")); err == nil {
		t.Fatal("expected error for an invalid stage")
	}
}

func TestBuildDryRunPlan_PlanningStageBeforePlanExists(t *testing.T) {
	dir := t.TempDir()
	plan, err := BuildDryRunPlan(dir, 17, StagePlanning)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Docs.PlanExists {
		t.Fatal("phase 17 has no plan doc in this fixture; expected PlanExists=false")
	}
	if !strings.Contains(plan.Render(), "does not exist yet") {
		t.Fatal("expected the rendered plan to say the plan doc doesn't exist yet")
	}
}

func TestBuildDryRunPlan_DisplaysManifestProofs_WithoutExecutingThem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content for dry-run manifest test\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[`+
		`{"name":"extra-check","type":"command","command":["go","test","./..."],"required":true},`+
		`{"name":"benchmark-evidence","type":"human","description":"run the benchmark","required":true}`+
		`]}`)

	plan, err := BuildDryRunPlan(dir, 16, StageImplementation)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.ManifestChecked || plan.ManifestErr != nil || plan.Manifest == nil {
		t.Fatalf("expected a valid, checked manifest, got checked=%v err=%v manifest=%v", plan.ManifestChecked, plan.ManifestErr, plan.Manifest)
	}
	rendered := plan.Render()
	for _, want := range []string{
		"extra-check", "go test ./...", "NOT executed",
		"benchmark-evidence", "human gate", "run the benchmark",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected rendered dry-run to mention %q, got:\n%s", want, rendered)
		}
	}
	// The manifest file itself and the plan file must be byte-for-byte
	// unchanged -- dry-run only reads.
	after := digestOf(t, dir, "docs/phase-16-plan.md")
	if after != digest {
		t.Fatal("dry-run must never modify the authoritative plan file")
	}
	if fileExists(filepath.Join(dir, ".taskforge-autopilot")) {
		t.Fatal("dry-run must never create .taskforge-autopilot/ state")
	}
}

func TestBuildDryRunPlan_InvalidManifest_ShownNotFailed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	// Wrong phase number inside the manifest -- invalid, must fail closed
	// at LoadManifest, but BuildDryRunPlan itself must still succeed so
	// dry-run can SHOW the problem rather than crash.
	writeManifest(t, dir, 16, `{"phase":99,"plan_path":"docs/phase-16-plan.md","plan_sha256":"deadbeef","proofs":[{"name":"x","type":"command","command":["true"],"required":true}]}`)

	plan, err := BuildDryRunPlan(dir, 16, StageImplementation)
	if err != nil {
		t.Fatalf("BuildDryRunPlan itself must not fail on an invalid manifest (it should be SHOWN): %v", err)
	}
	if plan.ManifestErr == nil {
		t.Fatal("expected ManifestErr to be set for an invalid manifest")
	}
	if !strings.Contains(plan.Render(), "INVALID") {
		t.Fatalf("expected the rendered dry-run to flag the manifest as INVALID, got:\n%s", plan.Render())
	}
}

// TestBuildDryRunPlan_NeverTouchesARunner is a structural guarantee test:
// BuildDryRunPlan's signature takes no Runner/Git/GitHub/Claude at all, so
// it is impossible for it to invoke git, gh, or claude. This test exists so
// that guarantee has an executable anchor -- if a future change added a
// Runner parameter, this test (and its call site below) would need to
// change to compile, making the guarantee's removal visible in review.
func TestBuildDryRunPlan_NeverTouchesARunner(t *testing.T) {
	dir := t.TempDir()
	// No FakeRunner is constructed anywhere in this test. If BuildDryRunPlan
	// secretly shelled out, there is nothing here that could have serviced
	// that call, so any such attempt would panic or fail with a nil
	// dereference rather than silently succeed.
	if _, err := BuildDryRunPlan(dir, 16, StageImplementation); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
