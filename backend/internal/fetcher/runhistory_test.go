package fetcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func historyRun(buildID string, started time.Time, passed bool) models.BuildResult {
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID: buildID, Started: started, Passed: passed, Result: "SUCCESS",
			JUnitComplete: true,
		},
		TestCases: []models.TestCase{{Name: "TestOne", Status: "passed"}},
	}
}

func buildIDs(runs []models.BuildResult) []string {
	out := make([]string, len(runs))
	for i, run := range runs {
		out[i] = run.BuildID
	}
	return out
}

// TestSelectRetainedRuns_KeepsOnlyBuildsOutsideTheWindow pins that retention is
// the complement of the analysis window, so no build is published twice.
func TestSelectRetainedRuns_KeepsOnlyBuildsOutsideTheWindow(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	window := []models.BuildResult{
		historyRun("5", base.Add(5*time.Hour), true),
		historyRun("4", base.Add(4*time.Hour), false),
	}
	history := []models.BuildResult{
		historyRun("5", base.Add(5*time.Hour), true),
		historyRun("4", base.Add(4*time.Hour), false),
		historyRun("3", base.Add(3*time.Hour), false),
		historyRun("2", base.Add(2*time.Hour), true),
	}

	got := selectRetainedRuns(window, history)
	if want := []string{"3", "2"}; fmt.Sprint(buildIDs(got)) != fmt.Sprint(want) {
		t.Fatalf("retained = %v, want %v", buildIDs(got), want)
	}
}

// TestSelectRetainedRuns_DropsTestCases pins that an aged-out build costs only
// its metadata, and that the counts the run metadata panel renders survive.
func TestSelectRetainedRuns_DropsTestCases(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	older := historyRun("1", base, false)
	older.TestsTotal, older.TestsFailed = 12, 3

	got := selectRetainedRuns(nil, []models.BuildResult{older})
	if len(got) != 1 {
		t.Fatalf("retained = %d entries, want 1", len(got))
	}
	if got[0].TestCases != nil {
		t.Errorf("TestCases = %v, want nil", got[0].TestCases)
	}
	if got[0].TestsTotal != 12 || got[0].TestsFailed != 3 {
		t.Errorf("counts = %d/%d, want 12/3", got[0].TestsTotal, got[0].TestsFailed)
	}
}

// TestSelectRetainedRuns_BoundsTotalHistory pins that the window and retention
// together stay under maxRunHistory, oldest builds dropping first.
func TestSelectRetainedRuns_BoundsTotalHistory(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	window := make([]models.BuildResult, 0, 10)
	for i := range 10 {
		window = append(window, historyRun(fmt.Sprintf("w%02d", i), base.Add(time.Duration(100-i)*time.Hour), true))
	}
	history := make([]models.BuildResult, 0, 60)
	for i := range 60 {
		history = append(history, historyRun(fmt.Sprintf("h%02d", i), base.Add(time.Duration(60-i)*time.Hour), true))
	}

	got := selectRetainedRuns(window, history)
	if want := maxRunHistory - len(window); len(got) != want {
		t.Fatalf("retained = %d entries, want %d", len(got), want)
	}
	if got[0].BuildID != "h00" {
		t.Errorf("newest retained = %s, want h00", got[0].BuildID)
	}
}

// TestSelectRetainedRuns_SkipsUnfinishedBuilds pins that a build is retained only
// once it has a final result, so the strip never freezes a stale PENDING dot.
func TestSelectRetainedRuns_SkipsUnfinishedBuilds(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	pending := historyRun("2", base.Add(time.Hour), false)
	pending.Result = "PENDING"

	got := selectRetainedRuns(nil, []models.BuildResult{pending, historyRun("1", base, true)})
	if want := []string{"1"}; fmt.Sprint(buildIDs(got)) != fmt.Sprint(want) {
		t.Fatalf("retained = %v, want %v", buildIDs(got), want)
	}
}

// TestRetainedRunsFromDetails_CarriesBothPools pins that a published detail hands
// its window and its prior retention to the next pass, which is what lets history
// accumulate past one fetch window.
func TestRetainedRunsFromDetails_CarriesBothPools(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	details := map[string]models.JobDetail{"job": {
		JobID:        "job",
		Runs:         []models.BuildResult{historyRun("3", base.Add(3*time.Hour), true)},
		RetainedRuns: []models.BuildResult{historyRun("2", base.Add(2*time.Hour), false)},
	}}

	history := retainedRunsFromDetails(details)["job"]
	if want := []string{"3", "2"}; fmt.Sprint(buildIDs(history)) != fmt.Sprint(want) {
		t.Fatalf("history = %v, want %v", buildIDs(history), want)
	}
	for _, run := range history {
		if run.TestCases != nil {
			t.Errorf("build %s kept test cases", run.BuildID)
		}
	}
}

// TestRetentionAccumulatesAcrossPasses pins the behavior the run history strip
// depends on: a build that slides out of the fetch window stays plottable, so the
// visible arc keeps growing while the analysis window stays at its configured width.
func TestRetentionAccumulatesAcrossPasses(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	published := models.JobDetail{JobID: "job", Runs: []models.BuildResult{
		historyRun("2", base.Add(2*time.Hour), false),
		historyRun("1", base.Add(time.Hour), true),
	}}

	// The next pass fetches a two-build window that has moved past build 1.
	window := []models.BuildResult{
		historyRun("4", base.Add(4*time.Hour), true),
		historyRun("3", base.Add(3*time.Hour), false),
	}
	history := retainedRunsFromDetails(map[string]models.JobDetail{"job": published})["job"]

	retained := selectRetainedRuns(window, history)
	if want := []string{"2", "1"}; fmt.Sprint(buildIDs(retained)) != fmt.Sprint(want) {
		t.Fatalf("retained = %v, want %v", buildIDs(retained), want)
	}
	if len(window) != 2 {
		t.Errorf("analysis window = %d runs, want 2 (retention must not widen it)", len(window))
	}
}
