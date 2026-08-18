package fetcher

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

// attentionRuns builds newest-first runs for testName where pattern lists each
// run's outcome newest-first: true is a pass.
func attentionRuns(testName string, pattern []bool) []models.BuildResult {
	runs := make([]models.BuildResult, 0, len(pattern))
	for i, passed := range pattern {
		status, msg := "failed", "boom"
		if passed {
			status, msg = "passed", ""
		}
		runs = append(runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: strconv.Itoa(len(pattern) - i), Passed: passed},
			TestCases: []models.TestCase{{Name: testName, Status: status, FailureMessage: msg}},
		})
	}
	return runs
}

// TestConsecutiveFailureCounts pins that the streak the AI analyzer receives is
// independent of the project's classification threshold, so the engine-owned
// transient gate cannot be weakened by raising attention.persistent_after.
func TestConsecutiveFailureCounts(t *testing.T) {
	details := []models.JobDetail{{
		JobID: "test-job",
		Runs:  attentionRuns("TestA", []bool{false, false, false, true, true}),
	}}
	counts := aggregator.ConsecutiveFailureCounts(details)
	if got := counts["test-job::TestA"]; got != 3 {
		t.Fatalf("streak = %d, want 3", got)
	}

	// The same runs classify as one-off, not persistent, at a raised threshold,
	// yet the streak the analyzer sees is unchanged.
	report := aggregator.ComputeFlakinessReport(
		map[string][]models.BuildResult{"test-job": details[0].Runs},
		[]models.ProwJob{{Name: "test-job", JobID: "test-job"}},
		time.Now(),
		aggregator.Settings{PersistentAfter: 5},
	)
	if len(report.PersistentFailures) != 0 {
		t.Fatalf("PersistentFailures = %d, want 0 at persistent_after=5", len(report.PersistentFailures))
	}
	if got := aggregator.ConsecutiveFailureCounts(details)["test-job::TestA"]; got != 3 {
		t.Errorf("streak = %d, want 3 regardless of the configured threshold", got)
	}
}

// TestConsecutiveFailureCountsOmitsRecoveredTests pins that a test whose latest
// run passed carries no streak.
func TestConsecutiveFailureCountsOmitsRecoveredTests(t *testing.T) {
	details := []models.JobDetail{{
		JobID: "test-job",
		Runs:  attentionRuns("TestA", []bool{true, false, false}),
	}}
	if _, ok := aggregator.ConsecutiveFailureCounts(details)["test-job::TestA"]; ok {
		t.Error("a recovered test should carry no streak")
	}
}

func TestAttentionSettings(t *testing.T) {
	threshold := 0.95
	cases := []struct {
		name string
		cfg  *project.Config
		want aggregator.Settings
	}{
		{
			name: "nil config uses engine defaults",
			cfg:  nil,
			want: aggregator.Settings{PersistentAfter: 3},
		},
		{
			name: "omitted section leaves the rule off",
			cfg:  &project.Config{},
			want: aggregator.Settings{PersistentAfter: 3},
		},
		{
			name: "configured rule carries resolved guards",
			cfg: &project.Config{Attention: &project.Attention{
				PersistentAfter: 2,
				LowPassRate:     &project.LowPassRate{Threshold: &threshold, RecentRuns: 10},
			}},
			want: aggregator.Settings{
				PersistentAfter: 2,
				LowPassRate: &aggregator.LowPassRateRule{
					Threshold: 0.95, MinRuns: 5, RecentRuns: 10, MaxItems: 50,
				},
			},
		},
		{
			name: "rule without a threshold stays off",
			cfg:  &project.Config{Attention: &project.Attention{LowPassRate: &project.LowPassRate{}}},
			want: aggregator.Settings{PersistentAfter: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attentionSettings(tc.cfg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("attentionSettings() = %+v (rule %+v), want %+v (rule %+v)",
					got, got.LowPassRate, tc.want, tc.want.LowPassRate)
			}
		})
	}
}
