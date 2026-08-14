package issues

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

func TestBuildSpecsSkipsAnalysisOnlyCausalGroups(t *testing.T) {
	pattern := models.PatternAnalysis{
		JobID: "job", Systemic: true, Recurrence: models.PatternRecurrenceSharedCause,
		SharedRootCause: "cause", Summary: "summary",
	}
	specs := BuildSpecs(BuildInput{
		Report:   models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}},
		Triggers: []string{project.IssueTriggerPatterns},
	})
	if len(specs) != 0 {
		t.Fatalf("specs=%+v", specs)
	}
}
