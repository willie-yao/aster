package attribution

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	e2eJob          = "pull-project-e2e"
	basePeriodicJob = "periodic-project-e2e"
	testName        = "[It] creates a cluster"
)

var testRepo = Repository{Owner: "example", Name: "project"}

func failure(name string) models.PullRequestFailure {
	return models.PullRequestFailure{TestCase: models.TestCase{Name: name, Status: "failed"}}
}

func buildFailure() models.PullRequestFailure {
	return models.PullRequestFailure{TestCase: models.NewProwJobExecutionFailure(60)}
}

func detail(number int, jobName string, failures ...models.PullRequestFailure) models.PullRequestDetail {
	return models.PullRequestDetail{
		PullRequestSummary: models.PullRequestSummary{Number: number, BaseRef: "main"},
		Checks: []models.PullRequestCheck{{
			JobName: jobName, JobID: "example/project/" + jobName, Failures: failures,
		}},
	}
}

// detailOnBase is detail with an explicit base branch, for correlation tests.
func detailOnBase(number int, baseRef, jobName string, failures ...models.PullRequestFailure) models.PullRequestDetail {
	d := detail(number, jobName, failures...)
	d.BaseRef = baseRef
	return d
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
	Annotate(details, map[string]Baseline{"main": baseline}, testRepo, nil)
	got := details[0].Checks[0].Failures[0].Attribution
	if got == nil {
		t.Fatal("expected an attribution")
	}
	return got
}

func evidenceKinds(a *models.FailureAttribution) []string {
	kinds := make([]string, len(a.Evidence))
	for i, e := range a.Evidence {
		kinds[i] = e.Kind
	}
	return kinds
}

func TestBaseBranchFailureRulesOutThePullRequest(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FailingOnBase[testName] = []string{basePeriodicJob}

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
	baseline.FailingOnBase[testName] = []string{basePeriodicJob}
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
		detail(3, e2eJob, failure(testName)),
	}
	if got := annotateOne(t, baseline, details); got.Verdict != models.AttributionPreExisting {
		t.Fatalf("verdict = %q, want pre_existing", got.Verdict)
	}
}

// pre_existing is decided before peers are consulted, so it carries only the
// base-branch fact. A failure the base branch already explains does not need
// corroboration from other pull requests.
func TestPreExistingDoesNotRecordPeers(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FailingOnBase[testName] = []string{basePeriodicJob}
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
	}
	got := annotateOne(t, baseline, details)

	if kinds := evidenceKinds(got); !reflect.DeepEqual(kinds, []string{models.AttributionEvidenceBaseBranch}) {
		t.Errorf("evidence kinds = %v, want only the base-branch fact", kinds)
	}
}

// A single peer is mutual, uncorroborated citation: with exactly two pull
// requests each cites the other and neither is independently confirmed. It must
// not preempt base-branch evidence that the test passes, because widespread is
// not escalation-eligible and the misclassification takes away the only
// investigation tool the failure has.
func TestSinglePeerDoesNotPreemptBaseBranchEvidence(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
	}
	got := annotateOne(t, observedBaseline(testName), details)

	if got.Verdict != models.AttributionUnexplained || got.Confidence != models.AttributionConfidenceHigh {
		t.Fatalf("attribution = %+v, want the escalation-eligible high-confidence unexplained", got)
	}
	// The peer is reported rather than dropped, so the summary must name it
	// instead of claiming there is none.
	if strings.Contains(got.Summary, "not failing on other open pull requests") {
		t.Errorf("summary denies a peer that exists: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "#2") {
		t.Errorf("summary should name the peer, got %q", got.Summary)
	}
	want := []string{models.AttributionEvidenceBaseBranch, models.AttributionEvidenceOtherPulls}
	if kinds := evidenceKinds(got); !reflect.DeepEqual(kinds, want) {
		t.Errorf("evidence kinds = %v, want the base-branch fact and the peer", kinds)
	}
}

