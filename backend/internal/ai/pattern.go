package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// patternPromptVersion is bumped when the pattern prompt or output contract
// changes, so cached verdicts from an older contract are re-run.
const patternPromptVersion = 10

const patternCacheVersion = 3

const patternFailureCacheVersion = 1

const defaultPatternFailureCooldown = 6 * time.Hour

// patternRepairVersion is included in pattern cache keys so repair contract
// changes invalidate verdicts produced by an older repair prompt.
const patternRepairVersion = 1

// maxPatternBuilds caps how many per-build analyses are fed into one pattern
// call, keeping the prompt bounded for a test that failed in many builds.
const maxPatternBuilds = 10

const maxPatternResponseBytes = 1 << 20

// PatternFailure is one build's analyzed job failure, used as input to
// cross-failure correlation. FailingTest is the specific test or spec that
// failed in this build and may differ across builds.
type PatternFailure struct {
	BuildID        string
	FailingTest    string
	FailureMessage string
	RootCause      string
	// SuggestedFix is per-build evidence that can help distinguish mechanisms.
	SuggestedFix string
	// RelevantFiles are the source files this build's analysis implicated.
	RelevantFiles []string
	// LocationFile is the failing test's source file. It is published as
	// supporting context but kept out of the prompt and pattern cache key.
	LocationFile string
	// Prow job source metadata identifies the exact test-infra snapshot that
	// selected and configured the failing job.
	ProwJobName        string
	ProwConfigFile     string
	ProwConfigRevision string
	IsTransient        bool
	Severity           string
	RecentRuns         []PatternRun
}

// PatternRun is one completed run in the recent correlation window.
type PatternRun struct {
	BuildID        string
	Result         string
	Passed         bool
	StartedAt      time.Time
	SourceRevision string
}

// PatternInput is the bounded, deterministic model input for one job-level
// correlation. Failures are newest-first and capped to the prompt limit.
type PatternInput struct {
	SystemPrompt string
	UserPrompt   string
	Failures     []PatternFailure
}

type patternCausalGroup struct {
	Builds     []string `json:"builds"`
	RootCause  string   `json:"root_cause"`
	Confidence string   `json:"confidence"`
}

// patternResponse is the model's analysis-only causal-group contract.
type patternResponse struct {
	Groups             []patternCausalGroup `json:"groups"`
	UnclassifiedBuilds []string             `json:"unclassified_builds"`
	Summary            string               `json:"summary"`
}

type patternCacheData struct {
	Version  int             `json:"version"`
	Response patternResponse `json:"response"`
}

