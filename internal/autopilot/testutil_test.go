package autopilot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// initGitRepo creates a minimal real git repository at dir, so cli_test.go
// can exercise Run() against a real repository root (resolveRepoRoot walks
// up looking for .git).
func initGitRepo(t *testing.T, dir string) error {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main", dir},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return nil
}

// chdir switches the process working directory to dir and returns a func
// that restores it. Tests using this must not run in parallel with each
// other (none in this package call t.Parallel()).
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(orig) }
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeEmptyReviewedManifest writes a minimal, valid, explicitly-
// "no extra proofs" phase manifest -- for tests that need doLocalGates'
// manifest check to pass without exercising manifest content itself.
func writeEmptyReviewedManifest(t *testing.T, repoRoot string, phase int, planPath string) {
	t.Helper()
	digest, err := ComputeFileDigestSHA256(repoRoot + "/" + planPath)
	if err != nil {
		t.Fatal(err)
	}
	writeManifest(t, repoRoot, phase, fmt.Sprintf(`{"phase":%d,"plan_path":%q,"plan_sha256":%q,"no_extra_proofs_reviewed":true,"proofs":[]}`, phase, planPath, digest))
}

// writeRequiredWorkflowFiles writes minimal .github/workflows/ci.yml and
// codeql.yml files under repoRoot, so RequiredPostMergeWorkflows (which
// reads real files, not a Runner) has something to discover in tests that
// exercise doPostMerge.
func writeRequiredWorkflowFiles(t *testing.T, repoRoot string) {
	t.Helper()
	writeFile(t, repoRoot+"/.github/workflows/ci.yml", "name: CI\non: push\n")
	writeFile(t, repoRoot+"/.github/workflows/codeql.yml", "name: CodeQL\non: push\n")
}

// FakeCall records one Runner.Run invocation, for assertions in tests.
type FakeCall struct {
	Dir  string
	Name string
	Args []string
}

func (c FakeCall) String() string {
	return fmt.Sprintf("%s %s (dir=%s)", c.Name, strings.Join(c.Args, " "), c.Dir)
}

// FakeHandler matches a call and returns its scripted result. The bool
// return reports whether this handler matched at all.
type FakeHandler func(c FakeCall) (Result, error, bool)

// FakeRunner is the Runner test double every unit test in this package
// uses instead of touching real git/gh/claude. By default an unmatched
// call is a test failure (FailUnmatched=true) -- this is what lets a test
// assert "zero mutating calls happened" simply by not registering a
// handler for them.
type FakeRunner struct {
	mu            sync.Mutex
	Calls         []FakeCall
	Handlers      []FakeHandler
	FailUnmatched bool
}

func NewFakeRunner() *FakeRunner {
	return &FakeRunner{FailUnmatched: true}
}

func (f *FakeRunner) Run(ctx context.Context, dir string, name string, args ...string) (Result, error) {
	f.mu.Lock()
	call := FakeCall{Dir: dir, Name: name, Args: append([]string{}, args...)}
	f.Calls = append(f.Calls, call)
	handlers := append([]FakeHandler{}, f.Handlers...)
	failUnmatched := f.FailUnmatched
	f.mu.Unlock()

	for _, h := range handlers {
		if res, err, matched := h(call); matched {
			return res, err
		}
	}
	if failUnmatched {
		return Result{}, fmt.Errorf("FakeRunner: no handler registered for call: %s", call)
	}
	return Result{}, nil
}

// On registers a handler for calls to `name` whose leading args match
// argPrefix exactly (e.g. On("git", []string{"status"}, ...)).
func (f *FakeRunner) On(name string, argPrefix []string, fn func(c FakeCall) (Result, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Handlers = append(f.Handlers, func(c FakeCall) (Result, error, bool) {
		if c.Name != name || len(c.Args) < len(argPrefix) {
			return Result{}, nil, false
		}
		for i, p := range argPrefix {
			if c.Args[i] != p {
				return Result{}, nil, false
			}
		}
		res, err := fn(c)
		return res, err, true
	})
}

// OnSeq registers a handler like On, but returns each result in results in
// order across successive matching calls (the last result repeats once
// exhausted) -- used to script "pending, pending, pass" style sequences.
func (f *FakeRunner) OnSeq(name string, argPrefix []string, results []struct {
	Res Result
	Err error
}) {
	idx := 0
	var mu sync.Mutex
	f.On(name, argPrefix, func(c FakeCall) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		i := idx
		if i >= len(results) {
			i = len(results) - 1
		}
		idx++
		return results[i].Res, results[i].Err
	})
}

func (f *FakeRunner) CallsMatching(name string) []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeCall
	for _, c := range f.Calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *FakeRunner) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// resultOK is a small helper for building a passing Result inline.
func resultOK(stdout string) Result { return Result{Stdout: stdout} }
