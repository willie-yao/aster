package fixpr

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestEligibleSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := models.PatternAnalysis{
		Systemic: true, Recurrence: models.PatternRecurrenceSharedCause,
		Confidence: "high", SuggestedFix: "fix",
	}
	if got := eligible([]models.PatternAnalysis{pattern}, "low"); len(got) != 0 {
		t.Fatalf("patterns=%+v", got)
	}
}
