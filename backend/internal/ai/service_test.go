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
	tc.AISummary = &models.AISummary{Summary: "cached"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "cached", Mode: AgenticMode, PromptHash: PromptFingerprint("sys"), ModelHash: client.modelFingerprint(), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}

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

// TestStaleTransientVerdict verifies the predicate guarding a cached transient
// verdict: it is stale only once the failure recurs at or above the threshold.
func TestStaleTransientVerdict(t *testing.T) {
	transient := &models.TestCase{AISummary: &models.AISummary{IsTransient: true}}
	real := &models.TestCase{AISummary: &models.AISummary{IsTransient: false}}

	if staleTransientVerdict(transient, transientPersistThreshold-1) {
		t.Error("transient below threshold should not be stale")
	}
	if !staleTransientVerdict(transient, transientPersistThreshold) {
		t.Error("transient at threshold should be stale")
	}
	if staleTransientVerdict(real, transientPersistThreshold+5) {
		t.Error("non-transient verdict is never stale by this rule")
	}
	if staleTransientVerdict(&models.TestCase{}, transientPersistThreshold) {
		t.Error("nil summary should not be stale")
	}
}

// TestService_ReanalyzesStaleTransientOnPersistence verifies a cached transient
// verdict is re-analyzed once the same test becomes persistent, closing the gap
// where a transient answer from consec=1 would otherwise serve forever.
func TestService_ReanalyzesStaleTransientOnPersistence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"real bug","is_transient":false,"root_cause":"r","severity":"High","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	consec := map[string]int{"j::Test A": transientPersistThreshold}
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", consec)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "flaky", IsTransient: true}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "flake", Mode: AgenticMode, PromptHash: PromptFingerprint("sys"), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got == 0 {
		t.Error("expected re-analysis (server call) for a persistent failure with a cached transient verdict")
	}
	if tc.AISummary.IsTransient {
		t.Errorf("expected the transient verdict to be replaced, got IsTransient=true (summary %q)", tc.AISummary.Summary)
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
}

// TestService_ShouldReanalyze_PromptHash verifies prompt changes invalidate
// cached analysis while matching prompts are reused.
func TestService_ShouldReanalyze_PromptHash(t *testing.T) {
	s := &Service{systemPrompt: "engine base + my prompt"}
	// Meets the (zero) floors and is agentic mode, so only the prompt gate
	// can force re-analysis here.
	mk := func(promptHash string) *models.TestCase {
		return &models.TestCase{AIAnalysis: &models.AIAnalysis{Mode: AgenticMode, PromptHash: promptHash, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}}
	}

	if s.shouldReanalyze(mk(PromptFingerprint("engine base + my prompt"))) {
		t.Error("matching prompt hash should be reused, got re-analyze")
	}
	if !s.shouldReanalyze(mk(PromptFingerprint("engine base + an OLD prompt"))) {
		t.Error("changed prompt hash should force re-analysis")
	}
	// Unstamped entries re-analyze once.
	if !s.shouldReanalyze(mk("")) {
		t.Error("unstamped (pre-feature) entry should re-analyze once")
	}
}

// TestService_ShouldReanalyze_ModelHash verifies a model or endpoint swap
// invalidates a cached analysis while a matching model is reused.
func TestService_ShouldReanalyze_ModelHash(t *testing.T) {
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)

	mk := func(modelHash string) *models.TestCase {
		return &models.TestCase{AIAnalysis: &models.AIAnalysis{Mode: AgenticMode, PromptHash: PromptFingerprint("sys"), ModelHash: modelHash, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion}}
	}

	if s.shouldReanalyze(mk(client.modelFingerprint())) {
		t.Error("matching model hash should be reused, got re-analyze")
	}
	if !s.shouldReanalyze(mk("some-other-model")) {
		t.Error("changed model hash should force re-analysis")
	}
	// Unstamped (pre-feature) entries re-analyze once so a stale verdict from an
	// unknown model does not persist.
	if !s.shouldReanalyze(mk("")) {
		t.Error("unstamped entry should re-analyze once")
	}
}

// TestService_ToolsUnsupported_SetsUnavailable verifies tools-unsupported
// endpoints mark failures unavailable and short-circuit subsequent failures.
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
		minToolCalls int
		minGCSBytes  int
		want         bool
	}{
		{"agentic_below_calls_floor", AgenticMode, 0, 0, 3, 0, true},
		{"agentic_at_calls_floor", AgenticMode, 3, 0, 3, 0, false},
		{"agentic_above_calls_floor", AgenticMode, 5, 0, 3, 0, false},
		{"agentic_zero_floors_no_invalidation", AgenticMode, 0, 0, 0, 0, false},
		{"stale_mode_always_reanalyzes", "old-mode", 5, 200_000, 0, 0, true},
		{"empty_mode_always_reanalyzes", "", 5, 200_000, 0, 0, true},
		{"agentic_below_gcs_floor_only", AgenticMode, 10, 1_000, 0, 50_000, true},
		{"agentic_at_gcs_floor_only", AgenticMode, 10, 50_000, 0, 50_000, false},
		{"agentic_above_gcs_floor_only", AgenticMode, 10, 200_000, 0, 50_000, false},
		{"agentic_meets_calls_misses_gcs", AgenticMode, 5, 10_000, 5, 50_000, true},
		{"agentic_misses_calls_meets_gcs", AgenticMode, 1, 200_000, 5, 50_000, true},
		{"agentic_meets_both", AgenticMode, 5, 50_000, 5, 50_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{systemPrompt: "sys", agenticOpts: AgenticOptions{MinToolCalls: tc.minToolCalls, MinGCSBytes: tc.minGCSBytes}}
			testCase := &models.TestCase{
				// Critique is always on, so a reusable entry must be
				// critique-passing; this table isolates the floor behavior.
				AIAnalysis: &models.AIAnalysis{Mode: tc.cachedMode, ToolCalls: tc.cachedCalls, GCSBytes: tc.cachedGCS, PromptHash: PromptFingerprint("sys"), CritiquePassed: true, CritiqueVersion: currentCritiqueVersion},
			}
			if got := s.shouldReanalyze(testCase); got != tc.want {
				t.Errorf("shouldReanalyze cached(mode=%q, calls=%d, gcs=%d) floors(calls=%d, gcs=%d) = %v, want %v",
					tc.cachedMode, tc.cachedCalls, tc.cachedGCS, tc.minToolCalls, tc.minGCSBytes, got, tc.want)
			}
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
	tc.AISummary = &models.AISummary{Summary: "stale zero-tool"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale", Mode: AgenticMode, ToolCalls: 0}

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
