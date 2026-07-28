package orka

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	containerRunIDLabel    = "prow-ai-dashboard/run-id"
	containerPassIDLabel   = "prow-ai-dashboard/pass-id"
	containerPassTypeLabel = "prow-ai-dashboard/pass-type"
	containerWorkItemLabel = "prow-ai-dashboard/work-item"
)

var safeCorrelationID = regexp.MustCompile(`^[0-9a-f]{16,48}$`)

func containerAnalysisCorrelation(progress *fetchprogress.Tracker, request ai.FailureAnalysisRequest) (string, map[string]string) {
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	if progress == nil {
		return workItem, nil
	}
	correlation, ok := progress.Correlation()
	if !ok || !safeCorrelationID.MatchString(correlation.RunID) || !safeCorrelationID.MatchString(correlation.PassID) {
		return workItem, nil
	}
	labels := map[string]string{
		containerRunIDLabel:    correlation.RunID,
		containerPassIDLabel:   correlation.PassID,
		containerPassTypeLabel: string(correlation.PassType),
		containerWorkItemLabel: workItem,
	}
	for key, value := range labels {
		if len(k8svalidation.IsQualifiedName(key)) > 0 || len(k8svalidation.IsValidLabelValue(value)) > 0 {
			return workItem, nil
		}
	}
	return workItem, labels
}

func containerAnalysisCacheHit(traces []ai.AnalysisTrace) bool {
	for _, trace := range traces {
		if strings.EqualFold(strings.TrimSpace(trace.Outcome), "ai_cache_hit") {
			return true
		}
	}
	return false
}

func terminalProgressPhase(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "Cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "TimedOut"
	default:
		return "Unknown"
	}
}
