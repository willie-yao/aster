package ai

import (
	"slices"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestAnalysisDisposition(t *testing.T) {
	base := func() *models.AIAnalysis {
		return &models.AIAnalysis{
			RootCause: "the controller rejected the request", Severity: "High", SuggestedFix: "correct the request",
			EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 7, LineEnd: 7, Quote: "request rejected"}},
			Mode:              AgenticMode, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		}
	}
	for _, test := range []struct {
		name        string
		mutate      func(*models.AIAnalysis)
		disposition string
		warnings    []string
	}{
		{name: "grounded", disposition: models.AnalysisDispositionGrounded},
		{name: "missing citation", mutate: func(value *models.AIAnalysis) {
			value.EvidenceCitations = nil
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleCitationMissing)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningArtifactGrounding}},
		{name: "unavailable evidence is advisory", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueSoftWarnings = []string{string(CritiqueRuleEvidenceUnavailable)}
		}, disposition: models.AnalysisDispositionGrounded, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "available evidence unread", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueSoftWarnings = []string{string(CritiqueRuleEvidenceAvailableUnread)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "budget exhausted", mutate: func(value *models.AIAnalysis) { value.BudgetExhausted = true }, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "semantic objection is advisory", mutate: func(value *models.AIAnalysis) {
			value.JudgeRan = true
			value.JudgeObjected = true
			value.SemanticJudgeMode = string(SemanticJudgeAdvisory)
			value.SemanticFindings = []string{semanticFindingCausalLinkUnsupported}
		}, disposition: models.AnalysisDispositionGrounded, warnings: []string{models.AnalysisWarningSemanticReview}},
		{name: "blocking semantic objection", mutate: func(value *models.AIAnalysis) {
			value.JudgeRan = true
			value.JudgeObjected = true
			value.SemanticJudgeMode = string(SemanticJudgeBlocking)
			value.SemanticFindings = []string{semanticFindingCausalLinkUnsupported}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningSemanticReview}},
		{name: "source warning", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleSourceUnverified)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningSourceGrounding}},
		{name: "rerun only remediation", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleRerunOnlyRemediation)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningRemediation}},
		{name: "unknown semantic judge mode", mutate: func(value *models.AIAnalysis) {
			value.SemanticJudgeMode = "unknown"
		}},
		{name: "unknown semantic finding", mutate: func(value *models.AIAnalysis) {
			value.SemanticFindings = []string{"unknown"}
		}},
		{name: "unsafe", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRulePathUnsafe)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis := base()
			if test.mutate != nil {
				test.mutate(analysis)
			}
			disposition, warnings := AnalysisDisposition(analysis)
			if disposition != test.disposition || !slices.Equal(warnings, test.warnings) {
				t.Fatalf("disposition=%q warnings=%v", disposition, warnings)
			}
		})
	}
}

func TestMeetsCurrentCritiqueContractRequiresGroundedDisposition(t *testing.T) {
	analysis := &models.AIAnalysis{
		RootCause: "cause", Severity: "High", SuggestedFix: "fix", Mode: AgenticMode,
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "failure"}},
		Disposition:       models.AnalysisDispositionPreliminary,
	}
	if MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("preliminary analysis passed the action-facing critique contract")
	}
	analysis.Disposition = models.AnalysisDispositionGrounded
	if !MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("grounded current analysis did not pass the critique contract")
	}
}
