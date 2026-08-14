package fixpr

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestEligibleSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := models.PatternAnalysis{
		Systemic: true, Recurrence: models.PatternRecurrenceSharedCause, RemediationInvestigations: []models.PatternRemediationInvestigationSummary{{CausalGroupID: "group", CausalGroupHash: "hash", State: models.PatternRemediationNotInvestigated}},
		Confidence: "high", SuggestedFix: "fix",
	}
	if got := eligible([]models.PatternAnalysis{pattern}, "low"); len(got) != 0 {
		t.Fatalf("patterns=%+v", got)
	}
}