// One job often runs on several release branches. Correlating across them
// compares pull requests testing different code.
func TestPullRequestsOnDifferentBaseBranchesDoNotCorrelate(t *testing.T) {
	details := []models.PullRequestDetail{
		detailOnBase(1, "release-1.25", e2eJob, failure(testName)),
		detailOnBase(2, "release-1.24", e2eJob, failure(testName)),
		detailOnBase(3, "release-1.24", e2eJob, failure(testName)),
	}
	Annotate(details, map[string]Baseline{"release-1.25": observedBaseline(testName)}, testRepo, nil)
	got := details[0].Checks[0].Failures[0].Attribution

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want unexplained rather than a cross-branch correlation", got.Verdict)
	}
	for _, e := range got.Evidence {
		if e.Kind == models.AttributionEvidenceOtherPulls {
			t.Errorf("cited a peer on another base branch: %q", e.Detail)
		}
	}
	// Peers exist, just not on this base branch, so the summary must scope its
	// negative claim rather than deny them outright.
	if !strings.Contains(got.Summary, "targeting release-1.25") {
		t.Errorf("summary makes an unscoped claim about other pull requests: %q", got.Summary)
	}
}

// The comparison is bounded to one base branch, so the evidence names which one
// rather than leaving the scope for the reader to infer.
func TestWidespreadEvidenceNamesTheComparedBaseBranch(t *testing.T) {
	details := []models.PullRequestDetail{
		detailOnBase(1, "release-1.25", e2eJob, failure(testName)),
		detailOnBase(2, "release-1.25", e2eJob, failure(testName)),
		detailOnBase(3, "release-1.25", e2eJob, failure(testName)),
	}
	got := annotateOne(t, observedBaseline(testName), details)

	if got.Verdict != models.AttributionWidespread {
		t.Fatalf("verdict = %q, want widespread", got.Verdict)
	}
	if len(got.Evidence) != 1 || !strings.Contains(got.Evidence[0].Detail, "release-1.25") {
		t.Errorf("evidence should name the compared base branch, got %+v", got.Evidence)
	}
}

// A verdict the base branch already explains must not move because an unrelated
// pull request's presubmit went green between passes.
func TestRemovingAPeerDoesNotChangeAnExplainedVerdict(t *testing.T) {
	withPeer := annotateOne(t, observedBaseline(testName), []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
		detail(2, e2eJob, failure(testName)),
	})
	withoutPeer := annotateOne(t, observedBaseline(testName), []models.PullRequestDetail{
		detail(1, e2eJob, failure(testName)),
	})

	if withPeer.Verdict != withoutPeer.Verdict || withPeer.Confidence != withoutPeer.Confidence {
		t.Errorf("a peer leaving changed the verdict:\nwith    = %s/%s\nwithout = %s/%s",
			withPeer.Verdict, withPeer.Confidence, withoutPeer.Verdict, withoutPeer.Confidence)
	}
}

// The build-level summary claims no other pull request hit the failure, which
// must not survive a peer reaching it.
func TestBuildLevelSummaryDoesNotDenyASinglePeer(t *testing.T) {
	details := []models.PullRequestDetail{
		detail(1, e2eJob, buildFailure()),
		detail(2, e2eJob, buildFailure()),
	}
	got := annotateOne(t, observedBaseline(), details)

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want unexplained", got.Verdict)
	}
	if strings.Contains(got.Summary, "no other open pull request") {
		t.Errorf("summary denies a peer that exists: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "#2") {
		t.Errorf("summary should name the peer, got %q", got.Summary)
	}
	// The peer is recorded as evidence, so the build log is no longer the only
	// evidence and the summary must not say it is.
	if strings.Contains(got.Summary, "only evidence") {
		t.Errorf("summary contradicts the peer evidence beside it: %q", got.Summary)
	}
	want := []string{models.AttributionEvidenceBuildFailer, models.AttributionEvidenceOtherPulls}
	if kinds := evidenceKinds(got); !reflect.DeepEqual(kinds, want) {
		t.Errorf("evidence kinds = %v, want the build-level fact and the peer", kinds)
	}
}

