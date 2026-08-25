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

	"github.com/willie-yao/aster/backend/internal/aiusage"
)

func newPatternBackoffService(t *testing.T, serverURL, cacheDir, model string) *Service {
	t.Helper()
	client := NewClientWithOptions(Options{Token: "test-token", CacheDir: cacheDir, Endpoint: serverURL, Model: model})
	return NewService(ServiceConfig{Client: client, Module: &stubModule{name: "kubernetes"}, SystemPrompt: "sys", ConsecutiveFailures: nil})
}

func TestPatternFailureBackoffSuppressesUnchangedFailureAndSurvivesRestart(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	const privateOutput = "PRIVATE_PATTERN_OUTPUT"
	srv.push(200, chatRespFinal(privateOutput))
	cacheDir := t.TempDir()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	service := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	service.patternNow = func() time.Time { return now }
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	service.usageRecorder, service.usageOrigin = usage, aiusage.OriginFetcher

	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); PatternFailureCategoryOf(err) != PatternFailureMissing {
		t.Fatalf("error=%v category=%s", err, PatternFailureCategoryOf(err))
	}
	if err := service.client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, CacheFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privateOutput) || strings.Contains(string(raw), "Job: job") {
		t.Fatalf("failure cache exposed private content: %s", raw)
	}

	var suppressed int
	_, err = service.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		OnFailureSuppressed: func(PatternFailureCategory) { suppressed++ },
	})
	if !IsPatternFailureSuppressed(err) || suppressed != 1 || atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("error=%v suppressed=%d calls=%d", err, suppressed, srv.calls)
	}

	restarted := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	restarted.patternNow = func() time.Time { return now.Add(time.Hour) }
	if _, err := restarted.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); !IsPatternFailureSuppressed(err) {
		t.Fatalf("restart error=%v", err)
	}
	if atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("calls=%d", srv.calls)
	}
	if usage.Snapshot().Days[0].Totals.SuppressedOperations != 1 {
		t.Fatalf("usage=%+v", usage.Snapshot())
	}
}

func TestPatternFailureBackoffIdentityChangesCallProvider(t *testing.T) {
	shrinkCallDelay(t)
	for _, testCase := range []struct {
		name   string
		change func(*Service, []PatternFailure)
	}{
		{name: "input", change: func(_ *Service, failures []PatternFailure) { failures[0].FailureMessage += " changed" }},
		{name: "model", change: func(service *Service, _ []PatternFailure) { service.client.model = "changed" }},
		{name: "generation", change: func(service *Service, _ []PatternFailure) { service.cacheGeneration = "changed" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			srv := newScriptedChatServer(t)
			srv.push(200, chatRespFinal("invalid one"))
			srv.push(200, chatRespFinal("invalid two"))
			service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
			failures := patternFailures(3)
			if _, err := service.AnalyzePattern(t.Context(), "job", "job", failures); err == nil {
				t.Fatal("first attempt succeeded")
			}
			testCase.change(service, failures)
			if _, err := service.AnalyzePattern(t.Context(), "job", "job", failures); err == nil || IsPatternFailureSuppressed(err) {
				t.Fatalf("second error=%v", err)
			}
			if atomic.LoadInt32(&srv.calls) != 2 {
				t.Fatalf("calls=%d", srv.calls)
			}
		})
	}
}

func TestPatternFailureBackoffSourceIdentityDoesNotChangeAnalysisContract(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	service.sourceRepoOwner, service.sourceRepoName = "owner", "repo"
	service.patternRepo = &unusedPatternRepo{}
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatal(err)
	}
	service.sourceRepoOwner, service.sourceRepoName = "other", "source"
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("source identity invalidated analysis-only cache: calls=%d", srv.calls)
	}
}

func TestPatternFailureBackoffExpiresIntoFreshRetry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal("invalid"))
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	service.patternNow = func() time.Time { return now }
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err == nil {
		t.Fatal("first attempt succeeded")
	}
	now = now.Add(defaultPatternFailureCooldown + time.Second)
	retries := 0
	pattern, err := service.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		OnFreshRetry: func() { retries++ },
	})
	if err != nil || pattern == nil || retries != 1 || atomic.LoadInt32(&srv.calls) != 2 {
		t.Fatalf("pattern=%+v error=%v retries=%d calls=%d", pattern, err, retries, srv.calls)
	}
}

func TestPatternFailureBackoffDoesNotPersistTransientOrCancellation(t *testing.T) {
	shrinkCallDelay(t)
	for _, status := range []int{429, 500} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			srv := newScriptedChatServer(t)
			for range 6 {
				srv.push(status, "private transient response")
			}
			service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
			var previous int32
			for attempt := 0; attempt < 2; attempt++ {
				_, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
				if err == nil || IsPatternFailureSuppressed(err) || atomic.LoadInt32(&srv.calls) <= previous {
					t.Fatalf("attempt=%d error=%v calls=%d previous=%d", attempt, err, srv.calls, previous)
				}
				previous = atomic.LoadInt32(&srv.calls)
			}
		})
	}

	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternBackoffService(t, srv.URL, t.TempDir(), "claude-test")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.AnalyzePattern(ctx, "job", "job", patternFailures(3)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatalf("post-cancel error=%v", err)
	}
}

func httpStatusName(status int) string {
	if status == 429 {
		return "rate-limited"
	}
	return "provider-5xx"
}
