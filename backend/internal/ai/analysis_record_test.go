package ai

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestAnalysisRecordProjectsPublicAndCacheShapes(t *testing.T) {
	record := analysisRecord{
		response: analysisResponse{
			Summary: "summary", IsTransient: true, RootCause: "root cause", Severity: "High", SuggestedFix: "fix",
			RelevantFiles: []string{"pkg/file.go"}, SearchSuggestions: []string{"symbol"},
			CauseLocation:     &models.AnalysisCauseLocation{Repository: "example/repo", Files: []string{"pkg/file.go"}},
			EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 10, LineEnd: 11, Quote: "failure"}},
		},
		generatedAt: "2026-08-25T12:00:00Z", model: "model", modelHash: "model-hash",
		promptHash: "prompt-hash", skillSetHash: "skill-hash", cacheGeneration: "generation",
		critiquePassed: true, critiqueSoftWarnings: []string{string(CritiqueRuleEvidenceUnavailable)},
		critiqueVersion:     currentCritiqueVersion,
		disposition:         models.AnalysisDispositionPreliminary,
		dispositionWarnings: []string{models.AnalysisWarningInvestigation},
		mode:                AgenticMode, toolCalls: 4, contextBytes: 500, gcsBytes: 300,
		evidencePlanCovered: true, gcsFloorRetryExhausted: true, budgetExhausted: true,
		cacheHit: true, sameFailureReuse: true, elapsedMs: 42,
		cachePersistenceAttempted: true, cachePersistenceAccepted: true,
	}

	result := projectFailureAnalysis(record)
	analysis := result.Analysis
	if result.Summary == nil || result.Summary.Summary != "summary" || !result.Summary.IsTransient ||
		analysis == nil || analysis.RootCause != "root cause" || analysis.Severity != "High" || analysis.SuggestedFix != "fix" ||
		!slices.Equal(analysis.RelevantFiles, []string{"pkg/file.go"}) || analysis.CauseLocation == nil ||
		len(analysis.EvidenceCitations) != 1 || analysis.Disposition != models.AnalysisDispositionPreliminary ||
		!slices.Equal(analysis.DispositionWarnings, []string{models.AnalysisWarningInvestigation}) ||
		analysis.ModelHash != "model-hash" || analysis.PromptHash != "prompt-hash" || analysis.SkillSetHash != "skill-hash" ||
		analysis.CacheGeneration != "generation" || analysis.ToolCalls != 4 || analysis.ContextBytes != 500 || analysis.GCSBytes != 300 ||
		!analysis.EvidencePlanCovered || !analysis.GCSFloorRetryExhausted || !analysis.BudgetExhausted || !analysis.CacheHit ||
		!analysis.SameFailureReuse || analysis.ElapsedMs != 42 || !analysis.CritiquePassed ||
		!analysis.CachePersistenceAttempted || !analysis.CachePersistenceAccepted {
		t.Fatalf("public projection = %+v %+v", result.Summary, analysis)
	}

	cached := projectAgenticCacheData(record)
	if cached.Summary != "summary" || cached.RootCause != "root cause" || cached.GeneratedAt != record.generatedAt ||
		cached.Model != "model" || cached.ToolCalls != 4 || cached.ModelBytes != 500 || cached.GCSBytes != 300 ||
		!cached.EvidencePlanCovered || !cached.GCSFloorRetryExhausted || !cached.BudgetExhausted || !cached.SameFailureReuse ||
		!cached.CritiquePassed || cached.CritiqueVersion != currentCritiqueVersion || cached.ModelHash != "model-hash" ||
		cached.PromptHash != "prompt-hash" || cached.SkillSetHash != "skill-hash" {
		t.Fatalf("cache projection = %+v", cached)
	}
	encoded, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"disposition", "cache_hit", "cache_generation", "elapsed_ms", "cache_persistence"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("private cache unexpectedly contains %q: %s", field, encoded)
		}
	}
}
