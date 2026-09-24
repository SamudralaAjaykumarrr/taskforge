package autopilot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return DefaultConfig(dir)
}

func TestState_SaveLoadRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	s := NewState(16, StageImplementation, "phase-16-implementation", "main")
	s.PRNumber = 42
	s.CIRunIDs = []string{"111", "222"}
	s.ReviewCount = 1
	s.LastSuccessfulGate = "local_gates"

	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadState(cfg)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Phase != 16 || loaded.Stage != StageImplementation || loaded.PRNumber != 42 {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	if len(loaded.CIRunIDs) != 2 || loaded.CIRunIDs[0] != "111" {
		t.Fatalf("CIRunIDs did not round-trip: %+v", loaded.CIRunIDs)
	}
	if loaded.SchemaVersion != StateSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", StateSchemaVersion, loaded.SchemaVersion)
	}
}

func TestState_LoadMissing_ReturnsNotExist(t *testing.T) {
	cfg := testConfig(t)
	_, err := LoadState(cfg)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestState_LoadMalformedJSON_Fails(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(cfg.RepoRoot, cfg.StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(cfg), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(cfg); err == nil {
		t.Fatal("expected error loading malformed state file, got nil")
	}
}

func TestState_LoadNewerSchemaVersion_RefusesToGuess(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(cfg.RepoRoot, cfg.StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a state file written by a future, incompatible binary.
	future := `{"schema_version": 999, "phase": 1, "stage": "planning", "step": "not_started", "status": "running"}`
	if err := os.WriteFile(StatePath(cfg), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(cfg); err == nil {
		t.Fatal("expected LoadState to refuse a newer schema_version, got nil error")
	}
}

func TestState_Save_IsAtomic_NoTempFilesLeftBehind(t *testing.T) {
	cfg := testConfig(t)
	s := NewState(1, StagePlanning, "phase-1-planning", "main")
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.RepoRoot, cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("unexpected leftover file after Save: %s", e.Name())
		}
	}
}

func TestState_IncrementReview_EnforcesBound(t *testing.T) {
	s := NewState(1, StagePlanning, "b", "main")
	if err := s.IncrementReview(1); err != nil {
		t.Fatalf("first review should be allowed: %v", err)
	}
	if err := s.IncrementReview(1); err == nil {
		t.Fatal("second review should be rejected by the bound")
	}
}

func TestState_IncrementRecheck_EnforcesBound(t *testing.T) {
	s := NewState(1, StagePlanning, "b", "main")
	if err := s.IncrementRecheck(1); err != nil {
		t.Fatalf("first recheck should be allowed: %v", err)
	}
	if err := s.IncrementRecheck(1); err == nil {
		t.Fatal("second recheck should be rejected by the bound")
	}
}

func TestState_IncrementCIRepair_EnforcesBound(t *testing.T) {
	s := NewState(1, StagePlanning, "b", "main")
	for i := 0; i < 3; i++ {
		if err := s.IncrementCIRepair(3); err != nil {
			t.Fatalf("attempt %d should be allowed: %v", i, err)
		}
	}
	if err := s.IncrementCIRepair(3); err == nil {
		t.Fatal("4th CI repair attempt should be rejected by the bound")
	}
}

func TestState_Advance_RejectsInvalidTransition(t *testing.T) {
	s := NewState(1, StagePlanning, "b", "main")
	s.Step = StepBranch
	if err := s.Advance(StepMerge); err == nil {
		t.Fatal("expected Branch -> Merge to be rejected")
	}
	if s.Step != StepBranch {
		t.Fatalf("state must not change on a rejected transition, got step=%s", s.Step)
	}
}

func TestState_PauseAndFail_SetStatusAndReason(t *testing.T) {
	s := NewState(1, StagePlanning, "b", "main")
	s.Pause(PauseMergeConfirmation, "waiting on a human")
	if s.Status != StatusAwaitingApproval || s.PausedReason != PauseMergeConfirmation {
		t.Fatalf("Pause did not set expected fields: %+v", s)
	}
	s2 := NewState(1, StagePlanning, "b", "main")
	s2.Fail("boom")
	if s2.Status != StatusFailed || s2.PausedDetail != "boom" {
		t.Fatalf("Fail did not set expected fields: %+v", s2)
	}
}
