package ai

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// stubModule satisfies ai.Module for service tests. The prompt is returned
// verbatim by AnalysisPrompt.
type stubModule struct {
	name   string
	prompt string
}

func (m *stubModule) Name() string { return m.name }
func (m *stubModule) AnalysisPrompt(_ context.Context, _ *http.Client, _ *models.BuildResult, _ *models.TestCase, _ int) string {
	return m.prompt
}

func newRun(jobName, buildID string) *models.BuildResult {
	return &models.BuildResult{
		BuildInfo: models.BuildInfo{JobName: jobName, BuildID: buildID},
	}
}

func newFailedTC(name, msg string) *models.TestCase {
	return &models.TestCase{Name: name, FailureMessage: msg, Status: "failed"}
}

func reusablePublishedTestCase(analysis *models.AIAnalysis) *models.TestCase {
	generatedAt := analysis.GeneratedAt
	if generatedAt == "" {
		generatedAt = time.Now().UTC().Format(time.RFC3339)
		analysis.GeneratedAt = generatedAt
	}
	return &models.TestCase{
		AISummary:  &models.AISummary{GeneratedAt: generatedAt, Summary: "cached summary"},
		AIAnalysis: analysis,
	}
}

func TestService_Agentic_TagsModeAgentic(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"x","is_transient":false,"root_cause":"y","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)
	tc := newFailedTC("Test A", "failure msg")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis == nil {
		t.Fatal("AIAnalysis nil")
	}
	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q", tc.AIAnalysis.Mode, AgenticMode)
	}
}

