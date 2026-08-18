package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"log"
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
	if err := p.refreshPullRequests(context.Background(), nil); err == nil {
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
	writePullRequestOutput = func(string, models.PullRequestIndex, []models.PullRequestDetail, models.SharedFailureIndex) error {
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

	published := aggregator.ComputeFlakinessReport(results, jobs, now)
	base := baseBranchFlakiness(results, jobs, published, now)

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

	published := aggregator.ComputeFlakinessReport(results, jobs, now)
	if got := baseBranchFlakiness(results, jobs, published, now); !reflect.DeepEqual(got, published) {
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
