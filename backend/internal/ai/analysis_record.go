package ai

import (
	"slices"

	"github.com/willie-yao/aster/backend/internal/models"
)

// analysisRecord is the accepted, sanitized result from which public and cache
// representations are projected.
type analysisRecord struct {
	response analysisResponse

	generatedAt     string
	model           string
	modelHash       string
	promptHash      string
	skillSetHash    string
	cacheGeneration string

	critiquePassed        bool
	critiqueHardFailures  []string
	critiqueSoftWarnings  []string
	critiqueVersion       int
	judgeRan              bool
	judgeObjected         bool
	judgeRevised          bool
	judgeRevisionRejected bool
	disposition           string
	dispositionWarnings   []string

	mode                   string
	toolCalls              int
	contextBytes           int
	gcsBytes               int
	evidencePlanCovered    bool
	gcsFloorRetryExhausted bool
	budgetExhausted        bool
	cacheHit               bool
	sameFailureReuse       bool
	elapsedMs              int

	cachePersistenceAttempted bool
	cachePersistenceAccepted  bool
	cacheRejectionReason      CacheRejectionReason
}

func analysisRecordFromState(parsed analysisResponse, client *Client, state *agentState, mode, generatedAt string, elapsedMs int) analysisRecord {
	if mode == "" {
		mode = AgenticMode
	}
	record := analysisRecord{response: parsed, generatedAt: generatedAt, mode: mode, elapsedMs: elapsedMs}
	if client != nil {
		record.model = client.model
		record.modelHash = client.modelFingerprint()
	}
	if state == nil {
		return record
	}
	record.promptHash = state.promptHash
	if state.skillSet != nil {
		record.skillSetHash = state.skillSet.Hash()
	}
	record.critiquePassed = state.critiquePassed
	record.critiqueHardFailures = slices.Clone(state.critiqueHardFailures)
	record.critiqueSoftWarnings = slices.Clone(state.critiqueSoftWarnings)
	record.critiqueVersion = currentCritiqueVersion
	record.judgeRan = state.judgeRan
	record.judgeObjected = state.judgeObjected
	record.judgeRevised = state.judgeRevised
	record.judgeRevisionRejected = state.judgeRevisionRejected
	record.toolCalls = state.calls
	record.contextBytes = state.modelBytes
	record.gcsBytes = state.gcsBytes
	record.evidencePlanCovered = state.evidencePlanCovered()
	record.gcsFloorRetryExhausted = state.gcsFloorRetryExhausted
	record.budgetExhausted = state.budgetExhausted
	record.cachePersistenceAttempted = state.cachePersistenceAttempted
	record.cachePersistenceAccepted = state.cachePersistenceAccepted
	record.cacheRejectionReason = state.cacheRejectionReason
	return record
}

func analysisRecordFromCache(cached agenticCacheData, generatedAt, cacheGeneration string) analysisRecord {
	return analysisRecord{
		response: cached.analysisResponse, generatedAt: generatedAt,
		model: cached.Model, modelHash: cached.ModelHash, promptHash: cached.PromptHash,
		skillSetHash: cached.SkillSetHash, cacheGeneration: cacheGeneration,
		critiquePassed:       cached.CritiquePassed,
		critiqueHardFailures: slices.Clone(cached.CritiqueHardFailures),
		critiqueSoftWarnings: slices.Clone(cached.CritiqueSoftWarnings),
		critiqueVersion:      cached.CritiqueVersion,
		judgeRan:             cached.JudgeRan, judgeObjected: cached.JudgeObjected,
		judgeRevised: cached.JudgeRevised, judgeRevisionRejected: cached.JudgeRevisionRejected,
		mode: AgenticMode, toolCalls: cached.ToolCalls, contextBytes: cached.ModelBytes,
		gcsBytes: cached.GCSBytes, evidencePlanCovered: cached.EvidencePlanCovered,
		gcsFloorRetryExhausted: cached.GCSFloorRetryExhausted,
		budgetExhausted:        cached.BudgetExhausted, cacheHit: true,
		sameFailureReuse: cached.SameFailureReuse,
	}
}

