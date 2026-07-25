package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
)

// writeJobDetail writes a jobs/<name>.json fixture under dataDir.
func writeJobDetail(t *testing.T, dataDir, name string, detail models.JobDetail) {
	t.Helper()
	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func systemicPattern() models.PatternAnalysis {
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout"}
	pa.ID = models.PatternID(pa)
	return pa
}

func TestResolve_Unresolve_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout", SharedBuilds: []string{"100", "250", "175"}}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.Resolve(pa.ID, "willie-yao", "fixed by test-infra #123"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	st := resolve.Load(dataDir)
	e, ok := st.Resolved[pa.ID]
	if !ok {
		t.Fatal("pattern should be resolved")
	}
	if e.ResolvedBy != "willie-yao" || e.Note != "fixed by test-infra #123" {
		t.Errorf("entry metadata wrong: %+v", e)
	}
	if e.Watermark != "250" { // highest of the shared builds
		t.Errorf("watermark = %q, want 250", e.Watermark)
	}

	if err := s.Unresolve(pa.ID); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}
	if resolve.Load(dataDir).IsResolved(pa.ID) {
		t.Fatal("pattern should be unresolved")
	}
}

func TestResolve_NonSystemicRejected(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: false, SharedRootCause: "flake"}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "willie-yao", ""); err == nil {
		t.Fatal("expected non-systemic resolve to be rejected")
	}
}

func TestUnresolve_NotFound(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	if err := s.Unresolve("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPreviewIssue_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x"})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), "does-not-exist", "tok", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPreviewIssue_NoRepoResolved(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	// No issues repo and no branding source repo -> unresolved.
	cfg := &project.Config{}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want repo-resolution error, got %v", err)
	}
}

func TestPreviewIssue_NonSystemicNotActionable(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: false, SharedRootCause: "flake"}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want not-actionable error, got %v", err)
	}
}

func TestPreviewFix_AINotConfigured(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewFix(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want AI-not-configured error, got %v", err)
	}
}

func TestPreviewCache_TokenOwnershipAndConsumption(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	tok, err := s.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}

	// A different admin's token must not resolve the draft.
	if _, err := s.take("someone-else", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("cross-admin take: want ErrPreviewNotFound, got %v", err)
	}
	// The owning admin resolves it once...
	if _, err := s.take("owner-token", tok); err != nil {
		t.Fatalf("owner take: %v", err)
	}
	// ...and the token is single-use.
	if _, err := s.take("owner-token", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("reuse take: want ErrPreviewNotFound, got %v", err)
	}
}

func TestPreviewCache_Expiry(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	tok, err := s.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.previewStore.update(func(state *previewState, _ time.Time) (bool, error) {
		state.Previews[tokenHash(tok)].CreatedAt = time.Now().Add(-previewTTL - time.Minute).UTC().Format(time.RFC3339)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.take("owner-token", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("expired take: want ErrPreviewNotFound, got %v", err)
	}
}

func TestSafeReason_RedactsAIErrorsPassesOurs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"chat returned 401: unauthorized: <provider body>", "the AI service could not complete the request"},
		{"AuthenticateToken authentication failed: unauthorized", "the AI service could not complete the request"},
		{"the model could not produce a code change for this failure", "the model could not produce a code change for this failure"},
		{"no candidate files in the repo matched the failure", "no candidate files in the repo matched the failure"},
		{"", "the fix could not be generated"},
	}
	for _, c := range cases {
		if got := safeReason(c.in); got != c.want {
			t.Errorf("safeReason(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeReason_Truncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := safeReason(long)
	if len([]rune(got)) > 302 { // 300 + ellipsis
		t.Errorf("safeReason did not truncate: len=%d", len([]rune(got)))
	}
}

func TestPreviewFixWithContextRejectsMismatchedPatternTarget(t *testing.T) {
	dataDir := t.TempDir()
	pattern := systemicPattern()
	pattern.SharedBuilds = []string{"123"}
	pattern.SuggestedFix = "bound retries"
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{
		JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pattern},
	})
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	generationContext := fixpr.GenerationContext{
		AssistantAnswer:   "selected answer",
		ArtifactCitations: []fixpr.Evidence{{Path: "build-log.txt", Quote: "failure"}},
	}
	for _, target := range []FixTarget{
		{JobID: "other-job", BuildID: "123"},
		{JobID: "periodic-x", BuildID: "other-build"},
	} {
		if _, err := service.PreviewFixWithContext(
			t.Context(), pattern, "token", "", target, generationContext,
		); !errors.Is(err, ErrPatternMismatch) {
			t.Fatalf("target %+v error = %v", target, err)
		}
	}
}