type patternFailureCacheData struct {
	Version    int                    `json:"version"`
	JobID      string                 `json:"job_id,omitempty"`
	Category   PatternFailureCategory `json:"category"`
	FailedAt   time.Time              `json:"failed_at"`
	RetryAfter time.Time              `json:"retry_after"`
	PatternFailureDiagnostics
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type patternValidationCategory string

const (
	patternValidationJSON      patternValidationCategory = "json"
	patternValidationMissing   patternValidationCategory = "missing"
	patternValidationSchema    patternValidationCategory = "schema"
	patternValidationBuilds    patternValidationCategory = "builds"
	patternValidationAmbiguous patternValidationCategory = "ambiguous"
)

type patternParseStats struct {
	CandidateCount            int
	ValidCount                int
	UniqueValidCount          int
	IncompleteCount           int
	ContractLikeRejectedCount int
	ScanTruncated             bool
}

type patternValidationError struct {
	category patternValidationCategory
	issue    string
	stats    patternParseStats
}

// PatternRepairAttempt reports one bounded validation-repair completion.
type PatternRepairAttempt struct {
	Succeeded       bool
	FailureCategory PatternFailureCategory
}

// PatternAnalyzeOptions configure one full correlation attempt.
type PatternAnalyzeOptions struct {
	AllowAmbiguityRepair  bool
	AllowValidationRepair bool
	OnRepair              func(PatternRepairAttempt)
	OnCacheHit            func()
	OnFailureSuppressed   func(PatternFailureCategory)
	OnFreshRetry          func()
	diagnostics           *patternFailureDiagnosticsRecorder
}

// PatternFailureCategory is a privacy-safe pattern-attempt outcome.
type PatternFailureCategory string

const (
	PatternFailureNone             PatternFailureCategory = ""
	PatternFailureJSON             PatternFailureCategory = "json"
	PatternFailureMissing          PatternFailureCategory = "missing"
	PatternFailureSchema           PatternFailureCategory = "schema"
	PatternFailureBuilds           PatternFailureCategory = "builds"
	PatternFailureAmbiguous        PatternFailureCategory = "ambiguous"
	PatternFailureRequestTimeout   PatternFailureCategory = "request-timeout"
	PatternFailureRateLimited      PatternFailureCategory = "rate-limited"
	PatternFailureProvider5xx      PatternFailureCategory = "provider-5xx"
	PatternFailureProvider         PatternFailureCategory = "provider"
	PatternFailureContextHeadroom  PatternFailureCategory = "context-headroom"
	PatternFailureToolsUnsupported PatternFailureCategory = "tools-unsupported"
	PatternFailureCancelled        PatternFailureCategory = "cancelled"
	PatternFailureDeadline         PatternFailureCategory = "deadline"
	PatternFailureUnknown          PatternFailureCategory = "unknown"
)

// PatternProviderError reports only a provider status class. It never includes
// the response body.
type PatternProviderError struct {
	StatusCode int
}

type patternFailureSuppressedError struct {
	category PatternFailureCategory
}

func (e *patternFailureSuppressedError) Error() string {
	return fmt.Sprintf("pattern analysis: deterministic failure suppressed by cooldown (%s)", e.category)
}

func (e *PatternProviderError) Error() string {
	return fmt.Sprintf("pattern analysis: provider request failed (%s)", patternProviderFailureCategory(e.StatusCode))
}

func (e *patternValidationError) Error() string {
	return fmt.Sprintf("pattern analysis: response validation failed (%s)", e.category)
}

func patternValidationCategoryOf(err error) patternValidationCategory {
	var validationErr *patternValidationError
	if errors.As(err, &validationErr) {
		return validationErr.category
	}
	return ""
}

func patternValidationIssueOf(err error) string {
	var validationErr *patternValidationError
	if errors.As(err, &validationErr) {
		return validationErr.issue
	}
	return ""
}

// PatternFailureCategoryOf classifies a pattern error without exposing model
// output, provider bodies, prompts, or private paths.
func PatternFailureCategoryOf(err error) PatternFailureCategory {
	if err == nil {
		return PatternFailureNone
	}
	var classifiedErr *patternClassifiedError
	if errors.As(err, &classifiedErr) {
		return classifiedErr.category
	}
	if category := patternValidationCategoryOf(err); category != "" {
		return PatternFailureCategory(category)
	}
	var suppressedErr *patternFailureSuppressedError
	if errors.As(err, &suppressedErr) {
		return suppressedErr.category
	}
	var providerErr *PatternProviderError
	if errors.As(err, &providerErr) {
		return patternProviderFailureCategory(providerErr.StatusCode)
	}
	switch {
	case errors.Is(err, ErrContextHeadroom):
		return PatternFailureContextHeadroom
	case errors.Is(err, ErrToolsUnsupported):
		return PatternFailureToolsUnsupported
	case errors.Is(err, context.Canceled):
		return PatternFailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return PatternFailureDeadline
	default:
		return PatternFailureUnknown
	}
}

// IsPatternFailureSuppressed reports whether a deterministic failure was
// skipped because its exact input identity is still cooling down.
func IsPatternFailureSuppressed(err error) bool {
	var suppressedErr *patternFailureSuppressedError
	return errors.As(err, &suppressedErr)
}

// IsRetryablePatternError reports whether one fresh correlation attempt is
// allowed after the first failed attempt.
func IsRetryablePatternError(err error) bool {
	switch PatternFailureCategoryOf(err) {
	case PatternFailureRequestTimeout, PatternFailureRateLimited, PatternFailureProvider5xx:
		return true
	default:
		return false
	}
}

func patternProviderFailureCategory(statusCode int) PatternFailureCategory {
	switch {
	case statusCode == http.StatusRequestTimeout:
		return PatternFailureRequestTimeout
	case statusCode == http.StatusTooManyRequests:
		return PatternFailureRateLimited
	case statusCode >= 500 && statusCode <= 599:
		return PatternFailureProvider5xx
	default:
		return PatternFailureProvider
	}
}

func safePatternProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrContextHeadroom) || errors.Is(err, ErrToolsUnsupported) {
		return err
	}
	var patternErr *PatternProviderError
	if errors.As(err, &patternErr) {
		return patternErr
	}
	var httpErr *modelHTTPError
	if errors.As(err, &httpErr) {
		return &PatternProviderError{StatusCode: httpErr.StatusCode}
	}
	return &PatternProviderError{}
}

// patternSystemPrompt is frozen from the promoted causal-group evaluation.
const patternSystemPrompt = `You analyze failed builds of one CI job and return causal groups, not a product recurrence label. Put every failed build exactly once in one causal group or unclassified_builds. Group builds only when they share the same specific causal mechanism. Use singleton groups for failures whose individual cause is supported but does not repeat. Use unclassified_builds only when the evidence is insufficient to assign a cause. Recent passing runs are prevalence and lifecycle context and never group members. Root causes must be specific and non-empty. Call submit_causal_groups exactly once. Return no remediation, suggested fix, target, action, source-change, issue, or Fix PR field.`

func patternResponseFormat() ResponseFormat {
	stringProperty := func() map[string]any { return map[string]any{"type": "string"} }
	return ResponseFormat{
		Name:        "submit_causal_groups",
		Description: "Submit causal groups only.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"groups": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"builds":     map[string]any{"type": "array", "minItems": 1, "items": stringProperty()},
							"root_cause": stringProperty(),
							"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
						},
						"required": []string{"builds", "root_cause", "confidence"}, "additionalProperties": false,
					},
				},
				"unclassified_builds": map[string]any{"type": "array", "items": stringProperty()},
				"summary":             stringProperty(),
			},
			"required":             []string{"groups", "unclassified_builds", "summary"},
			"additionalProperties": false,
		},
	}
}

// AnalyzePattern correlates the per-build analyses of one repeatedly-failing
// job into a single PatternAnalysis. It permits one bounded contract repair.
func (s *Service) AnalyzePattern(ctx context.Context, jobID, subject string, failures []PatternFailure) (*models.PatternAnalysis, error) {
	return s.AnalyzePatternWithOptions(ctx, jobID, subject, failures, PatternAnalyzeOptions{AllowAmbiguityRepair: true, AllowValidationRepair: true})
}

