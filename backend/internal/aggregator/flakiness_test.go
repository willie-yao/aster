package aggregator

import (
	"math"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// makeFlakyBuild creates a BuildResult with the provided tests.
func makeFlakyBuild(buildID string, started time.Time, passed bool, tests []models.TestCase) models.BuildResult {
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
			BuildID:         buildID,
			JobName:         "test-job",
			Started:         started,
			Finished:        started.Add(time.Duration(dur) * time.Second),
			Passed:          passed,
			Result:          result,
			DurationSeconds: dur,
			Commit:          "abc123",
			ProwURL:         "https://prow.example.com/" + buildID,
			BuildLogURL:     "https://logs.example.com/" + buildID,
		},
		TestCases:    tests,
		TestsTotal:   total,
		TestsPassed:  p,
		TestsFailed:  f,
		TestsSkipped: s,
	}
}

func makeTC(name, status string, duration float64, failMsg string) models.TestCase {
	return models.TestCase{
		Name:            name,
		Status:          status,
		DurationSeconds: duration,
		FailureMessage:  failMsg,
	}
}

var flakyBaseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func flakyHoursAgo(h int) time.Time {
	return flakyBaseTime.Add(-time.Duration(h) * time.Hour)
}

func TestComputeTestFlakiness_FlipRate(t *testing.T) {
	// fail, pass, fail, pass newest-first yields 3 flips over 3 transitions.
	runs := []models.BuildResult{
		makeFlakyBuild("4", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestA", "failed", 1.0, "err")}),
		makeFlakyBuild("3", flakyHoursAgo(2), true, []models.TestCase{makeTC("TestA", "passed", 1.5, "")}),
		makeFlakyBuild("2", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestA", "failed", 2.0, "err")}),
		makeFlakyBuild("1", flakyHoursAgo(4), true, []models.TestCase{makeTC("TestA", "passed", 1.0, "")}),
	}

	tf := ComputeTestFlakiness("TestA", "test-job", "test-job", runs)

	if tf.TotalRuns != 4 {
		t.Errorf("TotalRuns = %d, want 4", tf.TotalRuns)
	}
	if tf.Failures != 2 {
		t.Errorf("Failures = %d, want 2", tf.Failures)
	}
	if tf.Passes != 2 {
		t.Errorf("Passes = %d, want 2", tf.Passes)
	}
	if math.Abs(tf.FlipRate-1.0) > 0.001 {
		t.Errorf("FlipRate = %f, want 1.0", tf.FlipRate)
	}
	if math.Abs(tf.FailRate-0.5) > 0.001 {
		t.Errorf("FailRate = %f, want 0.5", tf.FailRate)
	}
}

func TestComputeTestFlakiness_NoFlips(t *testing.T) {
	// fail, fail, fail newest-first yields no flips.
	runs := []models.BuildResult{
		makeFlakyBuild("3", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestB", "failed", 1.0, "err")}),
		makeFlakyBuild("2", flakyHoursAgo(2), false, []models.TestCase{makeTC("TestB", "failed", 1.0, "err")}),
		makeFlakyBuild("1", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestB", "failed", 1.0, "err")}),
	}

	tf := ComputeTestFlakiness("TestB", "test-job", "test-job", runs)

	if tf.FlipRate != 0 {
		t.Errorf("FlipRate = %f, want 0", tf.FlipRate)
	}
	if tf.FailRate != 1.0 {
		t.Errorf("FailRate = %f, want 1.0", tf.FailRate)
	}
}

func TestComputeTestFlakiness_ConsecutiveFailures(t *testing.T) {
	// fail, fail, pass, fail newest-first yields 2 consecutive latest failures.
	runs := []models.BuildResult{
		makeFlakyBuild("4", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestC", "failed", 1.0, "err4")}),
		makeFlakyBuild("3", flakyHoursAgo(2), false, []models.TestCase{makeTC("TestC", "failed", 1.0, "err3")}),
		makeFlakyBuild("2", flakyHoursAgo(3), true, []models.TestCase{makeTC("TestC", "passed", 1.0, "")}),
		makeFlakyBuild("1", flakyHoursAgo(4), false, []models.TestCase{makeTC("TestC", "failed", 1.0, "err1")}),
	}

	tf := ComputeTestFlakiness("TestC", "test-job", "test-job", runs)

	if tf.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", tf.ConsecutiveFailures)
	}
	// FirstFailedAt should be the oldest failure in the streak.
	expectedTime := flakyHoursAgo(2).UTC().Format(time.RFC3339)
	if tf.FirstFailedAt != expectedTime {
		t.Errorf("FirstFailedAt = %q, want %q", tf.FirstFailedAt, expectedTime)
	}
}

func TestComputeTestFlakiness_Classification(t *testing.T) {
	// Five consecutive failures classify as persistent.
	runs := []models.BuildResult{
		makeFlakyBuild("5", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestD", "failed", 1.0, "timeout")}),
		makeFlakyBuild("4", flakyHoursAgo(2), false, []models.TestCase{makeTC("TestD", "failed", 1.0, "timeout")}),
		makeFlakyBuild("3", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestD", "failed", 1.0, "timeout")}),
		makeFlakyBuild("2", flakyHoursAgo(4), false, []models.TestCase{makeTC("TestD", "failed", 1.0, "timeout")}),
		makeFlakyBuild("1", flakyHoursAgo(5), false, []models.TestCase{makeTC("TestD", "failed", 1.0, "timeout")}),
	}

	tf := ComputeTestFlakiness("TestD", "test-job", "test-job", runs)

	if tf.Classification != models.ClassificationPersistent {
		t.Errorf("Classification = %q, want %q", tf.Classification, models.ClassificationPersistent)
	}
}

func TestComputeTestFlakiness_ErrorPatternGrouping(t *testing.T) {
	// Two different error messages that normalize to the same thing, plus one different.
	runs := []models.BuildResult{
		makeFlakyBuild("3", flakyHoursAgo(1), false, []models.TestCase{
			makeTC("TestE", "failed", 1.0, "Expected 42 pods but got 3 pods"),
		}),
		makeFlakyBuild("2", flakyHoursAgo(2), false, []models.TestCase{
			makeTC("TestE", "failed", 1.0, "Expected 10 pods but got 1 pods"),
		}),
		makeFlakyBuild("1", flakyHoursAgo(3), false, []models.TestCase{
			makeTC("TestE", "failed", 1.0, "connection refused"),
		}),
	}

	tf := ComputeTestFlakiness("TestE", "test-job", "test-job", runs)

	if len(tf.ErrorPatterns) != 2 {
		t.Fatalf("ErrorPatterns length = %d, want 2", len(tf.ErrorPatterns))
	}
	// First pattern is sorted by count desc and should have count 2.
	if tf.ErrorPatterns[0].Count != 2 {
		t.Errorf("ErrorPatterns[0].Count = %d, want 2", tf.ErrorPatterns[0].Count)
	}
	if tf.ErrorPatterns[1].Count != 1 {
		t.Errorf("ErrorPatterns[1].Count = %d, want 1", tf.ErrorPatterns[1].Count)
	}
}

func TestComputeTestFlakiness_DurationHistory(t *testing.T) {
	runs := []models.BuildResult{
		makeFlakyBuild("2", flakyHoursAgo(1), true, []models.TestCase{makeTC("TestF", "passed", 5.5, "")}),
		makeFlakyBuild("1", flakyHoursAgo(2), false, []models.TestCase{makeTC("TestF", "failed", 10.0, "err")}),
	}

	tf := ComputeTestFlakiness("TestF", "test-job", "test-job", runs)

	if len(tf.DurationHistory) != 2 {
		t.Fatalf("DurationHistory length = %d, want 2", len(tf.DurationHistory))
	}
	if tf.DurationHistory[0].BuildID != "2" || tf.DurationHistory[0].Duration != 5.5 || !tf.DurationHistory[0].Passed {
		t.Errorf("DurationHistory[0] = %+v, unexpected", tf.DurationHistory[0])
	}
	if tf.DurationHistory[1].BuildID != "1" || tf.DurationHistory[1].Duration != 10.0 || tf.DurationHistory[1].Passed {
		t.Errorf("DurationHistory[1] = %+v, unexpected", tf.DurationHistory[1])
	}
}

func TestComputeTestFlakiness_LastFailure(t *testing.T) {
	runs := []models.BuildResult{
		makeFlakyBuild("3", flakyHoursAgo(1), true, []models.TestCase{makeTC("TestG", "passed", 1.0, "")}),
		makeFlakyBuild("2", flakyHoursAgo(2), false, []models.TestCase{makeTC("TestG", "failed", 1.0, "boom")}),
		makeFlakyBuild("1", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestG", "failed", 1.0, "crash")}),
	}

	tf := ComputeTestFlakiness("TestG", "test-job", "test-job", runs)

	if tf.LastFailure == nil {
		t.Fatal("LastFailure should not be nil")
	}
	if tf.LastFailure.BuildID != "2" {
		t.Errorf("LastFailure.BuildID = %q, want %q", tf.LastFailure.BuildID, "2")
	}
	if tf.LastFailure.FailureMessage != "boom" {
		t.Errorf("LastFailure.FailureMessage = %q, want %q", tf.LastFailure.FailureMessage, "boom")
	}
}

func TestComputeTestFlakiness_SingleRun(t *testing.T) {
	runs := []models.BuildResult{
		makeFlakyBuild("1", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestH", "failed", 1.0, "err")}),
	}

	tf := ComputeTestFlakiness("TestH", "test-job", "test-job", runs)

	if tf.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1", tf.TotalRuns)
	}
	if tf.FlipRate != 0 {
		t.Errorf("FlipRate = %f, want 0 (only 1 run)", tf.FlipRate)
	}
	if tf.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", tf.ConsecutiveFailures)
	}
}

func TestComputeTestFlakiness_TestNotInAllRuns(t *testing.T) {
	// Test only appears in 2 of 3 runs.
	runs := []models.BuildResult{
		makeFlakyBuild("3", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestI", "failed", 1.0, "err")}),
		makeFlakyBuild("2", flakyHoursAgo(2), true, []models.TestCase{makeTC("OtherTest", "passed", 1.0, "")}),
		makeFlakyBuild("1", flakyHoursAgo(3), true, []models.TestCase{makeTC("TestI", "passed", 1.0, "")}),
	}

	tf := ComputeTestFlakiness("TestI", "test-job", "test-job", runs)

	if tf.TotalRuns != 2 {
		t.Errorf("TotalRuns = %d, want 2", tf.TotalRuns)
	}
}

func TestComputeFlakinessReport_MostFlakySorting(t *testing.T) {
	now := flakyBaseTime
	// Two tests: one very flaky (flip_rate=1.0), one less flaky (flip_rate=0.5).
	jobResults := map[string][]models.BuildResult{
		"job1": {
			makeFlakyBuild("4", flakyHoursAgo(1), false, []models.TestCase{
				makeTC("HighFlip", "failed", 1.0, "err"),
				makeTC("LowFlip", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("3", flakyHoursAgo(2), true, []models.TestCase{
				makeTC("HighFlip", "passed", 1.0, ""),
				makeTC("LowFlip", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("2", flakyHoursAgo(3), false, []models.TestCase{
				makeTC("HighFlip", "failed", 1.0, "err"),
				makeTC("LowFlip", "passed", 1.0, ""),
			}),
			makeFlakyBuild("1", flakyHoursAgo(4), true, []models.TestCase{
				makeTC("HighFlip", "passed", 1.0, ""),
				makeTC("LowFlip", "passed", 1.0, ""),
			}),
		},
	}

	report := ComputeFlakinessReport(jobResults, nil, now)

	if len(report.MostFlaky) != 2 {
		t.Fatalf("MostFlaky length = %d, want 2", len(report.MostFlaky))
	}
	if report.MostFlaky[0].TestName != "HighFlip" {
		t.Errorf("MostFlaky[0] = %q, want HighFlip (highest flip rate)", report.MostFlaky[0].TestName)
	}
	if report.MostFlaky[1].TestName != "LowFlip" {
		t.Errorf("MostFlaky[1] = %q, want LowFlip", report.MostFlaky[1].TestName)
	}
}

func TestComputeFlakinessReport_PersistentFailures(t *testing.T) {
	now := flakyBaseTime
	jobResults := map[string][]models.BuildResult{
		"job1": {
			makeFlakyBuild("4", flakyHoursAgo(1), false, []models.TestCase{
				makeTC("PersistTest", "failed", 1.0, "err"),
				makeTC("OkTest", "passed", 1.0, ""),
			}),
			makeFlakyBuild("3", flakyHoursAgo(2), false, []models.TestCase{
				makeTC("PersistTest", "failed", 1.0, "err"),
				makeTC("OkTest", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("2", flakyHoursAgo(3), false, []models.TestCase{
				makeTC("PersistTest", "failed", 1.0, "err"),
				makeTC("OkTest", "passed", 1.0, ""),
			}),
			makeFlakyBuild("1", flakyHoursAgo(4), false, []models.TestCase{
				makeTC("PersistTest", "failed", 1.0, "err"),
				makeTC("OkTest", "passed", 1.0, ""),
			}),
		},
	}

	report := ComputeFlakinessReport(jobResults, nil, now)

	if len(report.PersistentFailures) != 1 {
		t.Fatalf("PersistentFailures length = %d, want 1", len(report.PersistentFailures))
	}
	if report.PersistentFailures[0].TestName != "PersistTest" {
		t.Errorf("PersistentFailures[0] = %q, want PersistTest", report.PersistentFailures[0].TestName)
	}
	if report.PersistentFailures[0].ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d, want 4", report.PersistentFailures[0].ConsecutiveFailures)
	}
}

func TestComputeFlakinessReport_RecentlyBroken(t *testing.T) {
	now := flakyBaseTime
	// TestRecent started failing 1 hour ago, within 48h.
	// TestOld has been failing since 72 hours ago, outside 48h.
	jobResults := map[string][]models.BuildResult{
		"job1": {
			makeFlakyBuild("4", flakyHoursAgo(1), false, []models.TestCase{
				makeTC("TestRecent", "failed", 1.0, "err"),
				makeTC("TestOld", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("3", flakyHoursAgo(2), true, []models.TestCase{
				makeTC("TestRecent", "passed", 1.0, ""),
				makeTC("TestOld", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("2", flakyHoursAgo(60), false, []models.TestCase{
				makeTC("TestRecent", "passed", 1.0, ""),
				makeTC("TestOld", "failed", 1.0, "err"),
			}),
			makeFlakyBuild("1", flakyHoursAgo(72), true, []models.TestCase{
				makeTC("TestRecent", "passed", 1.0, ""),
				makeTC("TestOld", "passed", 1.0, ""),
			}),
		},
	}

	report := ComputeFlakinessReport(jobResults, nil, now)

	if len(report.RecentlyBroken) != 1 {
		t.Fatalf("RecentlyBroken length = %d, want 1", len(report.RecentlyBroken))
	}
	if report.RecentlyBroken[0].TestName != "TestRecent" {
		t.Errorf("RecentlyBroken[0] = %q, want TestRecent", report.RecentlyBroken[0].TestName)
	}
}

func TestComputeFlakinessReport_ExcludesPassingTests(t *testing.T) {
	now := flakyBaseTime
	jobResults := map[string][]models.BuildResult{
		"job1": {
			makeFlakyBuild("2", flakyHoursAgo(1), true, []models.TestCase{
				makeTC("AlwaysPass", "passed", 1.0, ""),
			}),
			makeFlakyBuild("1", flakyHoursAgo(2), true, []models.TestCase{
				makeTC("AlwaysPass", "passed", 1.0, ""),
			}),
		},
	}

	report := ComputeFlakinessReport(jobResults, nil, now)

	if len(report.MostFlaky) != 0 {
		t.Errorf("MostFlaky length = %d, want 0 (no failures)", len(report.MostFlaky))
	}
	if len(report.PersistentFailures) != 0 {
		t.Errorf("PersistentFailures length = %d, want 0", len(report.PersistentFailures))
	}
}

func TestComputeFlakinessReport_MultipleJobs(t *testing.T) {
	now := flakyBaseTime
	jobResults := map[string][]models.BuildResult{
		"job1": {
			makeFlakyBuild("3", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestX", "failed", 1.0, "err")}),
			makeFlakyBuild("2", flakyHoursAgo(2), true, []models.TestCase{makeTC("TestX", "passed", 1.0, "")}),
			makeFlakyBuild("1", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestX", "failed", 1.0, "err")}),
		},
		"job2": {
			makeFlakyBuild("3", flakyHoursAgo(1), false, []models.TestCase{makeTC("TestY", "failed", 1.0, "err")}),
			makeFlakyBuild("2", flakyHoursAgo(2), true, []models.TestCase{makeTC("TestY", "passed", 1.0, "")}),
			makeFlakyBuild("1", flakyHoursAgo(3), false, []models.TestCase{makeTC("TestY", "failed", 1.0, "err")}),
		},
	}

	report := ComputeFlakinessReport(jobResults, []models.ProwJob{
		{Name: "job1", JobID: "job1", JobType: models.JobTypePeriodic},
		{Name: "job2", JobID: "job2", JobType: models.JobTypePeriodic},
	}, now)

	if len(report.MostFlaky) != 2 {
		t.Fatalf("MostFlaky length = %d, want 2 (one from each job)", len(report.MostFlaky))
	}

	// Both jobs should be represented.
	jobs := make(map[string]bool)
	for _, tf := range report.MostFlaky {
		jobs[tf.JobName] = true
	}
	if !jobs["job1"] || !jobs["job2"] {
		t.Errorf("Expected both jobs represented, got %v", jobs)
	}
}

func TestComputeFlakinessReport_GeneratedAt(t *testing.T) {
	now := flakyBaseTime
	report := ComputeFlakinessReport(nil, nil, now)

	expected := now.UTC().Format(time.RFC3339)
	if report.GeneratedAt != expected {
		t.Errorf("GeneratedAt = %q, want %q", report.GeneratedAt, expected)
	}
}
