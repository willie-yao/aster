// Package models defines shared data types for the Prow AI dashboard.
package models

import (
	"time"
)

// JobType identifies how a Prow job is triggered, which also determines
// where its build artifacts live in the GCS bucket.
const (
	JobTypePeriodic  = "periodic"
	JobTypePresubmit = "presubmit"
)

// ProwJob represents a Prow job definition.
type ProwJob struct {
	Name            string `json:"name" yaml:"name"`
	TabName         string `json:"tab_name"`
	Category        string `json:"category"`
	Branch          string `json:"branch"`
	Description     string `json:"description"`
	MinimumInterval string `json:"minimum_interval" yaml:"minimum_interval"`
	Timeout         string `json:"timeout"`
	ConfigFile      string `json:"config_file"`
	// JobType is "periodic" or "presubmit".
	JobType string `json:"job_type"`
	// Repo is the "org/repo" the job runs against. Populated for presubmits
	// from the YAML map key; empty for periodics.
	Repo string `json:"repo"`
	// JobID uniquely distinguishes same-named jobs across repos or job types.
	// The frontend uses it for routing and file fetches; cache loaders use it as
	// a map key.
	JobID string `json:"job_id"`
}

// JobIDFor builds a stable per-job identifier. Periodics use the bare name
// the bare name because Prow periodics are unique. Presubmits use
// "<repo>/<name>" so same-named jobs do not collide downstream.
func JobIDFor(jobType, repo, name string) string {
	if jobType == JobTypePresubmit {
		return repo + "/" + name
	}
	return name
}

// BuildInfo represents metadata for a single prow build.
type BuildInfo struct {
	BuildID         string    `json:"build_id"`
	JobName         string    `json:"job_name"`
	Started         time.Time `json:"started"`
	Finished        time.Time `json:"finished"`
	Passed          bool      `json:"passed"`
	Result          string    `json:"result"`
	DurationSeconds float64   `json:"duration_seconds"`
	Commit          string    `json:"commit"`
	RepoVersion     string    `json:"repo_version,omitempty"`
	ProwURL         string    `json:"prow_url"`
	BuildLogURL     string    `json:"build_log_url"`
	// JUnitURLs lists every junit*.xml under the build's artifacts/ dir,
	// discovered at fetch time. Empty when discovery failed or the build
	// has no junit output. Stable ordering keeps cache reuse deterministic.
	JUnitURLs []string `json:"junit_urls,omitempty"`
	// PullNumber is the PR number that triggered this build for presubmits.
	// Empty for periodics. Required for reconstructing presubmit GCS paths
	// from cached BuildResults without reparsing the job config.
	PullNumber string `json:"pull_number,omitempty"`
	// WebURL is the human-clickable artifact directory for this build.
	// The frontend uses it instead of recomposing storage paths from job identity.
	WebURL string `json:"web_url,omitempty"`
}

// AISummary is a brief AI-generated explanation of a test failure.
type AISummary struct {
	GeneratedAt string `json:"generated_at"`
	Summary     string `json:"summary"`
	IsTransient bool   `json:"is_transient"`
}

