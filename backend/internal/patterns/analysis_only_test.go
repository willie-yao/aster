package patterns

import (
	"context"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type analysisOnlyVerifier struct {
	calls int
}

func (*analysisOnlyVerifier) AnalyzePattern(context.Context, string, string, []ai.PatternFailure) (*models.PatternAnalysis, error) {
	return nil, nil
}

func (v *analysisOnlyVerifier) VerifyPatternRemediation(context.Context, models.PatternAnalysis, models.JobDetail) (models.PatternRemediationVerification, error) {
	v.calls++
	return models.PatternRemediationVerification{}, nil
}

func TestApplyRemediationVerificationSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	verifier := &analysisOnlyVerifier{}
	pattern := &models.PatternAnalysis{Systemic: true, Recurrence: models.PatternRecurrenceSharedCause}
	applyRemediationVerification(t.Context(), verifier, pattern, models.JobDetail{})
	if verifier.calls != 0 || pattern.RemediationVerification != nil {
		t.Fatalf("calls=%d pattern=%+v", verifier.calls, pattern)
	}
}
