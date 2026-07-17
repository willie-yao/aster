// Package aggregator computes per-job and per-test aggregate statistics
// from build results, including pass rates, overall status, and persistent,
// flaky, or one-off failure classifications.
package aggregator

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const maxRecentRuns = 20

const (
	passRateRecentRuns = 10  // number of recent runs the pass rate covers
	passingThreshold   = 0.9 // recent pass rate at/above this is PASSING
	failingThreshold   = 0.3 // recent pass rate at/below this is FAILING
)

// FailureInfo holds the result of classifying a test failure.
type FailureInfo struct {
	Classification      models.FailureClassification
	ConsecutiveFailures int
	ErrorHash           string
}

// ComputeJobSummary computes a JobSummary from newest-first build results.
func ComputeJobSummary(job models.ProwJob, runs []models.BuildResult) models.JobSummary {
	summary := models.JobSummary{
		ProwJob:    job,
		RecentRuns: []models.RunSummary{},
	}

	if len(runs) == 0 {
		return summary
	}

	last := BuildRunSummary(runs[0])
	summary.LastRun = &last

	limit := len(runs)
	if limit > maxRecentRuns {
		limit = maxRecentRuns
	}
	summary.RecentRuns = make([]models.RunSummary, limit)
	for i := 0; i < limit; i++ {
		summary.RecentRuns[i] = BuildRunSummary(runs[i])
	}

	// OverallStatus and pass rate are computed over the most recent runs.
	summary.OverallStatus = computeOverallStatus(runs)
	summary.PassRateRecent = recentPassRate(runs, passRateRecentRuns)

	return summary
}

// computeOverallStatus classifies a job from its most recent runs using the
// pass rate over the last passRateRecentRuns runs:
//   - PASSING when the recent pass rate is at least passingThreshold
//   - FAILING when it is at or below failingThreshold
//   - FLAKY otherwise
func computeOverallStatus(runs []models.BuildResult) string {
	if len(runs) == 0 {
		return "FLAKY"
	}
	rate := recentPassRate(runs, passRateRecentRuns)
	switch {
	case rate >= passingThreshold:
		return "PASSING"
	case rate <= failingThreshold:
		return "FAILING"
	default:
		return "FLAKY"
	}
}

// recentPassRate calculates the fraction of passing runs among the most recent
// n runs. Runs are expected newest-first. Returns 0 when there are no runs.
func recentPassRate(runs []models.BuildResult, n int) float64 {
	if len(runs) == 0 {
		return 0
	}
	if n > len(runs) {
		n = len(runs)
	}
	passed := 0
	for i := 0; i < n; i++ {
		if runs[i].Passed {
			passed++
		}
	}
	return float64(passed) / float64(n)
}

// BuildRunSummary converts a BuildResult into a compact RunSummary.
func BuildRunSummary(result models.BuildResult) models.RunSummary {
	return models.RunSummary{
		BuildID:         result.BuildID,
		Passed:          result.Passed,
		Result:          result.Result,
		Timestamp:       result.Started,
		DurationSeconds: result.DurationSeconds,
		TestsTotal:      result.TestsTotal,
		TestsPassed:     result.TestsPassed,
		TestsFailed:     result.TestsFailed,
		TestsSkipped:    result.TestsSkipped,
	}
}

// ClassifyFailure examines the most recent runs to determine whether a test's
// failure is persistent, flaky, or a one-off. threshold sets the consecutive
// failure count required for a persistent classification.
func ClassifyFailure(testName string, runs []models.BuildResult, threshold int) FailureInfo {
	return classifyOutcomes(outcomesForTest(testName, runs), threshold)
}

func classifyOutcomes(outcomes []testOutcome, threshold int) FailureInfo {
	if threshold <= 0 {
		threshold = 3
	}
	if len(outcomes) == 0 {
		return FailureInfo{Classification: models.ClassificationOneOff}
	}

	consecutiveFailures := 0
	var firstFailMsg string
	for _, outcome := range outcomes {
		if outcome.passed {
			break
		}
		consecutiveFailures++
		if firstFailMsg == "" {
			firstFailMsg = outcome.message
		}
	}
	errHash := HashError(NormalizeErrorMessage(firstFailMsg))
	if consecutiveFailures >= threshold {
		return FailureInfo{Classification: models.ClassificationPersistent, ConsecutiveFailures: consecutiveFailures, ErrorHash: errHash}
	}

	failCount := 0
	for _, outcome := range outcomes {
		if !outcome.passed {
			failCount++
		}
	}
	if failCount == 1 {
		return FailureInfo{Classification: models.ClassificationOneOff, ConsecutiveFailures: consecutiveFailures, ErrorHash: errHash}
	}
	if failCount > 0 && failCount < len(outcomes) {
		return FailureInfo{Classification: models.ClassificationFlaky, ConsecutiveFailures: consecutiveFailures, ErrorHash: errHash}
	}
	return FailureInfo{Classification: models.ClassificationOneOff, ConsecutiveFailures: consecutiveFailures, ErrorHash: errHash}
}

// numericRegex matches integers and decimal numbers.
var numericRegex = regexp.MustCompile(`\b\d[\d.]*\b`)

// timestampRegex matches timestamps like 2026-03-15T10:30:00Z.
var timestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[^\s]*`)

// whitespaceRegex matches runs of whitespace.
var whitespaceRegex = regexp.MustCompile(`\s+`)

// NormalizeErrorMessage normalizes an error message for similarity comparison.
func NormalizeErrorMessage(msg string) string {
	s := strings.TrimSpace(msg)
	// Replace timestamps before numeric values so dates stay grouped.
	s = timestampRegex.ReplaceAllString(s, "<timestamp>")
	// Replace remaining numeric values.
	s = numericRegex.ReplaceAllString(s, "<num>")
	// Collapse whitespace.
	s = whitespaceRegex.ReplaceAllString(s, " ")
	return s
}

// HashError returns the first 8 hex characters of the SHA-256 hash of
// the normalized message for use as a deduplication key.
func HashError(normalizedMsg string) string {
	h := sha256.Sum256([]byte(normalizedMsg))
	return fmt.Sprintf("%x", h[:4])
}
