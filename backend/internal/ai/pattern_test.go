package ai

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func newPatternTestService(t *testing.T, serverURL string) *Service {
	t.Helper()
	client := newAgenticTestClient(t, serverURL)
	return NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
}

func patternFailures(n int) []PatternFailure {
	out := make([]PatternFailure, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, PatternFailure{
			BuildID:        string(rune('a'+i)) + "build",
			FailingTest:    "spec",
			FailureMessage: "Timed out after 3600s",
			RootCause:      "etcd-join deadlock on burstable VM",
			IsTransient:    true,
			Severity:       "Transient-Ignore",
		})
	}
	return out
}

func TestAnalyzePattern_TooFewBuilds_NoCall(t *testing.T) {
	srv := newScriptedChatServer(t)
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(1))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa != nil {
		t.Errorf("expected nil verdict for <2 failures, got %+v", pa)
	}
	if atomic.LoadInt32(&srv.calls) != 0 {
		t.Errorf("expected no model call, got %d", srv.calls)
	}
}

func TestAnalyzePattern_SystemicVerdict(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	verdict := `{"systemic":true,"confidence":"high","shared_root_cause":"undersized burstable control-plane VM starves etcd","shared_builds":["abuild","bbuild","cbuild"],"suggested_fix":"use a non-burstable VM size","summary":"8/10 builds share an etcd-join deadlock"}`
	srv.push(200, chatRespFinal(verdict))
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "the-job", patternFailures(3))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa == nil {
		t.Fatal("expected a verdict")
	}
	if !pa.Systemic {
		t.Error("expected systemic=true")
	}
	if pa.Confidence != "high" {
		t.Errorf("confidence = %q, want high", pa.Confidence)
	}
	if pa.Subject != "the-job" {
		t.Errorf("subject = %q, want the-job", pa.Subject)
	}
	if pa.BuildsAnalyzed != 3 {
		t.Errorf("builds_analyzed = %d, want 3", pa.BuildsAnalyzed)
	}
	if !strings.Contains(pa.SharedRootCause, "etcd") {
		t.Errorf("shared_root_cause = %q", pa.SharedRootCause)
	}
	if len(pa.SharedBuilds) != 3 {
		t.Errorf("shared_builds = %v", pa.SharedBuilds)
	}
}

func TestAnalyzePattern_CacheHit_NoSecondCall(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","summary":"independent flakes"}`))
	s := newPatternTestService(t, srv.URL)

	in := patternFailures(3)
	if _, err := s.AnalyzePattern(context.Background(), "job", "job", in); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call with the same failure set must be served from cache.
	pa, err := s.AnalyzePattern(context.Background(), "job", "job", in)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if pa == nil || pa.Systemic {
		t.Errorf("unexpected verdict: %+v", pa)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("expected 1 model call (second cached), got %d", got)
	}
}