// AnalyzePatternWithOptions runs one full correlation attempt.
func (s *Service) AnalyzePatternWithOptions(ctx context.Context, jobID, subject string, failures []PatternFailure, options PatternAnalyzeOptions) (_ *models.PatternAnalysis, resultErr error) {
	diagnostics := &patternFailureDiagnosticsRecorder{}
	options.diagnostics = diagnostics
	var trace *TraceSession
	if s.traceStore != nil {
		trace = s.traceStore.Start(TraceMetadata{JobID: jobID, TestName: subject, APIMode: s.client.APIMode(), Model: s.client.ModelName(), ReasoningEffort: string(s.client.ReasoningEffort())})
		ctx = withAnalysisTrace(ctx, trace)
		defer func() {
			outcome := "pattern_success"
			if IsPatternFailureSuppressed(resultErr) {
				outcome = "pattern_suppressed"
			} else if resultErr != nil {
				outcome = "pattern_error"
			}
			trace.Finish(outcome, resultErr)
		}()
	}

	input := BuildPatternInput(subject, failures)
	if len(input.Failures) < 2 {
		return nil, nil
	}
	failures = input.Failures
	usageOutcome := aiusage.OutcomeSuccess
	ctx, usageOperation := aiusage.Begin(ctx, s.usageRecorder, aiusage.Metadata{
		LogicalID: jobID + "\x00" + subject, Origin: s.usageOrigin,
		Feature: aiusage.FeaturePatternAnalysis, ModelFingerprint: s.client.modelFingerprint(), Model: s.client.ModelName(), ReasoningEffort: string(s.client.ReasoningEffort()),
		Correlation: aiusage.Correlation{JobID: jobID, TestName: subject},
	})
	defer func() {
		if IsPatternFailureSuppressed(resultErr) {
			usageOutcome = aiusage.OutcomeSuppressed
		} else if errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded) {
			usageOutcome = aiusage.OutcomeCancelled
		} else if resultErr != nil {
			usageOutcome = aiusage.OutcomeError
		}
		usageOperation.Finish(usageOutcome)
	}()
	userPrompt := input.UserPrompt
	groundKey := "causal-groups"

	key := patternCacheKey(s.module.Name(), s.cacheGeneration, jobID, subject, userPrompt, groundKey, s.client.modelFingerprint())
	failureKey := patternFailureCacheKey(key)
	buildIDs := patternBuildIDs(failures)
	if raw, ok := s.client.cache.Get(key); ok {
		var cachedData patternCacheData
		if json.Unmarshal(raw, &cachedData) == nil && cachedData.Version == patternCacheVersion {
			if _, _, err := parsePatternResponseWithStats(string(mustJSON(cachedData.Response)), buildIDs); err == nil {
				if options.OnCacheHit != nil {
					options.OnCacheHit()
				}
				s.client.cache.Delete(failureKey)
				usageOutcome = aiusage.OutcomeCacheHit
				return buildPatternAnalysis(subject, len(failures), cachedData.Response, collectRelevantFiles(failures)), nil
			}
		}
		if cached, stats, err := parsePatternResponseWithStats(string(raw), buildIDs); err == nil {
			if options.OnCacheHit != nil {
				options.OnCacheHit()
			}
			s.client.cache.Delete(failureKey)
			usageOutcome = aiusage.OutcomeCacheHit
			recordPatternParseTrace(ctx, "cache", stats, nil)
			return buildPatternAnalysis(subject, len(failures), cached, collectRelevantFiles(failures)), nil
		}
	}

	if failure, ok := s.patternFailureBackoff(failureKey); ok {
		now := s.patternFailureNow()
		if now.Before(failure.RetryAfter) {
			if options.OnFailureSuppressed != nil {
				options.OnFailureSuppressed(failure.Category)
			}
			return nil, &patternFailureSuppressedError{category: failure.Category}
		}
		s.client.cache.Delete(failureKey)
		aiusage.MarkCooldownRetry(ctx)
		if options.OnFreshRetry != nil {
			options.OnFreshRetry()
		}
	}
	defer func() {
		if resultErr == nil {
			s.client.cache.Delete(failureKey)
			return
		}
		details := diagnostics.snapshot()
		category := patternFailureCategoryWithDiagnostics(resultErr, details)
		if category != PatternFailureCategoryOf(resultErr) {
			resultErr = &patternClassifiedError{cause: resultErr, category: category}
		}
		s.persistPatternFailureBackoff(failureKey, jobID, resultErr, details)
	}()

	var parsed patternResponse
	parsed, err := s.toolFreePatternVerdict(ctx, userPrompt, buildIDs, options)
	if err != nil {
		return nil, err
	}
	_ = s.client.cache.Set(key, patternCacheData{Version: patternCacheVersion, Response: parsed})
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures)), nil
}

func (s *Service) patternFailureNow() time.Time {
	if s.patternNow == nil {
		return time.Now().UTC()
	}
	return s.patternNow().UTC()
}

func (s *Service) patternFailureBackoff(key string) (patternFailureCacheData, bool) {
	raw, ok := s.client.cache.Get(key)
	if !ok {
		return patternFailureCacheData{}, false
	}
	var failure patternFailureCacheData
	now := s.patternFailureNow()
	if json.Unmarshal(raw, &failure) != nil || !validPatternFailureCacheData(failure, now) {
		s.client.cache.Delete(key)
		return patternFailureCacheData{}, false
	}
	failure.PatternFailureDiagnostics = sanitizePatternFailureDiagnostics(failure.PatternFailureDiagnostics)
	return failure, true
}

func validPatternFailureCacheData(failure patternFailureCacheData, now time.Time) bool {
	return failure.Version == patternFailureCacheVersion &&
		isDeterministicPatternFailureCategory(failure.Category) && !failure.FailedAt.IsZero() &&
		!failure.FailedAt.After(now.Add(cacheMaxFutureSkew)) && !failure.RetryAfter.IsZero() &&
		failure.RetryAfter.After(failure.FailedAt) && failure.RetryAfter.Sub(failure.FailedAt) <= defaultPatternFailureCooldown
}

