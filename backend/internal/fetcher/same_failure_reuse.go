package fetcher

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

func systemicContainerAnalysisError(err error) error {
	switch {
	case analysisruntime.IsProjectBundleSourceError(err):
		return fmt.Errorf("systemic Orka project setup failure: %w", err)
	case orka.IsResultAuthorizationError(err):
		return fmt.Errorf("systemic Orka result API authorization failure: %w", err)
	case analysisruntime.IsContainerStateDecryptionError(err), analysisruntime.IsContainerStateIdentityError(err):
		return fmt.Errorf("systemic Orka result state integrity failure: %w", err)
	default:
		return nil
	}
}

func analysisExecutionRequest(execution analysisExecution, consecutiveFailures int, cacheGeneration string) ai.FailureAnalysisRequest {
	request := execution.Work[0].request(consecutiveFailures, cacheGeneration)
	if len(execution.Work) > 1 {
		names := make([]string, 0, len(execution.Work))
		for _, item := range execution.Work {
			names = append(names, item.tc.Name)
		}
		request.FailureCohort = &ai.FailureCohortContext{Count: len(execution.Work), TestNames: names}
	}
	return request
}

func cloneFailureAnalysisResult(result ai.FailureAnalysisResult) ai.FailureAnalysisResult {
	out := result
	if result.Summary != nil {
		summary := *result.Summary
		out.Summary = &summary
	}
	if result.Analysis != nil {
		analysis := *result.Analysis
		analysis.RelevantFiles = slices.Clone(result.Analysis.RelevantFiles)
		analysis.SearchSuggestions = slices.Clone(result.Analysis.SearchSuggestions)
		analysis.EvidenceCitations = slices.Clone(result.Analysis.EvidenceCitations)
		analysis.FileLinks = maps.Clone(result.Analysis.FileLinks)
		out.Analysis = &analysis
	}
	return out
}

func prepareSameFailureReuse(ctx context.Context, client *http.Client, item aiWork, result ai.FailureAnalysisResult, state *analysisruntime.ContainerStateStore, planner *ai.Service, consecutiveFailures int, cacheGeneration string) (ai.FailureAnalysisResult, bool) {
	if result.Summary == nil || result.Analysis == nil || state == nil || planner == nil {
		return ai.FailureAnalysisResult{}, false
	}
	request := item.request(consecutiveFailures, cacheGeneration)
	policy, err := analysisruntime.FailureCachePolicy(ctx, client, request, planner)
	if err != nil || ai.AgenticResultRejection(result, policy) != ai.CacheAccepted {
		return ai.FailureAnalysisResult{}, false
	}
	createdAt, err := time.Parse(time.RFC3339, result.Analysis.GeneratedAt)
	if err != nil {
		return ai.FailureAnalysisResult{}, false
	}
	shared := cloneFailureAnalysisResult(result)
	shared.Analysis.CacheHit = false
	shared.Analysis.SameFailureReuse = true
	entry, err := ai.NewAgenticCacheEntry(analysisruntime.FailureCacheKey(request), shared, createdAt)
	if err != nil || state.StageCacheEntry(entry) != nil {
		return ai.FailureAnalysisResult{}, false
	}
	return shared, true
}
