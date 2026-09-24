package autopilot

import (
	"context"
	"fmt"
	"strings"
)

// GateResult is the outcome of one local quality gate.
type GateResult struct {
	Name    string
	Passed  bool
	Output  string
	LogPath string
}

// gateSpec is one command in the fixed local-gate sequence, mirroring the
// repository's own Makefile/CI steps exactly (see docs/autopilot.md
// "Quality Gates") -- Autopilot does not invent extra gates or reorder
// these.
type gateSpec struct {
	Name string
	Args []string // argv[0] is the program name, resolved against Dir
}

var coreGates = []gateSpec{
	{"gofmt", []string{"gofmt", "-l", "."}},
	{"go vet", []string{"go", "vet", "./..."}},
	{"go build", []string{"go", "build", "./..."}},
	{"go test", []string{"go", "test", "-p", "1", "./..."}},
	{"go test -race", []string{"go", "test", "-race", "-p", "1", "./..."}},
	{"go mod verify", []string{"go", "mod", "verify"}},
	{"git diff --check", []string{"git", "diff", "--check"}},
}

// RunLocalGates runs the fixed core gate sequence plus any phase-specific
// proof commands, stopping at the first failure (a later gate is not run
// against a tree already known to be broken by an earlier one). Every
// gate's full output is captured to LogsDir; only failures are echoed to
// out unless cfg.Verbose is set.
func RunLocalGates(ctx context.Context, cfg Config, runner Runner, extra []gateSpec, out func(string)) ([]GateResult, error) {
	var results []GateResult
	all := append(append([]gateSpec{}, coreGates...), extra...)
	for _, g := range all {
		out(fmt.Sprintf("[RUN ] %s", g.Name))
		res, err := runner.Run(ctx, cfg.RepoRoot, g.Args[0], g.Args[1:]...)
		combined := res.Stdout + res.Stderr
		passed := err == nil
		// gofmt -l prints non-empty output (a list of unformatted files)
		// on failure even with a zero exit code -- treat non-empty output
		// as failure, matching Makefile's fmt-check target exactly.
		if g.Name == "gofmt" && strings.TrimSpace(res.Stdout) != "" {
			passed = false
		}
		logPath, logErr := WriteLog(cfg, "gates", g.Name, combined)
		gr := GateResult{Name: g.Name, Passed: passed, Output: combined, LogPath: logPath}
		results = append(results, gr)
		if logErr != nil {
			out(fmt.Sprintf("[WARN] could not write gate log for %s: %v", g.Name, logErr))
		}
		if !passed {
			out(fmt.Sprintf("[FAIL] %s", g.Name))
			if cfg.Verbose {
				out(combined)
			}
			return results, fmt.Errorf("local gate %q failed (see %s)", g.Name, logPath)
		}
		out(fmt.Sprintf("[PASS] %s", g.Name))
		if cfg.Verbose {
			out(combined)
		}
	}
	return results, nil
}