func (s *Service) persistPatternFailureBackoff(key, jobID string, err error, diagnostics PatternFailureDiagnostics) {
	category := PatternFailureCategoryOf(err)
	if !isDeterministicPatternFailureCategory(category) {
		return
	}
	cooldown := s.patternFailureCooldown
	if cooldown <= 0 {
		cooldown = defaultPatternFailureCooldown
	}
	now := s.patternFailureNow()
	jobID = strings.TrimSpace(jobID)
	if !patternDiagnosticJobRE.MatchString(jobID) {
		jobID = ""
	}
	_ = s.client.cache.Set(key, patternFailureCacheData{
		Version: patternFailureCacheVersion, JobID: jobID, Category: category,
		FailedAt: now, RetryAfter: now.Add(cooldown),
		PatternFailureDiagnostics: sanitizePatternFailureDiagnostics(diagnostics),
	})
}

func isDeterministicPatternFailureCategory(category PatternFailureCategory) bool {
	switch category {
	case PatternFailureJSON, PatternFailureMissing, PatternFailureSchema, PatternFailureBuilds, PatternFailureAmbiguous:
		return true
	default:
		return false
	}
}

func patternFailureCacheKey(patternKey string) string {
	return "pattern-failure:" + strings.TrimPrefix(patternKey, "pattern:")
}

// BuildPatternInput renders the pattern-analysis contract.
func BuildPatternInput(subject string, failures []PatternFailure) PatternInput {
	prepared := append([]PatternFailure(nil), failures...)
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].BuildID > prepared[j].BuildID })
	if len(prepared) > maxPatternBuilds {
		prepared = prepared[:maxPatternBuilds]
	}
	return PatternInput{
		SystemPrompt: patternSystemPrompt,
		UserPrompt:   buildPatternUserPrompt(subject, prepared),
		Failures:     prepared,
	}
}

// ParsePatternResult validates a model correlation result and converts it to
// the published PatternAnalysis shape.
func ParsePatternResult(subject string, failures []PatternFailure, result string) (*models.PatternAnalysis, error) {
	parsed, err := parsePatternResponse(result, patternBuildIDs(failures))
	if err != nil {
		return nil, err
	}
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures)), nil
}

// toolFreePatternVerdict forces one exact causal-group function call.
func (s *Service) toolFreePatternVerdict(ctx context.Context, userPrompt string, buildIDs map[string]struct{}, options PatternAnalyzeOptions) (patternResponse, error) {
	started := time.Now()
	var parsed patternResponse
	var parseStats patternParseStats
	var parseErr error
	var rawOutput string
	validate := func(raw json.RawMessage) error {
		rawOutput = string(raw)
		candidate, stats, err := parsePatternResponseWithStats(rawOutput, buildIDs)
		parseStats, parseErr = stats, err
		if err != nil {
			options.diagnostics.recordValidation("tool_free", stats, err)
			return err
		}
		parsed = candidate
		return nil
	}
	err := s.client.completeForcedFunction(ctx, patternSystemPrompt, userPrompt, patternResponseFormat(), validate)
	if err != nil {
		var structuredErr *structuredCompletionError
		if parseErr != nil && errors.As(err, &structuredErr) && structuredErr.cause == nil {
			err = parseErr
			recordPatternParseTrace(ctx, "tool_free", parseStats, err)
			recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "tool_free", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
			switch patternValidationCategoryOf(err) {
			case patternValidationAmbiguous:
				if options.AllowAmbiguityRepair {
					return s.repairPatternAmbiguity(ctx, rawOutput, buildIDs, options)
				}
			case patternValidationSchema, patternValidationBuilds:
				if options.AllowValidationRepair {
					return s.repairPatternValidation(ctx, rawOutput, buildIDs, err, options)
				}
			}
		} else if errors.As(err, &structuredErr) && structuredErr.cause == nil {
			err = &patternValidationError{category: patternValidationMissing, issue: "missing_message"}
			options.diagnostics.recordValidation("tool_free", patternParseStats{}, err)
		} else {
			err = safePatternProviderError(err)
		}
		recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "tool_free", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		return patternResponse{}, err
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "tool_free", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	recordPatternParseTrace(ctx, "tool_free", parseStats, nil)
	return parsed, nil
}

func (s *Service) repairPatternValidation(ctx context.Context, output string, buildIDs map[string]struct{}, validationErr error, options PatternAnalyzeOptions) (patternResponse, error) {
	options.diagnostics.beginRepair("validation")
	observe := options.OnRepair
	category := patternValidationCategoryOf(validationErr)
	issue := patternValidationIssueOf(validationErr)
	if issue == "" {
		issue = "contract_validation"
	}
	prompt := fmt.Sprintf("Repair contract version %d. The prior bounded investigation produced a recurring-pattern contract that failed deterministic validation. Validation category: %s. Validation issue: %s. Correct the contract without weakening or omitting any required field. Do not add an explanation or code fence. Return the corrected contract.\n\nPrior output:\n%s", patternRepairVersion, category, issue, output)
	started := time.Now()
	var parsed patternResponse
	var acceptedStats patternParseStats
	var parseStats patternParseStats
	var parseErr error
	validate := func(raw json.RawMessage) error {
		candidate, stats, err := parsePatternResponseWithStats(string(raw), buildIDs)
		if err != nil {
			parseStats, parseErr = stats, err
			options.diagnostics.recordRepair("validation", stats, err)
			return err
		}
		parsed, acceptedStats = candidate, stats
		return nil
	}
	err := s.client.completeForcedFunction(ctx, "Return exactly one valid causal-group contract.", prompt, patternResponseFormat(), validate)
	if err != nil {
		var structuredErr *structuredCompletionError
		validationOnly := errors.As(err, &structuredErr) && structuredErr.cause == nil
		switch {
		case parseErr != nil && validationOnly:
			err = parseErr
			recordPatternParseTrace(ctx, "repair", parseStats, err)
		case validationOnly:
			err = &patternValidationError{category: patternValidationMissing, issue: "repair_no_contract"}
			options.diagnostics.recordRepair("validation", patternParseStats{}, err)
			recordPatternParseTrace(ctx, "repair", patternParseStats{}, err)
		default:
			err = safePatternProviderError(err)
		}
		recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "validation", Outcome: "rejected", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err), ValidationCode: patternValidationIssueOf(err)})
		if observe != nil {
			observe(PatternRepairAttempt{Succeeded: false, FailureCategory: PatternFailureCategoryOf(err)})
		}
		return patternResponse{}, err
	}
	recordPatternParseTrace(ctx, "repair", acceptedStats, nil)
	recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "validation", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	if observe != nil {
		observe(PatternRepairAttempt{Succeeded: true})
	}
	return parsed, nil
}

