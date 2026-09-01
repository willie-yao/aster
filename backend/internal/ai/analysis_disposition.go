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
	citationsVerified := len(analysis.EvidenceCitations) > 0
	if !citationsVerified {
		warnings[models.AnalysisWarningArtifactGrounding] = true
	}
	for _, rule := range slices.Concat(analysis.CritiqueHardFailures, analysis.CritiqueSoftWarnings) {
		descriptor, ok := critiqueRuleDescriptors[CritiqueRuleID(rule)]
		if !ok {
			// safeStructuredAnalysis already rejected unregistered rules, so this
			// degrades rather than discarding an otherwise usable diagnosis.
			citationsVerified = false
			warnings[models.AnalysisWarningInvestigation] = true
			continue
		}
		if descriptor.Effect == critiqueEffectWithhold {
			return "", nil
		}
		if descriptor.Effect == critiqueEffectDegrade {
			citationsVerified = false
		}
		if descriptor.Warning != "" {
			warnings[descriptor.Warning] = true
		}
	}
	if analysis.BudgetExhausted {
		citationsVerified = false
		warnings[models.AnalysisWarningInvestigation] = true
	}
	codes := make([]string, 0, len(warnings))
	for code := range warnings {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if citationsVerified {
		return models.AnalysisDispositionCitationsVerified, codes
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

// AnalysisCitationsVerified reports whether an analysis has passed the current
// deterministic citation contract. An unstamped or unrecognized disposition
// must be refreshed before it regains action eligibility.
func AnalysisCitationsVerified(analysis *models.AIAnalysis) bool {
	return analysis != nil && analysis.Disposition == models.AnalysisDispositionCitationsVerified
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
