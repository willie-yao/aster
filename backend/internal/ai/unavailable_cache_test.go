package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func testUnavailablePolicy() AgenticCachePolicy {
	return AgenticCachePolicy{
		MinToolCalls: 2, MinGCSBytes: 100, ConsecutiveFailures: 3,
		SkillSetHash: "skills", ModelHash: "model-hash", PromptHash: "prompt-hash",
		CacheGeneration: "generation", CritiquePolicy: CritiqueCachePolicyHard,
	}
}

func TestPolicyUnavailableCacheRoundTrip(t *testing.T) {
	cache := NewCache(t.TempDir())
	policy := testUnavailablePolicy()
	now := time.Now().UTC()
	key := PolicyUnavailableCacheKey("agentic:key")
	if err := storePolicyUnavailable(cache, key, policy, now); err != nil {
		t.Fatal(err)
	}
	if !LookupPolicyUnavailableCooldown(cache, key, policy, now.Add(time.Hour)) {
		t.Fatal("stored cooldown was not accepted")
	}
	entry, ok := cache.Lookup(key)
	if !ok {
		t.Fatal("stored cooldown is missing")
	}
	if strings.Contains(string(entry.Data), "endpoint") || strings.Contains(string(entry.Data), "model-name") {
		t.Fatalf("cooldown leaked provider coordinates: %s", entry.Data)
	}
	var data policyUnavailableCacheData
	if err := json.Unmarshal(entry.Data, &data); err != nil || data.Reason != policyUnavailableReason || data.Identity != policyUnavailableIdentity(policy) {
		t.Fatalf("data = %+v, error = %v", data, err)
	}
}

func TestPolicyUnavailableCacheExpiresAndInvalidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AgenticCachePolicy)
		age    time.Duration
	}{
		{name: "expired", age: policyUnavailableCooldown + time.Second},
		{name: "prompt changed", mutate: func(policy *AgenticCachePolicy) { policy.PromptHash = "changed" }},
		{name: "skills changed", mutate: func(policy *AgenticCachePolicy) { policy.SkillSetHash = "changed" }},
		{name: "streak changed", mutate: func(policy *AgenticCachePolicy) { policy.ConsecutiveFailures++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewCache(t.TempDir())
			stored := testUnavailablePolicy()
			now := time.Now().UTC()
			key := PolicyUnavailableCacheKey("agentic:key")
			if err := storePolicyUnavailable(cache, key, stored, now.Add(-tc.age)); err != nil {
				t.Fatal(err)
			}
			lookup := stored
			if tc.mutate != nil {
				tc.mutate(&lookup)
			}
			if LookupPolicyUnavailableCooldown(cache, key, lookup, now) {
				t.Fatal("stale or mismatched cooldown was accepted")
			}
			if _, ok := cache.Lookup(key); ok {
				t.Fatal("stale or mismatched cooldown was not removed")
			}
		})
	}
}

func TestPolicyUnavailableCacheHardPolicyOnly(t *testing.T) {
	cache := NewCache(t.TempDir())
	policy := testUnavailablePolicy()
	policy.CritiquePolicy = CritiqueCachePolicyAdvisory
	key := PolicyUnavailableCacheKey("agentic:key")
	if err := storePolicyUnavailable(cache, key, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(key); ok {
		t.Fatal("advisory policy stored a cooldown")
	}
}

func TestServiceIgnoresPersistedLegacyPolicyUnavailableCooldown(t *testing.T) {
	cacheDir := t.TempDir()
	client := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://provider.example.invalid/chat/completions", Model: "model", CacheDir: cacheDir})
	service := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	service.EnableAgentic(AgenticOptions{MinToolCalls: 2, CritiqueCachePolicy: CritiqueCachePolicyHard}, nil, nil, nil)

	run := newRun("job", "1")
	tc := newFailedTC("Test A", "failure")
	promptHash := service.analysisPromptHash(tc, service.baseFailurePrompt(t.Context(), &http.Client{}, run, tc, 3))
	policy := service.agenticCachePolicyFor(tc, promptHash, 3)
	key := PolicyUnavailableCacheKey(service.agenticCacheKey("job", "1", tc.Name, tc.FailureMessage))
	if err := storePolicyUnavailable(client.cache, key, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := client.cache.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://provider.example.invalid/chat/completions", Model: "model", CacheDir: cacheDir})
	reloadedService := NewService(reloaded, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	reloadedService.EnableAgentic(AgenticOptions{MinToolCalls: 2, CritiqueCachePolicy: CritiqueCachePolicyHard}, nil, nil, nil)
	result, err := reloadedService.AnalyzeFailure(context.Background(), &http.Client{}, FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1/", Build: run.BuildInfo, TestCase: *tc, ConsecutiveFailures: 3,
	})
	if err == nil || !strings.Contains(err.Error(), "browser factory") {
		t.Fatalf("error = %v, want normal analysis path", err)
	}
	if result.Analysis != nil || result.Summary == nil || !strings.Contains(result.Summary.Summary, "browser factory") {
		t.Fatalf("result = %+v", result)
	}
}

func TestServiceAdvisoryPolicyDoesNotReuseUnavailableCooldown(t *testing.T) {
	client := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://provider.example.invalid/chat/completions", Model: "model", CacheDir: t.TempDir()})
	hardService := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	hardService.EnableAgentic(AgenticOptions{CritiqueCachePolicy: CritiqueCachePolicyHard}, nil, nil, nil)
	run := &models.BuildResult{BuildInfo: models.BuildInfo{JobName: "job", BuildID: "1"}}
	tc := newFailedTC("Test A", "failure")
	promptHash := hardService.analysisPromptHash(tc, hardService.baseFailurePrompt(t.Context(), &http.Client{}, run, tc, 1))
	key := PolicyUnavailableCacheKey(hardService.agenticCacheKey("job", "1", tc.Name, tc.FailureMessage))
	if err := storePolicyUnavailable(client.cache, key, hardService.agenticCachePolicyFor(tc, promptHash, 1), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	advisory := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	advisory.EnableAgentic(AgenticOptions{CritiqueCachePolicy: CritiqueCachePolicyAdvisory}, nil, nil, nil)
	_, err := advisory.AnalyzeFailure(t.Context(), &http.Client{}, FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1/", Build: run.BuildInfo, TestCase: *tc, ConsecutiveFailures: 1,
	})
	if err == nil || errors.Is(err, ErrMissingArtifactCitation) || !strings.Contains(err.Error(), "browser factory") {
		t.Fatalf("error = %v, want normal advisory analysis path", err)
	}
}
