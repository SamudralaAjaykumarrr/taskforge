package autopilot

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Exit codes. A caller scripting Autopilot can distinguish "healthy/done"
// from "needs a human" from "broken" without parsing text output.
const (
	ExitOK            = 0
	ExitError         = 1
	ExitUsage         = 2
	ExitDoctorFailed  = 3
	ExitAwaitingHuman = 4
)

// Run is the CLI entry point: parses argv and dispatches to a subcommand.
// It never calls os.Exit itself, so it is directly testable.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitUsage
	}
	cmd := args[0]
	rest := args[1:]

	repoRoot, err := resolveRepoRoot()
	if err != nil && cmd != "doctor" {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	cfg := DefaultConfig(repoRoot)

	switch cmd {
	case "doctor":
		return runDoctorCmd(cfg, stdout)
	case "dry-run":
		return runDryRunCmd(cfg, rest, stdout, stderr)
	case "status":
		return runStatusCmd(cfg, stdout, stderr)
	case "start":
		return runStartCmd(cfg, rest, stdout, stderr)
	case "resume":
		return runResumeCmd(cfg, stdout, stderr)
	case "approve":
		return runApproveCmd(cfg, stdout, stderr)
	case "pause":
		return runPauseCmd(cfg, rest, stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		printUsage(stderr)
		return ExitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `taskforge-autopilot -- bounded, resume-safe roadmap-phase automation

Usage:
  taskforge-autopilot status
  taskforge-autopilot start --phase N --stage planning|implementation [--force]
  taskforge-autopilot resume
  taskforge-autopilot approve
  taskforge-autopilot pause [--reason TEXT]
  taskforge-autopilot doctor
  taskforge-autopilot dry-run --phase N --stage planning|implementation

See docs/autopilot.md for the full design and safety model.
`)
}

func resolveRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(dir + "/.git"); err == nil {
			return dir, nil
		}
		parent := parentDir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repository root (no .git found above %s)", wd)
		}
		dir = parent
	}
}

func parentDir(dir string) string {
	idx := strings.LastIndex(dir, "/")
	if idx <= 0 {
		return "/"
	}
	return dir[:idx]
}

func newRunner() Runner { return ExecRunner{} }

func newGit(cfg Config) *Git       { return &Git{Runner: newRunner(), Dir: cfg.RepoRoot} }
func newGitHub(cfg Config) *GitHub { return &GitHub{Runner: newRunner(), Dir: cfg.RepoRoot} }
func newClaude(cfg Config) *Claude { return &Claude{Runner: newRunner(), Dir: cfg.RepoRoot} }

func runDoctorCmd(cfg Config, stdout io.Writer) int {
	ctx := context.Background()
	var git *Git
	var gh *GitHub
	if cfg.RepoRoot != "" {
		git = newGit(cfg)
	}
	if _, err := exec.LookPath("gh"); err == nil {
		gh = newGitHub(cfg)
	}
	claude := newClaude(cfg)
	checks := RunDoctor(ctx, cfg, git, gh, claude)
	for _, c := range checks {
		fmt.Fprintf(stdout, "[%s] %-40s %s\n", c.Severity, c.Name, c.Detail)
	}
	if DoctorFailed(checks) {
		return ExitDoctorFailed
	}
	return ExitOK
}

func runDryRunCmd(cfg Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	phase := fs.Int("phase", 0, "roadmap phase number")
	stage := fs.String("stage", "", "planning|implementation")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *phase <= 0 || *stage == "" {
		fmt.Fprintln(stderr, "dry-run requires --phase N and --stage planning|implementation")
		return ExitUsage
	}
	plan, err := BuildDryRunPlan(cfg.RepoRoot, *phase, Stage(*stage))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	fmt.Fprint(stdout, plan.Render())
	return ExitOK
}

func newWorkflow(cfg Config, stdout io.Writer) *Workflow {
	return &Workflow{Cfg: cfg, Git: newGit(cfg), GH: newGitHub(cfg), Claude: newClaude(cfg), Out: stdout}
}

