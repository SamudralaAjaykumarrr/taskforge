package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GitHub wraps a Runner with the "gh"-CLI operations Autopilot needs. Every
// mutating call here is additive (create a PR, merge one that is already
// green) -- there is no method that force-merges, uses "gh --admin", or
// bypasses branch protection; see docs/autopilot.md "Safety Model."
type GitHub struct {
	Runner Runner
	Dir    string
}

func (g *GitHub) run(ctx context.Context, args ...string) (Result, error) {
	return g.Runner.Run(ctx, g.Dir, "gh", args...)
}

// AuthStatus reports whether gh is authenticated.
func (g *GitHub) AuthStatus(ctx context.Context) (bool, string, error) {
	res, err := g.run(ctx, "auth", "status")
	if err != nil {
		return false, res.Stdout + res.Stderr, nil //nolint:nilerr -- "not authenticated" is a normal doctor finding, not a Go error
	}
	return true, res.Stdout + res.Stderr, nil
}

// RepoView resolves the current repository's owner/name via gh, proving gh
// can reach GitHub and resolve this repository.
func (g *GitHub) RepoView(ctx context.Context) (owner, name string, err error) {
	res, err := g.run(ctx, "repo", "view", "--json", "owner,name")
	if err != nil {
		return "", "", fmt.Errorf("resolving repository via gh: %w", err)
	}
	var parsed struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
		return "", "", fmt.Errorf("parsing gh repo view output: %w", err)
	}
	return parsed.Owner.Login, parsed.Name, nil
}