func TestFlakyHistoryExplainsAnIsolatedFailure(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FlakyTests[testName] = []string{basePeriodicJob}

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
	baseline.FailingOnBase[models.ProwJobExecutionFailureName] = []string{basePeriodicJob}
	baseline.FlakyTests[models.ProwJobExecutionFailureName] = []string{basePeriodicJob}
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
		detail(3, e2eJob, buildFailure()),
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
	Annotate(details, map[string]Baseline{"main": observedBaseline()}, testRepo, nil)

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
		BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "release-1.30"}, Started: time.Unix(1000, 0)},
		TestCases: []models.TestCase{{Name: testName, Status: "failed"}},
	}
	newer := models.BuildResult{
		BuildInfo: models.BuildInfo{BuildID: "2", RepoRefs: map[string]string{"example/project": "main"}, Started: time.Unix(2000, 0)},
		TestCases: []models.TestCase{{Name: testName, Status: "passed"}},
	}
	// Runs are given oldest-first to prove ordering is derived, not assumed.
	baseline := BuildBaseline([]models.JobDetail{{
		Name: basePeriodicJob, JobID: basePeriodicJob, JobType: models.JobTypePeriodic, Runs: []models.BuildResult{older, newer},
	}}, models.FlakinessReport{}, testRepo)["main"]

	if !baseline.Observed || !baseline.KnownTests[testName] {
		t.Fatalf("baseline = %+v, want the test observed", baseline)
	}
	if len(baseline.FailingOnBase[testName]) != 0 {
		t.Errorf("the newest run passed, so it must not be recorded as failing: %+v", baseline.FailingOnBase)
	}
}

func TestBaselineBranch(t *testing.T) {
	const fullSHA = "1111111111111111111111111111111111111111"
	tests := []struct {
		name string
		refs map[string]string
		want string
	}{
		{name: "bare branch", refs: map[string]string{"example/project": "main"}, want: "main"},
		{name: "qualified branch", refs: map[string]string{"example/project": " refs/heads/release-1.30 "}, want: "release-1.30"},
		{name: "qualified hexadecimal branch", refs: map[string]string{"example/project": "refs/heads/1234:abc123"}, want: "1234"},
		{name: "short revision", refs: map[string]string{"example/project": "release-1.30:abc123"}, want: "release-1.30"},
		{name: "full revision", refs: map[string]string{"example/project": "refs/heads/main:" + fullSHA}, want: "main"},
		{name: "multi-repo periodic", refs: map[string]string{
			" EXAMPLE/PROJECT ": "main:abc123", "other/repo": "refs/heads/release-1.30:" + fullSHA,
		}, want: "main"},
		{name: "matching duplicate", refs: map[string]string{
			"example/project": "main:abc123", " EXAMPLE/PROJECT ": "refs/heads/main:" + fullSHA,
		}, want: "main"},
		{name: "wrong repository", refs: map[string]string{"other/project": "main"}},
		{name: "missing source ref"},
		{name: "partial repository", refs: map[string]string{"example/project-extra": "main"}},
		{name: "bare sha", refs: map[string]string{"example/project": fullSHA}},
		{name: "bare short sha", refs: map[string]string{"example/project": "abc123"}},
		{name: "composite", refs: map[string]string{"example/project": "main:abc123,7:abc456"}},
		{name: "invalid suffix", refs: map[string]string{"example/project": "main:not-hex"}},
		{name: "empty suffix", refs: map[string]string{"example/project": "main:"}},
		{name: "extra colon", refs: map[string]string{"example/project": "main:abc:123"}},
		{name: "empty branch", refs: map[string]string{"example/project": "refs/heads/"}},
		{name: "ambiguous", refs: map[string]string{"example/project": "ambiguous"}},
		{name: "tag ref", refs: map[string]string{"example/project": "refs/tags/v1:abc123"}},
		{name: "pull ref", refs: map[string]string{"example/project": "refs/pull/7/head:abc123"}},
		{name: "conflicting matching refs", refs: map[string]string{
			"example/project": "main", " EXAMPLE/PROJECT ": "release-1.30",
		}},
		{name: "invalid matching duplicate", refs: map[string]string{
			"example/project": "main", " EXAMPLE/PROJECT ": "main:invalid",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := baselineBranch(models.BuildInfo{RepoRefs: tt.refs}, Repository{Owner: " example ", Name: " project "})
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("baselineBranch(%v) = %q, %t; want %q, %t", tt.refs, got, ok, tt.want, tt.want != "")
			}
		})
	}
}

