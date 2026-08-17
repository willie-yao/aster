package prtriage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	"github.com/willie-yao/aster/backend/internal/storage"
)

const (
	testRepo    = "example/project"
	testRepoSeg = "example_project"
	headSHA     = "1111111111111111111111111111111111111111"
	oldSHA      = "2222222222222222222222222222222222222222"
	baseSHA     = "3333333333333333333333333333333333333333"
)

type fakeLister struct {
	pulls []ghpr.PullRequest
	err   error
	limit int
}

func (f *fakeLister) ListOpenPullRequests(_ context.Context, _, _ string, limit int) ([]ghpr.PullRequest, error) {
	f.limit = limit
	return f.pulls, f.err
}

func pull(number int, base, head string) ghpr.PullRequest {
	return ghpr.PullRequest{
		Number: number, Title: fmt.Sprintf("pull %d", number), Author: "octocat",
		HTMLURL:   fmt.Sprintf("https://github.com/%s/pull/%d", testRepo, number),
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
		Head:      ghpr.PullRequestRef{SHA: head, Ref: "feature"},
		Base:      ghpr.PullRequestRef{SHA: baseSHA, Ref: base},
	}
}

func presubmit(name string) jobconfig.JobDefinition {
	return jobconfig.JobDefinition{Name: name, JobType: models.JobTypePresubmit, Repo: testRepo}
}

func catalogOf(defs ...jobconfig.JobDefinition) *jobconfig.Catalog {
	jobs := make(map[string]jobconfig.JobDefinition, len(defs))
	for _, def := range defs {
		jobs[def.ID()] = def
	}
	return &jobconfig.Catalog{Revision: "rev", Jobs: jobs}
}

// buildSpec describes one presubmit build to materialize in the fake bucket.
type buildSpec struct {
	job        string
	pullNumber int
	buildID    string
	passed     bool
	running    bool
	testedSHA  string
	failures   []string
}

