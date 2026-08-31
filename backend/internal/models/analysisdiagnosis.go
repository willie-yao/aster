package models

import "slices"

// AnalysisHasUsableDiagnosis reports whether an analysis carries a safe
// structured diagnosis. A blocking semantic objection contests the diagnosis;
// an advisory objection remains available to downstream evidence-backed flows.
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
		if slices.Contains(analysis.DispositionWarnings, AnalysisWarningSemanticReview) {
			return analysis.SemanticJudgeMode == "advisory"
		}
		return true
	default:
		return false
	}
}