func (s *Service) repairPatternAmbiguity(ctx context.Context, output string, buildIDs map[string]struct{}, options PatternAnalyzeOptions) (patternResponse, error) {
	options.diagnostics.beginRepair("ambiguity")
	observe := options.OnRepair
	prompt := fmt.Sprintf("Repair contract version %d. Resolve the prior conflicting causal-group candidates into exactly one contract with groups, unclassified_builds, and summary. Preserve exact build coverage and add no remediation or action fields.\n\nInvestigation output:\n%s", patternRepairVersion, output)
	started := time.Now()
	var parsed patternResponse
	var stats patternParseStats
	var parseErr error
	validate := func(raw json.RawMessage) error {
		candidate, candidateStats, err := parsePatternResponseWithStats(string(raw), buildIDs)
		stats, parseErr = candidateStats, err
		if err == nil {
			parsed = candidate
		}
		return err
	}
	err := s.client.completeForcedFunction(ctx, "Return one final causal-group contract.", prompt, patternResponseFormat(), validate)
	if err != nil {
		var structuredErr *structuredCompletionError
		if parseErr != nil && errors.As(err, &structuredErr) && structuredErr.cause == nil {
			err = parseErr
		} else {
			err = safePatternProviderError(err)
		}
		recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "ambiguity", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		if observe != nil {
			observe(PatternRepairAttempt{FailureCategory: PatternFailureCategoryOf(err)})
		}
		return patternResponse{}, err
	}
	recordPatternParseTrace(ctx, "repair", stats, err)
	options.diagnostics.recordRepair("ambiguity", stats, err)
	outcome := "success"
	if err != nil {
		outcome = "validation_error"
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "ambiguity", Outcome: outcome, DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
	if observe != nil {
		observe(PatternRepairAttempt{Succeeded: err == nil, FailureCategory: PatternFailureCategoryOf(err)})
	}
	return parsed, err
}

func patternTraceErrorCode(err error) string {
	category := PatternFailureCategoryOf(err)
	if category == PatternFailureNone {
		return ""
	}
	return strings.ReplaceAll(string(category), "-", "_")
}

func recordPatternParseTrace(ctx context.Context, stage string, stats patternParseStats, err error) {
	outcome := "accepted"
	if err != nil {
		outcome = "rejected"
	}
	recordTrace(ctx, TraceEvent{
		Kind: "pattern_parse", Status: stage, Outcome: outcome,
		CandidateCount: stats.CandidateCount, ValidCount: stats.ValidCount,
		UniqueCandidateCount: stats.UniqueValidCount, IncompleteCount: stats.IncompleteCount,
		ContractLikeRejectedCount: stats.ContractLikeRejectedCount, ScanTruncated: stats.ScanTruncated,
		ErrorCode: patternTraceErrorCode(err), ValidationCode: patternValidationIssueOf(err),
	})
}

func parsePatternResponse(raw string, buildIDs map[string]struct{}) (patternResponse, error) {
	parsed, _, err := parsePatternResponseWithStats(raw, buildIDs)
	return parsed, err
}

func parsePatternResponseWithStats(raw string, buildIDs map[string]struct{}) (patternResponse, patternParseStats, error) {
	stats := patternParseStats{}
	validationError := func(category patternValidationCategory, issue string) (patternResponse, patternParseStats, error) {
		return patternResponse{}, stats, &patternValidationError{category: category, issue: issue, stats: stats}
	}
	if len(raw) > maxPatternResponseBytes {
		return validationError(patternValidationJSON, "response_too_large")
	}
	scan := scanAnalysisChatJSONCandidates(raw)
	stats.CandidateCount = len(scan.candidates)
	stats.IncompleteCount = len(scan.incomplete)
	stats.ScanTruncated = scan.truncated
	if scan.truncated {
		return validationError(patternValidationAmbiguous, "candidate_scan_truncated")
	}
	type validCandidate struct {
		response patternResponse
		start    int
		end      int
	}
	type rejectedCandidate struct {
		start        int
		end          int
		category     patternValidationCategory
		issue        string
		contractLike bool
	}
	valid := make([]validCandidate, 0, 1)
	rejected := make([]rejectedCandidate, 0, len(scan.candidates))
	contractLikeSeen := false
	bestCategory := patternValidationJSON
	bestIssue := "invalid_json"
	unique := map[string]patternResponse{}
	for _, candidate := range scan.candidates {
		decoded, category, issue := decodePatternCandidate(candidate.value, buildIDs)
		if category == "" {
			valid = append(valid, validCandidate{response: decoded.response, start: candidate.start, end: candidate.end})
			unique[decoded.identity] = decoded.response
			continue
		}
		contractLike := patternCandidateIsContractLike(candidate.value)
		rejected = append(rejected, rejectedCandidate{
			start: candidate.start, end: candidate.end, category: category, issue: issue, contractLike: contractLike,
		})
		if contractLike {
			stats.ContractLikeRejectedCount++
		}
		contractLikeSeen = contractLikeSeen || contractLike
		if patternValidationRank(category) > patternValidationRank(bestCategory) {
			bestCategory = category
			bestIssue = issue
		}
	}
	stats.ValidCount = len(valid)
	stats.UniqueValidCount = len(unique)
	if len(valid) == 0 {
		if !contractLikeSeen && len(scan.incomplete) == 0 {
			return validationError(patternValidationMissing, "no_contract")
		}
		return validationError(bestCategory, bestIssue)
	}

	firstStart, firstEnd, lastEnd := valid[0].start, valid[0].end, valid[0].end
	for _, candidate := range valid[1:] {
		if candidate.start < firstStart {
			firstStart, firstEnd = candidate.start, candidate.end
		}
		if candidate.end > lastEnd {
			lastEnd = candidate.end
		}
	}
	for _, incomplete := range scan.incomplete {
		if incomplete.start > firstEnd {
			return validationError(patternValidationJSON, "trailing_incomplete_json")
		}
	}
	for _, candidate := range rejected {
		if candidate.contractLike && (candidate.start > firstEnd ||
			(candidate.start < firstStart && candidate.end > lastEnd)) {
			return validationError(candidate.category, candidate.issue)
		}
	}
	if len(unique) != 1 {
		return validationError(patternValidationAmbiguous, "conflicting_valid_contracts")
	}
	for _, parsed := range unique {
		return parsed, stats, nil
	}
	panic("unreachable canonical pattern response")
}

type decodedPatternCandidate struct {
	response patternResponse
	identity string
}

func canonicalizePatternResponse(parsed patternResponse) patternResponse {
	parsed.Summary = strings.TrimSpace(parsed.Summary)
	for index := range parsed.Groups {
		group := &parsed.Groups[index]
		group.RootCause = strings.TrimSpace(group.RootCause)
		group.Confidence = strings.ToLower(strings.TrimSpace(group.Confidence))
		group.Builds = canonicalBuildIDs(group.Builds)
	}
	sort.Slice(parsed.Groups, func(i, j int) bool {
		left, right := strings.Join(parsed.Groups[i].Builds, "\x00"), strings.Join(parsed.Groups[j].Builds, "\x00")
		if left != right {
			return left < right
		}
		if parsed.Groups[i].RootCause != parsed.Groups[j].RootCause {
			return parsed.Groups[i].RootCause < parsed.Groups[j].RootCause
		}
		return parsed.Groups[i].Confidence < parsed.Groups[j].Confidence
	})
	parsed.UnclassifiedBuilds = canonicalBuildIDs(parsed.UnclassifiedBuilds)
	return parsed
}

func canonicalBuildIDs(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.TrimSpace(value)
	}
	sort.Strings(out)
	return out
}

