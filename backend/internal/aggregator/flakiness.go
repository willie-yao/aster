package aggregator

import (
	"net/url"
	"sort"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	maxFlakyResults        = 50
	maxBuildFailureResults = 50
)

// ComputeTestFlakiness computes flakiness stats for one test across a job's runs.
// Runs are expected newest-first.
func ComputeTestFlakiness(testName, jobID, jobName string, runs []models.BuildResult, settings Settings) models.TestFlakiness {
	return computeTestFlakiness(testName, jobID, jobName, outcomesForTest(testName, runs), settings.persistentAfter())
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
			if tc.Source == models.TestCaseSourceBuild {
				continue
			}
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
			if tc.Source == models.TestCaseSourceBuild {
				continue
			}
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

func computeTestFlakiness(testName, jobID, jobName string, outcomes []testOutcome, persistentAfter int) models.TestFlakiness {
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
	tf.Classification = classifyOutcomes(outcomes, persistentAfter).Classification

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
// the search index and notification dedupe key. settings carries the project's
// classification threshold and optional pass-rate selection rule.
func ComputeFlakinessReport(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, now time.Time, settings Settings) models.FlakinessReport {
	jobName := make(map[string]string, len(jobs))
	for _, j := range jobs {
		jobName[j.JobID] = j.Name
	}

	persistentAfter := settings.persistentAfter()
	var allFlaky []models.TestFlakiness
	var lowPassRate []models.LowPassRateEntry

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
			outcomes := outcomesByTest[testName]
			tf := computeTestFlakiness(testName, jobID, jobName[jobID], outcomes, persistentAfter)
			if tf.Failures == 0 {
				continue
			}
			allFlaky = append(allFlaky, tf)
			// A test with no failures can never fall below a cutoff of at most
			// 1, so the pass-rate rule only ever considers this same set.
			if settings.LowPassRate != nil {
				if entry, ok := lowPassRateEntry(tf, outcomes, *settings.LowPassRate); ok {
					lowPassRate = append(lowPassRate, entry)
				}
			}
		}
	}

	report := models.FlakinessReport{
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		MostFlaky:          []models.TestFlakiness{},
		PersistentFailures: []models.TestFlakiness{},
		RecentlyBroken:     []models.TestFlakiness{},
		LowPassRate:        []models.LowPassRateEntry{},
		BuildFailures:      []models.BuildFailureSummary{},
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
		if tf.ConsecutiveFailures >= persistentAfter {
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

	// LowPassRate is sorted worst-first so the weakest tests lead the section.
	sort.Slice(lowPassRate, func(i, j int) bool {
		if lowPassRate[i].PassRate != lowPassRate[j].PassRate {
			return lowPassRate[i].PassRate < lowPassRate[j].PassRate
		}
		return testFlakinessLess(lowPassRate[i].TestFlakiness, lowPassRate[j].TestFlakiness)
	})
	if settings.LowPassRate != nil && settings.LowPassRate.MaxItems > 0 && len(lowPassRate) > settings.LowPassRate.MaxItems {
		lowPassRate = lowPassRate[:settings.LowPassRate.MaxItems]
	}
	if lowPassRate != nil {
		report.LowPassRate = lowPassRate
	}

	return report
}

// lowPassRateEntry reports whether a test qualifies for the pass-rate section.
// The rate and the minimum-run guard share one window, so narrowing the window
// cannot admit a test on thinner evidence than the guard requires. Outcomes are
// expected newest-first.
func lowPassRateEntry(tf models.TestFlakiness, outcomes []testOutcome, rule LowPassRateRule) (models.LowPassRateEntry, bool) {
	window := outcomes
	if rule.RecentRuns > 0 && rule.RecentRuns < len(window) {
		window = window[:rule.RecentRuns]
	}
	if len(window) == 0 || len(window) < rule.MinRuns {
		return models.LowPassRateEntry{}, false
	}
	passed := 0
	for _, outcome := range window {
		if outcome.passed {
			passed++
		}
	}
	rate := float64(passed) / float64(len(window))
	if rate >= rule.Threshold {
		return models.LowPassRateEntry{}, false
	}
	return models.LowPassRateEntry{TestFlakiness: tf, WindowRuns: len(window), PassRate: rate}, true
}

// ConsecutiveFailureCounts maps "jobID::testName" to each test's current
// consecutive-failure streak over newest-first runs, omitting tests whose
// latest run passed. It is deliberately independent of any classification
// threshold: callers that gate on the true streak, such as the engine-owned AI
// critique, must not inherit a project's attention.persistent_after.
func ConsecutiveFailureCounts(details []models.JobDetail) map[string]int {
	counts := make(map[string]int)
	for _, detail := range details {
		for testName, outcomes := range collectTestOutcomes(detail.Runs) {
			streak := 0
			for _, outcome := range outcomes {
				if outcome.passed {
					break
				}
				streak++
			}
			if streak > 0 {
				counts[detail.JobID+"::"+testName] = streak
			}
		}
	}
	return counts
}

// CollectBuildFailures builds a bounded public index without changing test flakiness calculations.
func CollectBuildFailures(details []models.JobDetail) []models.BuildFailureSummary {
	failures := make([]models.BuildFailureSummary, 0)
	for _, detail := range details {
		for _, run := range detail.Runs {
			for _, testCase := range run.TestCases {
				if testCase.Source != models.TestCaseSourceBuild || testCase.Status != "failed" {
					continue
				}
				entry := models.BuildFailureSummary{
					JobID: detail.JobID, JobName: detail.Name, BuildID: run.BuildID, Result: run.Result,
					AnalysisState: "unavailable", IsTransient: testCase.AISummary != nil && testCase.AISummary.IsTransient,
					BuildLogURL:  run.BuildLogURL,
					JobDetailURL: "/job/" + url.PathEscape(detail.JobID) + "/build/" + url.PathEscape(run.BuildID) + "/failure",
				}
				if !run.Started.IsZero() {
					entry.StartedAt = run.Started.UTC().Format(time.RFC3339)
				}
				if testCase.AISummary != nil {
					entry.Summary = testCase.AISummary.Summary
				}
				if testCase.AIAnalysis != nil {
					entry.AnalysisState = "succeeded"
					entry.Severity = testCase.AIAnalysis.Severity
					if testCase.AIAnalysis.CacheHit {
						entry.Provenance = "cache"
					}
				} else if entry.IsTransient {
					entry.Severity = "Transient-Ignore"
				}
				failures = append(failures, entry)
				break
			}
		}
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].StartedAt != failures[j].StartedAt {
			return failures[i].StartedAt > failures[j].StartedAt
		}
		if failures[i].JobID != failures[j].JobID {
			return failures[i].JobID < failures[j].JobID
		}
		return failures[i].BuildID > failures[j].BuildID
	})
	if len(failures) > maxBuildFailureResults {
		failures = failures[:maxBuildFailureResults]
	}
	return failures
}

func testFlakinessLess(a, b models.TestFlakiness) bool {
	if a.JobID != b.JobID {
		return a.JobID < b.JobID
	}
	return a.TestName < b.TestName
}
