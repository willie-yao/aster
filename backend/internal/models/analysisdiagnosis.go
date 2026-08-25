package models

import "slices"

// AnalysisHasUsableDiagnosis reports whether an analysis carries a safe
// structured diagnosis whose causal claim the semantic judge did not leave
// contested.
//
// It is deliberately weaker than a grounded disposition. Remediation quality,
// evidence coverage, and investigation budget make an analysis preliminary
// without making its root cause unusable. This is the bar for stages that
// consume the diagnosis as context and carry their own grounding, such as
// causal correlation and chat-derived fixes. It never authorizes a write on its
// own.
func AnalysisHasUsableDiagnosis(analysis *AIAnalysis) bool {
	if analysis == nil {
		return false
	}
	switch analysis.Disposition {
	case AnalysisDispositionGrounded:
		return true
	case AnalysisDispositionPreliminary:
		return !slices.Contains(analysis.DispositionWarnings, AnalysisWarningSemanticReview)
	default:
		return false
	}
}
