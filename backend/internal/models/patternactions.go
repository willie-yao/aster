package models

// PatternAllowsActions reports whether a pattern uses the legacy verified
// remediation contract. Causal-group results are analysis-only.
func PatternAllowsActions(pattern PatternAnalysis) bool {
	return pattern.Recurrence == "" && len(pattern.CausalGroups) == 0 && len(pattern.UnclassifiedBuilds) == 0
}