func TestBuildBaselineScopesRepositoryAndBranch(t *testing.T) {
	run := func(branch string, started int64, cases ...models.TestCase) models.BuildResult {
		return models.BuildResult{
			BuildInfo: models.BuildInfo{
				Started:  time.Unix(started, 0),
				RepoRefs: map[string]string{"example/project": branch, "other/repo": "release-1.30"},
			},
			TestCases: cases,
		}
	}
	job := func(id, name string, runs ...models.BuildResult) models.JobDetail {
		return models.JobDetail{JobID: id, Name: name, JobType: models.JobTypePeriodic, Runs: runs}
	}
	details := []models.JobDetail{
		job("main-fail", "main-fail", run("main", 10, models.TestCase{Name: "main-failure", Status: "failed"})),
		job("main-pass", "main-pass", run("main", 11, models.TestCase{Name: "main-passing", Status: "passed"})),
		job("release-fail", "release-fail", run("release-1.30", 12, models.TestCase{Name: "release-failure", Status: "failed"})),
		job("switching", "switching", run("release-1.30", 9, models.TestCase{Name: "older-failure", Status: "failed"}),
			run("main", 13, models.TestCase{Name: "newer-passing", Status: "passed"})),
		job("wrong-repo", "wrong-repo", models.BuildResult{
			BuildInfo: models.BuildInfo{RepoRefs: map[string]string{"other/repo": "release-1.30"}},
			TestCases: []models.TestCase{{Name: "wrong-repo-failure", Status: "failed"}},
		}),
		job("missing-ref", "missing-ref", models.BuildResult{TestCases: []models.TestCase{{Name: "branchless-failure", Status: "failed"}}}),
		job("no-runs", "no-runs"),
		{JobID: "presubmit", Name: "presubmit", JobType: models.JobTypePresubmit,
			Runs: []models.BuildResult{run("main", 14, models.TestCase{Name: "presubmit-failure", Status: "failed"})}},
	}
	report := models.FlakinessReport{MostFlaky: []models.TestFlakiness{
		{JobID: "main-pass", JobName: "main-pass", TestName: "main-flake", Classification: models.ClassificationFlaky},
		{JobID: "release-fail", JobName: "release-fail", TestName: "release-flake", Classification: models.ClassificationFlaky},
		{JobID: "unmatched", JobName: "main-pass", TestName: "unknown-job-flake", Classification: models.ClassificationFlaky},
		{JobID: "wrong-repo", JobName: "wrong-repo", TestName: "wrong-repo-flake", Classification: models.ClassificationFlaky},
		{JobID: "presubmit", JobName: "presubmit", TestName: "presubmit-flake", Classification: models.ClassificationFlaky},
	}}
	baselines := BuildBaseline(details, report, testRepo)
	if len(baselines) != 2 {
		t.Fatalf("branches = %v, want only main and release-1.30", baselines)
	}
	main := baselines["main"]
	release := baselines["release-1.30"]
	if !main.Observed || !release.Observed ||
		len(main.FailingOnBase["main-failure"]) != 1 || len(release.FailingOnBase["release-failure"]) != 1 ||
		!main.KnownTests["newer-passing"] || release.KnownTests["older-failure"] ||
		main.KnownTests["release-failure"] || release.KnownTests["main-passing"] {
		t.Fatalf("baseline leaked across branches or used an older run: main=%+v release=%+v", main, release)
	}
	if !reflect.DeepEqual(main.FlakyTests["main-flake"], []string{"main-pass"}) ||
		!reflect.DeepEqual(release.FlakyTests["release-flake"], []string{"release-fail"}) {
		t.Fatalf("flakiness was not mapped by JobID: main=%+v release=%+v", main.FlakyTests, release.FlakyTests)
	}
	for _, name := range []string{"unknown-job-flake", "wrong-repo-flake", "presubmit-flake"} {
		if len(main.FlakyTests[name]) != 0 || len(release.FlakyTests[name]) != 0 {
			t.Errorf("%s leaked into the baseline", name)
		}
	}
	for _, name := range []string{"wrong-repo-failure", "branchless-failure", "presubmit-failure"} {
		if main.KnownTests[name] || release.KnownTests[name] {
			t.Errorf("%s from an inadmissible job leaked into the baseline", name)
		}
	}

	for _, tt := range []struct {
		name, branch, test string
		wantVerdict        models.AttributionVerdict
		wantConfidence     string
	}{
		{"main failure cannot explain release", "release-1.30", "main-failure", models.AttributionUnexplained, models.AttributionConfidenceLow},
		{"main flake cannot explain release", "release-1.30", "main-flake", models.AttributionUnexplained, models.AttributionConfidenceLow},
		{"main pass cannot explain release", "release-1.30", "main-passing", models.AttributionUnexplained, models.AttributionConfidenceLow},
		{"matching release failure", "release-1.30", "release-failure", models.AttributionPreExisting, models.AttributionConfidenceHigh},
		{"matching release flake", "release-1.30", "release-flake", models.AttributionKnownFlake, models.AttributionConfidenceMedium},
		{"matching main pass", "main", "main-passing", models.AttributionUnexplained, models.AttributionConfidenceHigh},
		{"missing branch evidence", "release-1.31", "main-failure", models.AttributionInconclusive, models.AttributionConfidenceLow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prs := []models.PullRequestDetail{detailOnBase(1, tt.branch, e2eJob, failure(tt.test))}
			Annotate(prs, baselines, testRepo, nil)
			got := prs[0].Checks[0].Failures[0].Attribution
			if got.Verdict != tt.wantVerdict || got.Confidence != tt.wantConfidence {
				t.Fatalf("attribution = %+v; want %s/%s", got, tt.wantVerdict, tt.wantConfidence)
			}
			if !strings.Contains(got.Summary, tt.branch) || !strings.Contains(got.Evidence[0].Detail, tt.branch) {
				t.Errorf("summary/evidence omit base branch %s: %+v", tt.branch, got)
			}
		})
	}

	peers := []models.PullRequestDetail{
		detailOnBase(1, "release-1.31", e2eJob, failure(testName)),
		detailOnBase(2, "release-1.31", e2eJob, failure(testName)),
		detailOnBase(3, "release-1.31", e2eJob, failure(testName)),
		detailOnBase(4, "main", e2eJob, failure(testName)),
	}
	Annotate(peers, baselines, testRepo, nil)
	if got := peers[0].Checks[0].Failures[0].Attribution; got.Verdict != models.AttributionWidespread {
		t.Fatalf("two same-branch peers must still suffice without a baseline, got %+v", got)
	}
	if got := peers[3].Checks[0].Failures[0].Attribution; got.Verdict == models.AttributionWidespread {
		t.Fatalf("cross-branch peers counted for main: %+v", got)
	}
}

