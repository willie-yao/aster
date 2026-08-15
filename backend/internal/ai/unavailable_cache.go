package ai

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	policyUnavailableCacheVersion = 1
	policyUnavailableCooldown     = 6 * time.Hour
	policyUnavailableReason       = "missing_artifact_citation"
)

type policyUnavailableCacheIdentity struct {
	ModelHash           string              `json:"model_hash"`
	PromptHash          string              `json:"prompt_hash"`
	SkillSetHash        string              `json:"skill_set_hash,omitempty"`
	CacheGeneration     string              `json:"cache_generation,omitempty"`
	CritiqueVersion     int                 `json:"critique_version"`
	CritiquePolicy      CritiqueCachePolicy `json:"critique_policy"`
	MinToolCalls        int                 `json:"min_tool_calls"`
	MinGCSBytes         int                 `json:"min_gcs_bytes"`
	ConsecutiveFailures int                 `json:"consecutive_failures"`
}

type policyUnavailableCacheData struct {
	Version  int                            `json:"version"`
	Reason   string                         `json:"reason"`
	Identity policyUnavailableCacheIdentity `json:"identity"`
}

// PolicyUnavailableCacheKey returns the private cooldown key for one agentic failure key.
func PolicyUnavailableCacheKey(agenticKey string) string {
	return "policy-unavailable:" + agenticKey
}

func policyUnavailableIdentity(policy AgenticCachePolicy) policyUnavailableCacheIdentity {
	return policyUnavailableCacheIdentity{
		ModelHash: policy.ModelHash, PromptHash: policy.PromptHash, SkillSetHash: policy.SkillSetHash,
		CacheGeneration: policy.CacheGeneration, CritiqueVersion: currentCritiqueVersion,
		CritiquePolicy: policy.CritiquePolicy, MinToolCalls: policy.MinToolCalls, MinGCSBytes: policy.MinGCSBytes,
		ConsecutiveFailures: policy.ConsecutiveFailures,
	}
}

func storePolicyUnavailable(cache *Cache, key string, policy AgenticCachePolicy, now time.Time) error {
	if cache == nil || key == "" || policy.CritiquePolicy != CritiqueCachePolicyHard {
		return nil
	}
	data := policyUnavailableCacheData{
		Version: policyUnavailableCacheVersion, Reason: policyUnavailableReason,
		Identity: policyUnavailableIdentity(policy),
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode policy unavailable cache: %w", err)
	}
	return cache.StoreEntry(CacheEntry{Key: key, CreatedAt: now, Data: raw})
}

// LookupPolicyUnavailableCooldown validates a private grounded-unavailable marker.
func LookupPolicyUnavailableCooldown(cache *Cache, key string, policy AgenticCachePolicy, now time.Time) bool {
	if cache == nil || key == "" || policy.CritiquePolicy != CritiqueCachePolicyHard {
		return false
	}
	entry, ok := cache.Lookup(key)
	if !ok {
		return false
	}
	if entry.CreatedAt.IsZero() || entry.CreatedAt.After(now.Add(cacheMaxFutureSkew)) || now.Sub(entry.CreatedAt) > policyUnavailableCooldown {
		cache.Delete(key)
		return false
	}
	var data policyUnavailableCacheData
	if json.Unmarshal(entry.Data, &data) != nil || data.Version != policyUnavailableCacheVersion || data.Reason != policyUnavailableReason || data.Identity != policyUnavailableIdentity(policy) {
		cache.Delete(key)
		return false
	}
	return true
}
