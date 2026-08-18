package aggregator

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

// attentionRuns builds a single job's newest-first runs for testName where
// pattern lists each run's outcome newest-first: true is a pass.
func attentionRuns(testName string, pattern []bool) []models.BuildResult {
	runs := make([]models.BuildResult, 0, len(pattern))
	for i, passed := range pattern {
		status, msg := "failed", "boom"
		if passed {
			status, msg = "passed", ""
		}
		runs = append(runs, makeFlakyBuild(
			string(rune('a'+i)),
			flakyHoursAgo(i+1),
			passed,
			[]models.TestCase{makeTC(testName, status, 1.0, msg)},
		))
	}
	return runs
}

func attentionReport(t *testing.T, pattern []bool, settings Settings) models.FlakinessReport {
	t.Helper()
	return ComputeFlakinessReport(
		map[string][]models.BuildResult{"test-job": attentionRuns("TestA", pattern)},
		[]models.ProwJob{{Name: "test-job", JobID: "test-job"}},
		flakyBaseTime,
		settings,
	)
}

func lowPassRateRule(threshold float64, minRuns int) *LowPassRateRule {
	return &LowPassRateRule{Threshold: threshold, MinRuns: minRuns}
}

// TestLowPassRate_FullCutoffSurfacesSingleFailure covers the request behind the
// feature: a cutoff of 100% surfaces a test that failed exactly once.
func TestLowPassRate_FullCutoffSurfacesSingleFailure(t *testing.T) {
	pattern := []bool{true, true, true, false, true, true}
	report := attentionReport(t, pattern, Settings{LowPassRate: lowPassRateRule(1, 5)})

	if len(report.LowPassRate) != 1 {
		t.Fatalf("LowPassRate = %d entries, want 1", len(report.LowPassRate))
	}
	entry := report.LowPassRate[0]
	if entry.TestName != "TestA" {
		t.Errorf("TestName = %q, want TestA", entry.TestName)
	}
	if entry.WindowRuns != len(pattern) {
		t.Errorf("WindowRuns = %d, want %d", entry.WindowRuns, len(pattern))
	}
	if want := 5.0 / 6.0; entry.PassRate != want {
		t.Errorf("PassRate = %v, want %v", entry.PassRate, want)
	}
}

// TestLowPassRate_ZeroCutoffSurfacesNothing is the other end of the range: a
// cutoff of 0 can never be undercut, so no test is selected.
func TestLowPassRate_ZeroCutoffSurfacesNothing(t *testing.T) {
	report := attentionReport(t, []bool{false, false, false, false, false}, Settings{
		LowPassRate: lowPassRateRule(0, 5),
	})
	if len(report.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0", len(report.LowPassRate))
	}
}

// TestLowPassRate_DisabledByDefault pins that omitting the rule leaves the
// section empty rather than defaulting a consumer into new behavior.
func TestLowPassRate_DisabledByDefault(t *testing.T) {
	report := attentionReport(t, []bool{true, true, true, false, true, true}, Settings{})
	if len(report.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0 when unconfigured", len(report.LowPassRate))
	}
}

// TestLowPassRate_DoesNotAlterClassification pins that selection is a display
// rule: the published classification is unchanged by the cutoff.
func TestLowPassRate_DoesNotAlterClassification(t *testing.T) {
	pattern := []bool{true, true, true, false, true, true}
	selected := attentionReport(t, pattern, Settings{LowPassRate: lowPassRateRule(1, 5)})
	if len(selected.LowPassRate) != 1 {
		t.Fatalf("LowPassRate = %d entries, want 1", len(selected.LowPassRate))
	}
	if got := selected.LowPassRate[0].Classification; got != models.ClassificationOneOff {
		t.Errorf("Classification = %q, want %q", got, models.ClassificationOneOff)
	}
	if len(selected.PersistentFailures) != 0 || len(selected.MostFlaky) != 0 {
		t.Errorf("selection leaked into classification sections: %+v", selected)
	}
}

