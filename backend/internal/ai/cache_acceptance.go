package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// CacheRejectionReason is a privacy-safe cache acceptance outcome.
type CacheRejectionReason string

const (
	CacheAccepted              CacheRejectionReason = ""
	CacheRejectedMissing       CacheRejectionReason = "missing"
	CacheRejectedExpired       CacheRejectionReason = "expired"
	CacheRejectedToolFloor     CacheRejectionReason = "tool_floor"
	CacheRejectedEvidenceFloor CacheRejectionReason = "evidence_floor"
	CacheRejectedCritique      CacheRejectionReason = "critique"
	CacheRejectedMalformed     CacheRejectionReason = "malformed"
)

// AgenticCachePolicy contains the current cache acceptance contract.
type AgenticCachePolicy struct {
	MinToolCalls        int
	MinGCSBytes         int
	ConsecutiveFailures int
	SkillSetHash        string
	Model               string
	ModelHash           string
	PromptHash          string
	CacheGeneration     string
	// CritiqueRequired makes critique success part of acceptance. The version is
	// always required so cached output satisfies the current publication contract.
	CritiqueRequired   bool
	Now                time.Time
	entryTimeValidated bool
}

// LookupAgenticCache evaluates one private entry without mutating the cache.
func LookupAgenticCache(cache *Cache, key string, policy AgenticCachePolicy) (FailureAnalysisResult, CacheRejectionReason) {
	if cache == nil {
		return FailureAnalysisResult{}, CacheRejectedMissing
	}
	entry, ok := cache.Lookup(key)
	if !ok {
		return FailureAnalysisResult{}, CacheRejectedMissing
	}
	return AcceptAgenticCacheEntry(entry, key, policy)
}

// AcceptAgenticCacheEntry validates and reconstructs one private cache entry.
func AcceptAgenticCacheEntry(entry CacheEntry, expectedKey string, policy AgenticCachePolicy) (FailureAnalysisResult, CacheRejectionReason) {
	now := policy.Now
	if now.IsZero() {
		now = time.Now()
	}
	if entry.Key != expectedKey || expectedKey == "" || !json.Valid(entry.Data) {
		return FailureAnalysisResult{}, CacheRejectedMalformed
	}
	if !validCacheEntryTime(now, entry.CreatedAt) {
		return FailureAnalysisResult{}, CacheRejectedExpired
	}

	var cached agenticCacheData
	if err := json.Unmarshal(entry.Data, &cached); err != nil || (cached.RootCause == "" && cached.Summary == "") {
		return FailureAnalysisResult{}, CacheRejectedMalformed
	}
	generatedAt := entry.CreatedAt
	if cached.GeneratedAt != "" {
		parsedGeneratedAt, err := time.Parse(time.RFC3339, cached.GeneratedAt)
		if err != nil {
			return FailureAnalysisResult{}, CacheRejectedMalformed
		}
		if !validCacheEntryTime(now, parsedGeneratedAt) {
			return FailureAnalysisResult{}, CacheRejectedExpired
		}
		generatedAt = parsedGeneratedAt
	}
	summary, analysis := buildOutputs(cached.analysisResponse, cached.Model, cached.ModelHash, generatedAt)
	analysis.Mode = AgenticMode
	analysis.CacheHit = true
	analysis.ToolCalls = cached.ToolCalls
	analysis.ContextBytes = cached.ModelBytes
	analysis.GCSBytes = cached.GCSBytes
	analysis.EvidencePlanCovered = cached.EvidencePlanCovered
	analysis.BudgetExhausted = cached.BudgetExhausted
	analysis.SameFailureReuse = cached.SameFailureReuse
	analysis.CritiquePassed = cached.CritiquePassed
	analysis.CritiqueVersion = cached.CritiqueVersion
	analysis.SkillSetHash = cached.SkillSetHash
	analysis.ModelHash = cached.ModelHash
	analysis.PromptHash = cached.PromptHash
	analysis.CacheGeneration = policy.CacheGeneration
	result := FailureAnalysisResult{Summary: summary, Analysis: analysis}
	policy.Now = now
	policy.entryTimeValidated = true
	if reason := AgenticResultRejection(result, policy); reason != CacheAccepted {
		return FailureAnalysisResult{}, reason
	}
	return result, CacheAccepted
}

