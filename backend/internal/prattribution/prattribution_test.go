package prattribution

import (
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	e2eJob   = "pull-project-e2e"
	testName = "[It] creates a cluster"
)

func failure(name string) models.PullRequestFailure {
	return models.PullRequestFailure{TestCase: models.TestCase{Name: name, Status: "failed"}}
}

func buildFailure() models.PullRequestFailure {
	return models.PullRequestFailure{TestCase: models.NewProwJobExecutionFailure(60)}
}

func detail(number int, jobName string, failures ...models.PullRequestFailure) models.PullRequestDetail {
	return models.PullRequestDetail{
		PullRequestSummary: models.PullRequestSummary{Number: number},
		Checks: []models.PullRequestCheck{{
			JobName: jobName, JobID: "example/project/" + jobName, Failures: failures,
		}},
	}
}

// observedBaseline reports base-branch data with no failures, which is the
// common case a pull-request-specific failure is judged against.
func observedBaseline(known ...string) Baseline {
	baseline := Baseline{
		FailingOnBase: map[string][]string{},
		FlakyTests:    map[string][]string{},
		KnownTests:    map[string]bool{},
		Observed:      true,
	}
	for _, name := range known {
		baseline.KnownTests[name] = true
	}
	return baseline
}

func annotateOne(t *testing.T, baseline Baseline, details []models.PullRequestDetail) *models.FailureAttribution {
	t.Helper()
	Annotate(details, baseline, Repository{}, nil)
	got := details[0].Checks[0].Failures[0].Attribution
	if got == nil {
		t.Fatal("expected an attribution")
	}
	return got
}

func TestBaseBranchFailureRulesOutThePullRequest(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FailingOnBase[testName] = []string{"periodic-project-e2e"}

	got := annotateOne(t, baseline, []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if got.Verdict != models.AttributionPreExisting || got.Confidence != models.AttributionConfidenceHigh {
		t.Fatalf("attribution = %+v, want high-confidence pre_existing", got)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceBaseBranch {
		t.Errorf("evidence = %+v", got.Evidence)
	}
}

func TestConcurrentFailuresOnOtherPullRequestsAreWidespread(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
		detail(3, e2eJob, failure(testName)),
		detail(4, e2eJob, failure(testName)),
	}
	got := annotateOne(t, observedBaseline(testName), details)

	if got.Verdict != models.AttributionWidespread {
		t.Fatalf("verdict = %q, want widespread", got.Verdict)
	}
	// Three peers reach the high-confidence threshold.
	if got.Confidence != models.AttributionConfidenceHigh {
		t.Errorf("confidence = %q, want high", got.Confidence)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceOtherPulls {
		t.Errorf("evidence = %+v", got.Evidence)
	}
}

func TestWidespreadConfidenceScalesWithPeerCount(t *testing.T) {
	cases := []struct {
		name  string
		peers int
		want  string
	}{
		{name: "one peer", peers: 1, want: models.AttributionConfidenceLow},
		{name: "two peers", peers: 2, want: models.AttributionConfidenceMedium},
		{name: "three peers", peers: 3, want: models.AttributionConfidenceHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			details := []models.PullRequestDetail{detail(1, e2eJob, failure(testName))}
			for i := 0; i < tc.peers; i++ {
				details = append(details, detail(100+i, e2eJob, failure(testName)))
			}
			got := annotateOne(t, observedBaseline(testName), details)
			if got.Verdict != models.AttributionWidespread || got.Confidence != tc.want {
				t.Fatalf("attribution = %+v, want widespread/%s", got, tc.want)
			}
		})
	}
}

// The base branch outranks concurrent pull request failures: a test broken on
// main explains every pull request hitting it.
func TestBaseBranchOutranksWidespread(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FailingOnBase[testName] = []string{"periodic-project-e2e"}
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
	}
	if got := annotateOne(t, baseline, details); got.Verdict != models.AttributionPreExisting {
		t.Fatalf("verdict = %q, want pre_existing", got.Verdict)
	}
}

