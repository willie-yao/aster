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
		if PatternAllowsActions(PatternAnalysis{Recurrence: recurrence}) {
			t.Fatalf("recurrence %q allowed actions", recurrence)
		}
	}
}
