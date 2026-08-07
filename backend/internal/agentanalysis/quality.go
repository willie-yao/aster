package agentanalysis

import (
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// EvaluateQuality applies private deterministic critique telemetry to a validated shadow result.
func EvaluateQuality(bundle EvidenceBundle, analysis Analysis, skillSet *skills.Set, consecutiveFailures int) ShadowQuality {
	excerpts := make(map[string]EvidenceExcerpt, len(bundle.Excerpts))
	evidence := make([]ai.ExternalDraftEvidence, 0, len(bundle.Excerpts))
	for _, excerpt := range bundle.Excerpts {
		excerpts[excerpt.ID] = excerpt
		evidence = append(evidence, ai.ExternalDraftEvidence{Path: excerpt.Path, Content: excerpt.Content})
	}
	artifactCitations := make([]models.EvidenceCitation, 0, len(analysis.EvidenceCitations))
	for _, citation := range analysis.EvidenceCitations {
		excerpt, ok := excerpts[citation.ExcerptID]
		if !ok {
			continue
		}
		artifactCitations = append(artifactCitations, models.EvidenceCitation{
			Path: excerpt.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Quote: citation.Quote,
		})
	}
	var sourcePaths []string
	for _, citation := range analysis.SourceCitations {
		if citation.Verified {
			sourcePaths = append(sourcePaths, citation.Path)
		}
	}
	critique := ai.EvaluateExternalDraftCritique(ai.ExternalDraftCritiqueInput{
		Summary: &models.AISummary{Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		Analysis: &models.AIAnalysis{
			RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
			RelevantFiles: append([]string(nil), analysis.RelevantFiles...), EvidenceCitations: artifactCitations,
		},
		Evidence: evidence, SourcePaths: sourcePaths, Skills: skillSet, ConsecutiveFailures: consecutiveFailures,
	})
	return ShadowQuality{
		DeterministicStatus: critique.Status, DeterministicPassed: critique.Passed,
		RuleIDs: critique.RuleIDs, HardRules: critique.HardRules, SoftRules: critique.SoftRules,
		SemanticStatus: "unavailable", SemanticReason: "evidence_aware_semantic_judge_not_exposed",
	}
}