func TestService_ReanalyzeOnModeChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Agentic call returns final JSON; a cached entry with a foreign mode
	// should be invalidated.
	final := `{"summary":"new agentic","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "stale summary"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale root cause", Mode: "old-mode"}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q (stale non-agentic entry should be re-analyzed)", tc.AIAnalysis.Mode, AgenticMode)
	}
	if !strings.Contains(tc.AISummary.Summary, "new agentic") {
		t.Errorf("expected fresh agentic summary, got %q", tc.AISummary.Summary)
	}
}

func TestService_SkipWhenAlreadyAnalyzedSameMode(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)
	traces := NewTraceStore()
	s.SetTraceStore(traces)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Summary: "cached"}
	tc.AIAnalysis = &models.AIAnalysis{GeneratedAt: tc.AISummary.GeneratedAt, RootCause: "cached", Mode: AgenticMode, PromptHash: PromptFingerprint("sys"), ModelHash: client.modelFingerprint(), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Errorf("server calls = %d, want 0 (existing agentic analysis should be kept)", got)
	}
	if tc.AIAnalysis.RootCause != "cached" {
		t.Errorf("expected cached root cause to be preserved, got %q", tc.AIAnalysis.RootCause)
	}
	got := traces.Snapshot()
	if len(got.Traces) != 1 || got.Traces[0].Outcome != "build_cache_hit" || len(got.Traces[0].Events) != 1 || got.Traces[0].Events[0].Outcome != "build_hit" {
		t.Fatalf("trace = %+v", got)
	}
}

func TestService_ReusesTransientVerdictAfterPersistence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	consec := map[string]int{"j::Test A": transientPersistThreshold}
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", consec)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Summary: "flaky", IsTransient: true}
	tc.AIAnalysis = &models.AIAnalysis{
		GeneratedAt: tc.AISummary.GeneratedAt, RootCause: "flake", Mode: AgenticMode, SkillSetHash: "old-skills", ModelHash: "old-model",
		PromptHash: "old-prompt", CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
	}

	if s.NeedsAnalysis(t.Context(), &http.Client{}, newRun("j", "1"), tc, transientPersistThreshold) {
		t.Fatal("persistent streak made the cached transient verdict stale")
	}
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Fatalf("server calls = %d, want 0", got)
	}
	if !tc.AISummary.IsTransient || tc.AISummary.Summary != "flaky" || tc.AIAnalysis.RootCause != "flake" {
		t.Fatalf("cached transient result changed: summary=%+v analysis=%+v", tc.AISummary, tc.AIAnalysis)
	}
	if tc.AIAnalysis.SkillSetHash != "old-skills" || tc.AIAnalysis.ModelHash != "old-model" || tc.AIAnalysis.PromptHash != "old-prompt" {
		t.Fatalf("cached provenance changed: %+v", tc.AIAnalysis)
	}
}

func TestService_CacheKeyShape(t *testing.T) {
	s := &Service{module: &stubModule{name: "kubernetes"}}
	// Agentic key encodes job + build so two builds of the same test never collide.
	a1 := s.agenticCacheKey("job1", "build1", "Test A", "boom")
	a2 := s.agenticCacheKey("job1", "build2", "Test A", "boom")
	if a1 == a2 {
		t.Errorf("agentic key should differ between builds: %q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "agentic:kubernetes:job1:build1:") {
		t.Errorf("agentic key shape unexpected: %q", a1)
	}
	s.agenticOpts.CritiqueMaxRetries = 2
	if got := s.agenticCacheKey("job1", "build1", "Test A", "boom"); got != a1 {
		t.Errorf("critique retry budget changed cache key: %q vs %q", a1, got)
	}
	s.SetCacheGeneration("0123456789abcdef")
	generated := s.agenticCacheKey("job1", "build1", "Test A", "boom")
	if generated == a1 || !strings.HasPrefix(generated, "agentic:kubernetes:g:0123456789abcdef:job1:build1:") {
		t.Fatalf("generated cache key = %q", generated)
	}
	s.SetCacheGeneration("")
	if got := s.agenticCacheKey("job1", "build1", "Test A", "boom"); got != a1 {
		t.Fatalf("returning to empty generation changed key: %q vs %q", got, a1)
	}
}

func TestService_ShouldReanalyze_IgnoresProvenanceChanges(t *testing.T) {
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "kubernetes"}, "engine base + my prompt", nil)
	base := models.AIAnalysis{
		Mode: AgenticMode, SkillSetHash: "old-skills", ModelHash: "old-model", PromptHash: "old-prompt",
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*models.AIAnalysis)
	}{
		{name: "matching current provenance", mutate: func(analysis *models.AIAnalysis) {
			analysis.ModelHash = client.modelFingerprint()
			analysis.PromptHash = PromptFingerprint("engine base + my prompt")
		}},
		{name: "model changed"},
		{name: "endpoint changed", mutate: func(analysis *models.AIAnalysis) {
			analysis.ModelHash = ModelFingerprint(APIChatCompletions, "https://old-endpoint.invalid/v1/chat/completions", "old-model")
		}},
		{name: "prompt changed", mutate: func(analysis *models.AIAnalysis) { analysis.PromptHash = PromptFingerprint("old prompt") }},
		{name: "skill set changed", mutate: func(analysis *models.AIAnalysis) { analysis.SkillSetHash = "different-skills" }},
		{name: "missing provenance", mutate: func(analysis *models.AIAnalysis) {
			analysis.SkillSetHash = ""
			analysis.ModelHash = ""
			analysis.PromptHash = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := base
			if tc.mutate != nil {
				tc.mutate(&analysis)
			}
			if s.shouldReanalyze(reusablePublishedTestCase(&analysis)) {
				t.Fatalf("provenance change forced re-analysis: %+v", analysis)
			}
		})
	}
}

func TestService_ShouldReanalyze_AgeAndMalformedState(t *testing.T) {
	s := &Service{systemPrompt: "sys"}
	now := time.Now().UTC()
	base := models.AIAnalysis{
		GeneratedAt: now.Format(time.RFC3339), Mode: AgenticMode, RootCause: "root cause",
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*models.TestCase)
		want   bool
	}{
		{name: "current"},
		{name: "expired", mutate: func(tc *models.TestCase) {
			tc.AIAnalysis.GeneratedAt = now.Add(-cacheMaxAge - time.Second).Format(time.RFC3339)
		}, want: true},
		{name: "future", mutate: func(tc *models.TestCase) {
			tc.AIAnalysis.GeneratedAt = now.Add(cacheMaxFutureSkew + time.Second).Format(time.RFC3339)
		}, want: true},
		{name: "invalid timestamp", mutate: func(tc *models.TestCase) { tc.AIAnalysis.GeneratedAt = "not-a-time" }, want: true},
		{name: "missing result fields", mutate: func(tc *models.TestCase) {
			tc.AISummary.Summary = ""
			tc.AIAnalysis.RootCause = ""
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := base
			testCase := reusablePublishedTestCase(&analysis)
			if tc.mutate != nil {
				tc.mutate(testCase)
			}
			if got := s.shouldReanalyze(testCase); got != tc.want {
				t.Fatalf("shouldReanalyze = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestService_ToolsUnsupported_SetsUnavailable(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Only one server push: the second Analyze short-circuits before HTTP.
	srv.push(400, `{"error":{"message":"function calling not supported"}}`)

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc1 := newFailedTC("Test A", "msg-a")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc1)
	tc2 := newFailedTC("Test B", "msg-b")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc2)

	if tc1.AISummary == nil || !strings.Contains(tc1.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("first failure: expected unavailable summary, got %+v", tc1.AISummary)
	}
	if tc1.AIAnalysis != nil {
		t.Errorf("first failure: AIAnalysis should NOT be set under tools-unsupported, got %+v", tc1.AIAnalysis)
	}
	if tc2.AISummary == nil || !strings.Contains(tc2.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("second failure: expected unavailable summary (sticky flag), got %+v", tc2.AISummary)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (second failure must bail before HTTP)", got)
	}
}

// TestService_ShouldReanalyze_FloorTable covers cache invalidation for mode
// mismatches and agentic floor changes.
func TestService_ShouldReanalyze_FloorTable(t *testing.T) {
	cases := []struct {
		name         string
		cachedMode   string
		cachedCalls  int
		cachedGCS    int
		covered      bool
		minToolCalls int
		minGCSBytes  int
		want         bool
	}{
		{name: "agentic_below_calls_floor", cachedMode: AgenticMode, cachedCalls: 0, minToolCalls: 3, want: true},
		{name: "agentic_at_calls_floor", cachedMode: AgenticMode, cachedCalls: 3, minToolCalls: 3},
		{name: "agentic_above_calls_floor", cachedMode: AgenticMode, cachedCalls: 5, minToolCalls: 3},
		{name: "agentic_zero_floors_no_invalidation", cachedMode: AgenticMode},
		{name: "stale_mode_always_reanalyzes", cachedMode: "old-mode", cachedCalls: 5, cachedGCS: 200_000, want: true},
		{name: "empty_mode_always_reanalyzes", cachedCalls: 5, cachedGCS: 200_000, want: true},
		{name: "agentic_below_gcs_floor_only", cachedMode: AgenticMode, cachedCalls: 10, cachedGCS: 1_000, minGCSBytes: 50_000, want: true},
		{name: "agentic_below_gcs_with_covered_plan", cachedMode: AgenticMode, cachedCalls: 10, cachedGCS: 1_000, covered: true, minGCSBytes: 50_000},
		{name: "agentic_at_gcs_floor_only", cachedMode: AgenticMode, cachedCalls: 10, cachedGCS: 50_000, minGCSBytes: 50_000},
		{name: "agentic_above_gcs_floor_only", cachedMode: AgenticMode, cachedCalls: 10, cachedGCS: 200_000, minGCSBytes: 50_000},
		{name: "agentic_meets_calls_misses_gcs", cachedMode: AgenticMode, cachedCalls: 5, cachedGCS: 10_000, minToolCalls: 5, minGCSBytes: 50_000, want: true},
		{name: "agentic_misses_calls_meets_gcs", cachedMode: AgenticMode, cachedCalls: 1, cachedGCS: 200_000, minToolCalls: 5, minGCSBytes: 50_000, want: true},
		{name: "agentic_meets_both", cachedMode: AgenticMode, cachedCalls: 5, cachedGCS: 50_000, minToolCalls: 5, minGCSBytes: 50_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{systemPrompt: "sys", agenticOpts: AgenticOptions{MinToolCalls: tc.minToolCalls, MinGCSBytes: tc.minGCSBytes}}
			// Use a critique-passing entry so this table isolates floor behavior.
			testCase := reusablePublishedTestCase(&models.AIAnalysis{
				Mode: tc.cachedMode, ToolCalls: tc.cachedCalls, GCSBytes: tc.cachedGCS, EvidencePlanCovered: tc.covered,
				PromptHash: PromptFingerprint("sys"), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
			})
			if got := s.shouldReanalyze(testCase); got != tc.want {
				t.Errorf("shouldReanalyze cached(mode=%q, calls=%d, gcs=%d) floors(calls=%d, gcs=%d) = %v, want %v",
					tc.cachedMode, tc.cachedCalls, tc.cachedGCS, tc.minToolCalls, tc.minGCSBytes, got, tc.want)
			}
		})
	}
}

func TestService_EvidencePlanCoverageOnlyBypassesGCSFloor(t *testing.T) {
	client := newAgenticTestClient(t, "http://example.invalid")
	set := loadAgenticSkillsForTest(t, map[string]string{
		"profiled": "id: profiled\ntriggers: [profiled]\nrequired_evidence:\n  - id: log\n    any_of: [failure\\.log$]\n",
	})
	s := &Service{
		client: client, systemPrompt: "sys", skillSet: set,
		agenticOpts: AgenticOptions{MinToolCalls: 2, MinGCSBytes: 50_000, CritiqueMaxRetries: 1},
	}
	base := models.AIAnalysis{
		Mode: AgenticMode, ToolCalls: 2, GCSBytes: 1_000, EvidencePlanCovered: true,
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		SkillSetHash: set.Hash(), ModelHash: client.modelFingerprint(), PromptHash: PromptFingerprint("sys"),
	}
	cases := []struct {
		name   string
		mutate func(*models.AIAnalysis)
		want   bool
	}{
		{name: "current marked entry"},
		{name: "tool floor", mutate: func(analysis *models.AIAnalysis) { analysis.ToolCalls = 1 }, want: true},
		{name: "critique pass", mutate: func(analysis *models.AIAnalysis) { analysis.CritiquePassed = false }, want: true},
		{name: "critique version", mutate: func(analysis *models.AIAnalysis) { analysis.CritiqueVersion-- }, want: true},
		{name: "skill hash", mutate: func(analysis *models.AIAnalysis) { analysis.SkillSetHash = "stale" }},
		{name: "model hash", mutate: func(analysis *models.AIAnalysis) { analysis.ModelHash = "stale" }},
		{name: "prompt hash", mutate: func(analysis *models.AIAnalysis) { analysis.PromptHash = "stale" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			analysis := base
			if tc.mutate != nil {
				tc.mutate(&analysis)
			}
			if got := s.shouldReanalyze(reusablePublishedTestCase(&analysis)); got != tc.want {
				t.Fatalf("shouldReanalyze = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestService_ZeroCritiqueRetriesMakesCritiqueAdvisory(t *testing.T) {
	client := newAgenticTestClient(t, "http://example.invalid")
	s := &Service{
		client: client, systemPrompt: "sys",
		agenticOpts: AgenticOptions{MinToolCalls: 2, MinGCSBytes: 50_000, CritiqueMaxRetries: 0},
	}
	base := models.AIAnalysis{
		Mode: AgenticMode, ToolCalls: 2, GCSBytes: 50_000,
		PromptHash: PromptFingerprint("sys"), ModelHash: client.modelFingerprint(),
		CritiqueVersion: currentCritiqueVersion,
	}
	for _, tc := range []struct {
		name           string
		mutate         func(*models.AIAnalysis)
		wantReanalysis bool
	}{
		{name: "critique objection"},
		{name: "old critique version", mutate: func(analysis *models.AIAnalysis) {
			analysis.CritiquePassed = true
			analysis.CritiqueVersion = currentCritiqueVersion - 1
		}, wantReanalysis: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := base
			if tc.mutate != nil {
				tc.mutate(&analysis)
			}
			if got := s.shouldReanalyze(reusablePublishedTestCase(&analysis)); got != tc.wantReanalysis {
				t.Fatalf("shouldReanalyze = %t, want %t", got, tc.wantReanalysis)
			}
			s.agenticOpts.CritiqueMaxRetries = 1
			if !s.shouldReanalyze(reusablePublishedTestCase(&analysis)) {
				t.Fatal("enforced critique accepted advisory analysis")
			}
			s.agenticOpts.CritiqueMaxRetries = 0
		})
	}
}

// TestService_BelowFloor_ReanalyzesBuildCacheEntry exercises the full Analyze
// path: a build-cached agentic analysis with ToolCalls below the current floor
// must trigger a fresh API call instead of being served from the build cache.
func TestService_BelowFloor_ReanalyzesBuildCacheEntry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(`{"summary":"fresh post-floor","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(
		AgenticOptions{MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, MinToolCalls: 1},
		&fakeFactory{}, registry, enabled,
	)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Summary: "stale zero-tool"}
	tc.AIAnalysis = &models.AIAnalysis{GeneratedAt: tc.AISummary.GeneratedAt, RootCause: "stale", Mode: AgenticMode, ToolCalls: 0}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.ToolCalls < 1 {
		t.Errorf("ToolCalls = %d, want >= 1 (fresh analysis should satisfy floor)", tc.AIAnalysis.ToolCalls)
	}
	if !strings.Contains(tc.AISummary.Summary, "fresh post-floor") {
		t.Errorf("expected fresh summary, got %q (build-cached pre-floor entry should have been invalidated)", tc.AISummary.Summary)
	}
}