// TestLowPassRate_MinRunsGuard pins that a short window is not treated as
// signal no matter how aggressive the cutoff is.
func TestLowPassRate_MinRunsGuard(t *testing.T) {
	report := attentionReport(t, []bool{false, true}, Settings{LowPassRate: lowPassRateRule(1, 5)})
	if len(report.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0 below min_runs", len(report.LowPassRate))
	}

	relaxed := attentionReport(t, []bool{false, true}, Settings{LowPassRate: lowPassRateRule(1, 2)})
	if len(relaxed.LowPassRate) != 1 {
		t.Fatalf("LowPassRate = %d entries, want 1 at min_runs", len(relaxed.LowPassRate))
	}
}

// TestLowPassRate_RecentRunsNarrowsWindow pins that recent_runs measures the
// newest runs only, so an old failure that has since recovered drops out.
func TestLowPassRate_RecentRunsNarrowsWindow(t *testing.T) {
	// Newest-first: five passes, then a failure outside the recent window.
	pattern := []bool{true, true, true, true, true, false}
	report := attentionReport(t, pattern, Settings{
		LowPassRate: &LowPassRateRule{Threshold: 1, MinRuns: 5, RecentRuns: 5},
	})
	if len(report.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0 when the failure is outside recent_runs", len(report.LowPassRate))
	}

	full := attentionReport(t, pattern, Settings{LowPassRate: lowPassRateRule(1, 5)})
	if len(full.LowPassRate) != 1 {
		t.Fatalf("LowPassRate = %d entries, want 1 over the full window", len(full.LowPassRate))
	}
	if full.LowPassRate[0].WindowRuns != len(pattern) {
		t.Errorf("WindowRuns = %d, want %d", full.LowPassRate[0].WindowRuns, len(pattern))
	}
}

// TestLowPassRate_RecentRunsBelowMinRunsIsRejected pins that the guard applies
// to the narrowed window, so recent_runs cannot smuggle in thin evidence.
func TestLowPassRate_RecentRunsBelowMinRunsIsRejected(t *testing.T) {
	report := attentionReport(t, []bool{false, true, true, true, true, true}, Settings{
		LowPassRate: &LowPassRateRule{Threshold: 1, MinRuns: 5, RecentRuns: 3},
	})
	if len(report.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0 when recent_runs is below min_runs", len(report.LowPassRate))
	}
}

// TestLowPassRate_SortsWorstFirstAndCaps pins the section ordering and the
// max_items cap that bounds noise on a large dashboard.
func TestLowPassRate_SortsWorstFirstAndCaps(t *testing.T) {
	// Three tests in one job with distinct pass rates over five runs.
	rates := map[string][]bool{
		"TestMild":   {true, true, true, true, false},
		"TestMiddle": {true, true, true, false, false},
		"TestWorst":  {true, false, false, false, false},
	}
	runs := make([]models.BuildResult, 5)
	for i := range runs {
		var cases []models.TestCase
		for name, pattern := range rates {
			status, msg := "failed", "boom"
			if pattern[i] {
				status, msg = "passed", ""
			}
			cases = append(cases, makeTC(name, status, 1.0, msg))
		}
		runs[i] = makeFlakyBuild(string(rune('a'+i)), flakyHoursAgo(i+1), false, cases)
	}

	settings := Settings{LowPassRate: &LowPassRateRule{Threshold: 1, MinRuns: 5}}
	report := ComputeFlakinessReport(
		map[string][]models.BuildResult{"test-job": runs},
		[]models.ProwJob{{Name: "test-job", JobID: "test-job"}},
		flakyBaseTime,
		settings,
	)
	got := make([]string, 0, len(report.LowPassRate))
	for _, entry := range report.LowPassRate {
		got = append(got, entry.TestName)
	}
	want := []string{"TestWorst", "TestMiddle", "TestMild"}
	if len(got) != len(want) {
		t.Fatalf("LowPassRate = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LowPassRate order = %v, want %v", got, want)
		}
	}

	settings.LowPassRate.MaxItems = 2
	capped := ComputeFlakinessReport(
		map[string][]models.BuildResult{"test-job": runs},
		[]models.ProwJob{{Name: "test-job", JobID: "test-job"}},
		flakyBaseTime,
		settings,
	)
	if len(capped.LowPassRate) != 2 {
		t.Fatalf("capped LowPassRate = %d entries, want 2", len(capped.LowPassRate))
	}
	if capped.LowPassRate[0].TestName != "TestWorst" || capped.LowPassRate[1].TestName != "TestMiddle" {
		t.Errorf("cap dropped the wrong entries: %+v", capped.LowPassRate)
	}
}

