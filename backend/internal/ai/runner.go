package ai

import (
	"context"
	"maps"
	"net/http"
	"slices"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// FailureAnalysisRequest is the complete input for one test-failure analysis.
type FailureAnalysisRequest struct {
	JobID               string           `json:"job_id"`
	BuildPrefix         string           `json:"build_prefix"`
	Build               models.BuildInfo `json:"build"`
	TestCase            models.TestCase  `json:"test_case"`
	ConsecutiveFailures int              `json:"consecutive_failures,omitempty"`
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
	run := models.BuildResult{BuildInfo: cloneBuildInfo(request.Build)}
	tc := cloneTestCase(request.TestCase)
	consecutiveFailures := max(1, request.ConsecutiveFailures)
	err := s.analyze(ctx, httpClient, request.JobID, request.BuildPrefix, &run, &tc, consecutiveFailures)
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
		analysis.FileLinks = maps.Clone(analysis.FileLinks)
		tc.AIAnalysis = &analysis
	}
	return tc
}

var _ FailureAnalyzer = (*Service)(nil)