// newServiceTestRegistry returns a filesystem-only registry for service tests.
func newServiceTestRegistry(t *testing.T) (*tools.Registry, []string) {
	t.Helper()
	r := tools.NewRegistry()
	filesystem.Register(r)
	enabled, err := r.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatalf("registry.Enable: %v", err)
	}
	return r, enabled
}

type fakeFactory struct{}

func (f *fakeFactory) ForBuild(_, _ string) artifacts.Browser {
	return &fakeBrowser{}
}

var _ Module = (*stubModule)(nil)
var _ artifacts.Factory = (*fakeFactory)(nil)

// TestSetUnavailable_RetrySemantics verifies unavailable summaries are replaced
// on retry while transient and real summaries are preserved.
func TestSetUnavailable_RetrySemantics(t *testing.T) {
	s := &Service{}

	t.Run("sets when nil", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		s.setUnavailable(tc, errEndpointA)
		if tc.AISummary == nil || !strings.Contains(tc.AISummary.Summary, "endpoint A") {
			t.Fatalf("want endpoint A summary, got %+v", tc.AISummary)
		}
	})

	t.Run("overwrites a prior unavailable summary", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{
			GeneratedAt: "2000-01-01T00:00:00Z",
			Summary:     unavailablePrefix + "endpoint A is down",
		}
		s.setUnavailable(tc, errEndpointB)
		if !strings.Contains(tc.AISummary.Summary, "endpoint B") {
			t.Fatalf("stale error not replaced: %q", tc.AISummary.Summary)
		}
		if tc.AISummary.GeneratedAt == "2000-01-01T00:00:00Z" {
			t.Error("timestamp not refreshed on retry")
		}
	})

	t.Run("preserves a transient classification", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: "infra flake", IsTransient: true}
		s.setUnavailable(tc, errEndpointB)
		if !tc.AISummary.IsTransient || tc.AISummary.Summary != "infra flake" {
			t.Fatalf("transient summary clobbered: %+v", tc.AISummary)
		}
	})

	t.Run("preserves a real summary", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: "real root cause"}
		s.setUnavailable(tc, errEndpointB)
		if tc.AISummary.Summary != "real root cause" {
			t.Fatalf("real summary clobbered: %q", tc.AISummary.Summary)
		}
	})

	t.Run("preserves a real summary even with the unavailable prefix", func(t *testing.T) {
		// A model result is identified by an attached AIAnalysis, not just by
		// its text, so a summary that happens to start with the prefix is not
		// mistaken for an engine placeholder.
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: unavailablePrefix + "is part of the real analysis"}
		tc.AIAnalysis = &models.AIAnalysis{Mode: AgenticMode}
		s.setUnavailable(tc, errEndpointB)
		if !strings.Contains(tc.AISummary.Summary, "real analysis") {
			t.Fatalf("real summary with prefix clobbered: %q", tc.AISummary.Summary)
		}
	})
}

