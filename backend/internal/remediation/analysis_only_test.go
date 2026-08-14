package remediation

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestUntrackedPatternsSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := models.PatternAnalysis{Systemic: true, Recurrence: models.PatternRecurrenceSharedCause, RemediationInvestigations: []models.PatternRemediationInvestigationSummary{{CausalGroupID: "group", CausalGroupHash: "hash", State: models.PatternRemediationNotInvestigated}}}
	if got := UntrackedPatterns(nil, []models.PatternAnalysis{pattern}, nil); len(got) != 0 {
		t.Fatalf("patterns=%+v", got)
	}
}