func decodePatternCandidate(raw string, buildIDs map[string]struct{}) (decodedPatternCandidate, patternValidationCategory, string) {
	fields, category, issue := decodePatternObject(raw)
	if category != "" {
		return decodedPatternCandidate{}, category, issue
	}
	required := []string{"groups", "unclassified_builds", "summary"}
	if len(fields) != len(required) {
		return decodedPatternCandidate{}, patternValidationSchema, "required_fields"
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return decodedPatternCandidate{}, patternValidationSchema, "required_fields"
		}
	}
	var parsed patternResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return decodedPatternCandidate{}, patternValidationSchema, "field_types"
	}
	if !decodePatternGroups(fields["groups"], &parsed.Groups) {
		return decodedPatternCandidate{}, patternValidationSchema, "groups"
	}
	parsed = canonicalizePatternResponse(parsed)
	if category, issue := patternResponseValidation(parsed, buildIDs); category != "" {
		return decodedPatternCandidate{}, category, issue
	}
	identity, err := json.Marshal(parsed)
	if err != nil {
		return decodedPatternCandidate{}, patternValidationJSON, "canonical_json"
	}
	return decodedPatternCandidate{response: parsed, identity: string(identity)}, "", ""
}

func decodePatternGroups(raw json.RawMessage, groups *[]patternCausalGroup) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return false
	}
	parsed := make([]patternCausalGroup, 0)
	for decoder.More() {
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return false
		}
		fields, category, _ := decodePatternObject(string(item))
		if category != "" || len(fields) != 3 {
			return false
		}
		for _, name := range []string{"builds", "root_cause", "confidence"} {
			value, ok := fields[name]
			if !ok || strings.TrimSpace(string(value)) == "null" {
				return false
			}
		}
		var group patternCausalGroup
		if err := json.Unmarshal(item, &group); err != nil {
			return false
		}
		parsed = append(parsed, group)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	*groups = parsed
	return true
}

