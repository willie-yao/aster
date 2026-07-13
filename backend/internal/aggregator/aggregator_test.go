package aggregator

import (
	"fmt"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// makeBuild creates a BuildResult with sensible defaults.
func makeBuild(id string, started time.Time, passed bool, tests []models.TestCase) models.BuildResult {
	dur := 300.0
	total := len(tests)
	p, f, s := 0, 0, 0
	for _, tc := range tests {
		switch tc.Status {
		case "passed":
			p++
		case "failed":
			f++
		case "skipped":
			s++
		}
	}
	result := "SUCCESS"
	if !passed {
		result = "FAILURE"
	}
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID:         id,
			JobName:         "test-job",
			Started:         started,
			Finished:        started.Add(time.Duration(dur) * time.Second),
			Passed:          passed,
			Result:          result,
			DurationSeconds: dur,
			Commit:          "abc123",
			ProwURL:         "https://prow.example.com/" + id,
			BuildLogURL:     "https://logs.example.com/" + id,
		},
		TestCases:    tests,
		TestsTotal:   total,
		TestsPassed:  p,
		TestsFailed:  f,
		TestsSkipped: s,
	}
}

var baseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func hoursAgo(h int) time.Time {
	return baseTime.Add(-time.Duration(h) * time.Hour)
}

func TestComputeJobSummary_AllPassing(t *testing.T) {
	job := models.ProwJob{Name: "job-pass", Category: "e2e"}
	runs := []models.BuildResult{
		makeBuild("5", hoursAgo(1), true, nil),
		makeBuild("4", hoursAgo(2), true, nil),
		makeBuild("3", hoursAgo(3), true, nil),
		makeBuild("2", hoursAgo(4), true, nil),
	}

	s := ComputeJobSummary(job, runs)

	if s.OverallStatus != "PASSING" {
		t.Errorf("expected PASSING, got %s", s.OverallStatus)
	}
	if s.LastRun == nil || s.LastRun.BuildID != "5" {
		t.Errorf("expected LastRun.BuildID=5, got %v", s.LastRun)
	}
	if len(s.RecentRuns) != 4 {
		t.Errorf("expected 4 recent runs, got %d", len(s.RecentRuns))
	}
}

