package actions

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

func TestSubjectEligibilityBlocksAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern", Systemic: true, Recurrence: models.PatternRecurrenceSharedCause,
		SuggestedFix: "fix", RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}},
	}
	code, reason := subjectEligibilityReason(&ActionSubject{Kind: actionSubjectPattern, Pattern: pattern})
	if code != ReasonContractGenerationFailed || reason != "This causal-group result is analysis-only and cannot start an action." {
		t.Fatalf("code=%s reason=%q", code, reason)
	}
}

func TestManualFixResolverBlocksCausalGroupParent(t *testing.T) {
	dataDir := t.TempDir()
	pattern := models.PatternAnalysis{
		JobID: "periodic-parent", Systemic: true, Recurrence: models.PatternRecurrenceMixedCauses,
		CausalGroups: []models.PatternCausalGroup{{Signature: "cause-a"}},
	}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dataDir, "periodic-parent.json", models.JobDetail{
		JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern},
	})
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	if _, err := service.resolveSubjectForManualFix(pattern.ID); ReasonCodeOf(err) != ReasonContractGenerationFailed {
		t.Fatalf("manual parent resolution error = %v code=%s", err, ReasonCodeOf(err))
	}
}