func decodePatternObject(raw string) (map[string]json.RawMessage, patternValidationCategory, string) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, patternValidationJSON, "invalid_json"
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, patternValidationJSON, "invalid_json"
		}
		name, ok := token.(string)
		if !ok {
			return nil, patternValidationJSON, "invalid_json"
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, patternValidationSchema, "duplicate_field"
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, patternValidationJSON, "invalid_json"
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, patternValidationJSON, "invalid_json"
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, patternValidationJSON, "trailing_json"
	}
	return fields, "", ""
}

func patternCandidateIsContractLike(raw string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) == nil {
		for _, field := range []string{"groups", "unclassified_builds", "summary"} {
			if _, ok := fields[field]; ok {
				return true
			}
		}
		return false
	}
	for _, field := range []string{"groups", "unclassified_builds", "summary"} {
		if strings.Contains(raw, `"`+field+`"`) {
			return true
		}
	}
	return false
}

func patternResponseValidation(p patternResponse, buildIDs map[string]struct{}) (patternValidationCategory, string) {
	if strings.TrimSpace(p.Summary) == "" {
		return patternValidationSchema, "summary"
	}
	seen := make(map[string]struct{}, len(buildIDs))
	for _, group := range p.Groups {
		if len(group.Builds) == 0 || strings.TrimSpace(group.RootCause) == "" {
			return patternValidationSchema, "groups"
		}
		switch strings.ToLower(strings.TrimSpace(group.Confidence)) {
		case "high", "medium", "low":
		default:
			return patternValidationSchema, "confidence"
		}
		for _, buildID := range group.Builds {
			if category, issue := validatePatternBuild(buildID, buildIDs, seen); category != "" {
				return category, issue
			}
		}
	}
	for _, buildID := range p.UnclassifiedBuilds {
		if category, issue := validatePatternBuild(buildID, buildIDs, seen); category != "" {
			return category, issue
		}
	}
	if buildIDs != nil && len(seen) != len(buildIDs) {
		return patternValidationBuilds, "missing_build"
	}
	return "", ""
}

func validatePatternBuild(buildID string, buildIDs, seen map[string]struct{}) (patternValidationCategory, string) {
	buildID = strings.TrimSpace(buildID)
	if buildID == "" {
		return patternValidationBuilds, "unknown_build"
	}
	if buildIDs != nil {
		if _, ok := buildIDs[buildID]; !ok {
			return patternValidationBuilds, "unknown_build"
		}
	}
	if _, duplicate := seen[buildID]; duplicate {
		return patternValidationBuilds, "duplicate_build"
	}
	seen[buildID] = struct{}{}
	return "", ""
}