func runStatusCmd(cfg Config, stdout, stderr io.Writer) int {
	w := newWorkflow(cfg, stdout)
	s, warnings, err := w.StatusReport(context.Background())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "no run in progress (never started, or already cleaned up)")
			return ExitOK
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	for _, warn := range warnings {
		fmt.Fprintf(stdout, "[WARN] %s\n", warn)
	}
	printState(stdout, s)
	if s.Status == StatusAwaitingApproval || s.Status == StatusPaused {
		return ExitAwaitingHuman
	}
	if s.Status == StatusFailed {
		return ExitError
	}
	return ExitOK
}

func printState(w io.Writer, s *State) {
	fmt.Fprintf(w, "phase:            %d\n", s.Phase)
	fmt.Fprintf(w, "stage:            %s\n", s.Stage)
	fmt.Fprintf(w, "step:             %s\n", s.Step)
	fmt.Fprintf(w, "status:           %s\n", s.Status)
	fmt.Fprintf(w, "branch:           %s (base: %s)\n", s.Branch, s.BaseBranch)
	if s.PRNumber != 0 {
		fmt.Fprintf(w, "pr:               #%d\n", s.PRNumber)
	}
	if s.MergeCommitSHA != "" {
		fmt.Fprintf(w, "merge_commit:     %s\n", s.MergeCommitSHA)
		for _, r := range s.PostMergeRuns {
			fmt.Fprintf(w, "  post_merge:     %-10s run=%-12s status=%-12s conclusion=%s\n", r.Workflow, orDash(r.RunID), orDash(r.Status), orDash(r.Conclusion))
		}
	}
	fmt.Fprintf(w, "review_count:     %d\n", s.ReviewCount)
	fmt.Fprintf(w, "recheck_count:    %d\n", s.RecheckCount)
	fmt.Fprintf(w, "ci_repair_count:  %d\n", s.CIRepairCount)
	if len(s.ApprovedHumanProofs) > 0 {
		fmt.Fprintf(w, "approved_proofs:  %s\n", strings.Join(s.ApprovedHumanProofs, ", "))
	}
	if s.LastSuccessfulGate != "" {
		fmt.Fprintf(w, "last_gate:        %s\n", s.LastSuccessfulGate)
	}
	if s.PausedReason != "" {
		fmt.Fprintf(w, "paused_reason:    %s\n", s.PausedReason)
		fmt.Fprintf(w, "paused_detail:    %s\n", s.PausedDetail)
	}
	if s.PendingHumanProof != "" {
		fmt.Fprintf(w, "pending_proof:    %s\n", s.PendingHumanProof)
	}
	fmt.Fprintf(w, "updated_at:       %s\n", s.UpdatedAt.Format("2006-01-02T15:04:05Z"))
}

func runStartCmd(cfg Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	phase := fs.Int("phase", 0, "roadmap phase number")
	stage := fs.String("stage", "", "planning|implementation")
	force := fs.Bool("force", false, "abandon an existing unfinished run")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *phase <= 0 || *stage == "" {
		fmt.Fprintln(stderr, "start requires --phase N and --stage planning|implementation")
		return ExitUsage
	}
	w := newWorkflow(cfg, stdout)
	if err := w.Start(context.Background(), *phase, Stage(*stage), *force); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	return finalExitCode(w, stdout, stderr)
}

func runResumeCmd(cfg Config, stdout, stderr io.Writer) int {
	w := newWorkflow(cfg, stdout)
	if err := w.Resume(context.Background()); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	return finalExitCode(w, stdout, stderr)
}

func runApproveCmd(cfg Config, stdout, stderr io.Writer) int {
	w := newWorkflow(cfg, stdout)
	if err := w.Approve(context.Background()); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	return finalExitCode(w, stdout, stderr)
}

func runPauseCmd(cfg Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "operator requested pause", "why this run is being paused")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	w := newWorkflow(cfg, stdout)
	if err := w.Pause(context.Background(), *reason); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitError
	}
	fmt.Fprintln(stdout, "paused")
	return ExitOK
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func finalExitCode(w *Workflow, stdout, stderr io.Writer) int {
	s, err := LoadState(w.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error re-reading state: %v\n", err)
		return ExitError
	}
	printState(stdout, s)
	switch s.Status {
	case StatusAwaitingApproval, StatusPaused:
		return ExitAwaitingHuman
	case StatusFailed:
		return ExitError
	default:
		return ExitOK
	}
}
