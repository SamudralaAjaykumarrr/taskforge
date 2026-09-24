package autopilot

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPhase_PlanAndADRFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "enterprise-roadmap.md"), "# Roadmap\n\n| 16 | Tracing & Operator Diagnostics |\n")
	writeFile(t, filepath.Join(dir, "docs", "phase-16-plan.md"), "See [ADR-0012](adr/0012-distributed-tracing-and-durable-trace-context.md) for the ratified design.\n")
	writeFile(t, filepath.Join(dir, "docs", "adr", "0012-distributed-tracing-and-durable-trace-context.md"), "# ADR-0012\n")

	docs, err := DiscoverPhase(dir, 16)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !docs.RoadmapExists || !docs.PlanExists {
		t.Fatalf("expected roadmap and plan to be found: %+v", docs)
	}
	if len(docs.ADRPaths) != 1 || docs.ADRPaths[0] != "docs/adr/0012-distributed-tracing-and-durable-trace-context.md" {
		t.Fatalf("expected exactly one discovered ADR, got %v", docs.ADRPaths)
	}
}

func TestDiscoverPhase_MissingPlan_IsNotAnError(t *testing.T) {
	dir := t.TempDir()
	docs, err := DiscoverPhase(dir, 17)
	if err != nil {
		t.Fatalf("a not-yet-planned phase must not be an error: %v", err)
	}
	if docs.PlanExists {
		t.Fatal("expected PlanExists=false")
	}
	if len(docs.ADRPaths) != 0 {
		t.Fatalf("expected no ADRs discovered, got %v", docs.ADRPaths)
	}
}

func TestDiscoverPhase_RejectsNonPositivePhase(t *testing.T) {
	if _, err := DiscoverPhase(t.TempDir(), 0); err == nil {
		t.Fatal("expected error for phase=0")
	}
	if _, err := DiscoverPhase(t.TempDir(), -1); err == nil {
		t.Fatal("expected error for negative phase")
	}
}

func TestDiscoverPhase_ADRReferenceThatDoesNotExistIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "phase-9-plan.md"), "See adr/0099-does-not-exist.md.\n")
	docs, err := DiscoverPhase(dir, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs.ADRPaths) != 0 {
		t.Fatalf("expected a nonexistent ADR reference to be skipped, got %v", docs.ADRPaths)
	}
}

func TestAuthoritativeDocs_FiltersToExistingFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "invariants.md"), "# Invariants\n")
	docs := AuthoritativeDocs(dir)
	if len(docs) != 1 || docs[0] != "docs/invariants.md" {
		t.Fatalf("expected only the existing doc to be returned, got %v", docs)
	}
}
