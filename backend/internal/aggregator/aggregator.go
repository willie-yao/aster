// Package aggregator computes per-job and per-test aggregate statistics
// from build results, including pass rates, overall status, and persistent,
// flaky, or one-off failure classifications.
package aggregator

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
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

// defaultPersistentAfter is the consecutive failure count required for a
// persistent classification when a project does not configure one.
const defaultPersistentAfter = 3

// Settings tunes classification and attention selection. The zero value uses
// the engine defaults, so callers that do not care can pass Settings{}.
type Settings struct {
	// PersistentAfter is the consecutive failure count required for a
	// persistent classification. Values below 1 fall back to the default.
	PersistentAfter int
	// LowPassRate optionally selects tests by pass rate. Nil disables the rule.
	LowPassRate *LowPassRateRule
}

// LowPassRateRule selects tests whose pass rate over a window falls strictly
// below Threshold. It is a selection rule and never alters a classification.
type LowPassRateRule struct {
	// Threshold is the exclusive pass-rate cutoff in [0, 1]. 1 selects every
	// test that failed at least once; 0 selects none.
	Threshold float64
	// MinRuns is the number of runs in the window required before the rule
	// applies.
	MinRuns int
	// RecentRuns limits the window to the newest N runs. Zero uses every run.
	RecentRuns int
	// MaxItems caps the selected set. Zero means unbounded.
	MaxItems int
}

// persistentAfter resolves the configured consecutive-failure threshold.
func (s Settings) persistentAfter() int {
	if s.PersistentAfter > 0 {
		return s.PersistentAfter
	}
	return defaultPersistentAfter
}

// ComputeJobSummary computes a JobSummary from newest-first build results.
func ComputeJobSummary(job models.ProwJob, runs []models.BuildResult) models.JobSummary {
	summary := models.JobSummary{
		ProwJob:       job,
		CurrentStatus: CurrentJobStatus(runs),
		RecentRuns:    []models.RunSummary{},
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

// CurrentJobStatus reports the newest observed run independently of rolling reliability.
func CurrentJobStatus(runs []models.BuildResult) models.JobCurrentStatus {
	if len(runs) == 0 {
		return models.JobCurrentUnknown
	}
	if runs[0].Result == "PENDING" {
		return models.JobCurrentRunning
	}
	if runs[0].Passed {
		return models.JobCurrentPassing
	}
	return models.JobCurrentFailing
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
		threshold = defaultPersistentAfter
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

// numberRunRegex matches a run of digits with optional decimal groups anywhere in
// a token, including one glued to a unit or identifier such as "3600.001s",
// "1823ms", or "worker-7".
var numberRunRegex = regexp.MustCompile(`\d+(?:\.\d+)*`)

// NormalizeErrorRecurrence normalizes an error message for counting how long a
// failure has been recurring. It collapses every run of digits, unlike
// NormalizeErrorMessage, whose word-boundary matching leaves numbers glued to a
// unit or identifier intact ("3600.001s" survives as "<num>001s").
//
// Recall is the whole point here. Durations, elapsed counters, and generated
// names change on every run, so any identity that preserves them reports a
// long-lived flake as a brand new cause each time it fails. Over-grouping costs a
// count that is too generous; under-grouping costs the history entirely.
//
// It is deliberately not used for durable verdict identity, which needs the
// opposite trade-off. See NormalizeErrorSignature.
func NormalizeErrorRecurrence(msg string) string {
	s := strings.TrimSpace(msg)
	s = timestampRegex.ReplaceAllString(s, "<timestamp>")
	s = numberRunRegex.ReplaceAllString(s, "<num>")
	return whitespaceRegex.ReplaceAllString(s, " ")
}

// NormalizeErrorSignature normalizes an error message for durable identity rather
// than display grouping. It strips timestamps and collapses whitespace like
// NormalizeErrorMessage, but deliberately preserves numeric values: status codes,
// exit codes, and error numbers are often the only thing distinguishing causes
// that need different answers, and collapsing "status 401" and "status 503" into
// one identity would let a conclusion about one be applied to the other.
//
// The cost is recall, not correctness. A message whose numbers vary between
// builds yields a different identity each time, which means no memory and another
// investigation.
func NormalizeErrorSignature(msg string) string {
	s := strings.TrimSpace(msg)
	s = timestampRegex.ReplaceAllString(s, "<timestamp>")
	return whitespaceRegex.ReplaceAllString(s, " ")
}

// HashError returns the first 8 hex characters of the SHA-256 hash of
// the normalized message for use as a deduplication key.
func HashError(normalizedMsg string) string {
	h := sha256.Sum256([]byte(normalizedMsg))
	return fmt.Sprintf("%x", h[:4])
}