// PRBodyFile writes body to a temp file under the state dir and returns its
// path. Every PR create/edit call must go through a body file, never a
// heredoc/command-substitution string, per docs/autopilot.md "PR Creation"
// (this repository's own history of shell/heredoc PR-body corruption).
func PRBodyFile(cfg Config, body string) (string, error) {
	dir := filepath.Join(cfg.RepoRoot, cfg.StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "pr-body-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// CreatePR opens a PR using --body-file, never an inline --body string.
func (g *GitHub) CreatePR(ctx context.Context, title, base, head, bodyFile string) (int, error) {
	res, err := g.run(ctx, "pr", "create",
		"--title", title,
		"--base", base,
		"--head", head,
		"--body-file", bodyFile,
	)
	if err != nil {
		return 0, fmt.Errorf("creating PR: %w", err)
	}
	return parsePRNumberFromURL(res.Stdout)
}

// EditPRBody updates an existing PR's body via --body-file.
func (g *GitHub) EditPRBody(ctx context.Context, number int, bodyFile string) error {
	_, err := g.run(ctx, "pr", "edit", strconv.Itoa(number), "--body-file", bodyFile)
	return err
}

func parsePRNumberFromURL(stdout string) (int, error) {
	stdout = strings.TrimSpace(stdout)
	lines := strings.Split(stdout, "\n")
	last := lines[len(lines)-1]
	idx := strings.LastIndex(last, "/")
	if idx < 0 || idx == len(last)-1 {
		return 0, fmt.Errorf("could not parse PR number from gh output: %q", stdout)
	}
	n, err := strconv.Atoi(strings.TrimSpace(last[idx+1:]))
	if err != nil {
		return 0, fmt.Errorf("could not parse PR number from gh output %q: %w", stdout, err)
	}
	return n, nil
}

// CheckRun is one named required-check's consolidated status, as reported
// by "gh pr checks --json". gh itself already consolidates duplicate
// push/pull_request runs of the same check name, so Autopilot does not
// reimplement that de-duplication.
type CheckRun struct {
	Name     string `json:"name"`
	State    string `json:"state"`  // e.g. SUCCESS, FAILURE, PENDING, IN_PROGRESS, ERROR, CANCELLED, SKIPPED
	Bucket   string `json:"bucket"` // gh's own summary: "pass", "fail", "pending", "skipping", "cancel"
	Link     string `json:"link"`
	Workflow string `json:"workflow"`
}

// PRChecks lists the current, consolidated check state for a PR.
func (g *GitHub) PRChecks(ctx context.Context, number int) ([]CheckRun, error) {
	res, err := g.run(ctx, "pr", "checks", strconv.Itoa(number), "--json", "name,state,bucket,link,workflow")
	if err != nil {
		// gh pr checks exits non-zero when any check has failed or is
		// pending -- that is a normal outcome to observe, not a tool
		// failure, as long as it produced parseable JSON.
		if res.Stdout == "" {
			return nil, fmt.Errorf("listing PR checks: %w", err)
		}
	}
	var checks []CheckRun
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &checks); jsonErr != nil {
		return nil, fmt.Errorf("parsing gh pr checks output: %w", jsonErr)
	}
	return checks, nil
}

// ChecksSummary buckets a check list into overall pass/fail/pending.
type ChecksSummary struct {
	AllPass bool
	Failed  []CheckRun
	Pending []CheckRun
}

func SummarizeChecks(checks []CheckRun) ChecksSummary {
	var s ChecksSummary
	s.AllPass = true
	for _, c := range checks {
		switch strings.ToLower(c.Bucket) {
		case "fail", "cancel":
			s.Failed = append(s.Failed, c)
			s.AllPass = false
		case "pending", "":
			s.Pending = append(s.Pending, c)
			s.AllPass = false
		}
	}
	return s
}

// FailedCheckLog fetches the log for a failed check's run, via its GitHub
// Actions run ID, for evidence-based repair. workflowRunID is parsed from
// the check's Link field by the caller (githubActionsRunIDFromLink).
func (g *GitHub) FailedCheckLog(ctx context.Context, runID string) (string, error) {
	res, err := g.run(ctx, "run", "view", runID, "--log-failed")
	if err != nil && res.Stdout == "" {
		return "", fmt.Errorf("fetching failed run log for run %s: %w", runID, err)
	}
	return res.Stdout, nil
}

// ActionsRunIDFromLink extracts the numeric GitHub Actions run ID from a
// check's HTML link (".../actions/runs/<id>/job/<job-id>").
func ActionsRunIDFromLink(link string) (string, bool) {
	idx := strings.Index(link, "/actions/runs/")
	if idx < 0 {
		return "", false
	}
	rest := link[idx+len("/actions/runs/"):]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// MergeCommitSHA resolves the real merge commit SHA for an already-merged
// PR via gh -- never invented, and never assumed to equal the PR's own head
// SHA (which, for a squash or rebase merge, is not the commit that actually
// lands on the base branch). This is the durable anchor post-merge
// verification checks against; see docs/autopilot.md "Post-Merge
// Verification."
func (g *GitHub) MergeCommitSHA(ctx context.Context, number int) (string, error) {
	res, err := g.run(ctx, "pr", "view", strconv.Itoa(number), "--json", "state,mergeCommit")
	if err != nil {
		return "", fmt.Errorf("viewing PR %d: %w", number, err)
	}
	var parsed struct {
		State       string `json:"state"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
		return "", fmt.Errorf("parsing gh pr view output: %w", err)
	}
	if parsed.State != "MERGED" || parsed.MergeCommit == nil || parsed.MergeCommit.OID == "" {
		return "", fmt.Errorf("PR %d is not reported as MERGED with a resolvable merge commit (state=%q)", number, parsed.State)
	}
	return parsed.MergeCommit.OID, nil
}

// WorkflowRun is one GitHub Actions run, as reported by "gh run list".
type WorkflowRun struct {
	DatabaseID   int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`     // queued, in_progress, completed, ...
	Conclusion   string `json:"conclusion"` // success, failure, cancelled, ... (only meaningful once Status=="completed")
	HeadSHA      string `json:"headSha"`
	URL          string `json:"url"`
}

// RunsForCommit lists GitHub Actions runs whose head commit is sha. This is
// the *only* sanctioned source of post-merge evidence: the pre-merge
// "gh pr checks" set is a different, earlier group of runs against the
// PR's own head commit, not against what actually landed on the base
// branch, and must never be substituted for this.
func (g *GitHub) RunsForCommit(ctx context.Context, sha string) ([]WorkflowRun, error) {
	res, err := g.run(ctx, "run", "list", "--commit", sha, "--json", "databaseId,workflowName,status,conclusion,headSha,url", "--limit", "50")
	if err != nil {
		return nil, fmt.Errorf("listing runs for commit %s: %w", sha, err)
	}
	var runs []WorkflowRun
	if err := json.Unmarshal([]byte(res.Stdout), &runs); err != nil {
		return nil, fmt.Errorf("parsing gh run list output: %w", err)
	}
	return runs, nil
}

// RequiredPostMergeWorkflows returns the display names of the workflows
// that must be observed, green, against the actual merged commit before a
// phase can be marked complete -- discovered from this repository's own
// .github/workflows/*.yml files (never hardcoded run IDs). At minimum, per
// the current TaskForge repository, this resolves to CI and CodeQL.
func RequiredPostMergeWorkflows(repoRoot string) ([]string, error) {
	var names []string
	for _, file := range []string{"ci.yml", "codeql.yml"} {
		display, err := WorkflowDisplayName(repoRoot, file)
		if err != nil {
			return nil, fmt.Errorf("resolving required post-merge workflow name for %s: %w", file, err)
		}
		names = append(names, display)
	}
	return names, nil
}

// PostMergeOutcome is the result of evaluating a commit's observed workflow
// runs against the required workflow set.
type PostMergeOutcome struct {
	Runs       []PostMergeRun
	AllSuccess bool
	Failed     []PostMergeRun
}

// EvaluatePostMergeRuns matches each required workflow name against the
// most recent run observed for that commit (gh run list is newest-first)
// and classifies the overall outcome. The absence of any run for a
// required workflow is always treated as "not yet satisfied" (pending,
// AllSuccess=false), never as success -- an expected run that hasn't
// appeared yet must never be silently interpreted as passing.
func EvaluatePostMergeRuns(required []string, runs []WorkflowRun) PostMergeOutcome {
	var out PostMergeOutcome
	out.AllSuccess = true
	for _, name := range required {
		var latest *WorkflowRun
		for i := range runs {
			if runs[i].WorkflowName == name {
				latest = &runs[i]
				break
			}
		}
		if latest == nil {
			out.AllSuccess = false
			out.Runs = append(out.Runs, PostMergeRun{Workflow: name})
			continue
		}
		pmr := PostMergeRun{
			Workflow:   name,
			RunID:      strconv.FormatInt(latest.DatabaseID, 10),
			Status:     latest.Status,
			Conclusion: latest.Conclusion,
		}
		out.Runs = append(out.Runs, pmr)
		switch {
		case latest.Status == "completed" && latest.Conclusion == "success":
			// satisfied
		case latest.Status == "completed":
			out.AllSuccess = false
			out.Failed = append(out.Failed, pmr)
		default:
			out.AllSuccess = false
		}
	}
	return out
}

// MergePR merges an already-green PR using the repository's normal merge
// workflow (an ordinary merge commit, gh's default) -- never --admin, never
// while required checks are failing (the caller must have already verified
// ChecksSummary.AllPass).
func (g *GitHub) MergePR(ctx context.Context, number int) error {
	_, err := g.run(ctx, "pr", "merge", strconv.Itoa(number), "--merge")
	return err
}

// PRState reports whether a PR is open/merged/closed, for stale-state
// reconciliation.
func (g *GitHub) PRState(ctx context.Context, number int) (state string, err error) {
	res, err := g.run(ctx, "pr", "view", strconv.Itoa(number), "--json", "state")
	if err != nil {
		return "", fmt.Errorf("viewing PR %d: %w", number, err)
	}
	var parsed struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
		return "", fmt.Errorf("parsing gh pr view output: %w", err)
	}
	return parsed.State, nil
}

// WorkflowFileNames lists the .yml/.yaml files in .github/workflows, purely
// from the filesystem -- no network call, so it works in doctor/dry-run
// without requiring gh authentication.
func WorkflowFileNames(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			names = append(names, n)
		}
	}
	return names, nil
}

// WorkflowDisplayName does a light, dependency-free scrape of a workflow
// file's top-level "name:" field (no YAML library needed for this one
// field), for doctor's "required workflow names discoverable" check.
func WorkflowDisplayName(repoRoot, fileName string) (string, error) {
	path := filepath.Join(repoRoot, ".github", "workflows", fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), nil
		}
	}
	return "", fmt.Errorf("no top-level name: field found in %s", fileName)
}
