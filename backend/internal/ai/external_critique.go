package ai

import (
	"path"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
)

// ExternalDraftEvidence is one bounded artifact excerpt already exposed to an external analyzer.
type ExternalDraftEvidence struct {
	Path    string
	Content string
}

// ExternalDraftCritiqueInput supplies the existing deterministic critique with validated external output.
type ExternalDraftCritiqueInput struct {
	Summary             *models.AISummary
	Analysis            *models.AIAnalysis
	Evidence            []ExternalDraftEvidence
	SourcePaths         []string
	Skills              *skills.Set
	ConsecutiveFailures int
}

// ExternalDraftCritiqueResult is content-free private quality telemetry.
type ExternalDraftCritiqueResult struct {
	Status    string
	Passed    bool
	RuleIDs   []string
	HardRules []string
	SoftRules []string
}

// EvaluateExternalDraftCritique applies the production deterministic critique without retries or publication.
func EvaluateExternalDraftCritique(input ExternalDraftCritiqueInput) ExternalDraftCritiqueResult {
	if input.Summary == nil || input.Analysis == nil {
		return ExternalDraftCritiqueResult{Status: "unavailable"}
	}
	parsed := analysisResponse{
		Summary: input.Summary.Summary, IsTransient: input.Summary.IsTransient,
		RootCause: input.Analysis.RootCause, Severity: input.Analysis.Severity, SuggestedFix: input.Analysis.SuggestedFix,
		RelevantFiles: append([]string(nil), input.Analysis.RelevantFiles...), EvidenceCitations: append([]models.EvidenceCitation(nil), input.Analysis.EvidenceCitations...),
	}
	readsFull := map[string]bool{}
	readsBase := map[string]bool{}
	contentByPath := map[string][]string{}
	citationEvidence := map[string]*analysisChatEvidence{}
	for _, item := range input.Evidence {
		clean, err := artifacts.SafePath(strings.TrimSpace(item.Path))
		if err != nil || clean == "" {
			continue
		}
		norm := NormalizeArtifactCitation(clean)
		readsFull[norm] = true
		readsBase[strings.ToLower(path.Base(norm))] = true
		contentByPath[norm] = append(contentByPath[norm], item.Content)
		lines := map[int]string{}
		for i, line := range strings.Split(strings.ReplaceAll(item.Content, "\r\n", "\n"), "\n") {
			lines[i+1] = line
		}
		citationEvidence[clean] = &analysisChatEvidence{Segments: []string{item.Content}, Lines: lines, Bytes: len(item.Content)}
	}
	sourceReads := map[string]bool{}
	for _, sourcePath := range input.SourcePaths {
		clean, err := artifacts.SafePath(strings.TrimSpace(sourcePath))
		if err == nil && clean != "" {
			sourceReads[strings.ToLower(clean)] = true
		}
	}
	var matched []skills.Skill
	if input.Skills != nil {
		matched = input.Skills.Match(strings.Join(parsed.proseFields(), "\n"))
	}
	out := critiqueDraftWithContent(parsed, readsFull, readsBase, contentByPath, sourceReads, matched, input.ConsecutiveFailures, analysisCitationContext{Evidence: citationEvidence})
	status := "passed"
	if !out.Passed {
		status = "objected"
	}
	return ExternalDraftCritiqueResult{
		Status: status, Passed: out.Passed, RuleIDs: critiqueRuleStrings(out.RuleIDs()),
		HardRules: critiqueRuleStrings(out.HardRuleIDs()), SoftRules: critiqueRuleStrings(out.SoftRuleIDs()),
	}
}
