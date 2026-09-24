package autopilot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// claudeTestTimeout keeps capability-check tests fast; the production
// default (claudeDoctorTimeout) is generous for a real, possibly cold CLI
// start.
const claudeTestTimeout = 2 * time.Second

// claudeHelpFixture is a trimmed, realistic "claude --help" excerpt
// containing every flag requiredClaudeFlags checks for -- not the full help
// text, but real-shaped enough to prove the substring-match logic works.
const claudeHelpFixture = `Usage: claude [options] [command] [prompt]

Options:
  -p, --print                           Print response and exit
  --output-format <format>              Output format (only works with --print)
  --permission-mode <mode>              Permission mode to use for the session
  --no-session-persistence              Disable session persistence
`

func findCheck(checks []DoctorCheck, name string) (DoctorCheck, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return DoctorCheck{}, false
}

// withLookPath temporarily overrides the package-level lookPath seam and
// returns a restore func. Never run in parallel with another test using it
// (none in this package call t.Parallel()).
func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := lookPath
	lookPath = fn
	t.Cleanup(func() { lookPath = orig })
}

func TestClaude_CheckCapabilities_Compatible_Passes(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return resultOK("2.1.0 (Claude Code)\n"), nil
	})
	fr.On("claude", []string{"--help"}, func(c FakeCall) (Result, error) {
		return resultOK(claudeHelpFixture), nil
	})
	claude := &Claude{Runner: fr, Dir: "/repo"}
	check := claude.CheckCapabilities(context.Background(), claudeTestTimeout)
	if !check.OK() {
		t.Fatalf("expected a compatible CLI to pass, got %+v", check)
	}
	if check.Version != "2.1.0 (Claude Code)" {
		t.Fatalf("expected the version to be captured, got %q", check.Version)
	}
}

func TestClaude_CheckCapabilities_MissingRequiredFlag_Fails(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return resultOK("1.0.0-old\n"), nil
	})
	fr.On("claude", []string{"--help"}, func(c FakeCall) (Result, error) {
		// An old CLI that predates --no-session-persistence and
		// --permission-mode.
		return resultOK("Usage: claude [options]\n\n  -p, --print   Print response and exit\n  --output-format <format>\n"), nil
	})
	claude := &Claude{Runner: fr, Dir: "/repo"}
	check := claude.CheckCapabilities(context.Background(), claudeTestTimeout)
	if check.OK() {
		t.Fatal("expected an incompatible CLI to fail capability verification")
	}
	if len(check.MissingFlags) == 0 {
		t.Fatal("expected MissingFlags to be populated")
	}
	if !containsString(check.MissingFlags, "--no-session-persistence") {
		t.Fatalf("expected --no-session-persistence to be reported missing, got %v", check.MissingFlags)
	}
}

func TestClaude_CheckCapabilities_HelpCommandFails(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return resultOK("2.1.0\n"), nil
	})
	fr.On("claude", []string{"--help"}, func(c FakeCall) (Result, error) {
		return Result{}, &exitErrStub{}
	})
	claude := &Claude{Runner: fr, Dir: "/repo"}
	check := claude.CheckCapabilities(context.Background(), claudeTestTimeout)
	if check.OK() {
		t.Fatal("expected a failing --help invocation to fail capability verification")
	}
	if check.Err == nil {
		t.Fatal("expected Err to be set when --help itself fails")
	}
}

func TestClaude_CheckCapabilities_VersionCommandFails(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return Result{}, &exitErrStub{}
	})
	claude := &Claude{Runner: fr, Dir: "/repo"}
	check := claude.CheckCapabilities(context.Background(), claudeTestTimeout)
	if check.OK() {
		t.Fatal("expected a failing --version invocation to fail capability verification")
	}
	if check.Err == nil {
		t.Fatal("expected Err to be set when --version itself fails")
	}
}

func TestClaude_CheckCapabilities_Timeout_Fails(t *testing.T) {
	// Simulates what exec.CommandContext produces when the bounded timeout
	// CheckCapabilities applies (via context.WithTimeout) actually fires
	// against a hung/misbehaving binary: the Runner call fails with the
	// context's own deadline-exceeded error, not a hang.
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return Result{}, context.DeadlineExceeded
	})
	claude := &Claude{Runner: fr, Dir: "/repo"}
	check := claude.CheckCapabilities(context.Background(), 30*time.Millisecond)
	if check.OK() {
		t.Fatal("expected a timed-out --version invocation to fail capability verification")
	}
	if check.Err == nil {
		t.Fatal("expected Err to be set on timeout")
	}
}

func TestRunDoctor_ClaudeBinaryMissing_FailsClosed(t *testing.T) {
	withLookPath(t, func(name string) (string, error) {
		if name == "claude" {
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
		}
		return "/usr/bin/" + name, nil
	})
	cfg := testConfig(t)
	checks := RunDoctor(context.Background(), cfg, nil, nil, nil)

	claudeAvail, ok := findCheck(checks, "claude available")
	if !ok || claudeAvail.Severity != SeverityFail {
		t.Fatalf("expected 'claude available' to FAIL, got %+v (ok=%v)", claudeAvail, ok)
	}
	capability, ok := findCheck(checks, "claude CLI capability")
	if !ok || capability.Severity != SeverityFail {
		t.Fatalf("expected 'claude CLI capability' to FAIL when the binary is missing, got %+v (ok=%v)", capability, ok)
	}
	if !DoctorFailed(checks) {
		t.Fatal("expected DoctorFailed to report true when claude is missing")
	}
}

func TestRunDoctor_ClaudeCLIIncompatible_FailsNotWarn(t *testing.T) {
	withLookPath(t, func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return resultOK("0.1.0-ancient\n"), nil
	})
	fr.On("claude", []string{"--help"}, func(c FakeCall) (Result, error) {
		return resultOK("Usage: claude\n\n  --help   show help\n"), nil
	})
	cfg := testConfig(t)
	claude := &Claude{Runner: fr, Dir: cfg.RepoRoot}
	checks := RunDoctor(context.Background(), cfg, nil, nil, claude)

	capability, ok := findCheck(checks, "claude CLI capability")
	if !ok {
		t.Fatal("expected a 'claude CLI capability' check")
	}
	if capability.Severity != SeverityFail {
		t.Fatalf("an incompatible CLI must FAIL doctor, not merely WARN: got %s (%s)", capability.Severity, capability.Detail)
	}
	if !DoctorFailed(checks) {
		t.Fatal("expected DoctorFailed to report true for an incompatible claude CLI")
	}
}

func TestRunDoctor_ClaudeCLICompatible_PassesWithVersionReported(t *testing.T) {
	withLookPath(t, func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	fr := NewFakeRunner()
	fr.On("claude", []string{"--version"}, func(c FakeCall) (Result, error) {
		return resultOK("2.1.0 (Claude Code)\n"), nil
	})
	fr.On("claude", []string{"--help"}, func(c FakeCall) (Result, error) {
		return resultOK(claudeHelpFixture), nil
	})
	cfg := testConfig(t)
	claude := &Claude{Runner: fr, Dir: cfg.RepoRoot}
	checks := RunDoctor(context.Background(), cfg, nil, nil, claude)

	capability, ok := findCheck(checks, "claude CLI capability")
	if !ok || capability.Severity != SeverityPass {
		t.Fatalf("expected a compatible CLI to PASS, got %+v (ok=%v)", capability, ok)
	}
	if !strings.Contains(capability.Detail, "2.1.0") {
		t.Fatalf("expected the detected version in the detail, got %q", capability.Detail)
	}
}
