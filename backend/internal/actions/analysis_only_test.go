package actions

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestSubjectEligibilityBlocksAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern", Systemic: true, Recurrence: models.PatternRecurrenceSharedCause, RemediationInvestigations: []models.PatternRemediationInvestigationSummary{{CausalGroupID: "group", CausalGroupHash: "hash", State: models.PatternRemediationNotInvestigated}},
		SuggestedFix: "fix", RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}},
	}
	code, reason := subjectEligibilityReason(&ActionSubject{Kind: actionSubjectPattern, Pattern: pattern})
	if code != ReasonContractGenerationFailed || reason != "This causal-group result is analysis-only and cannot start an action." {
		t.Fatalf("code=%s reason=%q", code, reason)
	}
}
