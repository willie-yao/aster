package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
)

func newPatternBackoffService(t *testing.T, serverURL, cacheDir, model string) *Service {
	t.Helper()
	client := NewClientWithOptions(Options{
		Token: "test-token", CacheDir: cacheDir, Endpoint: serverURL, Model: model,
	})
	return NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
}

func TestPatternFailureBackoffSuppressesUnchangedFailureAndSurvivesRestart(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	const privateOutput = "PRIVATE_PATTERN_OUTPUT not json"
	srv.push(200, chatRespFinal(privateOutput))
	cacheDir := t.TempDir()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	s := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	s.patternNow = func() time.Time { return now }
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	s.SetUsageRecorder(usage, aiusage.OriginFetcher)

	if _, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); PatternFailureCategoryOf(err) != PatternFailureMissing {
		t.Fatalf("first error = %v category=%s", err, PatternFailureCategoryOf(err))
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("first model calls = %d, want 1", got)
	}
	if err := s.client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, CacheFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privateOutput) || strings.Contains(string(raw), "Job: job") {
		t.Fatalf("failure cache persisted private content: %s", raw)
	}

	var suppressed int
	_, err = s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		OnFailureSuppressed: func(PatternFailureCategory) { suppressed++ },
	})
	if !IsPatternFailureSuppressed(err) || PatternFailureCategoryOf(err) != PatternFailureMissing || suppressed != 1 {
		t.Fatalf("second error=%v category=%s suppressed=%d", err, PatternFailureCategoryOf(err), suppressed)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("suppressed model calls = %d, want 1 total", got)
	}

	restarted := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	restarted.patternNow = func() time.Time { return now.Add(time.Hour) }
	_, err = restarted.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if !IsPatternFailureSuppressed(err) {
		t.Fatalf("restart error = %v, want suppressed", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("restart model calls = %d, want 1 total", got)
	}

	snapshot := usage.Snapshot()
	if len(snapshot.RecentOperations) != 2 || snapshot.Days[0].Totals.ModelRequests != 1 || snapshot.Days[0].Totals.SuppressedOperations != 1 {
		t.Fatalf("usage = %+v", snapshot)
	}
	var sawSuppressed bool
	for _, operation := range snapshot.RecentOperations {
		if operation.Outcome == aiusage.OutcomeSuppressed {
			sawSuppressed = true
			if operation.ModelRequests != 0 || operation.ReportedRequests != 0 || operation.UnreportedRequests != 0 {
				t.Fatalf("suppressed operation counted provider usage: %+v", operation)
			}
		}
	}
	if !sawSuppressed {
		t.Fatalf("usage omitted suppressed outcome: %+v", snapshot.RecentOperations)
	}
}

func TestPatternFailureBackoffIdentityChangesCallProvider(t *testing.T) {
	shrinkCallDelay(t)
	for _, testCase := range []struct {
		name   string
		change func(*Service, []PatternFailure) (*Service, []PatternFailure)
	}{
		{
			name: "input",
			change: func(service *Service, failures []PatternFailure) (*Service, []PatternFailure) {
				failures[0].FailureMessage += " changed"
				return service, failures
			},
		},
		{
			name: "model",
			change: func(service *Service, failures []PatternFailure) (*Service, []PatternFailure) {
				service.client.model = "claude-changed"
				return service, failures
			},
		},
		{
			name: "generation",
			change: func(service *Service, failures []PatternFailure) (*Service, []PatternFailure) {
				service.SetCacheGeneration("changed-generation")
				return service, failures
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			srv := newScriptedChatServer(t)
			srv.push(200, chatRespFinal("invalid first"))
			srv.push(200, chatRespFinal("invalid second"))
			service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
			failures := patternFailures(3)
			if _, err := service.AnalyzePattern(t.Context(), "job", "job", failures); err == nil {
				t.Fatal("first call unexpectedly succeeded")
			}
			if _, err := service.AnalyzePattern(t.Context(), "job", "job", failures); !IsPatternFailureSuppressed(err) {
				t.Fatalf("unchanged identity error = %v, want suppressed", err)
			}
			if got := atomic.LoadInt32(&srv.calls); got != 1 {
				t.Fatalf("unchanged identity model calls = %d, want 1", got)
			}
			service, failures = testCase.change(service, failures)
			if _, err := service.AnalyzePattern(t.Context(), "job", "job", failures); err == nil || IsPatternFailureSuppressed(err) {
				t.Fatalf("changed identity error = %v, want fresh deterministic failure", err)
			}
			if got := atomic.LoadInt32(&srv.calls); got != 2 {
				t.Fatalf("model calls = %d, want 2", got)
			}
		})
	}
}