func TestBuildBaselineIgnoresPresubmitJobDetails(t *testing.T) {
	baseline := BuildBaseline([]models.JobDetail{{
		Name: e2eJob, JobType: models.JobTypePresubmit,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "main"}},
			TestCases: []models.TestCase{{Name: testName, Status: "failed"}},
		}},
	}}, models.FlakinessReport{}, testRepo)["main"]

	if baseline.Observed {
		t.Fatal("presubmit runs describe other pull requests, not the base branch")
	}
	if len(baseline.FailingOnBase) != 0 {
		t.Errorf("FailingOnBase = %+v", baseline.FailingOnBase)
	}
}

func TestBuildBaselineTakesOnlyFlakyClassifications(t *testing.T) {
	details := []models.JobDetail{
		{
			Name: "periodic-a", JobID: "periodic-a", JobType: models.JobTypePeriodic,
			Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{RepoRefs: map[string]string{"example/project": "main"}}}},
		},
		{
			Name: "periodic-b", JobID: "periodic-b", JobType: models.JobTypePeriodic,
			Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{RepoRefs: map[string]string{"example/project": "main"}}}},
		},
	}
	report := models.FlakinessReport{
		MostFlaky: []models.TestFlakiness{
			{TestName: "flaky-test", JobName: "periodic-a", JobID: "periodic-a", Classification: models.ClassificationFlaky},
		},
		PersistentFailures: []models.TestFlakiness{
			{TestName: "broken-test", JobName: "periodic-b", JobID: "periodic-b", Classification: models.ClassificationPersistent},
		},
	}
	baseline := BuildBaseline(details, report, testRepo)["main"]

	if len(baseline.FlakyTests["flaky-test"]) != 1 {
		t.Errorf("flaky test not recorded: %+v", baseline.FlakyTests)
	}
	if _, ok := baseline.FlakyTests["broken-test"]; ok {
		t.Error("a persistent failure is not a flake")
	}
}

