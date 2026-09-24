package autopilot

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestPRBodyFile_WritesContentToFile(t *testing.T) {
	cfg := testConfig(t)
	body := "## Summary\n\nsome body text\n"
	path, err := PRBodyFile(cfg, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read generated body file: %v", err)
	}
	if string(data) != body {
		t.Fatalf("body file contents mismatch: got %q want %q", data, body)
	}
}

func TestGitHub_CreatePR_UsesBodyFileNotInlineBody(t *testing.T) {
	fr := NewFakeRunner()
	var seenArgs []string
	fr.On("gh", []string{"pr", "create"}, func(c FakeCall) (Result, error) {
		seenArgs = c.Args
		return resultOK("https://github.com/o/r/pull/7\n"), nil
	})
	gh := &GitHub{Runner: fr, Dir: "/repo"}
	num, err := gh.CreatePR(context.Background(), "feat: implement Phase 16", "main", "phase-16-implementation", "/tmp/body.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if num != 7 {
		t.Fatalf("expected PR number 7, got %d", num)
	}
	joined := strings.Join(seenArgs, " ")
	if !strings.Contains(joined, "--body-file /tmp/body.md") {
		t.Fatalf("expected --body-file to be used, got args: %v", seenArgs)
	}
	if strings.Contains(joined, "--body ") {
		t.Fatalf("must never pass an inline --body, got args: %v", seenArgs)
	}
}

func TestParsePRNumberFromURL(t *testing.T) {
	n, err := parsePRNumberFromURL("Creating PR...\nhttps://github.com/owner/repo/pull/123\n")
	if err != nil || n != 123 {
		t.Fatalf("expected 123, nil; got %d, %v", n, err)
	}
	if _, err := parsePRNumberFromURL("no url here"); err == nil {
		t.Fatal("expected error for unparseable output")
	}
}

func TestActionsRunIDFromLink(t *testing.T) {
	id, ok := ActionsRunIDFromLink("https://github.com/o/r/actions/runs/123456789/job/987")
	if !ok || id != "123456789" {
		t.Fatalf("expected 123456789, true; got %s, %v", id, ok)
	}
	if _, ok := ActionsRunIDFromLink("https://github.com/o/r/actions"); ok {
		t.Fatal("expected no match for a link with no run ID")
	}
}

func TestSummarizeChecks(t *testing.T) {
	allPass := []CheckRun{{Name: "CI/test", Bucket: "pass"}, {Name: "CodeQL", Bucket: "pass"}}
	s := SummarizeChecks(allPass)
	if !s.AllPass || len(s.Failed) != 0 || len(s.Pending) != 0 {
		t.Fatalf("expected all-pass summary, got %+v", s)
	}

	withFailure := []CheckRun{{Name: "CI/test", Bucket: "pass"}, {Name: "CI/vulncheck", Bucket: "fail"}}
	s2 := SummarizeChecks(withFailure)
	if s2.AllPass || len(s2.Failed) != 1 {
		t.Fatalf("expected one failure, got %+v", s2)
	}

	pending := []CheckRun{{Name: "CI/test", Bucket: "pending"}}
	s3 := SummarizeChecks(pending)
	if s3.AllPass || len(s3.Pending) != 1 {
		t.Fatalf("expected one pending, got %+v", s3)
	}
}

func TestGitHub_AuthStatus_NotAuthenticated_IsNotAGoError(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("gh", []string{"auth", "status"}, func(c FakeCall) (Result, error) {
		return Result{Stderr: "not logged in"}, &exitErrStub{}
	})
	gh := &GitHub{Runner: fr, Dir: "/repo"}
	ok, detail, err := gh.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus must report failure via its bool return, not an error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false")
	}
	if !strings.Contains(detail, "not logged in") {
		t.Fatalf("expected detail to include the failure text, got %q", detail)
	}
}

type exitErrStub struct{}

func (e *exitErrStub) Error() string { return "exit status 1" }

func TestWorkflowFileNames_ListsYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	wfDir := dir + "/.github/workflows"
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wfDir+"/ci.yml", []byte("name: CI\non: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wfDir+"/notes.txt", []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := WorkflowFileNames(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 1 || names[0] != "ci.yml" {
		t.Fatalf("expected exactly [ci.yml], got %v", names)
	}
	display, err := WorkflowDisplayName(dir, "ci.yml")
	if err != nil || display != "CI" {
		t.Fatalf("expected name CI, got %q, %v", display, err)
	}
}

