package remediation

import (
	"context"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

type lifecycleClient struct {
	pull ghpr.PullRequest
}

func (c *lifecycleClient) GetPullRequest(context.Context, string, string, int) (ghpr.PullRequest, error) {
	return c.pull, nil
}
func (c *lifecycleClient) CompareCommits(context.Context, string, string, string, string) (bool, string, error) {
	return true, "ahead", nil
}

func TestReconcilerPresubmitThenPeriodicVerification(t *testing.T) {
	dir := t.TempDir()
	failure := models.TestCase{
		Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed",
		FailureMessage: "timed out after 42 seconds",
	}
	pattern := models.PatternAnalysis{
		ID: "pattern", JobID: "periodic-job", Subject: "periodic-job",
		Systemic: true, SharedRootCause: "timeout", SharedBuilds: []string{"10"},
	}
	before := []models.JobDetail{{
		JobID: "periodic-job", Name: "periodic-job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{BuildID: "10", Commit: "old", Result: "FAILURE"}, TestCases: []models.TestCase{failure}}},
	}}
	backend := memoryBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-e2e/20/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-e2e/20/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-e2e/20/prowjob.json":        `{"spec":{"type":"presubmit","job":"pull-e2e","refs":{"org":"example","repo":"project","pulls":[{"number":42,"sha":"head"}]}},"status":{"state":"success","build_id":"20"}}`,
		"pr-logs/pull/example_project/42/pull-e2e/20/artifacts/junit.xml": `<testsuite name="suite"><testcase name="test" classname="class"/></testsuite>`,
	}}
	client := &lifecycleClient{pull: ghpr.PullRequest{
		Number: 42, HTMLURL: "https://github.com/example/project/pull/42", State: "open",
		Head: ghpr.PullRequestRef{Repo: "fork/project", Ref: "fix", SHA: "head"},
		Base: ghpr.PullRequestRef{Repo: "example/project", Ref: "main", SHA: "base"},
	}}
	catalog := &jobconfig.Catalog{Revision: "catalog", Jobs: map[string]jobconfig.JobDefinition{
		"periodic-job": {Name: "periodic-job", JobType: models.JobTypePeriodic, Refs: []jobconfig.RepoRef{{Org: "example", Repo: "project", BaseRef: "main"}}},
	}}
	evidence := EvidenceForPattern(pattern, before)
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{
		evidence.Tests[0].Identity: {{JobID: "example/project/pull-e2e", JobName: "pull-e2e", Repo: "example/project"}},
	}}
	reconciler := NewReconciler(client, dir)
	reconciler.SetVerification(backend, catalog, coverage, client)
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, before,
		map[string]FixReference{"key": {URL: client.pull.HTMLURL}}, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Remediations["pattern"].Attempts[0].Status; got != StatusPremergeVerified {
		t.Fatalf("premerge status = %q", got)
	}

	client.pull.State = "closed"
	client.pull.Merged = true
	client.pull.MergeCommitSHA = "merge"
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	after := []models.JobDetail{{
		JobID: "periodic-job", Name: "periodic-job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "12", Commit: "new2", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
			{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "new1", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
		},
	}}
	state, err = reconciler.Reconcile(context.Background(), nil, after, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Remediations["pattern"].Attempts[0].Status; got != StatusVerifiedFixed {
		t.Fatalf("post-merge status = %q", got)
	}
}