func TestBuildBaselineIgnoresPresubmitFlakiness(t *testing.T) {
	presubmitID := models.JobIDFor(models.JobTypePresubmit, "example/project", e2eJob)
	details := []models.JobDetail{{
		Name: basePeriodicJob, JobID: basePeriodicJob, JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "main"}},
			TestCases: []models.TestCase{{Name: testName, Status: "passed"}},
		}},
	}, {
		// A presubmit with no fetched runs must still be recognized.
		Name: e2eJob, JobID: presubmitID, JobType: models.JobTypePresubmit,
	}}
	report := models.FlakinessReport{MostFlaky: []models.TestFlakiness{
		{TestName: testName, JobName: e2eJob, JobID: presubmitID, Classification: models.ClassificationFlaky},
		{TestName: testName, JobName: basePeriodicJob, JobID: basePeriodicJob, Classification: models.ClassificationFlaky},
	}}

	baseline := BuildBaseline(details, report, testRepo)["main"]

	got := baseline.FlakyTests[testName]
	if len(got) != 1 || got[0] != basePeriodicJob {
		t.Fatalf("FlakyTests[%q] = %v, want only the base-branch job", testName, got)
	}
}

// JobIDFor deliberately keeps a periodic and a presubmit that share a name
// distinct, so filtering presubmits must not take the periodic with them.
func TestBuildBaselineKeepsSameNamedPeriodicFlakiness(t *testing.T) {
	const shared = "project-e2e"
	presubmitID := models.JobIDFor(models.JobTypePresubmit, "example/project", shared)
	details := []models.JobDetail{{
		Name: shared, JobID: models.JobIDFor(models.JobTypePeriodic, "", shared), JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "main"}},
			TestCases: []models.TestCase{{Name: testName, Status: "passed"}},
		}},
	}, {
		Name: shared, JobID: presubmitID, JobType: models.JobTypePresubmit,
	}}
	report := models.FlakinessReport{MostFlaky: []models.TestFlakiness{
		{TestName: testName, JobName: shared, JobID: models.JobIDFor(models.JobTypePeriodic, "", shared), Classification: models.ClassificationFlaky},
		{TestName: testName, JobName: shared, JobID: presubmitID, Classification: models.ClassificationFlaky},
	}}

	baseline := BuildBaseline(details, report, testRepo)["main"]

	if got := baseline.FlakyTests[testName]; len(got) != 1 || got[0] != shared {
		t.Fatalf("FlakyTests[%q] = %v, want the periodic sharing the presubmit's name", testName, got)
	}
}

// Attribution must not depend on discovery.include_presubmits, which decides only
// whether presubmits are published as dashboard rows.
func TestPublishedPresubmitsDoNotChangeAttribution(t *testing.T) {
	presubmitID := models.JobIDFor(models.JobTypePresubmit, "example/project", e2eJob)
	base := models.JobDetail{
		Name: basePeriodicJob, JobID: basePeriodicJob, JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "main"}},
			TestCases: []models.TestCase{{Name: testName, Status: "passed"}},
		}},
	}
	presubmit := models.JobDetail{
		Name: e2eJob, JobID: presubmitID, JobType: models.JobTypePresubmit,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "2"},
			TestCases: []models.TestCase{{Name: testName, Status: "failed"}},
		}},
	}
	presubmitFlake := models.TestFlakiness{
		TestName: testName, JobName: e2eJob, JobID: presubmitID, Classification: models.ClassificationFlaky,
	}

	periodicsOnly := BuildBaseline([]models.JobDetail{base}, models.FlakinessReport{}, testRepo)["main"]
	withPresubmits := BuildBaseline([]models.JobDetail{base, presubmit},
		models.FlakinessReport{MostFlaky: []models.TestFlakiness{presubmitFlake}}, testRepo)["main"]

	want := annotateOne(t, periodicsOnly, []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})
	got := annotateOne(t, withPresubmits, []models.PullRequestDetail{detail(1, e2eJob, failure(testName))})

	if want.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want the escalation-eligible unexplained", want.Verdict)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("publishing presubmits changed the attribution:\nwith    = %+v\nwithout = %+v", got, want)
	}
}

func TestBuildBaselineSkipsBuildLevelCasesFromTheBaseBranch(t *testing.T) {
	baseline := BuildBaseline([]models.JobDetail{{
		Name: basePeriodicJob, JobID: basePeriodicJob, JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", RepoRefs: map[string]string{"example/project": "main"}},
			TestCases: []models.TestCase{models.NewProwJobExecutionFailure(10)},
		}},
	}}, models.FlakinessReport{}, testRepo)["main"]

	if len(baseline.FailingOnBase) != 0 || len(baseline.KnownTests) != 0 {
		t.Fatalf("baseline = %+v, want build-level cases excluded", baseline)
	}
}
