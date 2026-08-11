package models

import "testing"

func TestPatternAllowsActionsRejectsCausalGroupResults(t *testing.T) {
	if !PatternAllowsActions(PatternAnalysis{}) {
		t.Fatal("legacy verified contract was blocked")
	}
	for _, recurrence := range []PatternRecurrence{
		PatternRecurrenceSharedCause,
		PatternRecurrenceMixedCauses,
		PatternRecurrenceUnrelated,
		PatternRecurrenceInsufficientEvidence,
	} {
		pattern := PatternAnalysis{
			Recurrence: recurrence,
			RemediationInvestigations: []PatternRemediationInvestigationSummary{{
				CausalGroupID: "group", CausalGroupHash: "hash", State: PatternRemediationActionable,
			}},
		}
		if PatternAllowsActions(pattern) {
			t.Fatalf("recurrence %q allowed actions", recurrence)
		}
	}
}