func TestGitHub_MergeCommitSHA_Valid(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("gh", []string{"pr", "view", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"MERGED","mergeCommit":{"oid":"deadbeefcafef00d"}}`), nil
	})
	gh := &GitHub{Runner: fr, Dir: "/repo"}
	sha, err := gh.MergeCommitSHA(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "deadbeefcafef00d" {
		t.Fatalf("expected the real merge commit oid, got %q", sha)
	}
}

func TestGitHub_MergeCommitSHA_NotMerged_Errors(t *testing.T) {
	fr := NewFakeRunner()
	fr.On("gh", []string{"pr", "view", "1"}, func(c FakeCall) (Result, error) {
		return resultOK(`{"state":"OPEN","mergeCommit":null}`), nil
	})
	gh := &GitHub{Runner: fr, Dir: "/repo"}
	if _, err := gh.MergeCommitSHA(context.Background(), 1); err == nil {
		t.Fatal("expected an error: a PR that is not MERGED has no real merge commit to resolve, must never invent one")
	}
}

func TestGitHub_RunsForCommit_ParsesRunsAndQueriesTheExactCommit(t *testing.T) {
	fr := NewFakeRunner()
	var seenArgs []string
	fr.On("gh", []string{"run", "list"}, func(c FakeCall) (Result, error) {
		seenArgs = c.Args
		return resultOK(`[{"databaseId":111,"workflowName":"CI","status":"completed","conclusion":"success","headSha":"deadbeef"}]`), nil
	})
	gh := &GitHub{Runner: fr, Dir: "/repo"}
	runs, err := gh.RunsForCommit(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runs) != 1 || runs[0].WorkflowName != "CI" || runs[0].DatabaseID != 111 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
	joined := strings.Join(seenArgs, " ")
	if !strings.Contains(joined, "--commit deadbeef") {
		t.Fatalf("expected RunsForCommit to query the exact commit via --commit, got args: %v", seenArgs)
	}
}

func TestRequiredPostMergeWorkflows_DiscoversCIAndCodeQLNames(t *testing.T) {
	dir := t.TempDir()
	writeRequiredWorkflowFiles(t, dir)
	names, err := RequiredPostMergeWorkflows(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 2 || names[0] != "CI" || names[1] != "CodeQL" {
		t.Fatalf("expected [CI CodeQL] discovered from the workflow files' own name: fields, got %v", names)
	}
}

func TestEvaluatePostMergeRuns_AllSuccess(t *testing.T) {
	required := []string{"CI", "CodeQL"}
	runs := []WorkflowRun{
		{DatabaseID: 1, WorkflowName: "CI", Status: "completed", Conclusion: "success"},
		{DatabaseID: 2, WorkflowName: "CodeQL", Status: "completed", Conclusion: "success"},
	}
	out := EvaluatePostMergeRuns(required, runs)
	if !out.AllSuccess {
		t.Fatalf("expected AllSuccess, got %+v", out)
	}
	if len(out.Failed) != 0 {
		t.Fatalf("expected no failures, got %+v", out.Failed)
	}
	if len(out.Runs) != 2 || out.Runs[0].RunID != "1" || out.Runs[1].RunID != "2" {
		t.Fatalf("expected run IDs to be recorded, got %+v", out.Runs)
	}
}

func TestEvaluatePostMergeRuns_MissingRunIsPendingNotSuccess(t *testing.T) {
	required := []string{"CI", "CodeQL"}
	// Only CI has appeared so far; CodeQL's run hasn't shown up yet.
	runs := []WorkflowRun{
		{DatabaseID: 1, WorkflowName: "CI", Status: "completed", Conclusion: "success"},
	}
	out := EvaluatePostMergeRuns(required, runs)
	if out.AllSuccess {
		t.Fatal("an expected-but-absent run must never be treated as success")
	}
	if len(out.Failed) != 0 {
		t.Fatalf("an absent run is pending, not failed: got %+v", out.Failed)
	}
	found := false
	for _, r := range out.Runs {
		if r.Workflow == "CodeQL" && r.RunID == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a pending, run-id-less entry for CodeQL, got %+v", out.Runs)
	}
}

func TestEvaluatePostMergeRuns_InProgressIsPendingNotSuccess(t *testing.T) {
	required := []string{"CI"}
	runs := []WorkflowRun{{DatabaseID: 1, WorkflowName: "CI", Status: "in_progress"}}
	out := EvaluatePostMergeRuns(required, runs)
	if out.AllSuccess {
		t.Fatal("an in-progress run must not be treated as success")
	}
	if len(out.Failed) != 0 {
		t.Fatalf("an in-progress run is pending, not failed: got %+v", out.Failed)
	}
}

func TestEvaluatePostMergeRuns_FailedRunIsReportedAsFailed(t *testing.T) {
	required := []string{"CI", "CodeQL"}
	runs := []WorkflowRun{
		{DatabaseID: 1, WorkflowName: "CI", Status: "completed", Conclusion: "success"},
		{DatabaseID: 2, WorkflowName: "CodeQL", Status: "completed", Conclusion: "failure"},
	}
	out := EvaluatePostMergeRuns(required, runs)
	if out.AllSuccess {
		t.Fatal("expected AllSuccess=false when a required run failed")
	}
	if len(out.Failed) != 1 || out.Failed[0].Workflow != "CodeQL" || out.Failed[0].RunID != "2" {
		t.Fatalf("expected CodeQL(run=2) to be reported failed, got %+v", out.Failed)
	}
}

func TestEvaluatePostMergeRuns_OnlyMatchesRequiredWorkflowNames(t *testing.T) {
	// An unrelated workflow's run for this commit must not satisfy (or
	// otherwise affect) a required workflow it doesn't match by name.
	required := []string{"CI"}
	runs := []WorkflowRun{
		{DatabaseID: 9, WorkflowName: "Scheduled vulnerability scan", Status: "completed", Conclusion: "success"},
	}
	out := EvaluatePostMergeRuns(required, runs)
	if out.AllSuccess {
		t.Fatal("an unrelated workflow's success must not satisfy a required workflow it doesn't name-match")
	}
}
