package ai

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAgenticCacheAcceptanceReasons(t *testing.T) {
	now := time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)
	const key = "agentic:universal:job:1:failure"
	base := agenticCacheData{
		analysisResponse: analysisResponse{
			Summary: "summary", RootCause: "root cause", Severity: "High", SuggestedFix: "fix",
		},
		ToolCalls: 3, GCSBytes: 100, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		SkillSetHash: "skills", ModelHash: "model", PromptHash: "prompt",
	}
	policy := AgenticCachePolicy{
		MinToolCalls: 2, MinGCSBytes: 50, ConsecutiveFailures: 1,
		SkillSetHash: "skills", Model: "model-name", ModelHash: "model", PromptHash: "prompt", Now: now,
	}
	entry := func(data agenticCacheData) CacheEntry {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return CacheEntry{Key: key, CreatedAt: now.Add(-time.Hour), Data: raw}
	}
	cases := []struct {
		name   string
		entry  CacheEntry
		policy AgenticCachePolicy
		want   CacheRejectionReason
	}{
		{name: "accepted", entry: entry(base), policy: policy},
		{name: "expired", entry: func() CacheEntry { e := entry(base); e.CreatedAt = now.Add(-cacheMaxAge - time.Second); return e }(), policy: policy, want: CacheRejectedExpired},
		{name: "tool floor", entry: func() CacheEntry { d := base; d.ToolCalls = 1; return entry(d) }(), policy: policy, want: CacheRejectedToolFloor},
		{name: "evidence floor", entry: func() CacheEntry { d := base; d.GCSBytes = 1; return entry(d) }(), policy: policy, want: CacheRejectedEvidenceFloor},
		{name: "critique pass", entry: func() CacheEntry { d := base; d.CritiquePassed = false; return entry(d) }(), policy: policy, want: CacheRejectedCritique},
		{name: "critique version", entry: func() CacheEntry { d := base; d.CritiqueVersion--; return entry(d) }(), policy: policy, want: CacheRejectedCritique},
		{name: "skill", entry: func() CacheEntry { d := base; d.SkillSetHash = "old"; return entry(d) }(), policy: policy, want: CacheRejectedSkill},
		{name: "model", entry: func() CacheEntry { d := base; d.ModelHash = "old"; return entry(d) }(), policy: policy, want: CacheRejectedModel},
		{name: "prompt", entry: func() CacheEntry { d := base; d.PromptHash = "old"; return entry(d) }(), policy: policy, want: CacheRejectedPrompt},
		{name: "transient persistence", entry: func() CacheEntry { d := base; d.IsTransient = true; return entry(d) }(), policy: func() AgenticCachePolicy { p := policy; p.ConsecutiveFailures = transientPersistThreshold; return p }(), want: CacheRejectedTransientPersistence},
		{name: "malformed key", entry: func() CacheEntry { e := entry(base); e.Key = "other"; return e }(), policy: policy, want: CacheRejectedMalformed},
		{name: "malformed data", entry: CacheEntry{Key: key, CreatedAt: now, Data: json.RawMessage(`{"summary":`)}, policy: policy, want: CacheRejectedMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, got := AcceptAgenticCacheEntry(tc.entry, key, tc.policy)
			if got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			if got == CacheAccepted {
				if result.Summary == nil || result.Summary.Summary != "summary" || result.Analysis == nil || !result.Analysis.CacheHit || result.Analysis.Model != "model-name" {
					t.Fatalf("accepted result = %+v", result)
				}
			} else if result.Summary != nil || result.Analysis != nil {
				t.Fatalf("rejected result = %+v", result)
			}
		})
	}
}

func TestCachedAgenticAnalysisMatchesSharedAcceptance(t *testing.T) {
	now := time.Now().UTC()
	const key = "agentic:universal:job:1:failure"
	base := agenticCacheData{
		analysisResponse: analysisResponse{Summary: "summary", RootCause: "root", SuggestedFix: "fix"},
		ToolCalls:        2, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		ModelHash:  ModelFingerprint(APIChatCompletions, "https://model.invalid/v1/chat/completions", "model"),
		PromptHash: PromptFingerprint("sys"),
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agenticCacheData)
		want   CacheRejectionReason
	}{
		{name: "accepted"},
		{name: "tool floor", mutate: func(data *agenticCacheData) { data.ToolCalls = 1 }, want: CacheRejectedToolFloor},
		{name: "prompt", mutate: func(data *agenticCacheData) { data.PromptHash = "old" }, want: CacheRejectedPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := base
			if tc.mutate != nil {
				tc.mutate(&data)
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			client := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"})
			client.cache.entries[key] = CacheEntry{Key: key, CreatedAt: now, Data: raw}
			in := AgenticInputs{Opts: AgenticOptions{MinToolCalls: 2}, Mode: AgenticMode}
			policy := agenticCachePolicy(client, in.Opts, "", PromptFingerprint("sys"), 1)
			_, reason := LookupAgenticCache(client.cache, key, policy)
			_, _, accepted := client.cachedAgenticAnalysis(in, key, "sys", time.Now())
			if reason != tc.want || accepted != (reason == CacheAccepted) {
				t.Fatalf("shared reason=%q analyzer accepted=%t", reason, accepted)
			}
		})
	}
}

func TestModelFingerprintNormalizesDefaultAPI(t *testing.T) {
	implicit := ModelFingerprint("", "https://model.invalid/v1/chat/completions", "model")
	explicit := ModelFingerprint(APIChatCompletions, "https://model.invalid/v1/chat/completions", "model")
	if implicit != explicit {
		t.Fatalf("default API changed fingerprint: implicit=%q explicit=%q", implicit, explicit)
	}
	if implicit == ModelFingerprint(APIResponses, "https://model.invalid/v1/chat/completions", "model") {
		t.Fatal("responses API did not change fingerprint")
	}
}