// writeBucket materializes a local artifact tree matching Prow's presubmit
// layout and returns a backend over it.
func writeBucket(t *testing.T, specs []buildSpec) storage.Backend {
	t.Helper()
	root := t.TempDir()
	for _, spec := range specs {
		dir := filepath.Join(root, "pr-logs", "pull", testRepoSeg,
			fmt.Sprint(spec.pullNumber), spec.job, spec.buildID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		tested := spec.testedSHA
		if tested == "" {
			tested = headSHA
		}
		refs := fmt.Sprintf("main:%s,%d:%s", baseSHA, spec.pullNumber, tested)
		started := fmt.Sprintf(`{"timestamp":1700000000,"repos":{%q:%q}}`, testRepo, refs)
		writeFile(t, filepath.Join(dir, "started.json"), started)
		if !spec.running {
			result := "FAILURE"
			if spec.passed {
				result = "SUCCESS"
			}
			finished := fmt.Sprintf(`{"timestamp":1700000600,"passed":%t,"result":%q}`, spec.passed, result)
			writeFile(t, filepath.Join(dir, "finished.json"), finished)
		}
		if len(spec.failures) == 0 {
			continue
		}
		artifacts := filepath.Join(dir, "artifacts")
		if err := os.MkdirAll(artifacts, 0o755); err != nil {
			t.Fatalf("mkdir artifacts: %v", err)
		}
		writeFile(t, filepath.Join(artifacts, "junit_01.xml"), junitXML(spec.failures))
	}
	backend, err := storage.NewLocalBackend(root, "")
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	return backend
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func junitXML(failures []string) string {
	out := fmt.Sprintf("<testsuite tests=\"%d\">", len(failures)+1)
	out += `<testcase name="TestPasses" classname="pkg"></testcase>`
	for _, name := range failures {
		out += fmt.Sprintf(`<testcase name=%q classname="pkg"><failure message="boom">detail</failure></testcase>`, name)
	}
	return out + "</testsuite>"
}

func collect(t *testing.T, backend storage.Backend, lister PullRequestLister, catalog *jobconfig.Catalog) (models.PullRequestIndex, []models.PullRequestDetail) {
	t.Helper()
	index, details, err := Collect(context.Background(), backend, lister, catalog, Options{Owner: "example", Repo: "project"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return index, details
}

func TestCollectReportsFailingChecksAndTests(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-unit", pullNumber: 7, buildID: "100", passed: true},
		{job: "pull-e2e", pullNumber: 7, buildID: "200", failures: []string{"TestA", "TestB"}},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(presubmit("pull-unit"), presubmit("pull-e2e")))

	if index.Repo != testRepo || len(index.PullRequests) != 1 {
		t.Fatalf("index = %+v", index)
	}
	summary := index.PullRequests[0]
	if summary.CIState != models.PullRequestCIFailing {
		t.Errorf("ci state = %q, want FAILING", summary.CIState)
	}
	if summary.ChecksObserved != 2 || summary.ChecksFailing != 1 || summary.FailingTests != 2 {
		t.Errorf("summary counts = %+v", summary)
	}
	if summary.Title != "pull 7" || summary.Author != "octocat" || summary.BaseRef != "main" {
		t.Errorf("summary identity = %+v", summary)
	}

	if len(details) != 1 || len(details[0].Checks) != 2 {
		t.Fatalf("details = %+v", details)
	}
	// Failing checks sort ahead of passing ones.
	first := details[0].Checks[0]
	if first.JobName != "pull-e2e" || first.Passed || first.TestsFailed != 2 {
		t.Fatalf("first check = %+v", first)
	}
	if len(first.Failures) != 2 || first.Failures[0].Name != "TestA" {
		t.Errorf("failures = %+v", first.Failures)
	}
	if first.FailuresTruncated {
		t.Error("failures should not be truncated")
	}
	if first.TestedSHA != headSHA || first.Stale {
		t.Errorf("tested revision = %q stale=%t", first.TestedSHA, first.Stale)
	}
}

func TestCollectMarksStaleBuildsAgainstCurrentHead(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "200", passed: true, testedSHA: oldSHA},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	_, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e")))

	check := details[0].Checks[0]
	if check.TestedSHA != oldSHA || !check.Stale {
		t.Fatalf("check = %+v, want stale build of %s", check, oldSHA)
	}
}

func TestCollectReportsRunningBuildsAsPending(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "200", running: true},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e")))

	if index.PullRequests[0].CIState != models.PullRequestCIPending {
		t.Errorf("ci state = %q, want PENDING", index.PullRequests[0].CIState)
	}
	if index.PullRequests[0].ChecksFailing != 0 {
		t.Errorf("a running build must not count as failing: %+v", index.PullRequests[0])
	}
	if len(details[0].Checks) != 1 {
		t.Fatalf("checks = %+v", details[0].Checks)
	}
}

func TestCollectSelectsNewestBuildPerJob(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "100", failures: []string{"TestOld"}},
		{job: "pull-e2e", pullNumber: 7, buildID: "205", passed: true},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e")))

	if len(details[0].Checks) != 1 || details[0].Checks[0].BuildID != "205" {
		t.Fatalf("checks = %+v, want only newest build 205", details[0].Checks)
	}
	if index.PullRequests[0].CIState != models.PullRequestCIPassing {
		t.Errorf("ci state = %q, want PASSING", index.PullRequests[0].CIState)
	}
}

func TestCollectOmitsPresubmitsThatNeverRan(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "200", passed: true},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	_, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e"), presubmit("pull-never-run")))

	if len(details[0].Checks) != 1 || details[0].Checks[0].JobName != "pull-e2e" {
		t.Fatalf("checks = %+v, want only the job that ran", details[0].Checks)
	}
}

func TestCollectSkipsPresubmitsThatDoNotApplyToBaseBranch(t *testing.T) {
	release := presubmit("pull-release-only")
	release.Branches = []string{"^release-.*$"}
	backend := writeBucket(t, []buildSpec{
		{job: "pull-release-only", pullNumber: 7, buildID: "200", failures: []string{"TestA"}},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(release))

	if len(details[0].Checks) != 0 {
		t.Fatalf("checks = %+v, want none for a non-applicable branch", details[0].Checks)
	}
	if index.PullRequests[0].CIState != models.PullRequestCIUnknown {
		t.Errorf("ci state = %q, want UNKNOWN", index.PullRequests[0].CIState)
	}
}

func TestCollectIgnoresPresubmitsForOtherRepositories(t *testing.T) {
	other := presubmit("pull-other")
	other.Repo = "example/other"
	_, details := collect(t, writeBucket(t, nil),
		&fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}, catalogOf(other))

	if len(details[0].Checks) != 0 {
		t.Fatalf("checks = %+v, want none", details[0].Checks)
	}
}

