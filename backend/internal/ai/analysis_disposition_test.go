package ai

import (
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestTryParseAnalysisCanonicalizesSeverityForPublication(t *testing.T) {
	for _, severity := range []string{"Critical", "High", "Medium", "Low", "Transient-Ignore"} {
		for _, variant := range []string{severity, strings.ToLower(severity), strings.ToUpper(severity), " \t" + strings.ToLower(severity) + "\n "} {
			t.Run(severity+"/"+variant, func(t *testing.T) {
				raw := mustJSON(analysisResponse{
					RootCause: "the controller rejected the request", Severity: variant,
					EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 7, LineEnd: 7, Quote: "request rejected"}},
				})
				parsed, ok := tryParseAnalysis(string(raw))
				if !ok || parsed.Severity != severity {
					t.Fatalf("parsed severity=%q ok=%v, want %q", parsed.Severity, ok, severity)
				}
				result := projectFailureAnalysis(analysisRecord{response: parsed})
				if disposition, _ := AnalysisDisposition(result.Analysis); disposition != models.AnalysisDispositionCitationsVerified {
					t.Fatalf("disposition=%q, want %q", disposition, models.AnalysisDispositionCitationsVerified)
				}
			})
		}
	}
}

func TestTryParseAnalysisDoesNotDefaultSeverityForPublication(t *testing.T) {
	for _, test := range []struct {
		name     string
		raw      string
		severity string
	}{
		{name: "unknown", raw: `{"root_cause":"request rejected","severity":" urgent "}`, severity: " urgent "},
		{name: "partial match", raw: `{"root_cause":"request rejected","severity":"Highly"}`, severity: "Highly"},
		{name: "empty", raw: `{"root_cause":"request rejected","severity":""}`},
		{name: "whitespace", raw: `{"root_cause":"request rejected","severity":" \t "}`, severity: " \t "},
		{name: "missing", raw: `{"root_cause":"request rejected"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, ok := tryParseAnalysis(test.raw)
			if !ok || parsed.Severity != test.severity {
				t.Fatalf("parsed severity=%q ok=%v, want %q", parsed.Severity, ok, test.severity)
			}
			result := projectFailureAnalysis(analysisRecord{response: parsed})
			if disposition, _ := AnalysisDisposition(result.Analysis); disposition != "" {
				t.Fatalf("invalid severity published with disposition=%q", disposition)
			}
		})
	}
}

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
		{name: "citations verified", disposition: models.AnalysisDispositionCitationsVerified},
		{name: "missing citation", mutate: func(value *models.AIAnalysis) {
			value.EvidenceCitations = nil
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleCitationMissing)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningArtifactGrounding}},
		{name: "unavailable evidence is advisory", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueSoftWarnings = []string{string(CritiqueRuleEvidenceUnavailable)}
		}, disposition: models.AnalysisDispositionCitationsVerified, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "available evidence unread", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueSoftWarnings = []string{string(CritiqueRuleEvidenceAvailableUnread)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "budget exhausted", mutate: func(value *models.AIAnalysis) { value.BudgetExhausted = true }, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningInvestigation}},
		{name: "source warning", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleSourceUnverified)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningSourceGrounding}},
		{name: "rerun only remediation", mutate: func(value *models.AIAnalysis) {
			value.CritiquePassed = false
			value.CritiqueHardFailures = []string{string(CritiqueRuleRerunOnlyRemediation)}
		}, disposition: models.AnalysisDispositionPreliminary, warnings: []string{models.AnalysisWarningRemediation}},
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

func TestMeetsCurrentCritiqueContractRequiresCitationsVerifiedDisposition(t *testing.T) {
	analysis := &models.AIAnalysis{
		RootCause: "cause", Severity: "High", SuggestedFix: "fix", Mode: AgenticMode,
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "failure"}},
		Disposition:       models.AnalysisDispositionPreliminary,
	}
	if MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("preliminary analysis passed the action-facing critique contract")
	}
	analysis.Disposition = models.AnalysisDispositionCitationsVerified
	if !MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("citation-verified current analysis did not pass the critique contract")
	}
}