func TestComputeJobSummary_AllFailing(t *testing.T) {
	job := models.ProwJob{Name: "job-fail"}
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), false, nil),
		makeBuild("2", hoursAgo(2), false, nil),
		makeBuild("1", hoursAgo(3), false, nil),
	}

	s := ComputeJobSummary(job, runs)

	if s.OverallStatus != "FAILING" {
		t.Errorf("expected FAILING, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_Mixed(t *testing.T) {
	job := models.ProwJob{Name: "job-flaky"}
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), true, nil),
		makeBuild("2", hoursAgo(2), false, nil),
		makeBuild("1", hoursAgo(3), true, nil),
	}

	s := ComputeJobSummary(job, runs)

	if s.OverallStatus != "FLAKY" {
		t.Errorf("expected FLAKY, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_EmptyRuns(t *testing.T) {
	job := models.ProwJob{Name: "job-empty"}
	s := ComputeJobSummary(job, nil)

	if s.LastRun != nil {
		t.Error("expected nil LastRun for empty runs")
	}
	if len(s.RecentRuns) != 0 {
		t.Errorf("expected 0 recent runs, got %d", len(s.RecentRuns))
	}
	if s.PassRateRecent != 0 {
		t.Errorf("expected 0 pass rate, got %.2f", s.PassRateRecent)
	}
}

func TestComputeJobSummary_FewerThan3Runs_AllPass(t *testing.T) {
	job := models.ProwJob{Name: "job-two"}
	runs := []models.BuildResult{
		makeBuild("2", hoursAgo(1), true, nil),
		makeBuild("1", hoursAgo(2), true, nil),
	}

	s := ComputeJobSummary(job, runs)

	if s.OverallStatus != "PASSING" {
		t.Errorf("expected PASSING with 2 passing runs, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_FewerThan3Runs_OneFail(t *testing.T) {
	job := models.ProwJob{Name: "job-one-fail"}
	runs := []models.BuildResult{
		makeBuild("1", hoursAgo(1), false, nil),
	}

	s := ComputeJobSummary(job, runs)

	// With one failed run, the recent pass rate is 0 and status is FAILING.
	if s.OverallStatus != "FAILING" {
		t.Errorf("expected FAILING with single failed run, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_PassRates(t *testing.T) {
	job := models.ProwJob{Name: "job-rates"}
	// 12 runs newest-first. The last 10 hold 8 passes and 2 fails (0.8). The two
	// oldest runs fail but fall outside the 10-run window and must be excluded.
	runs := make([]models.BuildResult, 0, 12)
	for i := 0; i < 12; i++ {
		pass := i != 2 && i != 5 && i != 10 && i != 11
		runs = append(runs, makeBuild(fmt.Sprintf("%d", 12-i), hoursAgo(i), pass, nil))
	}

	s := ComputeJobSummary(job, runs)

	// Last 10 runs are 8 pass / 10 = 0.8; failing tail runs are ignored.
	if s.PassRateRecent < 0.79 || s.PassRateRecent > 0.81 {
		t.Errorf("expected PassRateRecent ~0.8 over the last 10 runs, got %.4f", s.PassRateRecent)
	}
	// 0.8 is between the thresholds, so status is FLAKY.
	if s.OverallStatus != "FLAKY" {
		t.Errorf("expected FLAKY at 0.8 pass rate, got %s", s.OverallStatus)
	}
}

func TestComputeOverallStatus_Thresholds(t *testing.T) {
	// passes is the number of passing runs among 10; the rest fail.
	cases := []struct {
		name   string
		passes int
		want   string
	}{
		{"all pass", 10, "PASSING"},
		{"one fail still passing", 9, "PASSING"},
		{"two fail is flaky", 8, "FLAKY"},
		{"four pass is flaky", 4, "FLAKY"},
		{"three pass is failing", 3, "FAILING"},
		{"all fail", 0, "FAILING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := make([]models.BuildResult, 10)
			for i := range runs {
				runs[i] = makeBuild(fmt.Sprintf("%d", 10-i), hoursAgo(i), i < tc.passes, nil)
			}
			if got := computeOverallStatus(runs); got != tc.want {
				t.Errorf("%d/10 passing: expected %s, got %s", tc.passes, tc.want, got)
			}
		})
	}
}

func TestRecentPassRate_LimitsToN(t *testing.T) {
	// 15 runs where only the most recent 10 pass; older ones fail. Rate is 1.0.
	runs := make([]models.BuildResult, 15)
	for i := range runs {
		runs[i] = makeBuild(fmt.Sprintf("%d", 15-i), hoursAgo(i), i < 10, nil)
	}
	if got := recentPassRate(runs, passRateRecentRuns); got != 1.0 {
		t.Errorf("expected 1.0 over the most recent 10 runs, got %.4f", got)
	}
	// Fewer runs than the window: average over what exists.
	short := []models.BuildResult{
		makeBuild("2", hoursAgo(1), true, nil),
		makeBuild("1", hoursAgo(2), false, nil),
	}
	if got := recentPassRate(short, passRateRecentRuns); got != 0.5 {
		t.Errorf("expected 0.5 over 2 runs, got %.4f", got)
	}
}

func TestComputeJobSummary_RecentRunsCapped(t *testing.T) {
	job := models.ProwJob{Name: "job-many"}
	runs := make([]models.BuildResult, 25)
	for i := range runs {
		runs[i] = makeBuild(
			fmt.Sprintf("%d", 25-i),
			hoursAgo(i),
			true,
			nil,
		)
	}

	s := ComputeJobSummary(job, runs)

	if len(s.RecentRuns) != 20 {
		t.Errorf("expected 20 recent runs (capped), got %d", len(s.RecentRuns))
	}
}

func TestBuildRunSummary(t *testing.T) {
	br := makeBuild("42", baseTime, true, []models.TestCase{
		{Name: "t1", Status: "passed", DurationSeconds: 1.0},
		{Name: "t2", Status: "failed", DurationSeconds: 2.0, FailureMessage: "boom"},
	})

	rs := BuildRunSummary(br)

	if rs.BuildID != "42" {
		t.Errorf("BuildID mismatch: %s", rs.BuildID)
	}
	if !rs.Passed {
		t.Error("expected Passed=true")
	}
	if rs.Result != "SUCCESS" {
		t.Errorf("expected Result=SUCCESS, got %q", rs.Result)
	}
	if rs.TestsTotal != 2 || rs.TestsPassed != 1 || rs.TestsFailed != 1 {
		t.Errorf("test counts wrong: total=%d passed=%d failed=%d", rs.TestsTotal, rs.TestsPassed, rs.TestsFailed)
	}
}

func makeTestCase(name, status, failMsg string) models.TestCase {
	return models.TestCase{
		Name:            name,
		Status:          status,
		DurationSeconds: 1.0,
		FailureMessage:  failMsg,
	}
}

func TestClassifyFailure_Persistent(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("5", hoursAgo(1), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("4", hoursAgo(2), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("3", hoursAgo(3), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("2", hoursAgo(4), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("1", hoursAgo(5), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
	}

	info := ClassifyFailure("SomeTest", runs, 3)

	if info.Classification != models.ClassificationPersistent {
		t.Errorf("expected persistent, got %s", info.Classification)
	}
	if info.ConsecutiveFailures != 5 {
		t.Errorf("expected 5 consecutive failures, got %d", info.ConsecutiveFailures)
	}
}

func TestClassifyFailure_Flaky(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("4", hoursAgo(1), false, []models.TestCase{makeTestCase("FlakyTest", "failed", "err")}),
		makeBuild("3", hoursAgo(2), true, []models.TestCase{makeTestCase("FlakyTest", "passed", "")}),
		makeBuild("2", hoursAgo(3), false, []models.TestCase{makeTestCase("FlakyTest", "failed", "err")}),
		makeBuild("1", hoursAgo(4), true, []models.TestCase{makeTestCase("FlakyTest", "passed", "")}),
	}

	info := ClassifyFailure("FlakyTest", runs, 3)

	if info.Classification != models.ClassificationFlaky {
		t.Errorf("expected flaky, got %s", info.Classification)
	}
}

func TestClassifyFailure_OneOff(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), false, []models.TestCase{makeTestCase("OneOffTest", "failed", "blip")}),
		makeBuild("2", hoursAgo(2), true, []models.TestCase{makeTestCase("OneOffTest", "passed", "")}),
		makeBuild("1", hoursAgo(3), true, []models.TestCase{makeTestCase("OneOffTest", "passed", "")}),
	}

	info := ClassifyFailure("OneOffTest", runs, 3)

	if info.Classification != models.ClassificationOneOff {
		t.Errorf("expected one-off, got %s", info.Classification)
	}
}

func TestClassifyFailure_LatestPasses(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), true, []models.TestCase{makeTestCase("GoodTest", "passed", "")}),
		makeBuild("2", hoursAgo(2), false, []models.TestCase{makeTestCase("GoodTest", "failed", "old")}),
		makeBuild("1", hoursAgo(3), true, []models.TestCase{makeTestCase("GoodTest", "passed", "")}),
	}

	info := ClassifyFailure("GoodTest", runs, 3)

	// consecutiveFailures == 0 with one historical failure is one-off.
	if info.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", info.ConsecutiveFailures)
	}
	if info.Classification != models.ClassificationOneOff {
		t.Errorf("expected one-off (single historical failure), got %s", info.Classification)
	}
}

