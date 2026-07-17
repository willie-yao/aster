package aggregator

import (
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const maxFlakyResults = 50

// ComputeTestFlakiness computes flakiness stats for one test across a job's runs.
// Runs are expected newest-first.
func ComputeTestFlakiness(testName, jobID, jobName string, runs []models.BuildResult) models.TestFlakiness {
	return computeTestFlakiness(testName, jobID, jobName, outcomesForTest(testName, runs))
}

type testOutcome struct {
	passed  bool
	message string
	buildID string
	started time.Time
	dur     float64
}

func outcomesForTest(testName string, runs []models.BuildResult) []testOutcome {
	var outcomes []testOutcome
	for _, run := range runs {
		for _, tc := range run.TestCases {
			if tc.Name != testName {
				continue
			}
			if tc.Status != "skipped" {
				outcomes = append(outcomes, testOutcome{
					passed:  tc.Status == "passed",
					message: tc.FailureMessage,
					buildID: run.BuildID,
					started: run.Started,
					dur:     tc.DurationSeconds,
				})
			}
			break
		}
	}
	return outcomes
}

func collectTestOutcomes(runs []models.BuildResult) map[string][]testOutcome {
	out := make(map[string][]testOutcome)
	for _, run := range runs {
		seen := make(map[string]bool, len(run.TestCases))
		for _, tc := range run.TestCases {
			if seen[tc.Name] {
				continue
			}
			seen[tc.Name] = true
			if tc.Status == "skipped" {
				continue
			}
			out[tc.Name] = append(out[tc.Name], testOutcome{
				passed:  tc.Status == "passed",
				message: tc.FailureMessage,
				buildID: run.BuildID,
				started: run.Started,
				dur:     tc.DurationSeconds,
			})
		}
	}
	return out
}

func computeTestFlakiness(testName, jobID, jobName string, outcomes []testOutcome) models.TestFlakiness {
	tf := models.TestFlakiness{TestName: testName, JobName: jobName, JobID: jobID, TotalRuns: len(outcomes)}
	if len(outcomes) == 0 {
		return tf
	}

	for _, outcome := range outcomes {
		if outcome.passed {
			tf.Passes++
		} else {
			tf.Failures++
		}
	}
	tf.FailRate = float64(tf.Failures) / float64(tf.TotalRuns)

	if tf.TotalRuns >= 2 {
		flips := 0
		for i := 1; i < len(outcomes); i++ {
			if outcomes[i].passed != outcomes[i-1].passed {
				flips++
			}
		}
		tf.FlipRate = float64(flips) / float64(tf.TotalRuns-1)
	}

	for _, outcome := range outcomes {
		if outcome.passed {
			break
		}
		tf.ConsecutiveFailures++
	}
	tf.Classification = classifyOutcomes(outcomes, 3).Classification

	if tf.ConsecutiveFailures > 0 {
		tf.FirstFailedAt = outcomes[tf.ConsecutiveFailures-1].started.UTC().Format(time.RFC3339)
	}
	for _, outcome := range outcomes {
		if outcome.passed {
			continue
		}
		normalized := NormalizeErrorMessage(outcome.message)
		tf.LastFailure = &models.TestFailureInfo{
			BuildID:        outcome.buildID,
			Timestamp:      outcome.started.UTC().Format(time.RFC3339),
			FailureMessage: outcome.message,
			ErrorHash:      HashError(normalized),
		}
		break
	}

	patterns := make(map[string]*models.ErrorPattern)
	for _, outcome := range outcomes {
		if outcome.passed {
			continue
		}
		normalized := NormalizeErrorMessage(outcome.message)
		hash := HashError(normalized)
		if pattern := patterns[hash]; pattern != nil {
			pattern.Count++
		} else {
			patterns[hash] = &models.ErrorPattern{
				NormalizedMessage: normalized,
				ErrorHash:         hash,
				Count:             1,
				ExampleMessage:    outcome.message,
			}
		}
	}
	for _, pattern := range patterns {
		tf.ErrorPatterns = append(tf.ErrorPatterns, *pattern)
	}
	sort.Slice(tf.ErrorPatterns, func(i, j int) bool {
		if tf.ErrorPatterns[i].Count != tf.ErrorPatterns[j].Count {
			return tf.ErrorPatterns[i].Count > tf.ErrorPatterns[j].Count
		}
		return tf.ErrorPatterns[i].ErrorHash < tf.ErrorPatterns[j].ErrorHash
	})

	for _, outcome := range outcomes {
		tf.DurationHistory = append(tf.DurationHistory, models.DurationPoint{
			BuildID:   outcome.buildID,
			Timestamp: outcome.started.UTC().Format(time.RFC3339),
			Duration:  outcome.dur,
			Passed:    outcome.passed,
		})
	}
	return tf
}

// ComputeFlakinessReport builds the full flakiness report across all jobs.
// jobResults is keyed by JobID. jobs supplies the JobID-to-name lookup used by
// the search index and notification dedupe key.
func ComputeFlakinessReport(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, now time.Time) models.FlakinessReport {
	jobName := make(map[string]string, len(jobs))
	for _, j := range jobs {
		jobName[j.JobID] = j.Name
	}

	var allFlaky []models.TestFlakiness

	jobIDs := make([]string, 0, len(jobResults))
	for jobID := range jobResults {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)
	for _, jobID := range jobIDs {
		outcomesByTest := collectTestOutcomes(jobResults[jobID])
		testNames := make([]string, 0, len(outcomesByTest))
		for testName := range outcomesByTest {
			testNames = append(testNames, testName)
		}
		sort.Strings(testNames)
		for _, testName := range testNames {
			tf := computeTestFlakiness(testName, jobID, jobName[jobID], outcomesByTest[testName])
			if tf.Failures > 0 {
				allFlaky = append(allFlaky, tf)
			}
		}
	}

	report := models.FlakinessReport{
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		MostFlaky:          []models.TestFlakiness{},
		PersistentFailures: []models.TestFlakiness{},
		RecentlyBroken:     []models.TestFlakiness{},
	}

	// MostFlaky includes flaky tests sorted by flip rate.
	var mostFlaky []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.Classification == models.ClassificationFlaky {
			mostFlaky = append(mostFlaky, tf)
		}
	}
	sort.Slice(mostFlaky, func(i, j int) bool {
		if mostFlaky[i].FlipRate != mostFlaky[j].FlipRate {
			return mostFlaky[i].FlipRate > mostFlaky[j].FlipRate
		}
		if mostFlaky[i].FailRate != mostFlaky[j].FailRate {
			return mostFlaky[i].FailRate > mostFlaky[j].FailRate
		}
		return testFlakinessLess(mostFlaky[i], mostFlaky[j])
	})
	if len(mostFlaky) > maxFlakyResults {
		mostFlaky = mostFlaky[:maxFlakyResults]
	}
	report.MostFlaky = mostFlaky

	// PersistentFailures is sorted by consecutive failure count.
	var persistent []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.ConsecutiveFailures >= 3 {
			persistent = append(persistent, tf)
		}
	}
	sort.Slice(persistent, func(i, j int) bool {
		if persistent[i].ConsecutiveFailures != persistent[j].ConsecutiveFailures {
			return persistent[i].ConsecutiveFailures > persistent[j].ConsecutiveFailures
		}
		return testFlakinessLess(persistent[i], persistent[j])
	})
	report.PersistentFailures = persistent

	// RecentlyBroken covers failures first seen within 48 hours.
	cutoff := now.Add(-48 * time.Hour)
	var recentlyBroken []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.FirstFailedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, tf.FirstFailedAt)
		if err != nil {
			continue
		}
		if !t.Before(cutoff) {
			recentlyBroken = append(recentlyBroken, tf)
		}
	}
	sort.Slice(recentlyBroken, func(i, j int) bool {
		// Sort by first_failed_at descending.
		if recentlyBroken[i].FirstFailedAt != recentlyBroken[j].FirstFailedAt {
			return recentlyBroken[i].FirstFailedAt > recentlyBroken[j].FirstFailedAt
		}
		return testFlakinessLess(recentlyBroken[i], recentlyBroken[j])
	})
	report.RecentlyBroken = recentlyBroken

	return report
}

func testFlakinessLess(a, b models.TestFlakiness) bool {
	if a.JobID != b.JobID {
		return a.JobID < b.JobID
	}
	return a.TestName < b.TestName
}