func TestPreviewFixWithContextHonorsMinConfidence(t *testing.T) {
	pattern := systemicPattern()
	pattern.Confidence = "medium"
	pattern.SuggestedFix = "bound retries"
	pattern.SharedBuilds = []string{"123"}
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
		Repo: &project.SourceRepo{Owner: "o", Name: "r"}, MinConfidence: "high",
	}}}
	service := NewService(cfg, t.TempDir(), AIConfig{
		API: "chat_completions", Endpoint: "https://ai.example/v1/chat/completions", Model: "model", Token: "token",
	})
	_, err := service.PreviewFixWithContext(
		t.Context(), pattern, "token", "", FixTarget{JobID: "periodic-x", BuildID: "123"}, fixpr.GenerationContext{
			AssistantAnswer: "selected answer",
			ArtifactCitations: []fixpr.Evidence{{
				Path: "build-log.txt", Quote: "failure",
			}},
		},
	)
	if !errors.Is(err, ErrPreviewRejected) || !strings.Contains(err.Error(), "not auto-fixable") {
		t.Fatalf("error = %v", err)
	}
}

func TestSafeFixPreviewErrorPreservesContextSentinels(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		wrapped := fmt.Errorf("agent generation: %w", cause)
		if got := safeFixPreviewError(wrapped); !errors.Is(got, cause) {
			t.Errorf("safeFixPreviewError(%v) = %v", wrapped, got)
		}
	}
	got := safeFixPreviewError(errors.New("chat returned 500: private provider body"))
	if !errors.Is(got, ErrPreviewRejected) || strings.Contains(got.Error(), "private provider body") {
		t.Fatalf("provider body leaked or rejection untyped: %v", got)
	}
}

func TestPreviewConfirmationLifecycleIsRecoverableAcrossServices(t *testing.T) {
	dataDir := t.TempDir()
	first := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := first.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	second := NewService(&project.Config{}, dataDir, AIConfig{})
	entry, resultURL, attemptID, _, err := second.beginConfirm("owner-token", token, time.Hour)
	if err != nil || entry == nil || resultURL != "" {
		t.Fatalf("begin confirmation = %+v, %q, %v", entry, resultURL, err)
	}
	if _, _, _, _, err := first.beginConfirm("owner-token", token, time.Hour); !errors.Is(err, ErrPreviewPending) {
		t.Fatalf("cross-service concurrent confirmation error = %v", err)
	}
	if err := second.finishConfirm("owner-token", token, attemptID, "https://github.com/o/r/issues/1", nil); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(&project.Config{}, dataDir, AIConfig{})
	entry, resultURL, _, _, err = restarted.beginConfirm("owner-token", token, time.Hour)
	if err != nil || entry != nil || resultURL != "https://github.com/o/r/issues/1" {
		t.Fatalf("recovered confirmation = %+v, %q, %v", entry, resultURL, err)
	}
	if _, _, _, _, err := restarted.beginConfirm("other-token", token, time.Hour); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("cross-owner confirmation error = %v", err)
	}
}

