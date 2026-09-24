package autopilot

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_NoArgs_UsageError(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Run(nil, &out, &errBuf)
	if code != ExitUsage {
		t.Fatalf("expected ExitUsage, got %d", code)
	}
}

func TestRun_UnknownCommand_UsageError(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Run([]string{"bogus"}, &out, &errBuf)
	if code != ExitUsage {
		t.Fatalf("expected ExitUsage, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Fatalf("expected an unknown-command message, got %q", errBuf.String())
	}
}

func TestRun_Help_PrintsUsageAndOK(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Run([]string{"--help"}, &out, &errBuf)
	if code != ExitOK {
		t.Fatalf("expected ExitOK, got %d", code)
	}
	if !strings.Contains(out.String(), "taskforge-autopilot") {
		t.Fatalf("expected usage text, got %q", out.String())
	}
}

func TestRun_DryRun_MissingFlags_UsageError(t *testing.T) {
	dir := t.TempDir()
	if err := initGitRepo(t, dir); err != nil {
		t.Fatal(err)
	}
	restoreWD := chdir(t, dir)
	defer restoreWD()

	var out, errBuf bytes.Buffer
	code := Run([]string{"dry-run"}, &out, &errBuf)
	if code != ExitUsage {
		t.Fatalf("expected ExitUsage for a dry-run with no --phase/--stage, got %d: %s", code, errBuf.String())
	}
}

func TestRun_DryRun_Phase16Implementation_ZeroMutation(t *testing.T) {
	dir := t.TempDir()
	if err := initGitRepo(t, dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/docs/phase-16-plan.md", "see adr/0012-distributed-tracing-and-durable-trace-context.md\n")
	writeFile(t, dir+"/docs/adr/0012-distributed-tracing-and-durable-trace-context.md", "# ADR-0012\n")
	restoreWD := chdir(t, dir)
	defer restoreWD()

	var out, errBuf bytes.Buffer
	code := Run([]string{"dry-run", "--phase", "16", "--stage", "implementation"}, &out, &errBuf)
	if code != ExitOK {
		t.Fatalf("expected ExitOK, got %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "docs/adr/0012-distributed-tracing-and-durable-trace-context.md") {
		t.Fatalf("expected the discovered ADR in dry-run output, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "zero repository/GitHub/Claude mutations performed") {
		t.Fatalf("expected the zero-mutation guarantee line, got:\n%s", out.String())
	}
	if fileExists(dir + "/.taskforge-autopilot") {
		t.Fatal("dry-run must never create .taskforge-autopilot/")
	}
}

func TestRun_Status_NoRunInProgress(t *testing.T) {
	dir := t.TempDir()
	if err := initGitRepo(t, dir); err != nil {
		t.Fatal(err)
	}
	restoreWD := chdir(t, dir)
	defer restoreWD()

	var out, errBuf bytes.Buffer
	code := Run([]string{"status"}, &out, &errBuf)
	if code != ExitOK {
		t.Fatalf("expected ExitOK, got %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "no run in progress") {
		t.Fatalf("expected a 'no run in progress' message, got %q", out.String())
	}
}