func TestAnalyzePattern_ConfidenceNormalized(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"VERY-SURE","summary":"independent flakes"}`))
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(2))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa.Confidence != "low" {
		t.Errorf("confidence = %q, want low (normalized fallback)", pa.Confidence)
	}
}

func TestAnalyzePattern_IncompleteVerdictRejected(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Empty object has no summary, so it is rejected and not cached.
	srv.push(200, chatRespFinal(`{}`))
	// Systemic verdict with no root cause is rejected.
	srv.push(200, chatRespFinal(`{"systemic":true,"confidence":"high","summary":"x"}`))
	s := newPatternTestService(t, srv.URL)

	if _, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(2)); err == nil {
		t.Error("expected error for empty verdict")
	}
	if _, err := s.AnalyzePattern(context.Background(), "job", "job2", patternFailures(2)); err == nil {
		t.Error("expected error for systemic verdict without a root cause")
	}
}

func TestPatternCacheKey_TracksModelInput(t *testing.T) {
	base := patternFailures(3)
	p1 := buildPatternUserPrompt("job", base)
	k1 := patternCacheKey("kubernetes", "job", "job", p1, "toolfree")

	// A changed root cause changes the rendered prompt, so the key changes.
	changed := patternFailures(3)
	changed[0].RootCause = "different cause"
	k2 := patternCacheKey("kubernetes", "job", "job", buildPatternUserPrompt("job", changed), "toolfree")
	if k1 == k2 {
		t.Error("expected cache key to change when the evidence changes")
	}

	// A changed failure message also changes the key because evidence differs.
	msgChanged := patternFailures(3)
	msgChanged[0].FailureMessage = "a totally different symptom"
	k3 := patternCacheKey("kubernetes", "job", "job", buildPatternUserPrompt("job", msgChanged), "toolfree")
	if k1 == k3 {
		t.Error("expected cache key to change when a failure message changes")
	}

	// Same inputs produce a stable key.
	if patternCacheKey("kubernetes", "job", "job", p1, "toolfree") != k1 {
		t.Error("expected stable cache key for identical inputs")
	}
}

func TestCollectRelevantFiles_LeadsWithLocation(t *testing.T) {
	failures := []PatternFailure{
		{LocationFile: "test/e2e/foo_test.go", RelevantFiles: []string{"config/a.yaml", "test/e2e/foo_test.go"}},
		{LocationFile: "test/e2e/foo_test.go", RelevantFiles: []string{"config/b.yaml"}},
	}
	got := collectRelevantFiles(failures)
	want := []string{"test/e2e/foo_test.go", "config/a.yaml", "config/b.yaml"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
}

// TestCollectRelevantFiles_NotInCacheKey guards the invariant that the failing-
// test location seeds the fix harness without perturbing the pattern cache key:
// LocationFile must not be rendered into the correlation prompt.
func TestCollectRelevantFiles_NotInCacheKey(t *testing.T) {
	base := patternFailures(2)
	withLoc := patternFailures(2)
	withLoc[0].LocationFile = "test/e2e/foo_test.go"

	k1 := patternCacheKey("kubernetes", "job", "job", buildPatternUserPrompt("job", base), "toolfree")
	k2 := patternCacheKey("kubernetes", "job", "job", buildPatternUserPrompt("job", withLoc), "toolfree")
	if k1 != k2 {
		t.Error("LocationFile must not change the pattern cache key (kept out of the prompt)")
	}
	// But it must still surface in the pattern's relevant files.
	if got := collectRelevantFiles(withLoc); len(got) == 0 || got[0] != "test/e2e/foo_test.go" {
		t.Errorf("LocationFile should lead collectRelevantFiles, got %v", got)
	}
}

func TestBuildPatternUserPrompt_RendersFixAndFiles(t *testing.T) {
	failures := patternFailures(2)
	failures[0].SuggestedFix = "serialize nodepool scaling operations"
	failures[0].RelevantFiles = []string{"exp/controllers/azuremachinepool_controller.go", "test/e2e/aks_byo_node.go"}

	prompt := buildPatternUserPrompt("job", failures)
	for _, want := range []string{
		"suggested_fix: serialize nodepool scaling operations",
		"relevant_files: exp/controllers/azuremachinepool_controller.go, test/e2e/aks_byo_node.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}

	// A changed per-build suggested fix is new evidence, so the cache key moves.
	base := patternCacheKey("kubernetes", "job", "job", buildPatternUserPrompt("job", patternFailures(2)), "toolfree")
	withFix := patternCacheKey("kubernetes", "job", "job", prompt, "toolfree")
	if base == withFix {
		t.Error("expected cache key to change when a per-build suggested_fix is added")
	}
}

func TestAnalyzePattern_CapsBuilds(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":true,"confidence":"high","shared_root_cause":"x","summary":"x"}`))
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(maxPatternBuilds+5))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa.BuildsAnalyzed != maxPatternBuilds {
		t.Errorf("builds_analyzed = %d, want capped at %d", pa.BuildsAnalyzed, maxPatternBuilds)
	}
	// The prompt the model received must reflect the cap, not the full set.
	var reqs int
	srv.mu.Lock()
	reqs = len(srv.requests)
	srv.mu.Unlock()
	if reqs != 1 {
		t.Fatalf("expected one request captured, got %d", reqs)
	}
	var sent agChatRequest
	if err := json.Unmarshal(srv.requests[0], &sent); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	user := *sent.Messages[len(sent.Messages)-1].Content
	if strings.Count(user, "--- Build ") != maxPatternBuilds {
		t.Errorf("prompt build count = %d, want %d", strings.Count(user, "--- Build "), maxPatternBuilds)
	}
}

func TestParsePatternResultMarksPathsUnverified(t *testing.T) {
	result := `{
		"systemic": true,
		"confidence": "high",
		"shared_root_cause": "the controller writes stale state",
		"shared_builds": ["2", "1"],
		"suggested_fix": "serialize updates in config/controller.yaml",
		"summary": "the same controller path failed twice"
	}`
	pa, err := ParsePatternResult("job", patternFailures(2), result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pa.SuggestedFix, "config/controller.yaml (unverified path)") {
		t.Fatalf("suggested fix = %q, want unverified path annotation", pa.SuggestedFix)
	}
}
