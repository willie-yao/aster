package ai

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAgenticCacheAcceptanceReasons(t *testing.T) {
	now := time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)
	const key = "agentic:universal:job:1:failure"
	base := agenticCacheData{
		analysisResponse: analysisResponse{
			Summary: "summary", RootCause: "root cause", Severity: "High", SuggestedFix: "fix",
		},
		ToolCalls: 3, GCSBytes: 100, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		SkillSetHash: "cached-skills", Model: "cached-model-name", ModelHash: "cached-model", PromptHash: "cached-prompt",
	}
	policy := AgenticCachePolicy{
		MinToolCalls: 2, MinGCSBytes: 50, ConsecutiveFailures: 1, CritiqueRequired: true,
		SkillSetHash: "cached-skills", Model: "current-model", ModelHash: "cached-model", PromptHash: "cached-prompt", Now: now,
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
		{name: "skill provenance differs", entry: entry(base), policy: func() AgenticCachePolicy { p := policy; p.SkillSetHash = "current-skills"; return p }()},
		{name: "model provenance differs", entry: entry(base), policy: func() AgenticCachePolicy { p := policy; p.ModelHash = "current-model"; return p }()},
		{name: "endpoint fingerprint differs", entry: entry(base), policy: func() AgenticCachePolicy {
			p := policy
			p.ModelHash = ModelFingerprint(APIChatCompletions, "https://new-model.invalid/v1/chat/completions", "current-model")
			return p
		}()},
		{name: "prompt provenance differs", entry: entry(base), policy: func() AgenticCachePolicy { p := policy; p.PromptHash = "current-prompt"; return p }()},
		{name: "transient verdict became persistent", entry: func() CacheEntry { d := base; d.IsTransient = true; return entry(d) }(), policy: func() AgenticCachePolicy {
			p := policy
			p.ConsecutiveFailures = transientPersistThreshold
			return p
		}()},
		{name: "expired", entry: func() CacheEntry { e := entry(base); e.CreatedAt = now.Add(-cacheMaxAge - time.Second); return e }(), policy: policy, want: CacheRejectedExpired},
		{name: "future timestamp", entry: func() CacheEntry { e := entry(base); e.CreatedAt = now.Add(cacheMaxFutureSkew + time.Second); return e }(), policy: policy, want: CacheRejectedExpired},
		{name: "tool floor", entry: func() CacheEntry { d := base; d.ToolCalls = 1; return entry(d) }(), policy: policy, want: CacheRejectedToolFloor},
		{name: "evidence floor", entry: func() CacheEntry { d := base; d.GCSBytes = 1; return entry(d) }(), policy: policy, want: CacheRejectedEvidenceFloor},
		{name: "critique pass", entry: func() CacheEntry { d := base; d.CritiquePassed = false; return entry(d) }(), policy: policy, want: CacheRejectedCritique},
		{name: "critique version", entry: func() CacheEntry { d := base; d.CritiqueVersion--; return entry(d) }(), policy: policy, want: CacheRejectedCritique},
		{name: "critique advisory accepts objection", entry: func() CacheEntry { d := base; d.CritiquePassed = false; return entry(d) }(), policy: func() AgenticCachePolicy { p := policy; p.CritiqueRequired = false; return p }()},
		{name: "publication contract rejects old version in advisory mode", entry: func() CacheEntry { d := base; d.CritiqueVersion--; return entry(d) }(), policy: func() AgenticCachePolicy { p := policy; p.CritiqueRequired = false; return p }(), want: CacheRejectedCritique},
		{name: "wrong cache key", entry: func() CacheEntry { e := entry(base); e.Key = "other"; return e }(), policy: policy, want: CacheRejectedMalformed},
		{name: "malformed JSON", entry: CacheEntry{Key: key, CreatedAt: now, Data: json.RawMessage(`{"summary":`)}, policy: policy, want: CacheRejectedMalformed},
		{name: "missing result fields", entry: CacheEntry{Key: key, CreatedAt: now, Data: json.RawMessage(`{}`)}, policy: policy, want: CacheRejectedMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, got := AcceptAgenticCacheEntry(tc.entry, key, tc.policy)
			if got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			if got == CacheAccepted {
				if result.Summary == nil || result.Summary.Summary != "summary" || result.Analysis == nil || !result.Analysis.CacheHit || result.Analysis.Model != "cached-model-name" {
					t.Fatalf("accepted result = %+v", result)
				}
				if result.Analysis.SkillSetHash != "cached-skills" || result.Analysis.ModelHash != "cached-model" || result.Analysis.PromptHash != "cached-prompt" {
					t.Fatalf("cache hit rewrote provenance: %+v", result.Analysis)
				}
				if result.Analysis.GeneratedAt != tc.entry.CreatedAt.UTC().Format(time.RFC3339) || result.Summary.GeneratedAt != result.Analysis.GeneratedAt {
					t.Fatalf("cache hit generation time = summary %q analysis %q, want %q", result.Summary.GeneratedAt, result.Analysis.GeneratedAt, tc.entry.CreatedAt.UTC().Format(time.RFC3339))
				}
			} else if result.Summary != nil || result.Analysis != nil {
				t.Fatalf("rejected result = %+v", result)
			}
		})
	}
}

