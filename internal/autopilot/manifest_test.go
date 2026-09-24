package autopilot

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeManifest(t *testing.T, repoRoot string, phase int, content string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ManifestRelDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, strconv.Itoa(phase)+".json"), content)
}

func digestOf(t *testing.T, repoRoot, relPath string) string {
	t.Helper()
	d, err := ComputeFileDigestSHA256(filepath.Join(repoRoot, relPath))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// repoRootForTest walks up from the current test binary's working directory
// (always <repoRoot>/internal/autopilot under "go test") to find the real
// module root, so tests can validate the checked-in
// autopilot/phases/16.json against the real docs/phase-16-plan.md without
// hardcoding an absolute path.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repository root (go.mod) above %s", wd)
		}
		dir = parent
	}
}

func TestLoadManifest_Missing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for a missing manifest, got nil (must fail closed)")
	}
}

func TestLoadManifest_Malformed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	writeManifest(t, dir, 16, "{not json")
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
}

func TestLoadManifest_WrongPhase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":17,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"x","type":"command","command":["true"],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for a phase-number mismatch")
	}
}

func TestLoadManifest_WrongPlanPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/wrong-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"x","type":"command","command":["true"],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for a plan_path mismatch")
	}
}

func TestLoadManifest_PlanDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "original content\n")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"0000000000000000000000000000000000000000000000000000000000000","proofs":[{"name":"x","type":"command","command":["true"],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for a stale plan digest")
	}
}

func TestLoadManifest_PlanChangedAfterManifestAuthored_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "docs", "phase-16-plan.md")
	writeFile(t, planPath, "original content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"x","type":"command","command":["true"],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err != nil {
		t.Fatalf("manifest should be valid before the plan changes: %v", err)
	}
	// The plan is edited after the manifest was authored/reviewed.
	writeFile(t, planPath, "original content, but now amended\n")
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected LoadManifest to fail closed once the plan digest no longer matches")
	}
}

func TestLoadManifest_ValidCommandManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"extra-check","type":"command","command":["go","test","./..."],"required":true}]}`)
	m, err := LoadManifest(dir, 16, "docs/phase-16-plan.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Proofs) != 1 || m.Proofs[0].Name != "extra-check" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	extra := m.RequiredCommandProofs()
	if len(extra) != 1 || extra[0].Name != "phase-proof: extra-check" {
		t.Fatalf("unexpected RequiredCommandProofs: %+v", extra)
	}
}

func TestLoadManifest_HumanProofMissingDescription_Invalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"needs-human","type":"human","required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error: a human proof with no description is incomplete")
	}
}

func TestLoadManifest_CommandProofEmptyCommand_Invalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"empty-cmd","type":"command","command":[],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error: a command proof with an empty command is incomplete")
	}
}

func TestLoadManifest_UnknownProofType_Invalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"weird","type":"telepathic","required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for an unrecognized proof type")
	}
}

func TestLoadManifest_EmptyProofsWithoutExplicitReview_Invalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-17-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-17-plan.md")
	writeManifest(t, dir, 17, `{"phase":17,"plan_path":"docs/phase-17-plan.md","plan_sha256":"`+digest+`","proofs":[]}`)
	if _, err := LoadManifest(dir, 17, "docs/phase-17-plan.md"); err == nil {
		t.Fatal("expected an error: empty proofs without no_extra_proofs_reviewed=true must never silently mean success")
	}
}

func TestLoadManifest_EmptyProofsWithExplicitReview_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-17-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-17-plan.md")
	writeManifest(t, dir, 17, `{"phase":17,"plan_path":"docs/phase-17-plan.md","plan_sha256":"`+digest+`","no_extra_proofs_reviewed":true,"proofs":[]}`)
	m, err := LoadManifest(dir, 17, "docs/phase-17-plan.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.RequiredCommandProofs()) != 0 || len(m.RequiredHumanProofs()) != 0 {
		t.Fatal("expected zero required proofs")
	}
}

func TestLoadManifest_DuplicateProofNames_Invalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "plan content\n")
	digest := digestOf(t, dir, "docs/phase-16-plan.md")
	writeManifest(t, dir, 16, `{"phase":16,"plan_path":"docs/phase-16-plan.md","plan_sha256":"`+digest+`","proofs":[{"name":"dup","type":"command","command":["true"],"required":true},{"name":"dup","type":"command","command":["false"],"required":true}]}`)
	if _, err := LoadManifest(dir, 16, "docs/phase-16-plan.md"); err == nil {
		t.Fatal("expected an error for duplicate proof names")
	}
}

func TestPhase16Manifest_LoadsAgainstRealRepo(t *testing.T) {
	repoRoot := repoRootForTest(t)
	m, err := LoadManifest(repoRoot, 16, "docs/phase-16-plan.md")
	if err != nil {
		t.Fatalf("the checked-in autopilot/phases/16.json must load and validate against the real docs/phase-16-plan.md: %v", err)
	}
	if len(m.Proofs) == 0 {
		t.Fatal("expected at least the tracing-overhead-benchmark proof")
	}
	human := m.RequiredHumanProofs()
	if len(human) != 1 || human[0].Name != "tracing-overhead-benchmark" {
		t.Fatalf("unexpected required human proofs: %+v", human)
	}
}