var (
	errEndpointA = fmtErr("endpoint A")
	errEndpointB = fmtErr("endpoint B")
)

func fmtErr(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func TestService_ShouldReanalyze_PreRankedEvidencePlanContract(t *testing.T) {
	s := &Service{systemPrompt: "sys", agenticOpts: AgenticOptions{CritiqueMaxRetries: 1}}
	analysis := &models.AIAnalysis{
		Mode: AgenticMode, PromptHash: PromptFingerprint("sys"),
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion - 1,
	}
	if !s.shouldReanalyze(reusablePublishedTestCase(analysis)) {
		t.Fatal("pre-ranked-plan analysis should be re-analyzed")
	}
	analysis.CritiqueVersion = currentCritiqueVersion
	if s.shouldReanalyze(reusablePublishedTestCase(analysis)) {
		t.Fatal("current ranked-plan analysis should be reusable")
	}
}

func TestServiceBuildFailureUsesSourceSpecificGCSFloor(t *testing.T) {
	client := newAgenticTestClient(t, "http://example.invalid")
	s := &Service{
		client: client, systemPrompt: "sys",
		agenticOpts: AgenticOptions{MinToolCalls: 5, MinGCSBytes: 50_000},
	}
	buildFailure := &models.TestCase{Source: models.TestCaseSourceBuild}
	if got := s.agenticOptionsFor(buildFailure); got.MinGCSBytes != 0 || got.MinToolCalls != 5 {
		t.Fatalf("build failure options = %+v", got)
	}
	if got := s.agenticOptionsFor(&models.TestCase{}); got.MinGCSBytes != 50_000 {
		t.Fatalf("JUnit options = %+v", got)
	}
	analysis := &models.AIAnalysis{
		Mode: AgenticMode, ToolCalls: 5, GCSBytes: 1_000,
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		ModelHash: client.modelFingerprint(), PromptHash: PromptFingerprint("sys"),
	}
	buildFailure = reusablePublishedTestCase(analysis)
	buildFailure.Source = models.TestCaseSourceBuild
	if s.shouldReanalyze(buildFailure) {
		t.Fatal("build failure below the project GCS floor was not reusable")
	}
	if !s.shouldReanalyze(reusablePublishedTestCase(analysis)) {
		t.Fatal("JUnit failure below the project GCS floor was reusable")
	}
}

func TestServiceBuildPromptChangeReusesPublishedAndAgenticCaches(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"old","is_transient":false,"root_cause":"old cleanup explanation","severity":"High","suggested_fix":"Correct the build configuration.","relevant_files":[]}`))

	module := &stubModule{name: "universal", prompt: "use the build log"}
	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	service := NewService(client, module, "sys", nil)
	service.EnableAgentic(AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	}, &fakeFactory{}, registry, enabled)
	run := newRun("job", "1")
	tc := newFailedTC("Prow job execution", "build failed")
	tc.Source = models.TestCaseSourceBuild

	service.Analyze(t.Context(), &http.Client{}, "job", "logs/job/1/", run, tc)
	oldHash := tc.AIAnalysis.PromptHash
	module.prompt = "use the build log and select the earliest causal error before cleanup"
	service.Analyze(t.Context(), &http.Client{}, "job", "logs/job/1/", run, tc)

	if tc.AIAnalysis.RootCause != "old cleanup explanation" {
		t.Fatalf("root cause = %q", tc.AIAnalysis.RootCause)
	}
	if tc.AIAnalysis.PromptHash != oldHash {
		t.Fatal("build prompt change rewrote cached provenance")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
}

func TestService_ShouldReanalyze_CacheGeneration(t *testing.T) {
	s := &Service{systemPrompt: "sys"}
	s.SetCacheGeneration("0123456789abcdef")
	analysis := &models.AIAnalysis{
		Mode: AgenticMode, RootCause: "root", CritiquePassed: true,
		CritiqueVersion: currentCritiqueVersion, CacheGeneration: "fedcba9876543210",
	}
	if !s.shouldReanalyze(reusablePublishedTestCase(analysis)) {
		t.Fatal("cross-generation published analysis was reused")
	}
	analysis.CacheGeneration = "0123456789abcdef"
	if s.shouldReanalyze(reusablePublishedTestCase(analysis)) {
		t.Fatal("matching generation was not reusable")
	}
}