func TestAgenticCacheAcceptancePreservesStoredGenerationTime(t *testing.T) {
	now := time.Now().UTC()
	generatedAt := now.Add(-2 * time.Hour).Truncate(time.Second)
	const key = "agentic:universal:job:1:failure"
	data := agenticCacheData{
		analysisResponse: analysisResponse{Summary: "summary", RootCause: "root"},
		GeneratedAt:      generatedAt.Format(time.RFC3339), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	entry := CacheEntry{Key: key, CreatedAt: now.Add(-time.Hour), Data: raw}
	result, reason := AcceptAgenticCacheEntry(entry, key, AgenticCachePolicy{Now: now})
	if reason != CacheAccepted || result.Analysis.GeneratedAt != generatedAt.Format(time.RFC3339) || result.Summary.GeneratedAt != result.Analysis.GeneratedAt {
		t.Fatalf("reason=%q result=%+v", reason, result)
	}
}

func TestAgenticResultRejectionRejectsNonAgenticState(t *testing.T) {
	policy := AgenticCachePolicy{}
	for _, result := range []FailureAnalysisResult{
		{},
		{Analysis: &models.AIAnalysis{Mode: "legacy", CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}},
	} {
		if got := AgenticResultRejection(result, policy); got != CacheRejectedMalformed {
			t.Fatalf("reason = %q, want %q", got, CacheRejectedMalformed)
		}
	}
}

func TestCachedAgenticAnalysisMatchesSharedAcceptance(t *testing.T) {
	now := time.Now().UTC()
	const key = "agentic:universal:job:1:failure"
	base := agenticCacheData{
		analysisResponse: analysisResponse{Summary: "summary", RootCause: "root", SuggestedFix: "fix"},
		ToolCalls:        2, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		SkillSetHash: "cached-skills",
		ModelHash:    ModelFingerprint(APIChatCompletions, "https://old-model.invalid/v1/chat/completions", "old-model"),
		PromptHash:   PromptFingerprint("old-sys"),
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agenticCacheData)
		input  AgenticInputs
		want   CacheRejectionReason
	}{
		{name: "accepted"},
		{name: "tool floor", mutate: func(data *agenticCacheData) { data.ToolCalls = 1 }, want: CacheRejectedToolFloor},
		{name: "prompt mismatch remains accepted", input: AgenticInputs{PromptHash: PromptFingerprint("new-sys")}},
		{name: "transient persistence remains accepted", mutate: func(data *agenticCacheData) { data.IsTransient = true }, input: AgenticInputs{ConsecutiveFailures: transientPersistThreshold}},
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
			client := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://new-model.invalid/v1/chat/completions", Model: "new-model"})
			client.cache.entries[key] = CacheEntry{Key: key, CreatedAt: now, Data: raw}
			in := tc.input
			in.Opts = AgenticOptions{MinToolCalls: 2}
			in.Mode = AgenticMode
			policy := agenticCachePolicy(client, in.Opts, "current-skills", effectiveAgenticPromptHash(in, "sys"), in.ConsecutiveFailures)
			_, reason := LookupAgenticCache(client.cache, key, policy)
			_, _, ok := client.cachedAgenticAnalysis(in, key, "sys", now)
			if reason != tc.want || ok != (tc.want == CacheAccepted) {
				t.Fatalf("shared reason = %q, cachedAgenticAnalysis ok = %t", reason, ok)
			}
		})
	}
}

func TestNewAgenticCacheEntryRoundTripsAcceptedResult(t *testing.T) {
	now := time.Now().UTC()
	const key = "agentic:universal:job:1:failure"
	policy := AgenticCachePolicy{MinToolCalls: 2, MinGCSBytes: 50, Model: "model", ModelHash: "model-hash", PromptHash: "prompt-hash", SkillSetHash: "skills", Now: now}
	result := FailureAnalysisResult{
		Summary: &models.AISummary{Summary: "summary", IsTransient: true},
		Analysis: &models.AIAnalysis{
			Mode: AgenticMode, Model: "model", RootCause: "root", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"a.go"},
			SearchSuggestions: []string{"search/a.go"}, EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 7, LineEnd: 7, Quote: "failure"}},
			ToolCalls: 2, ContextBytes: 100, GCSBytes: 50, EvidencePlanCovered: true, BudgetExhausted: true, SameFailureReuse: true,
			CritiquePassed: true, CritiqueVersion: currentCritiqueVersion, SkillSetHash: "skills", ModelHash: "model-hash", PromptHash: "prompt-hash",
		},
	}
	entry, err := NewAgenticCacheEntry(key, result, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, reason := AcceptAgenticCacheEntry(entry, key, policy)
	if reason != CacheAccepted || got.Summary == nil || got.Analysis == nil {
		t.Fatalf("round trip reason=%q result=%+v", reason, got)
	}
	if got.Summary.Summary != result.Summary.Summary || got.Summary.IsTransient != result.Summary.IsTransient ||
		got.Summary.GeneratedAt != entry.CreatedAt.UTC().Format(time.RFC3339) || got.Analysis.GeneratedAt != got.Summary.GeneratedAt ||
		got.Analysis.RootCause != result.Analysis.RootCause || got.Analysis.ToolCalls != result.Analysis.ToolCalls ||
		got.Analysis.ContextBytes != result.Analysis.ContextBytes || got.Analysis.GCSBytes != result.Analysis.GCSBytes ||
		!slices.Equal(got.Analysis.SearchSuggestions, result.Analysis.SearchSuggestions) || !slices.Equal(got.Analysis.EvidenceCitations, result.Analysis.EvidenceCitations) ||
		!got.Analysis.EvidencePlanCovered || !got.Analysis.BudgetExhausted || !got.Analysis.SameFailureReuse || got.Analysis.SkillSetHash != result.Analysis.SkillSetHash {
		t.Fatalf("round trip result = %+v", got)
	}
}

func TestMeetsCurrentCritiqueContract(t *testing.T) {
	analysis := &models.AIAnalysis{Mode: AgenticMode, CritiquePassed: true, CritiqueVersion: CurrentCritiqueVersion()}
	if !MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("current critique contract was rejected")
	}
	analysis.CritiqueVersion--
	if MeetsCurrentCritiqueContract(analysis) {
		t.Fatal("old critique contract was accepted")
	}
}
