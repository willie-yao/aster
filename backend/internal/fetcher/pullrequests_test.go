package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

const (
	basePeriodic = "periodic-project-e2e"
	flakyTest    = "[It] creates a cluster"
)

func TestPullRequestsEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *project.Config
		want bool
	}{
		{name: "no config"},
		{name: "block absent", cfg: &project.Config{}},
		{name: "explicitly disabled", cfg: &project.Config{PullRequests: &project.PullRequests{}}},
		{name: "enabled", cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &pipeline{cfg: tc.cfg}
			if got := p.pullRequestsEnabled(); got != tc.want {
				t.Fatalf("pullRequestsEnabled = %t, want %t", got, tc.want)
			}
		})
	}
}

// The warning is the only signal that anonymous GitHub reads will throttle
// triage, so it must fire exactly when triage is on and no token is set.
func TestWarnPullRequestTokenMissing(t *testing.T) {
	enabled := &project.Config{PullRequests: &project.PullRequests{Enabled: true}}
	cases := []struct {
		name      string
		cfg       *project.Config
		readToken string
		token     string
		wantWarn  bool
	}{
		{name: "disabled", cfg: &project.Config{}},
		{name: "disabled without token", cfg: nil},
		{name: "enabled with read token", cfg: enabled, readToken: "r"},
		{name: "enabled with fallback token", cfg: enabled, token: "t"},
		{name: "enabled without token", cfg: enabled, wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_READ_TOKEN", tc.readToken)
			t.Setenv("GITHUB_TOKEN", tc.token)

			var buf bytes.Buffer
			flags := log.Flags()
			log.SetOutput(&buf)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(os.Stderr)
				log.SetFlags(flags)
			})

			(&pipeline{cfg: tc.cfg}).warnPullRequestTokenMissing()

			got := strings.Contains(buf.String(), "GITHUB_READ_TOKEN")
			if got != tc.wantWarn {
				t.Fatalf("warned = %t, want %t (log: %q)", got, tc.wantWarn, buf.String())
			}
		})
	}
}