func patternBuildIDs(failures []PatternFailure) map[string]struct{} {
	ids := make(map[string]struct{}, len(failures))
	for _, failure := range failures {
		if id := strings.TrimSpace(failure.BuildID); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func patternValidationRank(category patternValidationCategory) int {
	switch category {
	case patternValidationBuilds:
		return 3
	case patternValidationSchema:
		return 2
	case patternValidationJSON:
		return 1
	default:
		return 0
	}
}

// collectRelevantFiles unions the files each build implicated, in first-seen
// order, leading with the failing test's own source file. These carry the
// analysis's own targeting into the pattern so the fix harness can ground
// candidate selection on them rather than re-deriving targets from scratch.
func collectRelevantFiles(failures []PatternFailure) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for _, f := range failures {
		add(f.LocationFile)
		for _, rf := range f.RelevantFiles {
			add(rf)
		}
	}
	return out
}

// buildPatternAnalysis converts causal groups into the published model.
func buildPatternAnalysis(subject string, builds int, p patternResponse, relevantFiles []string) *models.PatternAnalysis {
	groups := make([]models.PatternCausalGroup, 0, len(p.Groups))
	repeated := make([]patternCausalGroup, 0, len(p.Groups))
	sharedBuilds := make([]string, 0, builds)
	for _, group := range p.Groups {
		groups = append(groups, models.PatternCausalGroup{
			Builds: append([]string(nil), group.Builds...), RootCause: group.RootCause, Confidence: group.Confidence,
		})
		if len(group.Builds) < 2 {
			continue
		}
		repeated = append(repeated, group)
		sharedBuilds = append(sharedBuilds, group.Builds...)
	}
	sort.Strings(sharedBuilds)

	recurrence := models.PatternRecurrenceUnrelated
	if len(repeated) == 0 && len(p.UnclassifiedBuilds) == builds {
		recurrence = models.PatternRecurrenceInsufficientEvidence
	} else if len(repeated) == 1 {
		recurrence = models.PatternRecurrenceSharedCause
	} else if len(repeated) > 1 {
		recurrence = models.PatternRecurrenceMixedCauses
	}
	systemic := recurrence == models.PatternRecurrenceSharedCause || recurrence == models.PatternRecurrenceMixedCauses

	confidenceGroups := repeated
	if len(confidenceGroups) == 0 {
		confidenceGroups = p.Groups
	}
	confidence := "low"
	if len(confidenceGroups) > 0 {
		confidence = "high"
		for _, group := range confidenceGroups {
			if patternConfidenceRank(group.Confidence) < patternConfidenceRank(confidence) {
				confidence = group.Confidence
			}
		}
	}

	rootCause := ""
	if len(repeated) == 1 {
		rootCause = repeated[0].RootCause
	} else if len(repeated) > 1 {
		causes := make([]string, 0, len(repeated))
		for _, group := range repeated {
			causes = append(causes, group.RootCause)
		}
		rootCause = "Multiple recurring causes: " + strings.Join(causes, "; ")
	}

	return &models.PatternAnalysis{
		Subject: subject, GeneratedAt: time.Now().UTC().Format(time.RFC3339), BuildsAnalyzed: builds,
		Recurrence: recurrence, CausalGroups: groups,
		UnclassifiedBuilds: append([]string(nil), p.UnclassifiedBuilds...),
		Systemic:           systemic, Confidence: confidence, SharedRootCause: rootCause, SharedBuilds: sharedBuilds,
		Summary: strings.TrimSpace(p.Summary), RelevantFiles: relevantFiles,
	}
}

func patternConfidenceRank(confidence string) int {
	switch confidence {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// buildPatternUserPrompt renders the per-build analyses into the user message.
func buildPatternUserPrompt(subject string, failures []PatternFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job: %s\n", subject)
	runs := patternRecentRuns(failures)
	totalRuns, failedRuns, passingRuns := patternRunWindow(runs, len(failures))
	fmt.Fprintf(&b, "Recent completed run window: %d total, %d failed, %d passed.\n", totalRuns, failedRuns, passingRuns)
	if len(runs) > 0 {
		b.WriteString("Recent completed runs (newest first):\n")
		for _, run := range runs {
			outcome := "failed"
			if run.Passed {
				outcome = "passed"
			}
			fmt.Fprintf(&b, "- build %s: %s", run.BuildID, outcome)
			if result := strings.TrimSpace(run.Result); result != "" {
				fmt.Fprintf(&b, " (%s)", result)
			}
			if !run.StartedAt.IsZero() {
				fmt.Fprintf(&b, ", started %s", run.StartedAt.UTC().Format(time.RFC3339))
			}
			if revision := strings.TrimSpace(run.SourceRevision); revision != "" {
				fmt.Fprintf(&b, ", source revision %s", revision)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "This correlation includes %d analyzed failed builds. The per-build analyses follow (the failing test/spec may differ between builds).\n\n", len(failures))
	for i, f := range failures {
		fmt.Fprintf(&b, "--- Build %d (id %s) ---\n", i+1, f.BuildID)
		if f.FailingTest != "" {
			fmt.Fprintf(&b, "failing_test: %s\n", f.FailingTest)
		}
		if f.IsTransient {
			b.WriteString("classified_transient: yes\n")
		}
		if f.Severity != "" {
			fmt.Fprintf(&b, "severity: %s\n", f.Severity)
		}
		if f.RootCause != "" {
			fmt.Fprintf(&b, "root_cause: %s\n", clampPattern(f.RootCause, 1500))
		}
		if f.SuggestedFix != "" {
			fmt.Fprintf(&b, "suggested_fix: %s\n", clampPattern(f.SuggestedFix, 600))
		}
		if len(f.RelevantFiles) > 0 {
			fmt.Fprintf(&b, "relevant_files: %s\n", clampPattern(strings.Join(f.RelevantFiles, ", "), 400))
		}
		if f.ProwConfigFile != "" || f.ProwConfigRevision != "" {
			fmt.Fprintf(&b, "prow_job_name: %s\n", f.ProwJobName)
			fmt.Fprintf(&b, "test_infra_config_file: %s\n", f.ProwConfigFile)
			fmt.Fprintf(&b, "test_infra_revision: %s\n", f.ProwConfigRevision)
		}
		if f.FailureMessage != "" {
			fmt.Fprintf(&b, "failure_message: %s\n", clampPattern(f.FailureMessage, 600))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func patternRecentRuns(failures []PatternFailure) []PatternRun {
	for _, failure := range failures {
		if len(failure.RecentRuns) == 0 {
			continue
		}
		runs := append([]PatternRun(nil), failure.RecentRuns...)
		sort.SliceStable(runs, func(i, j int) bool {
			if !runs[i].StartedAt.Equal(runs[j].StartedAt) {
				if runs[i].StartedAt.IsZero() {
					return false
				}
				if runs[j].StartedAt.IsZero() {
					return true
				}
				return runs[i].StartedAt.After(runs[j].StartedAt)
			}
			return runs[i].BuildID > runs[j].BuildID
		})
		return runs
	}
	return nil
}

func patternRunWindow(runs []PatternRun, fallbackFailures int) (total, failed, passed int) {
	if len(runs) == 0 {
		return fallbackFailures, fallbackFailures, 0
	}
	for _, run := range runs {
		if run.Passed {
			passed++
		} else {
			failed++
		}
	}
	return len(runs), failed, passed
}

// patternCacheKey keys a verdict by the project module, job, contract version,
// model identity, and rendered input.
func patternCacheKey(module, generation, jobID, subject, userPrompt, groundKey, modelFingerprint string) string {
	return patternCacheKeyForVersions(patternPromptVersion, patternRepairVersion, module, generation, jobID, subject, userPrompt, groundKey, modelFingerprint)
}

func patternCacheKeyForVersions(promptVersion, repairVersion int, module, generation, jobID, subject, userPrompt, groundKey, modelFingerprint string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d:r%d\x00%s\x00%s\x00%s\x00%s\x00%s", promptVersion, repairVersion, groundKey, modelFingerprint, jobID, subject, userPrompt)
	digest := hex.EncodeToString(h.Sum(nil)[:12])
	if generation == "" {
		return fmt.Sprintf("pattern:%s:%s", module, digest)
	}
	return fmt.Sprintf("pattern:%s:g:%s:%s", module, generation, digest)
}

// clampPattern trims a field to max bytes so one verbose analysis can't blow
// the pattern prompt budget.
func clampPattern(s string, max int) string {
	return textutil.Truncate(strings.TrimSpace(s), max)
}