func TestPatternFailureBackoffVersionIdentityChanges(t *testing.T) {
	base := patternCacheKeyForVersions(5, 1, "module", "generation", "job", "subject", "prompt", "grounded:owner/repo@sha", "model")
	for name, changed := range map[string]string{
		"prompt": patternCacheKeyForVersions(6, 1, "module", "generation", "job", "subject", "prompt", "grounded:owner/repo@sha", "model"),
		"repair": patternCacheKeyForVersions(5, 2, "module", "generation", "job", "subject", "prompt", "grounded:owner/repo@sha", "model"),
		"source": patternCacheKeyForVersions(5, 1, "module", "generation", "job", "subject", "prompt", "grounded:owner/repo@other", "model"),
	} {
		if changed == base || patternFailureCacheKey(changed) == patternFailureCacheKey(base) {
			t.Fatalf("%s change did not change pattern failure identity", name)
		}
	}
}

type identityPatternRepo struct {
	ref string
}

func (r *identityPatternRepo) ListTree(context.Context) ([]string, error) {
	return []string{"config.yaml"}, nil
}

func (r *identityPatternRepo) ReadFile(context.Context, string) (string, bool, error) {
	return "kind: Config", true, nil
}

func (r *identityPatternRepo) SourceIdentity() (string, string, string) {
	return "owner", "repo", r.ref
}

func TestPatternFailureBackoffSourceChangeCallsProvider(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("tree", "list_repo_tree", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent"}`))
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	s := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	s.patternNow = func() time.Time { return now }
	s.SetSourceRepo("owner", "repo")
	s.SetPatternRepoReader(&identityPatternRepo{ref: "source-b"})
	input := BuildPatternInput("job", patternFailures(3))
	oldKey := patternCacheKey(s.module.Name(), s.cacheGeneration, "job", "job", input.UserPrompt, "grounded:owner/repo@source-a", s.client.modelFingerprint())
	if err := s.client.cache.Set(patternFailureCacheKey(oldKey), patternFailureCacheData{
		Version: patternFailureCacheVersion, Category: PatternFailureSchema,
		FailedAt: now, RetryAfter: now.Add(defaultPatternFailureCooldown),
	}); err != nil {
		t.Fatal(err)
	}
	pa, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if err != nil || pa == nil || pa.Systemic {
		t.Fatalf("pattern=%+v error=%v", pa, err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
}

func TestPatternFailureBackoffExpiresIntoFreshRetry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal("invalid first"))
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent"}`))
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	s := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	s.patternNow = func() time.Time { return now }
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	s.SetUsageRecorder(usage, aiusage.OriginFetcher)
	if _, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err == nil {
		t.Fatal("first call unexpectedly succeeded")
	}
	now = now.Add(defaultPatternFailureCooldown + time.Second)
	freshRetries := 0
	pa, err := s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		OnFreshRetry: func() { freshRetries++ },
	})
	if err != nil || pa == nil || pa.Systemic || freshRetries != 1 {
		t.Fatalf("fresh retry pattern=%+v error=%v retries=%d", pa, err, freshRetries)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
	snapshot := usage.Snapshot()
	if snapshot.Days[0].Totals.CooldownRetries != 1 {
		t.Fatalf("usage totals = %+v", snapshot.Days[0].Totals)
	}
	var sawCooldownRetry bool
	for _, operation := range snapshot.RecentOperations {
		sawCooldownRetry = sawCooldownRetry || operation.CooldownRetry
	}
	if !sawCooldownRetry {
		t.Fatalf("usage omitted cooldown retry: %+v", snapshot.RecentOperations)
	}
}

func TestPatternFailureBackoffDoesNotPersistTransientOrCancellation(t *testing.T) {
	shrinkCallDelay(t)
	for _, status := range []int{429, 500} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			srv := newScriptedChatServer(t)
			for range 4 {
				srv.push(status, "private transient response")
			}
			s := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
			var previousCalls int32
			for attempt := 0; attempt < 2; attempt++ {
				_, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
				if err == nil || IsPatternFailureSuppressed(err) {
					t.Fatalf("attempt %d error=%v", attempt, err)
				}
				if got := atomic.LoadInt32(&srv.calls); got <= previousCalls {
					t.Fatalf("attempt %d made no fresh provider call: before=%d after=%d", attempt, previousCalls, got)
				} else {
					previousCalls = got
				}
			}
		})
	}

	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent"}`))
	s := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.AnalyzePattern(ctx, "job", "job", patternFailures(3)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if _, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatalf("post-cancel call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("post-cancel model calls = %d, want 1", got)
	}
}

func httpStatusName(status int) string {
	if status == 429 {
		return "rate-limited"
	}
	return "provider-5xx"
}