func TestCollectCapsStoredFailuresButKeepsTrueCount(t *testing.T) {
	failures := make([]string, models.PullRequestCheckFailureCap+5)
	for i := range failures {
		failures[i] = fmt.Sprintf("Test%03d", i)
	}
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "200", failures: failures},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e")))

	check := details[0].Checks[0]
	if len(check.Failures) != models.PullRequestCheckFailureCap {
		t.Errorf("stored failures = %d, want %d", len(check.Failures), models.PullRequestCheckFailureCap)
	}
	if check.TestsFailed != len(failures) || !check.FailuresTruncated {
		t.Errorf("check = tests_failed %d truncated %t, want %d true", check.TestsFailed, check.FailuresTruncated, len(failures))
	}
	if index.PullRequests[0].FailingTests != len(failures) {
		t.Errorf("summary failing tests = %d, want the uncapped count %d",
			index.PullRequests[0].FailingTests, len(failures))
	}
}

func TestCollectPassesPullRequestLimit(t *testing.T) {
	lister := &fakeLister{}
	if _, _, err := Collect(context.Background(), writeBucket(t, nil), lister, catalogOf(),
		Options{Owner: "example", Repo: "project", MaxPullRequests: 5}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if lister.limit != 5 {
		t.Errorf("limit = %d, want 5", lister.limit)
	}
}

func TestCollectRequiresOwnerRepoAndDependencies(t *testing.T) {
	backend := writeBucket(t, nil)
	lister := &fakeLister{}
	cases := []struct {
		name    string
		backend storage.Backend
		lister  PullRequestLister
		opts    Options
	}{
		{name: "missing owner", backend: backend, lister: lister, opts: Options{Repo: "project"}},
		{name: "missing repo", backend: backend, lister: lister, opts: Options{Owner: "example"}},
		{name: "missing backend", lister: lister, opts: Options{Owner: "example", Repo: "project"}},
		{name: "missing lister", backend: backend, opts: Options{Owner: "example", Repo: "project"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Collect(context.Background(), tc.backend, tc.lister, catalogOf(), tc.opts); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestCollectPropagatesListerFailure(t *testing.T) {
	lister := &fakeLister{err: fmt.Errorf("github unavailable")}
	if _, _, err := Collect(context.Background(), writeBucket(t, nil), lister, catalogOf(),
		Options{Owner: "example", Repo: "project"}); err == nil {
		t.Fatal("want the lister error to surface")
	}
}

// A job can fail without any failing JUnit case (a build or verify step, for
// example). The check must still name a subject.
func TestCollectSynthesizesBuildFailureWhenJUnitReportsNoFailures(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-apidiff", pullNumber: 7, buildID: "200"},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	index, details := collect(t, backend, lister, catalogOf(presubmit("pull-apidiff")))

	check := details[0].Checks[0]
	if check.Passed || check.TestsFailed != 1 || len(check.Failures) != 1 {
		t.Fatalf("check = %+v, want one synthesized failure", check)
	}
	failure := check.Failures[0]
	if failure.Name != models.ProwJobExecutionFailureName || failure.Source != models.TestCaseSourceBuild {
		t.Errorf("failure = %+v, want the build-level stand-in", failure)
	}
	if check.FailuresTruncated {
		t.Error("a synthesized failure is not truncated")
	}
	if index.PullRequests[0].CIState != models.PullRequestCIFailing || index.PullRequests[0].FailingTests != 1 {
		t.Errorf("summary = %+v", index.PullRequests[0])
	}
}

// A passing build with no JUnit must not gain a synthesized failure.
func TestCollectDoesNotSynthesizeFailureForPassingBuilds(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-apidiff", pullNumber: 7, buildID: "200", passed: true},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	_, details := collect(t, backend, lister, catalogOf(presubmit("pull-apidiff")))

	check := details[0].Checks[0]
	if check.TestsFailed != 0 || len(check.Failures) != 0 {
		t.Fatalf("check = %+v, want no failures", check)
	}
}

// A failing build that already reports failing cases keeps them verbatim.
func TestCollectKeepsReportedFailuresInsteadOfSynthesizing(t *testing.T) {
	backend := writeBucket(t, []buildSpec{
		{job: "pull-e2e", pullNumber: 7, buildID: "200", failures: []string{"TestA"}},
	})
	lister := &fakeLister{pulls: []ghpr.PullRequest{pull(7, "main", headSHA)}}
	_, details := collect(t, backend, lister, catalogOf(presubmit("pull-e2e")))

	check := details[0].Checks[0]
	if check.TestsFailed != 1 || check.Failures[0].Name != "TestA" {
		t.Fatalf("check = %+v, want the reported failure", check)
	}
}
