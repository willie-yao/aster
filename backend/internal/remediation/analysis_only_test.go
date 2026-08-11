package remediation

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestUntrackedPatternsSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := models.PatternAnalysis{Systemic: true, Recurrence: models.PatternRecurrenceSharedCause}
	if got := UntrackedPatterns(nil, []models.PatternAnalysis{pattern}, nil); len(got) != 0 {
		t.Fatalf("patterns=%+v", got)
	}
}
