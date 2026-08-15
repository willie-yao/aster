package ai

import (
	"slices"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestEvaluateExternalDraftCritique(t *testing.T) {
	input := ExternalDraftCritiqueInput{
		Summary: &models.AISummary{Summary: "failure", IsTransient: false},
		Analysis: &models.AIAnalysis{
			RootCause: "build-log.txt line 1 shows the initiating error", Severity: "High",
			SuggestedFix: "Update the configuration and retry.", RelevantFiles: []string{"build-log.txt"},
			EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "initiating error"}},
		},
		Evidence: []ExternalDraftEvidence{{Path: "build-log.txt", Content: "initiating error\n"}},
	}
	got := EvaluateExternalDraftCritique(input)
	if got.Status != "passed" || !got.Passed || len(got.HardRules) != 0 {
		t.Fatalf("critique = %+v", got)
	}
	input.Analysis.SuggestedFix = "Investigate the logs."
	got = EvaluateExternalDraftCritique(input)
	if got.Status != "objected" || got.Passed || !slices.Contains(got.RuleIDs, "remediation.punt") {
		t.Fatalf("critique = %+v", got)
	}
}

func TestEvaluateExternalDraftCritiqueUnavailable(t *testing.T) {
	if got := EvaluateExternalDraftCritique(ExternalDraftCritiqueInput{}); got.Status != "unavailable" {
		t.Fatalf("critique = %+v", got)
	}
}