func TestPreviewConfirmationFailureCanRetryAcrossServices(t *testing.T) {
	dataDir := t.TempDir()
	first := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := first.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, attemptID, _, err := first.beginConfirm("owner-token", token, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.finishConfirm("owner-token", token, attemptID, "", errors.New("temporary failure")); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(&project.Config{}, dataDir, AIConfig{})
	retry, resultURL, attemptID, reconcile, err := restarted.beginConfirm("owner-token", token, time.Hour)
	if err != nil || retry == nil || resultURL != "" || !reconcile {
		t.Fatalf("reconcile confirmation = %+v, %q, reconcile=%t, %v", retry, resultURL, reconcile, err)
	}
	if err := restarted.finishConfirm("owner-token", token, attemptID, "", ErrPreviewOutcomeUnknown); err != nil {
		t.Fatal(err)
	}
}

func TestFixPreviewSnapshotPersistsAcrossServices(t *testing.T) {
	dataDir := t.TempDir()
	generated := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "retry failure", Rationale: "bound retries", Diff: "diff",
		Files: map[string]string{"retry.go": "fixed\n"}, Title: "Fix retry", Description: "description", Body: "body",
		Pattern: models.PatternAnalysis{Subject: "retry failure", JobID: "periodic-x"},
		Key:     "fix-key", Base: ghpr.Base{Branch: "main", HeadSHA: "abc", TreeSHA: "tree"},
	})
	first := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := first.stash("owner-token", &previewEntry{kind: gfKind, fix: generated})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(&project.Config{}, dataDir, AIConfig{})
	entry, _, _, _, err := restarted.beginConfirm("owner-token", token, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if entry.fix == nil || entry.fix.Preview.Diff != "diff" || entry.fix.Preview.Files["retry.go"] != "fixed\n" {
		t.Fatalf("restored fix = %+v", entry.fix)
	}
	info, err := os.Stat(filepath.Join(dataDir, "action_preview_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("preview state permissions = %o", info.Mode().Perm())
	}
}

func TestPreviewConfirmationLeaseFencesStaleAttempt(t *testing.T) {
	dataDir := t.TempDir()
	first := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := first.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, firstAttempt, _, err := first.beginConfirm("owner-token", token, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
		record := state.Previews[tokenHash(token)]
		record.CreatedAt = now.Add(-previewTTL - time.Minute).Format(time.RFC3339)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	second := NewService(&project.Config{}, dataDir, AIConfig{})
	if _, _, _, _, err := second.beginConfirm("owner-token", token, 30*time.Minute); !errors.Is(err, ErrPreviewPending) {
		t.Fatalf("active long confirmation error = %v", err)
	}
	if err := first.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
		record := state.Previews[tokenHash(token)]
		record.LeaseExpires = now.Add(-time.Second).Format(time.RFC3339)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _, secondAttempt, _, err := second.beginConfirm("owner-token", token, 30*time.Minute)
	if err != nil || secondAttempt == firstAttempt {
		t.Fatalf("second attempt = %q err=%v", secondAttempt, err)
	}
	if err := first.finishConfirm("owner-token", token, firstAttempt, "https://github.com/o/r/issues/old", nil); !errors.Is(err, ErrPreviewSuperseded) {
		t.Fatalf("stale completion error = %v", err)
	}
	if err := second.finishConfirm("owner-token", token, secondAttempt, "https://github.com/o/r/issues/new", nil); err != nil {
		t.Fatal(err)
	}
	_, resultURL, _, _, err := NewService(&project.Config{}, dataDir, AIConfig{}).beginConfirm("owner-token", token, time.Hour)
	if err != nil || resultURL != "https://github.com/o/r/issues/new" {
		t.Fatalf("fenced result = %q err=%v", resultURL, err)
	}
}

func TestFailedPreviewConfirmationRefreshesRetryWindow(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		resultURL  string
		confirmErr error
	}{
		{name: "transient error", confirmErr: errors.New("temporary failure")},
		{name: "empty result"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
			token, err := service.stash("owner-token", &previewEntry{kind: "issue"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, attemptID, _, err := service.beginConfirm("owner-token", token, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
				record := state.Previews[tokenHash(token)]
				record.CreatedAt = now.Add(-previewTTL - time.Minute).Format(time.RFC3339Nano)
				record.LeaseExpires = now.Add(time.Hour).Format(time.RFC3339Nano)
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			finishErr := service.finishConfirm("owner-token", token, attemptID, testCase.resultURL, testCase.confirmErr)
			if testCase.confirmErr != nil && finishErr != nil {
				t.Fatal(finishErr)
			}
			if testCase.confirmErr == nil && finishErr == nil {
				t.Fatal("empty result completion was accepted")
			}
			_, _, reconcileAttempt, reconcile, err := service.beginConfirm("owner-token", token, time.Hour)
			if err != nil || !reconcile {
				t.Fatalf("failed confirmation was not retryable: %v", err)
			}
			if err := service.finishConfirm("owner-token", token, reconcileAttempt, "", ErrPreviewOutcomeUnknown); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnknownPreviewBecomesRetryableAfterConsistencyWindow(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	token, err := service.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, attemptID, _, err := service.beginConfirm("owner-token", token, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.finishConfirm("owner-token", token, attemptID, "", errors.New("lost response")); err != nil {
		t.Fatal(err)
	}
	if err := service.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
		state.Previews[tokenHash(token)].CreatedAt = now.Add(-previewTTL - time.Minute).Format(time.RFC3339Nano)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _, reconcile, err := service.beginConfirm("owner-token", token, time.Hour)
	if err != nil || reconcile {
		t.Fatalf("expired unknown preview reconcile=%t err=%v", reconcile, err)
	}
}

func TestPreviewStoreRejectsDuplicateActionWhilePending(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	entry := &previewEntry{kind: "issue", spec: issues.IssueSpec{Key: "same-action"}}
	if _, err := service.stash("owner-token", entry); err != nil {
		t.Fatal(err)
	}
	if _, err := service.stash("other-owner", entry); !errors.Is(err, ErrPreviewPending) {
		t.Fatalf("duplicate action error = %v", err)
	}
}

func TestPreviewStoreRejectsOversizedWriteWithoutReplacingState(t *testing.T) {
	dataDir := t.TempDir()
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	firstToken, err := service.stash("owner-token", &previewEntry{
		kind: "issue", spec: issues.IssueSpec{Key: "first", Title: "First", Body: "small"},
	})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "action_preview_state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	service.previewStore.maxBytes = len(before) + 256
	if _, err := service.stash("owner-token", &previewEntry{
		kind: "issue", spec: issues.IssueSpec{Key: "large", Title: "Large", Body: strings.Repeat("x", len(before)*4)},
	}); err == nil {
		t.Fatal("oversized preview state write was accepted")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("oversized write replaced the last valid preview state")
	}
	if _, err := NewService(&project.Config{}, dataDir, AIConfig{}).take("owner-token", firstToken); err != nil {
		t.Fatalf("valid preview was not recoverable: %v", err)
	}
}

func TestPreviewStoreEvictsOldestNonRunningPreviewToFit(t *testing.T) {
	dataDir := t.TempDir()
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	firstToken, err := service.stash("owner-token", &previewEntry{
		kind: "issue", spec: issues.IssueSpec{Key: "first", Title: "First", Body: strings.Repeat("a", 600)},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dataDir, "action_preview_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	service.previewStore.maxBytes = int(info.Size()) + 256
	secondToken, err := service.stash("owner-token", &previewEntry{
		kind: "issue", spec: issues.IssueSpec{Key: "second", Title: "Second", Body: strings.Repeat("b", 600)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.take("owner-token", firstToken); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("oldest preview was not evicted: %v", err)
	}
	if _, err := service.take("owner-token", secondToken); err != nil {
		t.Fatalf("newest preview was not retained: %v", err)
	}
}

func TestPreviewStoreRejectsCountPressureWithoutEvictingConfirmedResults(t *testing.T) {
	dataDir := t.TempDir()
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := service.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, attemptID, _, err := service.beginConfirm("owner-token", token, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resultURL := "https://github.com/o/r/issues/confirmed"
	if err := service.finishConfirm("owner-token", token, attemptID, resultURL, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
		for i := 1; i < maxPersistedPreviews; i++ {
			state.Previews[fmt.Sprintf("confirmed-%03d", i)] = &persistedPreview{
				Owner:     "owner",
				Kind:      "issue",
				CreatedAt: now.Format(time.RFC3339Nano),
				Status:    previewStatusDone,
				ResultURL: fmt.Sprintf("https://github.com/o/r/issues/%d", i),
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "action_preview_state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.stash("owner-token", &previewEntry{kind: "issue"}); err == nil {
		t.Fatal("preview count pressure was accepted")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected write replaced confirmed preview state")
	}
	_, recoveredURL, _, _, err := NewService(&project.Config{}, dataDir, AIConfig{}).beginConfirm("owner-token", token, time.Hour)
	if err != nil || recoveredURL != resultURL {
		t.Fatalf("confirmed result = %q err=%v", recoveredURL, err)
	}
}

func TestPreviewStoreRejectsSizePressureWithoutEvictingConfirmedResult(t *testing.T) {
	dataDir := t.TempDir()
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := service.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, attemptID, _, err := service.beginConfirm("owner-token", token, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resultURL := "https://github.com/o/r/issues/confirmed"
	if err := service.finishConfirm("owner-token", token, attemptID, resultURL, nil); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "action_preview_state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	service.previewStore.maxBytes = len(before) + 256
	if _, err := service.stash("owner-token", &previewEntry{
		kind: "issue", spec: issues.IssueSpec{Key: "large", Title: "Large", Body: strings.Repeat("x", len(before)*4)},
	}); err == nil {
		t.Fatal("preview size pressure was accepted")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected write replaced confirmed preview state")
	}
	_, recoveredURL, _, _, err := NewService(&project.Config{}, dataDir, AIConfig{}).beginConfirm("owner-token", token, time.Hour)
	if err != nil || recoveredURL != resultURL {
		t.Fatalf("confirmed result = %q err=%v", recoveredURL, err)
	}
}

func TestFitPreviewStateProtectsInsertedPreview(t *testing.T) {
	protected := &persistedPreview{
		Kind: "issue", CreatedAt: "2026-01-01T00:00:00Z", Status: previewStatusReady,
		Issue: &issues.IssueSpec{Key: "protected", Body: strings.Repeat("p", 600)},
	}
	existing := &persistedPreview{
		Kind: "issue", CreatedAt: "2026-01-01T00:00:01Z", Status: previewStatusReady,
		Issue: &issues.IssueSpec{Key: "existing", Body: strings.Repeat("e", 600)},
	}
	state := &previewState{
		Version: previewStateVersion,
		Previews: map[string]*persistedPreview{
			"protected": protected,
			"existing":  existing,
		},
	}
	single := &previewState{Version: previewStateVersion, Previews: map[string]*persistedPreview{"protected": protected}}
	encoded, err := json.MarshalIndent(single, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fitPreviewState(state, len(encoded)+16, "protected"); err != nil {
		t.Fatal(err)
	}
	if state.Previews["protected"] == nil {
		t.Fatal("protected preview was evicted")
	}
	if state.Previews["existing"] != nil {
		t.Fatal("existing preview was retained instead of the protected preview")
	}
}

func TestFitPreviewStateOrdersTimestampsChronologically(t *testing.T) {
	newState := func() *previewState {
		return &previewState{
			Version: previewStateVersion,
			Previews: map[string]*persistedPreview{
				"oldest": {
					Kind: "issue", CreatedAt: "2026-01-01T00:00:00Z", Status: previewStatusReady,
					Issue: &issues.IssueSpec{Key: "oldest", Body: strings.Repeat("o", 600)},
				},
				"newer": {
					Kind: "issue", CreatedAt: "2026-01-01T00:00:00.1Z", Status: previewStatusReady,
					Issue: &issues.IssueSpec{Key: "newer", Body: strings.Repeat("n", 600)},
				},
			},
		}
	}

	t.Run("count", func(t *testing.T) {
		state := newState()
		for i := 0; i < maxPersistedPreviews-1; i++ {
			key := fmt.Sprintf("later-%03d", i)
			state.Previews[key] = &persistedPreview{
				Kind: "issue", CreatedAt: "2026-01-01T00:00:01Z", Status: previewStatusReady,
				Issue: &issues.IssueSpec{Key: key},
			}
		}
		if err := fitPreviewState(state, maxPreviewStateBytes, ""); err != nil {
			t.Fatal(err)
		}
		if state.Previews["oldest"] != nil || state.Previews["newer"] == nil {
			t.Fatalf("remaining timestamps = oldest:%t newer:%t", state.Previews["oldest"] != nil, state.Previews["newer"] != nil)
		}
	})

	t.Run("size", func(t *testing.T) {
		state := newState()
		oldestOnly := &previewState{Version: previewStateVersion, Previews: map[string]*persistedPreview{"oldest": state.Previews["oldest"]}}
		newerOnly := &previewState{Version: previewStateVersion, Previews: map[string]*persistedPreview{"newer": state.Previews["newer"]}}
		oldestBytes, err := json.MarshalIndent(oldestOnly, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		newerBytes, err := json.MarshalIndent(newerOnly, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		maxBytes := max(len(oldestBytes), len(newerBytes)) + 16
		if err := fitPreviewState(state, maxBytes, ""); err != nil {
			t.Fatal(err)
		}
		if state.Previews["oldest"] != nil || state.Previews["newer"] == nil {
			t.Fatalf("remaining timestamps = oldest:%t newer:%t", state.Previews["oldest"] != nil, state.Previews["newer"] != nil)
		}
	})
}