func TestFlakyHistoryExplainsAnIsolatedFailure(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FlakyTests[testName] = []string{"periodic-project-e2e"}

	got := annotateOne(t, baseline, []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if got.Verdict != models.AttributionKnownFlake || got.Confidence != models.AttributionConfidenceMedium {
		t.Fatalf("attribution = %+v, want medium-confidence known_flake", got)
	}
}

func TestUnexplainedFailureIsHighConfidenceWhenTheBaseBranchRunsTheTest(t *testing.T) {
	got := annotateOne(t, observedBaseline(testName), []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if got.Verdict != models.AttributionUnexplained || got.Confidence != models.AttributionConfidenceHigh {
		t.Fatalf("attribution = %+v, want high-confidence unexplained", got)
	}
	// The verdict must not claim the pull request caused the failure.
	if strings.Contains(got.Summary, "caused") {
		t.Errorf("summary asserts causation: %q", got.Summary)
	}
}

func TestUnexplainedFailureIsLowConfidenceWithoutABaselineForTheTest(t *testing.T) {
	// The base branch was observed, but it never runs this test.
	got := annotateOne(t, observedBaseline("some-other-test"),
		[]models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if got.Verdict != models.AttributionUnexplained || got.Confidence != models.AttributionConfidenceLow {
		t.Fatalf("attribution = %+v, want low-confidence unexplained", got)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceNoBaseline {
		t.Errorf("evidence = %+v", got.Evidence)
	}
}

func TestMissingBaseBranchDataIsInconclusive(t *testing.T) {
	got := annotateOne(t, Baseline{}, []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if got.Verdict != models.AttributionInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", got.Verdict)
	}
}

// A build-level failure carries the same generic name on every job, so it must
// never be matched against the base branch or across unrelated jobs by name.
func TestBuildLevelFailuresAreNotMatchedByName(t *testing.T) {
	baseline := observedBaseline()
	baseline.FailingOnBase[models.ProwJobExecutionFailureName] = []string{"periodic-project-e2e"}
	baseline.FlakyTests[models.ProwJobExecutionFailureName] = []string{"periodic-project-e2e"}
	details := []models.PullRequestDetail{
		detail(1, e2eJob, buildFailure()),
		// A different job's build failure shares the name but is unrelated.
		detail(2, "pull-project-verify", buildFailure()),
	}
	got := annotateOne(t, baseline, details)

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want unexplained rather than a name-matched verdict", got.Verdict)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceBuildFailer {
		t.Errorf("evidence = %+v", got.Evidence)
	}
}

// The same job failing at build level across pull requests is real evidence,
// because the job identity makes the correlation meaningful.
func TestBuildLevelFailuresCorrelateWithinTheSameJob(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, buildFailure()),
		detail(2, e2eJob, buildFailure()),
	}
	got := annotateOne(t, observedBaseline(), details)

	if got.Verdict != models.AttributionWidespread {
		t.Fatalf("verdict = %q, want widespread", got.Verdict)
	}
	if !strings.Contains(got.Summary, e2eJob) {
		t.Errorf("summary should name the job, got %q", got.Summary)
	}
}

// The same test failing in a different job is a different signal and must not
// be counted as the same failure across pull requests.
func TestDifferentJobsDoNotCorrelate(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, "pull-project-capi-e2e", failure(testName)),
	}
	got := annotateOne(t, observedBaseline(testName), details)

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want unexplained", got.Verdict)
	}
}

func TestAnnotateCoversEveryFailure(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure("TestA"), failure("TestB")),
	}
	details[0].Checks = append(details[0].Checks, models.PullRequestCheck{
		JobName: "pull-project-verify", Failures: []models.PullRequestFailure{failure("TestC")},
	})
	Annotate(details, observedBaseline(), Repository{}, nil)

	for _, check := range details[0].Checks {
		for _, f := range check.Failures {
			if f.Attribution == nil {
				t.Fatalf("%s/%s has no attribution", check.JobName, f.Name)
			}
		}
	}
}

func TestBuildBaselineUsesTheNewestBaseBranchRunOnly(t *testing.T) {
	older := models.BuildResult{
		BuildInfo: models.BuildInfo{BuildID: "1", Started: time.Unix(1000, 0)},
		TestCases: []models.TestCase{{Name: testName, Status: "failed"}},
	}
	newer := models.BuildResult{
		BuildInfo: models.BuildInfo{BuildID: "2", Started: time.Unix(2000, 0)},
		TestCases: []models.TestCase{{Name: testName, Status: "passed"}},
	}
	// Runs are given oldest-first to prove ordering is derived, not assumed.
	baseline := BuildBaseline([]models.JobDetail{{
		Name: "periodic-project-e2e", JobType: models.JobTypePeriodic, Runs: []models.BuildResult{older, newer},
	}}, models.FlakinessReport{})

	if !baseline.Observed || !baseline.KnownTests[testName] {
		t.Fatalf("baseline = %+v, want the test observed", baseline)
	}
	if len(baseline.FailingOnBase[testName]) != 0 {
		t.Errorf("the newest run passed, so it must not be recorded as failing: %+v", baseline.FailingOnBase)
	}
}

func TestBuildBaselineIgnoresPresubmitJobDetails(t *testing.T) {
	baseline := BuildBaseline([]models.JobDetail{{
		Name: e2eJob, JobType: models.JobTypePresubmit,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1"},
			TestCases: []models.TestCase{{Name: testName, Status: "failed"}},
		}},
	}}, models.FlakinessReport{})

	if baseline.Observed {
		t.Fatal("presubmit runs describe other pull requests, not the base branch")
	}
	if len(baseline.FailingOnBase) != 0 {
		t.Errorf("FailingOnBase = %+v", baseline.FailingOnBase)
	}
}

func TestBuildBaselineTakesOnlyFlakyClassifications(t *testing.T) {
	report := models.FlakinessReport{
		MostFlaky: []models.TestFlakiness{
			{TestName: "flaky-test", JobName: "periodic-a", Classification: models.ClassificationFlaky},
		},
		PersistentFailures: []models.TestFlakiness{
			{TestName: "broken-test", JobName: "periodic-b", Classification: models.ClassificationPersistent},
		},
	}
	baseline := BuildBaseline(nil, report)

	if len(baseline.FlakyTests["flaky-test"]) != 1 {
		t.Errorf("flaky test not recorded: %+v", baseline.FlakyTests)
	}
	if _, ok := baseline.FlakyTests["broken-test"]; ok {
		t.Error("a persistent failure is not a flake")
	}
}

func TestBuildBaselineSkipsBuildLevelCasesFromTheBaseBranch(t *testing.T) {
	baseline := BuildBaseline([]models.JobDetail{{
		Name: "periodic-project-e2e", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1"},
			TestCases: []models.TestCase{models.NewProwJobExecutionFailure(10)},
		}},
	}}, models.FlakinessReport{})

	if len(baseline.FailingOnBase) != 0 || len(baseline.KnownTests) != 0 {
		t.Fatalf("baseline = %+v, want build-level cases excluded", baseline)
	}
}
