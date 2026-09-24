package autopilot

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Result is the outcome of running one external command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner abstracts process execution so git/gh/claude invocations can be
// faked in unit tests. Production code must never call os/exec directly --
// every external command goes through a Runner, exactly one choke point,
// so tests never depend on live git/GitHub/Claude.
type Runner interface {
	// Run executes name with args, in dir (relative paths resolved against
	// dir; empty dir means the current process's working directory), and
	// returns its captured output. A non-zero exit code is reported via
	// Result.ExitCode and a non-nil error (exec's own *exec.ExitError
	// shape), never silently swallowed.
	Run(ctx context.Context, dir string, name string, args ...string) (Result, error)
}

// ExecRunner is the real Runner, backed by os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
		}
		return res, fmt.Errorf("%s %v: %w", name, args, err)
	}
	return res, nil
}
