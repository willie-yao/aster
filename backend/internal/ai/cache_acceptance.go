package ai

import (
	"encoding/json"
	"time"
)

// CacheRejectionReason is a privacy-safe cache acceptance outcome.
type CacheRejectionReason string

const (
	CacheAccepted                     CacheRejectionReason = ""
	CacheRejectedMissing              CacheRejectionReason = "missing"
	CacheRejectedExpired              CacheRejectionReason = "expired"
	CacheRejectedToolFloor            CacheRejectionReason = "tool_floor"
	CacheRejectedEvidenceFloor        CacheRejectionReason = "evidence_floor"
	CacheRejectedCritique             CacheRejectionReason = "critique"
	CacheRejectedSkill                CacheRejectionReason = "skill"
	CacheRejectedModel                CacheRejectionReason = "model"
	CacheRejectedPrompt               CacheRejectionReason = "prompt"
	CacheRejectedTransientPersistence CacheRejectionReason = "transient_persistence"
	CacheRejectedMalformed            CacheRejectionReason = "malformed"
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
	Now                 time.Time
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
	summary, analysis := buildOutputs(cached.analysisResponse, policy.Model, policy.ModelHash, now)
	analysis.Mode = AgenticMode
	analysis.CacheHit = true
	analysis.ToolCalls = cached.ToolCalls
	analysis.ContextBytes = cached.ModelBytes
	analysis.GCSBytes = cached.GCSBytes
	analysis.EvidencePlanCovered = cached.EvidencePlanCovered
	analysis.BudgetExhausted = cached.BudgetExhausted
	analysis.CritiquePassed = cached.CritiquePassed
	analysis.CritiqueVersion = cached.CritiqueVersion
	analysis.SkillSetHash = cached.SkillSetHash
	analysis.ModelHash = cached.ModelHash
	analysis.PromptHash = cached.PromptHash
	result := FailureAnalysisResult{Summary: summary, Analysis: analysis}
	if reason := AgenticResultRejection(result, policy); reason != CacheAccepted {
		return FailureAnalysisResult{}, reason
	}
	return result, CacheAccepted
}

// AgenticResultRejection evaluates current quality floors for an analysis.
func AgenticResultRejection(result FailureAnalysisResult, policy AgenticCachePolicy) CacheRejectionReason {
	if result.Analysis == nil || result.Analysis.Mode != AgenticMode {
		return CacheRejectedMalformed
	}
	analysis := result.Analysis
	if analysis.ToolCalls < policy.MinToolCalls {
		return CacheRejectedToolFloor
	}
	if gcsFloorUnmet(analysis.GCSBytes, policy.MinGCSBytes, analysis.EvidencePlanCovered) {
		return CacheRejectedEvidenceFloor
	}
	if !analysis.CritiquePassed || analysis.CritiqueVersion < currentCritiqueVersion {
		return CacheRejectedCritique
	}
	if analysis.SkillSetHash != policy.SkillSetHash {
		return CacheRejectedSkill
	}
	if analysis.ModelHash != policy.ModelHash {
		return CacheRejectedModel
	}
	if analysis.PromptHash != policy.PromptHash {
		return CacheRejectedPrompt
	}
	if result.Summary != nil && result.Summary.IsTransient && policy.ConsecutiveFailures >= transientPersistThreshold {
		return CacheRejectedTransientPersistence
	}
	return CacheAccepted
}

func agenticCachePolicy(client *Client, opts AgenticOptions, skillSetHash, promptHash string, consecutiveFailures int) AgenticCachePolicy {
	policy := AgenticCachePolicy{
		MinToolCalls:        opts.MinToolCalls,
		MinGCSBytes:         opts.MinGCSBytes,
		ConsecutiveFailures: consecutiveFailures,
		SkillSetHash:        skillSetHash,
		PromptHash:          promptHash,
	}
	if client != nil {
		policy.Model = client.model
		policy.ModelHash = client.modelFingerprint()
	}
	return policy
}