// setupPipeline runs once per process for both the one-shot and watch entry
// points, so the warning must fire there rather than once per pass.
func TestSetupPipelineWarnsWhenTriageHasNoToken(t *testing.T) {
	projectDir := t.TempDir()
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: repo
pull_requests:
  enabled: true
`, t.TempDir())
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_READ_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})

	if _, err := setupPipeline(Options{ProjectDir: projectDir, OutDir: t.TempDir()}); err != nil {
		t.Fatalf("setupPipeline: %v", err)
	}
	if !strings.Contains(buf.String(), "GITHUB_READ_TOKEN") {
		t.Fatalf("setupPipeline did not warn: %q", buf.String())
	}
}

func TestRefreshPullRequestsRequiresJobCatalog(t *testing.T) {
	p := &pipeline{cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}}
	if _, err := p.refreshPullRequests(context.Background(), nil); err == nil {
		t.Fatal("want an error when no job catalog is available")
	}
}

// A pull request refresh failure must not abort the surrounding pass, so the
// dashboard still publishes when GitHub or the catalog is unavailable.
func TestRunPullRequestPassSwallowsFailures(t *testing.T) {
	p := &pipeline{cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}}
	p.runPullRequestPass(context.Background(), nil)
}

func TestRunPullRequestPassSkipsWhenDisabled(t *testing.T) {
	called := false
	original := writePullRequestOutput
	writePullRequestOutput = func(string, models.PullRequestIndex, []models.PullRequestDetail, models.SharedFailureIndex, map[int]bool) error {
		called = true
		return nil
	}
	t.Cleanup(func() { writePullRequestOutput = original })

	(&pipeline{cfg: &project.Config{}}).runPullRequestPass(context.Background(), nil)
	if called {
		t.Fatal("disabled triage must not write output")
	}
}

// The published flakiness report is ranked and truncated across every published
// job, so presubmits can push a base-branch flake out of it. Attribution reads a
// base-only report to stay independent of what the dashboard publishes.
func TestBaseBranchFlakinessSurvivesPresubmitRanking(t *testing.T) {
	now := time.Now().UTC()
	flaky := func(name string) []models.BuildResult {
		var runs []models.BuildResult
		for i := 0; i < 6; i++ {
			status := "passed"
			if i%2 == 0 {
				status = "failed"
			}
			runs = append(runs, models.BuildResult{
				BuildInfo: models.BuildInfo{BuildID: fmt.Sprint(i), Started: now.Add(-time.Duration(i) * time.Hour)},
				TestCases: []models.TestCase{{Name: name, Status: status}},
			})
		}
		return runs
	}
	// One periodic flake, plus enough presubmit flakes to fill the ranked report.
	jobs := []models.ProwJob{{Name: basePeriodic, JobID: basePeriodic, JobType: models.JobTypePeriodic}}
	results := map[string][]models.BuildResult{basePeriodic: flaky(flakyTest)}
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("pull-project-e2e-%02d", i)
		id := models.JobIDFor(models.JobTypePresubmit, "example/project", name)
		jobs = append(jobs, models.ProwJob{Name: name, JobID: id, JobType: models.JobTypePresubmit, Repo: "example/project"})
		results[id] = flaky(fmt.Sprintf("[It] presubmit case %02d", i))
	}

	published := aggregator.ComputeFlakinessReport(results, jobs, now, aggregator.Settings{})
	base := baseBranchFlakiness(results, jobs, published, now, aggregator.Settings{})

	// Presubmit job IDs are repo-qualified, so they sort ahead of the periodic
	// on the tiebreak and deterministically fill the truncated report.
	if hasFlakyEntry(published.MostFlaky, flakyTest) {
		t.Fatalf("setup no longer displaces the base-branch flake: %d entries", len(published.MostFlaky))
	}
	if !hasFlakyEntry(base.MostFlaky, flakyTest) {
		t.Fatalf("base-branch flake missing from the attribution report: %d entries", len(base.MostFlaky))
	}
	for _, entry := range base.MostFlaky {
		if entry.JobID != basePeriodic {
			t.Fatalf("presubmit entry %q leaked into the attribution report", entry.JobID)
		}
	}
}

// With no presubmits published the two reports are the same value, so the extra
// aggregation pass is skipped.
func TestBaseBranchFlakinessReusesPublishedReport(t *testing.T) {
	now := time.Now().UTC()
	jobs := []models.ProwJob{{Name: basePeriodic, JobID: basePeriodic, JobType: models.JobTypePeriodic}}
	results := map[string][]models.BuildResult{basePeriodic: {{
		BuildInfo: models.BuildInfo{BuildID: "1", Started: now},
		TestCases: []models.TestCase{{Name: flakyTest, Status: "failed"}},
	}}}

	published := aggregator.ComputeFlakinessReport(results, jobs, now, aggregator.Settings{})
	if got := baseBranchFlakiness(results, jobs, published, now, aggregator.Settings{}); !reflect.DeepEqual(got, published) {
		t.Fatal("base-branch report diverged from the published report with no presubmits")
	}
}

func hasFlakyEntry(entries []models.TestFlakiness, testName string) bool {
	for _, entry := range entries {
		if entry.TestName == testName {
			return true
		}
	}
	return false
}

// Attribution must read the base-only report. Reading the published one would
// reintroduce the truncation and presubmit leaks it exists to avoid.
func TestAttributionBaselineReadsTheBaseOnlyReport(t *testing.T) {
	if got := (*refreshResult)(nil).attributionBaseline(); got.Observed {
		t.Fatal("a missing dashboard pass must not report base-branch evidence")
	}
	res := &refreshResult{
		details: []models.JobDetail{{
			Name: basePeriodic, JobID: basePeriodic, JobType: models.JobTypePeriodic,
			Runs: []models.BuildResult{{
				BuildInfo: models.BuildInfo{BuildID: "1"},
				TestCases: []models.TestCase{{Name: flakyTest, Status: "passed"}},
			}},
		}},
		// Only the published report carries the flake, standing in for one that
		// truncation or presubmit ranking would have distorted.
		flakiness: models.FlakinessReport{MostFlaky: []models.TestFlakiness{{
			TestName: flakyTest, JobName: basePeriodic, JobID: basePeriodic,
			Classification: models.ClassificationFlaky,
		}}},
	}

	if got := res.attributionBaseline().FlakyTests[flakyTest]; len(got) != 0 {
		t.Fatalf("FlakyTests[%q] = %v, want the base-only report to be authoritative", flakyTest, got)
	}
}

// refusingTransport fails the test if any HTTP request is attempted.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Helper()
	r.t.Fatalf("unexpected GitHub request to %s", req.URL)
	return nil, fmt.Errorf("unreachable")
}

// failingHTTPClient returns a client that turns any outbound call into a test
// failure, which is how "no write was attempted" is proven rather than assumed.
func failingHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: refusingTransport{t: t}}
}

// commentConfig builds a config with commenting in the given state.
func commentConfig(triage, comment bool) *project.Config {
	return &project.Config{
		PullRequests: &project.PullRequests{
			Enabled: triage,
			Comment: &project.PullRequestComment{Enabled: comment},
		},
	}
}

// TestRunPullRequestCommentsDisabledMakesNoCall is the default-off guarantee:
// with commenting unconfigured or disabled, the pass must not reach GitHub at
// all. A transport that fails the test on use proves it rather than asserting
// on a log line.
func TestRunPullRequestCommentsDisabledMakesNoCall(t *testing.T) {
	cases := []struct {
		name string
		cfg  *project.Config
	}{
		{name: "no pull request block", cfg: &project.Config{}},
		{name: "triage on, comment block absent", cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}},
		{name: "comment block present but disabled", cfg: commentConfig(true, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Credentials are present, so only the config gates the call.
			t.Setenv("ASTER_APP_ID", "1")
			t.Setenv("ASTER_APP_PRIVATE_KEY", "irrelevant")

			p := &pipeline{cfg: tc.cfg, client: failingHTTPClient(t)}
			p.runPullRequestComments(context.Background(), triageOutcome{})
		})
	}
}

// TestCommentOnPullRequestsRequiresAppCredentials proves the feature reports a
// missing App rather than silently doing nothing or falling back to a personal
// token, which would post as a human.
func TestCommentOnPullRequestsRequiresAppCredentials(t *testing.T) {
	t.Setenv("ASTER_APP_ID", "")
	t.Setenv("ASTER_APP_PRIVATE_KEY", "")

	p := &pipeline{cfg: commentConfig(true, true), client: failingHTTPClient(t)}
	err := p.commentOnPullRequests(context.Background(), triageOutcome{})
	if err == nil {
		t.Fatal("expected an error when App credentials are unset")
	}
	if !strings.Contains(err.Error(), "ASTER_APP_ID") {
		t.Fatalf("error = %v, want it to name the missing credential", err)
	}
}

// TestWarnCommentCredentialsMissing checks the startup signal fires exactly
// when commenting is enabled without an App.
func TestWarnCommentCredentialsMissing(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *project.Config
		appID    string
		wantWarn bool
	}{
		{name: "disabled", cfg: &project.Config{}},
		{name: "disabled with credentials", cfg: commentConfig(true, false), appID: "1"},
		{name: "enabled with credentials", cfg: commentConfig(true, true), appID: "1"},
		{name: "enabled without credentials", cfg: commentConfig(true, true), wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ASTER_APP_ID", tc.appID)
			t.Setenv("ASTER_APP_PRIVATE_KEY", "")

			var buf bytes.Buffer
			flags := log.Flags()
			log.SetOutput(&buf)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(os.Stderr)
				log.SetFlags(flags)
			})

			(&pipeline{cfg: tc.cfg}).warnCommentCredentialsMissing()

			if got := strings.Contains(buf.String(), "ASTER_APP_ID"); got != tc.wantWarn {
				t.Fatalf("warned = %t, want %t (log: %q)", got, tc.wantWarn, buf.String())
			}
		})
	}
}

// TestRunPullRequestCommentsRespectsSkipSideEffects proves -skip-side-effects
// suppresses commenting. It is documented as writing dashboard data without
// GitHub writes, and a comment on a contributor's pull request is a GitHub
// write like any other.
func TestRunPullRequestCommentsRespectsSkipSideEffects(t *testing.T) {
	t.Setenv("ASTER_APP_ID", "1")
	t.Setenv("ASTER_APP_PRIVATE_KEY", "irrelevant")

	p := &pipeline{
		cfg:    commentConfig(true, true),
		client: failingHTTPClient(t),
		opts:   Options{SkipSideEffects: true},
	}
	p.runPullRequestComments(context.Background(), triageOutcome{})
}

// TestCommentCandidatesComeFromPublishedDetails proves candidates are built
// from the pages this pass wrote, so a comment cannot link to a missing page.
func TestCommentCandidatesComeFromPublishedDetails(t *testing.T) {
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		{PullRequestSummary: models.PullRequestSummary{Number: 7, Author: "a", CreatedAt: created}},
		{PullRequestSummary: models.PullRequestSummary{Number: 9, Author: "b", CreatedAt: created}},
	}
	got := commentCandidates(details)
	if len(got) != 2 {
		t.Fatalf("built %d candidates, want 2", len(got))
	}
	if got[0].Number != 7 || got[0].Author != "a" {
		t.Fatalf("candidate = %+v, want the published detail's identity", got[0])
	}
	if got[1].Number != 9 {
		t.Fatalf("candidate = %+v, want pull request 9", got[1])
	}
}

// TestRunPullRequestPassSkipsCommentingWhenTriageFails proves a failed refresh
// suppresses commenting: without a successful publish, the triage pages a
// comment links to are missing or stale.
func TestRunPullRequestPassSkipsCommentingWhenTriageFails(t *testing.T) {
	t.Setenv("ASTER_APP_ID", "1")
	t.Setenv("ASTER_APP_PRIVATE_KEY", "irrelevant")
	// A nil job catalog makes refreshPullRequests fail before any GitHub call,
	// and the refusing transport proves commenting never runs afterwards.
	p := &pipeline{
		cfg:    commentConfig(true, true),
		client: failingHTTPClient(t),
	}
	p.runPullRequestPass(context.Background(), &refreshResult{})
}