// AIAnalysis is a deep AI-generated root cause analysis.
type AIAnalysis struct {
	GeneratedAt string `json:"generated_at"`
	// Model is the provider's model identifier used for the analysis. Kept
	// in-memory for cache validation and debug logging, but never serialized
	// to public JSON so internal-only model labels do not leak via the
	// deployed GitHub Pages data files.
	Model         string   `json:"-"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"` // Critical, High, Medium, Low, Transient-Ignore
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files,omitempty"`
	// Mode records the analysis pipeline. Cache gates reject non-agentic entries.
	Mode string `json:"mode,omitempty"`

	// ToolCalls is the number of agent tool invocations made during this
	// analysis.
	ToolCalls int `json:"tool_calls,omitempty"`
	// ModelBytes is the cumulative bytes sent to / received from the chat
	// completion endpoint.
	ModelBytes int `json:"model_bytes,omitempty"`
	// GCSBytes is the cumulative bytes fetched from GCS via agent tool
	// calls.
	GCSBytes int `json:"gcs_bytes,omitempty"`
	// ElapsedMs is the wall-clock duration of the analysis in milliseconds.
	ElapsedMs int `json:"elapsed_ms,omitempty"`
	// CacheHit reports whether the analysis was served from the AI cache.
	CacheHit bool `json:"cache_hit,omitempty"`
	// BudgetExhausted reports whether the agentic loop hit one of its
	// budget caps and was forced to finalize on best-effort evidence.
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`

	// CritiquePassed reports whether this analysis cleared the critique gate.
	CritiquePassed bool `json:"critique_passed,omitempty"`

	// JudgeRan / JudgeObjected / JudgeRevised are the semantic-judge telemetry:
	// whether the second-line LLM judge ran, whether it raised objections, and
	// whether its objections drove an accepted revision. Recorded so the judge's
	// value can be measured before deciding to keep it always-on.
	JudgeRan      bool `json:"judge_ran,omitempty"`
	JudgeObjected bool `json:"judge_objected,omitempty"`
	JudgeRevised  bool `json:"judge_revised,omitempty"`

	// CritiqueVersion records the critique contract this analysis passed.
	// Cache gates require the current version.
	CritiqueVersion int `json:"critique_version,omitempty"`

	// SkillSetHash fingerprints the loaded recipe set for this analysis.
	// Edits to triggers, required evidence, or procedure force re-analysis.
	// Empty when no recipes are loaded.
	SkillSetHash string `json:"skill_set_hash,omitempty"`

	// ModelHash fingerprints the model + endpoint that produced this
	// analysis. A provider or model swap changes it, forcing re-analysis so a
	// new model does not serve the prior model's cached verdict.
	ModelHash string `json:"model_hash,omitempty"`

	// PromptHash fingerprints the composed system prompt for this analysis.
	// Prompt edits refresh affected failures on the next run.
	PromptHash string `json:"prompt_hash,omitempty"`

	// FileLinks maps cited source-file paths to verified GitHub URLs.
	// It is the UI allowlist for source links. A present-but-empty map means
	// verification ran and found nothing linkable; an absent field means the
	// analysis has not run source-link verification. Artifact links are resolved
	// client-side.
	FileLinks map[string]string `json:"file_links"`
}

// TestCase represents a single test case from JUnit XML.
type TestCase struct {
	Name            string  `json:"name"`
	Status          string  `json:"status"` // "passed", "failed", "skipped"
	DurationSeconds float64 `json:"duration_seconds"`
	FailureMessage  string  `json:"failure_message,omitempty"`
	FailureBody     string  `json:"failure_body,omitempty"`
	FailureLocation string  `json:"failure_location,omitempty"`
	FailureLocURL   string  `json:"failure_location_url,omitempty"`
	// JUnitFile is the basename within artifacts/ that this case came from.
	// The UI uses it to disambiguate same-named cases across shards or suites.
	JUnitFile  string      `json:"junit_file,omitempty"`
	AISummary  *AISummary  `json:"ai_summary,omitempty"`
	AIAnalysis *AIAnalysis `json:"ai_analysis,omitempty"`
}

// BuildResult is a complete result for a single build.
type BuildResult struct {
	BuildInfo
	TestCases    []TestCase `json:"test_cases"`
	TestsTotal   int        `json:"tests_total"`
	TestsPassed  int        `json:"tests_passed"`
	TestsFailed  int        `json:"tests_failed"`
	TestsSkipped int        `json:"tests_skipped"`
}

// JobSummary represents aggregated data for a job on the landing page.
type JobSummary struct {
	ProwJob
	OverallStatus string       `json:"overall_status"` // "PASSING", "FAILING", "FLAKY"
	LastRun       *RunSummary  `json:"last_run,omitempty"`
	RecentRuns    []RunSummary `json:"recent_runs"`
	// PassRateRecent is the fraction of passing runs over the most recent runs.
	PassRateRecent float64 `json:"pass_rate_recent"`
}

// RunSummary is a compact summary of a single build run.
type RunSummary struct {
	BuildID         string    `json:"build_id"`
	Passed          bool      `json:"passed"`
	Result          string    `json:"result,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	TestsTotal      int       `json:"tests_total,omitempty"`
	TestsPassed     int       `json:"tests_passed,omitempty"`
	TestsFailed     int       `json:"tests_failed,omitempty"`
	TestsSkipped    int       `json:"tests_skipped,omitempty"`
}

// Dashboard is the top-level structure for dashboard.json.
type Dashboard struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Jobs        []JobSummary `json:"jobs"`
}

// JobDetail is the per-job detail structure for jobs/{job-id}.json.
type JobDetail struct {
	Name    string        `json:"name"`
	JobID   string        `json:"job_id"`
	JobType string        `json:"job_type"`
	Repo    string        `json:"repo"`
	Runs    []BuildResult `json:"runs"`
	// PatternAnalyses holds cross-build correlations for this job.
	// Empty unless the job failed in enough builds for pattern analysis.
	PatternAnalyses []PatternAnalysis `json:"pattern_analyses,omitempty"`
}