func TestNormalizeErrorMessage_Timestamps(t *testing.T) {
	msg := "failed at 2026-03-15T10:30:00Z with error"
	got := NormalizeErrorMessage(msg)
	expected := "failed at <timestamp> with error"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_NumericIDs(t *testing.T) {
	msg := "Expected 42 pods but got 3 pods"
	got := NormalizeErrorMessage(msg)
	expected := "Expected <num> pods but got <num> pods"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_WhitespaceCollapsed(t *testing.T) {
	msg := "  too   many    spaces   "
	got := NormalizeErrorMessage(msg)
	expected := "too many spaces"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_Combined(t *testing.T) {
	msg1 := "Expected <int>: 2 to equal <int>: 3"
	msg2 := "Expected <int>: 1 to equal <int>: 3"
	n1 := NormalizeErrorMessage(msg1)
	n2 := NormalizeErrorMessage(msg2)
	if n1 != n2 {
		t.Errorf("expected same normalized form, got %q vs %q", n1, n2)
	}
}

func TestHashError_SameInput(t *testing.T) {
	h1 := HashError("some error message")
	h2 := HashError("some error message")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: %s vs %s", h1, h2)
	}
	if len(h1) != 8 {
		t.Errorf("expected 8 char hash, got %d chars: %s", len(h1), h1)
	}
}

func TestHashError_DifferentInput(t *testing.T) {
	h1 := HashError("error A")
	h2 := HashError("error B")
	if h1 == h2 {
		t.Errorf("different inputs should (almost certainly) produce different hashes: %s", h1)
	}
}