// TestLowPassRate_ThresholdIsExclusive pins that a pass rate exactly equal to
// the cutoff is not selected, which is what makes threshold 0 select nothing.
func TestLowPassRate_ThresholdIsExclusive(t *testing.T) {
	// Four of five runs pass, so the rate is exactly 0.8.
	pattern := []bool{true, true, true, true, false}
	atRate := attentionReport(t, pattern, Settings{LowPassRate: lowPassRateRule(0.8, 5)})
	if len(atRate.LowPassRate) != 0 {
		t.Fatalf("LowPassRate = %d entries, want 0 when the rate equals the threshold", len(atRate.LowPassRate))
	}

	aboveRate := attentionReport(t, pattern, Settings{LowPassRate: lowPassRateRule(0.81, 5)})
	if len(aboveRate.LowPassRate) != 1 {
		t.Fatalf("LowPassRate = %d entries, want 1 just above the rate", len(aboveRate.LowPassRate))
	}
}

// TestPersistentAfter_DefaultMatchesLegacyThreshold pins that leaving the knob
// unset preserves the previously hardcoded consecutive-failure count of 3.
func TestPersistentAfter_DefaultMatchesLegacyThreshold(t *testing.T) {
	twoInARow := attentionReport(t, []bool{false, false, true, true, true}, Settings{})
	if len(twoInARow.PersistentFailures) != 0 {
		t.Errorf("two consecutive failures classified persistent by default: %+v", twoInARow.PersistentFailures)
	}

	threeInARow := attentionReport(t, []bool{false, false, false, true, true}, Settings{})
	if len(threeInARow.PersistentFailures) != 1 {
		t.Fatalf("PersistentFailures = %d entries, want 1", len(threeInARow.PersistentFailures))
	}
	if got := threeInARow.PersistentFailures[0].Classification; got != models.ClassificationPersistent {
		t.Errorf("Classification = %q, want %q", got, models.ClassificationPersistent)
	}
}

// TestPersistentAfter_ConfiguredValueMovesBoundary pins that the knob moves
// both the classification and the report section together.
func TestPersistentAfter_ConfiguredValueMovesBoundary(t *testing.T) {
	report := attentionReport(t, []bool{false, false, true, true, true}, Settings{PersistentAfter: 2})
	if len(report.PersistentFailures) != 1 {
		t.Fatalf("PersistentFailures = %d entries, want 1 at persistent_after=2", len(report.PersistentFailures))
	}
	if got := report.PersistentFailures[0].Classification; got != models.ClassificationPersistent {
		t.Errorf("Classification = %q, want %q", got, models.ClassificationPersistent)
	}

	raised := attentionReport(t, []bool{false, false, false, true, true}, Settings{PersistentAfter: 4})
	if len(raised.PersistentFailures) != 0 {
		t.Errorf("three failures still persistent at persistent_after=4: %+v", raised.PersistentFailures)
	}
	if got := raised.MostFlaky; len(got) != 1 || got[0].Classification != models.ClassificationFlaky {
		t.Errorf("raised threshold did not reclassify as flaky: %+v", raised.MostFlaky)
	}
}