// AgenticResultRejection evaluates the reusable quality contract for an analysis.
func AgenticResultRejection(result FailureAnalysisResult, policy AgenticCachePolicy) CacheRejectionReason {
	if result.Summary == nil || result.Analysis == nil || result.Analysis.Mode != AgenticMode {
		return CacheRejectedMalformed
	}
	analysis := result.Analysis
	if strings.TrimSpace(result.Summary.Summary) == "" && strings.TrimSpace(analysis.RootCause) == "" {
		return CacheRejectedMalformed
	}
	if !policy.entryTimeValidated {
		now := policy.Now
		if now.IsZero() {
			now = time.Now()
		}
		generatedAt, err := time.Parse(time.RFC3339, analysis.GeneratedAt)
		if err != nil || !validCacheEntryTime(now, generatedAt) {
			return CacheRejectedExpired
		}
	}
	if analysis.ToolCalls < policy.MinToolCalls {
		return CacheRejectedToolFloor
	}
	if gcsFloorUnmet(analysis.GCSBytes, policy.MinGCSBytes, analysis.EvidencePlanCovered) {
		return CacheRejectedEvidenceFloor
	}
	if analysis.CritiqueVersion < currentCritiqueVersion || policy.CritiqueRequired && !analysis.CritiquePassed {
		return CacheRejectedCritique
	}
	if analysis.CacheGeneration != policy.CacheGeneration {
		return CacheRejectedMissing
	}
	return CacheAccepted
}

// NewAgenticCacheEntry reconstructs the existing private cache shape from a validated result.
func NewAgenticCacheEntry(key string, result FailureAnalysisResult, createdAt time.Time) (CacheEntry, error) {
	if strings.TrimSpace(key) == "" {
		return CacheEntry{}, fmt.Errorf("agentic cache key is required")
	}
	if createdAt.IsZero() {
		return CacheEntry{}, fmt.Errorf("agentic cache creation time is required")
	}
	if result.Summary == nil || strings.TrimSpace(result.Summary.Summary) == "" || result.Analysis == nil || result.Analysis.Mode != AgenticMode {
		return CacheEntry{}, fmt.Errorf("agentic cache result is incomplete")
	}
	generatedAt := result.Analysis.GeneratedAt
	if generatedAt == "" {
		generatedAt = createdAt.UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, generatedAt); err != nil {
		return CacheEntry{}, fmt.Errorf("agentic analysis generation time is invalid: %w", err)
	}
	data := agenticCacheData{
		analysisResponse: analysisResponse{
			Summary:           result.Summary.Summary,
			IsTransient:       result.Summary.IsTransient,
			RootCause:         result.Analysis.RootCause,
			Severity:          result.Analysis.Severity,
			SuggestedFix:      result.Analysis.SuggestedFix,
			RelevantFiles:     append([]string(nil), result.Analysis.RelevantFiles...),
			SearchSuggestions: append([]string(nil), result.Analysis.SearchSuggestions...),
			EvidenceCitations: append([]models.EvidenceCitation(nil), result.Analysis.EvidenceCitations...),
		},
		GeneratedAt:         generatedAt,
		Model:               result.Analysis.Model,
		ToolCalls:           result.Analysis.ToolCalls,
		ModelBytes:          result.Analysis.ContextBytes,
		GCSBytes:            result.Analysis.GCSBytes,
		EvidencePlanCovered: result.Analysis.EvidencePlanCovered,
		BudgetExhausted:     result.Analysis.BudgetExhausted,
		SameFailureReuse:    result.Analysis.SameFailureReuse,
		CritiquePassed:      result.Analysis.CritiquePassed,
		CritiqueVersion:     result.Analysis.CritiqueVersion,
		SkillSetHash:        result.Analysis.SkillSetHash,
		ModelHash:           result.Analysis.ModelHash,
		PromptHash:          result.Analysis.PromptHash,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return CacheEntry{}, fmt.Errorf("encode agentic cache entry: %w", err)
	}
	return CacheEntry{Key: key, CreatedAt: createdAt, Data: raw}, nil
}

func agenticCachePolicy(client *Client, opts AgenticOptions, skillSetHash, promptHash string, consecutiveFailures int) AgenticCachePolicy {
	policy := AgenticCachePolicy{
		MinToolCalls:        opts.MinToolCalls,
		MinGCSBytes:         opts.MinGCSBytes,
		ConsecutiveFailures: consecutiveFailures,
		CritiqueRequired:    opts.CritiqueMaxRetries > 0,
		SkillSetHash:        skillSetHash,
		PromptHash:          promptHash,
	}
	if client != nil {
		policy.Model = client.model
		policy.ModelHash = client.modelFingerprint()
	}
	return policy
}

// MeetsCurrentCritiqueContract reports whether an analysis passed the current deterministic critique contract.
func MeetsCurrentCritiqueContract(analysis *models.AIAnalysis) bool {
	return analysis != nil && analysis.Mode == AgenticMode && analysis.CritiquePassed && analysis.CritiqueVersion >= currentCritiqueVersion
}

// CurrentCritiqueVersion returns the active deterministic critique contract version.
func CurrentCritiqueVersion() int { return currentCritiqueVersion }
