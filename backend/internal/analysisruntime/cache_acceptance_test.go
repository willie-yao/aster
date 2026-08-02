package analysisruntime

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestContainerStateStoreAcceptCachedFailure(t *testing.T) {
	now := time.Now().UTC()
	request := ai.FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1", Build: models.BuildInfo{BuildID: "1"},
		TestCase: models.TestCase{Name: "test", Status: "failed", FailureMessage: "failed"},
	}
	analysisProject := &Project{
		Config:       &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2, MinGCSBytes: 50}}},
		Provider:     project.AIProvider{API: project.AIAPIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"},
		SystemPrompt: "system prompt",
	}
	planner := NewReusePlanner(analysisProject)
	policyFor := func(request ai.FailureAnalysisRequest) ai.AgenticCachePolicy {
		run := models.BuildResult{BuildInfo: request.Build}
		testCase := request.TestCase
		return planner.FailureCachePolicy(t.Context(), &http.Client{}, &run, &testCase, max(1, request.ConsecutiveFailures))
	}
	cacheData := func(policy ai.AgenticCachePolicy, gcsBytes int) json.RawMessage {
		data := map[string]any{
			"summary": "summary", "is_transient": false, "root_cause": "root cause", "severity": "High", "suggested_fix": "fix", "relevant_files": []string{},
			"tool_calls": 2, "gcs_bytes": gcsBytes, "critique_passed": true, "critique_version": 999,
			"model_hash": policy.ModelHash, "prompt_hash": policy.PromptHash,
		}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	validData := cacheData(policyFor(request), 50)
	stickyData := func() json.RawMessage {
		var data map[string]any
		if err := json.Unmarshal(validData, &data); err != nil {
			t.Fatal(err)
		}
		data["skill_set_hash"] = "old-skills"
		data["model_hash"] = "old-model"
		data["prompt_hash"] = "old-prompt"
		data["is_transient"] = true
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}()
	stickyRequest := request
	stickyRequest.ConsecutiveFailures = 3
	buildRequest := request
	buildRequest.TestCase.Source = models.TestCaseSourceBuild
	cases := []struct {
		name    string
		entry   ai.CacheEntry
		request ai.FailureAnalysisRequest
		want    ai.CacheRejectionReason
	}{
		{name: "accepted", entry: ai.CacheEntry{Key: FailureCacheKey(request), CreatedAt: now, Data: validData}, request: request},
		{name: "configuration and transient changes remain accepted", entry: ai.CacheEntry{Key: FailureCacheKey(stickyRequest), CreatedAt: now, Data: stickyData}, request: stickyRequest},
		{name: "expired", entry: ai.CacheEntry{Key: FailureCacheKey(request), CreatedAt: now.Add(-31 * 24 * time.Hour), Data: validData}, request: request, want: ai.CacheRejectedExpired},
		{name: "future timestamp", entry: ai.CacheEntry{Key: FailureCacheKey(request), CreatedAt: now.Add(6 * time.Minute), Data: validData}, request: request, want: ai.CacheRejectedExpired},
		{name: "malformed", entry: ai.CacheEntry{Key: FailureCacheKey(request), CreatedAt: now, Data: json.RawMessage(`{}`)}, request: request, want: ai.CacheRejectedMalformed},
		{name: "build failure ignores junit evidence floor", entry: ai.CacheEntry{
			Key: FailureCacheKey(buildRequest), CreatedAt: now, Data: cacheData(policyFor(buildRequest), 0),
		}, request: buildRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cacheKey := FailureCacheKey(tc.request)
			entries := map[string]ai.CacheEntry{cacheKey: tc.entry}
			data, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ai.CacheFilename), data, 0o600); err != nil {
				t.Fatal(err)
			}
			state, err := NewContainerStateStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			result, reason, err := state.AcceptCachedFailure(t.Context(), &http.Client{}, tc.request, planner)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if reason == ai.CacheAccepted && (result.Analysis == nil || !result.Analysis.CacheHit || result.Summary == nil) {
				t.Fatalf("accepted result = %+v", result)
			}
		})
	}
}

func TestContainerStateStoreCritiqueRequirementFollowsRetryBudget(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	request := ai.FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1", Build: models.BuildInfo{BuildID: "1"},
		TestCase: models.TestCase{Name: "test", Status: "failed", FailureMessage: "failed"},
	}
	for _, tc := range []struct {
		name    string
		retries int
		want    ai.CacheRejectionReason
	}{
		{name: "zero retries is advisory"},
		{name: "positive retries requires critique", retries: 1, want: ai.CacheRejectedCritique},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysisProject := &Project{
				Config: &project.Config{AI: &project.AI{Agentic: project.Agentic{
					MinToolCalls: 2, MinGCSBytes: 50, Critique: project.AgenticCritique{MaxRetries: &tc.retries},
				}}},
				Provider:     project.AIProvider{API: project.AIAPIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"},
				SystemPrompt: "system prompt",
			}
			planner := NewReusePlanner(analysisProject)
			policy, err := FailureCachePolicy(t.Context(), &http.Client{}, request, planner)
			if err != nil {
				t.Fatal(err)
			}
			result := ai.FailureAnalysisResult{
				Summary: &models.AISummary{GeneratedAt: now.Format(time.RFC3339), Summary: "summary"},
				Analysis: &models.AIAnalysis{
					GeneratedAt: now.Format(time.RFC3339), Mode: ai.AgenticMode, RootCause: "root cause",
					ToolCalls: 2, GCSBytes: 50, CritiqueVersion: ai.CurrentCritiqueVersion(),
					ModelHash: policy.ModelHash, PromptHash: policy.PromptHash,
				},
			}
			key := FailureCacheKey(request)
			entry, err := ai.NewAgenticCacheEntry(key, result, now)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			data, err := json.Marshal(map[string]ai.CacheEntry{key: entry})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ai.CacheFilename), data, 0o600); err != nil {
				t.Fatal(err)
			}
			state, err := NewContainerStateStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, reason, err := state.AcceptCachedFailure(t.Context(), &http.Client{}, request, planner)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestContainerStateStoreStagesPromotedCacheForCheckpoint(t *testing.T) {
	dir := t.TempDir()
	state, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	request := ai.FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1", Build: models.BuildInfo{BuildID: "1"},
		TestCase: models.TestCase{Name: "test", Status: "failed", FailureMessage: "failed"},
	}
	key := FailureCacheKey(request)
	now := time.Now().UTC()
	newer := ai.CacheEntry{Key: key, CreatedAt: now, Data: json.RawMessage(`{"summary":"newer"}`)}
	entry := ai.CacheEntry{Key: key, CreatedAt: now.Add(-time.Minute), Data: json.RawMessage(`{"summary":"summary"}`)}
	if err := state.StageCacheEntry(newer); err != nil {
		t.Fatal(err)
	}
	if err := state.StageCacheEntry(entry); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.CacheSeed(request)[key]
	var gotData, wantData map[string]any
	if err := json.Unmarshal(got.Data, &gotData); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(entry.Data, &wantData); err != nil {
		t.Fatal(err)
	}
	if got.Key != key || !reflect.DeepEqual(gotData, wantData) {
		t.Fatalf("reloaded promoted cache = %+v", got)
	}
}
