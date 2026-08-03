package ai

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// ProwJobContext identifies the current source definition for one Prow job.
// The failed run's prowjob.json remains authoritative for what executed.
type ProwJobContext struct {
	Name           string `json:"name,omitempty"`
	JobType        string `json:"job_type,omitempty"`
	ConfigFile     string `json:"config_file,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`
}

const (
	maxProwJobNameBytes       = 512
	maxProwJobTypeBytes       = 32
	maxProwJobConfigFileBytes = 2048
	maxProwJobRevisionBytes   = 128
)

// CanonicalProwJobContext returns bounded single-line metadata without mutating input.
func CanonicalProwJobContext(context *ProwJobContext) *ProwJobContext {
	if context == nil {
		return nil
	}
	out := *context
	out.Name = canonicalProwJobField(out.Name, maxProwJobNameBytes)
	out.JobType = canonicalProwJobField(out.JobType, maxProwJobTypeBytes)
	out.ConfigFile = canonicalProwJobField(out.ConfigFile, maxProwJobConfigFileBytes)
	out.ConfigRevision = canonicalProwJobField(out.ConfigRevision, maxProwJobRevisionBytes)
	if out.Name == "" && out.JobType == "" && out.ConfigFile == "" && out.ConfigRevision == "" {
		return nil
	}
	return &out
}

func canonicalProwJobField(value string, maxBytes int) string {
	value = strings.ToValidUTF8(strings.Join(strings.Fields(value), " "), "")
	for len(value) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

// FailureCohortContext describes equivalent failures sharing one representative analysis.
type FailureCohortContext struct {
	Count     int      `json:"count"`
	TestNames []string `json:"test_names,omitempty"`
}

const maxFailureCohortNames = 8

// CanonicalFailureCohortContext returns bounded metadata without mutating input.
func CanonicalFailureCohortContext(context *FailureCohortContext) *FailureCohortContext {
	if context == nil || context.Count < 2 {
		return nil
	}
	out := FailureCohortContext{Count: context.Count}
	for _, name := range context.TestNames {
		if len(out.TestNames) >= maxFailureCohortNames {
			break
		}
		if name = canonicalProwJobField(name, maxProwJobNameBytes); name != "" {
			out.TestNames = append(out.TestNames, name)
		}
	}
	return &out
}

// FailureAnalysisRequest is the complete input for one test-failure analysis.
type FailureAnalysisRequest struct {
	JobID               string                `json:"job_id"`
	BuildPrefix         string                `json:"build_prefix"`
	Build               models.BuildInfo      `json:"build"`
	TestCase            models.TestCase       `json:"test_case"`
	ProwJob             *ProwJobContext       `json:"prow_job,omitempty"`
	FailureCohort       *FailureCohortContext `json:"failure_cohort,omitempty"`
	ConsecutiveFailures int                   `json:"consecutive_failures,omitempty"`
	CacheGeneration     string                `json:"cache_generation,omitempty"`
}

// FailureAnalysisResult is the dashboard analysis output for one test failure.
type FailureAnalysisResult struct {
	Summary  *models.AISummary  `json:"ai_summary,omitempty"`
	Analysis *models.AIAnalysis `json:"ai_analysis,omitempty"`
}

// FailureAnalyzer runs the dashboard-owned analysis policy for one failure.
type FailureAnalyzer interface {
	AnalyzeFailure(context.Context, *http.Client, FailureAnalysisRequest) (FailureAnalysisResult, error)
}

// AnalyzeFailure runs one analysis without mutating the request values. Errors
// are also represented by the returned unavailable summary for existing callers.
func (s *Service) AnalyzeFailure(ctx context.Context, httpClient *http.Client, request FailureAnalysisRequest) (FailureAnalysisResult, error) {
	if request.CacheGeneration != s.cacheGeneration {
		err := fmt.Errorf("analysis cache generation mismatch")
		return UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	run := models.BuildResult{BuildInfo: cloneBuildInfo(request.Build)}
	tc := cloneTestCase(request.TestCase)
	consecutiveFailures := max(1, request.ConsecutiveFailures)
	err := s.analyze(ctx, httpClient, request.JobID, request.BuildPrefix, &run, &tc, consecutiveFailures, request.ProwJob, request.FailureCohort)
	return FailureAnalysisResult{Summary: tc.AISummary, Analysis: tc.AIAnalysis}, err
}

func cloneBuildInfo(build models.BuildInfo) models.BuildInfo {
	build.JUnitURLs = slices.Clone(build.JUnitURLs)
	build.RepoRefs = maps.Clone(build.RepoRefs)
	return build
}

func cloneTestCase(tc models.TestCase) models.TestCase {
	if tc.AISummary != nil {
		summary := *tc.AISummary
		tc.AISummary = &summary
	}
	if tc.AIAnalysis != nil {
		analysis := *tc.AIAnalysis
		analysis.RelevantFiles = slices.Clone(analysis.RelevantFiles)
		analysis.SearchSuggestions = slices.Clone(analysis.SearchSuggestions)
		analysis.EvidenceCitations = slices.Clone(analysis.EvidenceCitations)
		analysis.FileLinks = maps.Clone(analysis.FileLinks)
		tc.AIAnalysis = &analysis
	}
	return tc
}

// UnavailableFailureAnalysisResult applies the standard error behavior without replacing an accepted result.
func UnavailableFailureAnalysisResult(testCase models.TestCase, err error) FailureAnalysisResult {
	tc := cloneTestCase(testCase)
	(&Service{}).setUnavailable(&tc, err)
	return FailureAnalysisResult{Summary: tc.AISummary, Analysis: tc.AIAnalysis}
}

var _ FailureAnalyzer = (*Service)(nil)