func analysisRecordFromResult(result FailureAnalysisResult, generatedAt string) analysisRecord {
	if result.Analysis == nil {
		return analysisRecord{generatedAt: generatedAt}
	}
	analysis := result.Analysis
	response := analysisResponse{
		RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
		RelevantFiles: slices.Clone(analysis.RelevantFiles), SearchSuggestions: slices.Clone(analysis.SearchSuggestions),
		CauseLocation: analysis.CauseLocation.Clone(), EvidenceCitations: slices.Clone(analysis.EvidenceCitations),
	}
	if result.Summary != nil {
		response.Summary = result.Summary.Summary
		response.IsTransient = result.Summary.IsTransient
	}
	return analysisRecord{
		response: response, generatedAt: generatedAt,
		model: analysis.Model, modelHash: analysis.ModelHash, promptHash: analysis.PromptHash,
		skillSetHash: analysis.SkillSetHash, cacheGeneration: analysis.CacheGeneration,
		critiquePassed:       analysis.CritiquePassed,
		critiqueHardFailures: slices.Clone(analysis.CritiqueHardFailures),
		critiqueSoftWarnings: slices.Clone(analysis.CritiqueSoftWarnings),
		critiqueVersion:      analysis.CritiqueVersion,
		judgeRan:             analysis.JudgeRan, judgeObjected: analysis.JudgeObjected,
		judgeRevised: analysis.JudgeRevised, judgeRevisionRejected: analysis.JudgeRevisionRejected,
		disposition: analysis.Disposition, dispositionWarnings: slices.Clone(analysis.DispositionWarnings),
		mode: analysis.Mode, toolCalls: analysis.ToolCalls, contextBytes: analysis.ContextBytes,
		gcsBytes: analysis.GCSBytes, evidencePlanCovered: analysis.EvidencePlanCovered,
		gcsFloorRetryExhausted: analysis.GCSFloorRetryExhausted,
		budgetExhausted:        analysis.BudgetExhausted, cacheHit: analysis.CacheHit,
		sameFailureReuse: analysis.SameFailureReuse, elapsedMs: analysis.ElapsedMs,
		cachePersistenceAttempted: analysis.CachePersistenceAttempted,
		cachePersistenceAccepted:  analysis.CachePersistenceAccepted,
		cacheRejectionReason:      CacheRejectionReason(analysis.CachePolicyRejectionReason),
	}
}

func projectFailureAnalysis(record analysisRecord) FailureAnalysisResult {
	summaryText := record.response.Summary
	if summaryText == "" {
		summaryText = firstSentence(record.response.RootCause)
	}
	return FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: record.generatedAt, Summary: summaryText, IsTransient: record.response.IsTransient},
		Analysis: &models.AIAnalysis{
			GeneratedAt: record.generatedAt, Model: record.model,
			RootCause: record.response.RootCause, Severity: record.response.Severity, SuggestedFix: record.response.SuggestedFix,
			RelevantFiles: slices.Clone(record.response.RelevantFiles), SearchSuggestions: slices.Clone(record.response.SearchSuggestions),
			EvidenceCitations: slices.Clone(record.response.EvidenceCitations), CauseLocation: record.response.CauseLocation.Clone(),
			Disposition: record.disposition, DispositionWarnings: slices.Clone(record.dispositionWarnings),
			Mode: record.mode, ToolCalls: record.toolCalls, ContextBytes: record.contextBytes, GCSBytes: record.gcsBytes,
			EvidencePlanCovered: record.evidencePlanCovered, GCSFloorRetryExhausted: record.gcsFloorRetryExhausted,
			ElapsedMs: record.elapsedMs, CacheHit: record.cacheHit, SameFailureReuse: record.sameFailureReuse,
			BudgetExhausted: record.budgetExhausted, CritiquePassed: record.critiquePassed,
			CritiqueHardFailures: slices.Clone(record.critiqueHardFailures), CritiqueSoftWarnings: slices.Clone(record.critiqueSoftWarnings),
			CachePersistenceAttempted: record.cachePersistenceAttempted, CachePersistenceAccepted: record.cachePersistenceAccepted,
			CachePolicyRejectionReason: string(record.cacheRejectionReason),
			JudgeRan:                   record.judgeRan, JudgeObjected: record.judgeObjected, JudgeRevised: record.judgeRevised,
			JudgeRevisionRejected: record.judgeRevisionRejected, CritiqueVersion: record.critiqueVersion,
			SkillSetHash: record.skillSetHash, ModelHash: record.modelHash, PromptHash: record.promptHash,
			CacheGeneration: record.cacheGeneration,
		},
	}
}

func projectAgenticCacheData(record analysisRecord) agenticCacheData {
	response := record.response
	response.RelevantFiles = slices.Clone(response.RelevantFiles)
	response.SearchSuggestions = slices.Clone(response.SearchSuggestions)
	response.CauseLocation = response.CauseLocation.Clone()
	response.EvidenceCitations = slices.Clone(response.EvidenceCitations)
	return agenticCacheData{
		analysisResponse: response, GeneratedAt: record.generatedAt, Model: record.model,
		ToolCalls: record.toolCalls, ModelBytes: record.contextBytes, GCSBytes: record.gcsBytes,
		EvidencePlanCovered: record.evidencePlanCovered, GCSFloorRetryExhausted: record.gcsFloorRetryExhausted,
		BudgetExhausted: record.budgetExhausted, SameFailureReuse: record.sameFailureReuse,
		JudgeRan: record.judgeRan, JudgeObjected: record.judgeObjected,
		JudgeRevised: record.judgeRevised, JudgeRevisionRejected: record.judgeRevisionRejected,
		CritiquePassed: record.critiquePassed, CritiqueHardFailures: slices.Clone(record.critiqueHardFailures),
		CritiqueSoftWarnings: slices.Clone(record.critiqueSoftWarnings), CritiqueVersion: record.critiqueVersion,
		SkillSetHash: record.skillSetHash, ModelHash: record.modelHash, PromptHash: record.promptHash,
	}
}

func stampRecordDisposition(record analysisRecord) (analysisRecord, bool) {
	result := projectFailureAnalysis(record)
	disposition, warnings := AnalysisDisposition(result.Analysis)
	if disposition == "" {
		return record, false
	}
	record.disposition = disposition
	record.dispositionWarnings = slices.Clone(warnings)
	return record, true
}