// PatternAnalysis is a job-level correlation across recent failed builds.
// It captures whether varied-looking failures share one recurring, fixable
// cause. The specific failing test may differ between builds.
type PatternAnalysis struct {
	// ID is a stable, URL-safe identifier for this pattern, used to address it
	// from the frontend and the actions API. See models.PatternID.
	ID string `json:"id,omitempty"`
	// Subject is what the correlated failures belong to.
	Subject string `json:"subject"`
	// JobID lets home-page aggregations link back to the job page.
	JobID          string `json:"job_id,omitempty"`
	GeneratedAt    string `json:"generated_at"`
	BuildsAnalyzed int    `json:"builds_analyzed"`
	// Systemic is true when most failures share one underlying cause.
	Systemic bool `json:"systemic"`
	// Confidence is the model's confidence in the systemic verdict:
	// "high", "medium", or "low".
	Confidence string `json:"confidence"`
	// SharedRootCause describes the common cause when Systemic is true.
	SharedRootCause string `json:"shared_root_cause,omitempty"`
	// SharedBuilds lists the build IDs the model judged to share the cause.
	SharedBuilds []string `json:"shared_builds,omitempty"`
	// SuggestedFix is the cross-cutting fix for the shared cause.
	SuggestedFix string `json:"suggested_fix,omitempty"`
	// RelevantFiles are the source files the per-build analyses implicated,
	// unioned across builds. They ground the fix harness's target selection.
	RelevantFiles []string `json:"relevant_files,omitempty"`
	// Summary is a one-paragraph human-readable verdict.
	Summary string `json:"summary"`
}

// FailureClassification indicates the type of failure.
type FailureClassification string

const (
	ClassificationPersistent FailureClassification = "persistent"
	ClassificationFlaky      FailureClassification = "flaky"
	ClassificationOneOff     FailureClassification = "one-off"
)

// TestFlakiness represents flakiness statistics for one test across a job's runs.
type TestFlakiness struct {
	TestName            string                `json:"test_name"`
	JobName             string                `json:"job_name"`
	JobID               string                `json:"job_id"`
	TotalRuns           int                   `json:"total_runs"`
	Failures            int                   `json:"failures"`
	Passes              int                   `json:"passes"`
	FlipRate            float64               `json:"flip_rate"`
	FailRate            float64               `json:"fail_rate"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	Classification      FailureClassification `json:"classification"`
	LastFailure         *TestFailureInfo      `json:"last_failure,omitempty"`
	FirstFailedAt       string                `json:"first_failed_at,omitempty"`
	ErrorPatterns       []ErrorPattern        `json:"error_patterns,omitempty"`
	DurationHistory     []DurationPoint       `json:"duration_history,omitempty"`
}

// TestFailureInfo captures the most recent failure details.
type TestFailureInfo struct {
	BuildID        string `json:"build_id"`
	Timestamp      string `json:"timestamp"`
	FailureMessage string `json:"failure_message"`
	ErrorHash      string `json:"error_hash"`
}

// ErrorPattern groups similar failures.
type ErrorPattern struct {
	NormalizedMessage string `json:"normalized_message"`
	ErrorHash         string `json:"error_hash"`
	Count             int    `json:"count"`
	ExampleMessage    string `json:"example_message"`
}

// DurationPoint is a single data point for duration trend charts.
type DurationPoint struct {
	BuildID   string  `json:"build_id"`
	Timestamp string  `json:"timestamp"`
	Duration  float64 `json:"duration"`
	Passed    bool    `json:"passed"`
}

// SearchEntry represents a searchable job or test case.
type SearchEntry struct {
	Kind     string  `json:"kind"`      // "job" or "test"
	TestName string  `json:"test_name"` // empty for job entries
	JobName  string  `json:"job_name"`
	JobID    string  `json:"job_id"`
	JobType  string  `json:"job_type"`
	Repo     string  `json:"repo"`
	TabName  string  `json:"tab_name"`
	Branch   string  `json:"branch"`
	Category string  `json:"category"`
	Status   string  `json:"status"`    // overall status for jobs, test status for tests
	FailRate float64 `json:"fail_rate"` // from flakiness data if available
}

// SearchIndex is the top-level structure for search-index.json.
type SearchIndex struct {
	GeneratedAt string        `json:"generated_at"`
	Entries     []SearchEntry `json:"entries"`
}

// FlakinessReport is the top-level structure for flakiness.json.
type FlakinessReport struct {
	GeneratedAt        string          `json:"generated_at"`
	MostFlaky          []TestFlakiness `json:"most_flaky"`
	PersistentFailures []TestFlakiness `json:"persistent_failures"`
	RecentlyBroken     []TestFlakiness `json:"recently_broken"`
	// RecurringPatterns holds systemic job-level verdicts across all jobs.
	// The home page uses these without loading every job file.
	RecurringPatterns []PatternAnalysis `json:"recurring_patterns,omitempty"`
}
