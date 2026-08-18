package ai

import (
	"slices"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

// AnalysisDisposition returns the safe publication disposition for one analysis.
// An empty result means the analysis must not be published as usable output.
func AnalysisDisposition(analysis *models.AIAnalysis) (string, []string) {
	if !safeStructuredAnalysis(analysis) {
		return "", nil
	}
	warnings := map[string]bool{}
	grounded := len(analysis.EvidenceCitations) > 0
	if !grounded {
		warnings[models.AnalysisWarningArtifactGrounding] = true
	}
	for _, rule := range analysis.CritiqueHardFailures {
		switch CritiqueRuleID(rule) {
		case CritiqueRulePathUnsafe, CritiqueRuleStructuredInvalid:
			return "", nil
		case CritiqueRuleCitationInvalidRange, CritiqueRuleCitationQuoteMismatch,
			CritiqueRuleCitationUnread, CritiqueRuleCitationMissing, CritiqueRuleClaimUncitedLine:
			grounded = false
			warnings[models.AnalysisWarningArtifactGrounding] = true
		case CritiqueRuleSourceUnverified:
			grounded = false
			warnings[models.AnalysisWarningSourceGrounding] = true
		case CritiqueRuleTransientConflict:
			warnings[models.AnalysisWarningClassification] = true
		default:
			return "", nil
		}
	}
	for _, rule := range analysis.CritiqueSoftWarnings {
		switch CritiqueRuleID(rule) {
		case CritiqueRuleEvidenceAvailableUnread, CritiqueRuleEvidenceUnavailable:
			warnings[models.AnalysisWarningInvestigation] = true
		case CritiqueRuleRemediationPunt:
			warnings[models.AnalysisWarningRemediation] = true
		default:
			return "", nil
		}
	}
	if analysis.BudgetExhausted {
		warnings[models.AnalysisWarningInvestigation] = true
	}
	if analysis.JudgeObjected && !analysis.JudgeRevised {
		warnings[models.AnalysisWarningSemanticReview] = true
	}
	codes := make([]string, 0, len(warnings))
	for code := range warnings {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if grounded && len(codes) == 0 {
		return models.AnalysisDispositionGrounded, codes
	}
	return models.AnalysisDispositionPreliminary, codes
}

// StampAnalysisDisposition records the current deterministic publication state.
// It returns false when the analysis must remain unavailable.
func StampAnalysisDisposition(analysis *models.AIAnalysis) bool {
	disposition, warnings := AnalysisDisposition(analysis)
	if disposition == "" {
		return false
	}
	analysis.Disposition = disposition
	analysis.DispositionWarnings = warnings
	return true
}

// IsGroundedAnalysis reports whether an analysis is grounded under the current contract.
func IsGroundedAnalysis(analysis *models.AIAnalysis) bool {
	if analysis == nil {
		return false
	}
	switch analysis.Disposition {
	case models.AnalysisDispositionGrounded:
		return true
	case models.AnalysisDispositionPreliminary:
		return false
	}
	// Analyses written before dispositions were introduced retain their prior
	// publication behavior until they are refreshed and stamped.
	return true
}

func safeStructuredAnalysis(analysis *models.AIAnalysis) bool {
	if analysis == nil || strings.TrimSpace(analysis.RootCause) == "" || strings.TrimSpace(analysis.Severity) == "" {
		return false
	}
	if !slices.Contains([]string{"Critical", "High", "Medium", "Low", "Transient-Ignore"}, analysis.Severity) {
		return false
	}
	for _, citation := range analysis.EvidenceCitations {
		if strings.TrimSpace(citation.Path) == "" || citation.LineStart < 1 || citation.LineEnd < citation.LineStart || strings.TrimSpace(citation.Quote) == "" {
			return false
		}
	}
	return validCritiqueRuleClassification(analysis.CritiqueHardFailures, analysis.CritiqueSoftWarnings)
}
