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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
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
	models.AssignPatternIdentity(&pa)
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

func TestPreviewIssueRejectsUnsafeGeneratedSpec(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	pa.SharedRootCause = "The user wants me to expose the plan. I need to include the reasoning. Let me draft it."
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	if _, err := s.PreviewIssue(context.Background(), pa.ID, "tok", ""); !errors.Is(err, ErrPreviewRejected) {
		t.Fatalf("unsafe generated issue error = %v", err)
	}
}

func TestConfirmRejectsUnsafePersistedPreview(t *testing.T) {
	dataDir := t.TempDir()
	key := issues.KeyPrefixPattern + "periodic-x"
	unsafe := issues.IssueSpec{
		Key: key, Title: "Unsafe",
		Body: "The user wants me to expose the plan. I need to include the reasoning. Let me draft it.\n\n" + issues.MarkerFor(key),
	}
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	token, err := s.stash("owner-token", &previewEntry{kind: "issue", spec: unsafe})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(context.Background(), token, "owner-token"); !errors.Is(err, ErrPreviewRejected) {
		t.Fatalf("unsafe persisted preview error = %v", err)
	}
	if _, _, _, _, err := s.beginConfirm("owner-token", token, time.Hour); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("unsafe preview was not discarded: %v", err)
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
	usage, usageErr := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10})
	if usageErr != nil {
		t.Fatal(usageErr)
	}
	service := NewService(cfg, t.TempDir(), AIConfig{
		API: "chat_completions", Endpoint: "https://ai.example/v1/chat/completions", Model: "model", Token: "token", UsageRecorder: usage,
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
	snapshot := usage.Snapshot()
	if len(snapshot.RecentOperations) != 1 || snapshot.RecentOperations[0].Feature != aiusage.FeatureFixPreview || snapshot.RecentOperations[0].Outcome != aiusage.OutcomeError {
		t.Fatalf("usage = %+v", snapshot)
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
	firstToken, err := service.stash("owner-token", entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.stash("other-owner", entry); err != nil {
		t.Fatalf("ready replacement was blocked: %v", err)
	}
	if _, _, _, _, err := service.beginConfirm("owner-token", firstToken, time.Hour); err != nil {
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

func TestPreviewConfirmationRejectsTargetRepositoryDrift(t *testing.T) {
	cfg := &project.Config{
		Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "new", Name: "issues"}},
		AI:     &project.AI{FixPRs: &project.FixPRs{Repo: &project.SourceRepo{Owner: "new", Name: "fixes"}}},
	}
	service := NewService(cfg, t.TempDir(), AIConfig{})
	entries := []*previewEntry{
		{kind: "issue", targetRepo: "old/issues", spec: issues.IssueSpec{Key: "issue-key"}},
		{kind: gfKind, targetRepo: "old/fixes", fix: fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{Key: "fix-key"})},
	}
	for _, entry := range entries {
		if _, err := service.confirmEntry(t.Context(), entry, "token"); !errors.Is(err, ErrPreviewTargetChanged) {
			t.Fatalf("confirm %s error = %v", entry.kind, err)
		}
		if _, _, err := service.reconcileEntry(t.Context(), entry, "token"); !errors.Is(err, ErrPreviewTargetChanged) {
			t.Fatalf("reconcile %s error = %v", entry.kind, err)
		}
	}
}

func TestTargetDriftRetiresPreviewForReplacement(t *testing.T) {
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "new", Name: "issues"}}}
	service := NewService(cfg, t.TempDir(), AIConfig{})
	key := "same-action"
	old := &previewEntry{kind: "issue", targetRepo: "old/issues", spec: issues.IssueSpec{
		Key: key, Title: "Valid issue", Body: "## Summary\nValid body\n\n" + issues.MarkerFor(key),
	}}
	token, err := service.stash("owner-token", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(t.Context(), token, "owner-token"); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("confirm error = %v", err)
	}
	replacement := &previewEntry{kind: "issue", targetRepo: "new/issues", spec: issues.IssueSpec{
		Key: key, Title: "Valid issue", Body: "## Summary\nValid body\n\n" + issues.MarkerFor(key),
	}}
	if _, err := service.stash("owner-token", replacement); err != nil {
		t.Fatalf("replacement preview was blocked: %v", err)
	}
}

func TestStalePatternIsNotActionable(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout", SharedBuilds: []string{"100"}}
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{
		JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa},
		PatternRefresh: &models.PatternRefreshStatus{State: models.PatternRefreshRetained, EvidenceAvailable: true},
	})
	if err := (&resolve.State{Resolved: map[string]resolve.Entry{pa.ID: {Watermark: "100"}}}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "alice", ""); err == nil || !strings.Contains(err.Error(), "stale pattern evidence") {
		t.Fatalf("Resolve error = %v", err)
	}
	if err := s.Unresolve(pa.ID); err == nil || !strings.Contains(err.Error(), "stale pattern evidence") {
		t.Fatalf("Unresolve error = %v", err)
	}
}

func TestLegacyReadyPreviewWithoutFailureIDIsRejected(t *testing.T) {
	dataDir := t.TempDir()
	store := newPreviewStore(dataDir)
	state := &previewState{Version: 1, Previews: map[string]*persistedPreview{
		"legacy":  {Owner: tokenHash("owner"), Kind: "issue", TargetRepo: "owner/repo", Status: previewStatusReady, Issue: &issues.IssueSpec{Key: "pattern::job"}},
		"unknown": {Owner: tokenHash("owner"), Kind: "issue", TargetRepo: "owner/repo", Status: previewStatusUnknown, Issue: &issues.IssueSpec{Key: "pattern::job"}},
	}}
	if err := statefile.WriteJSONDurable(store.path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != previewStateVersion || loaded.Previews["legacy"] != nil || loaded.Previews["unknown"] == nil {
		t.Fatalf("migrated state = %+v", loaded)
	}
}

func analyzedBuildDetail(withSource bool) models.JobDetail {
	analysis := &models.AIAnalysis{
		GeneratedAt: "2026-07-30T12:00:00Z", Mode: ai.AgenticMode, CritiquePassed: true, CritiqueVersion: ai.CurrentCritiqueVersion(),
		RootCause: "K8sVersionNotSupported rejected Kubernetes 1.33.2 because AKS requires Long-Term Support.",
		Severity:  "High", SuggestedFix: "Update the repository version selection or enable AKS LTS.",
		RelevantFiles: []string{"templates/aks.yaml"}, FileLinks: map[string]string{},
	}
	if withSource {
		analysis.FileLinks["templates/aks.yaml"] = "https://github.com/example/repo/blob/sha/templates/aks.yaml"
	}
	return models.JobDetail{Name: "periodic-aks", JobID: "periodic-aks", JobType: models.JobTypePeriodic, Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "123", JobName: "periodic-aks", ProwURL: "https://prow.example/123", BuildLogURL: "https://gcs.example/123/build-log.txt"},
		TestCases: []models.TestCase{{Name: "Prow job execution", SuiteName: "Prow", ClassName: "job", Source: models.TestCaseSourceBuild, Status: "failed", AISummary: &models.AISummary{Summary: "AKS bootstrap creation failed."}, AIAnalysis: analysis}},
	}}}
}

func TestBuildIssuePreviewUsesSingleRunLanguage(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}, Branding: project.Branding{SiteURL: "https://dashboard.example"}}
	service := NewService(cfg, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	preview, err := service.PreviewIssue(t.Context(), id, "token", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"K8sVersionNotSupported", "1.33.2", "Long-Term Support", "build-log.txt", "one analyzed build failure"} {
		if !strings.Contains(preview.Body, want) {
			t.Fatalf("issue body missing %q: %s", want, preview.Body)
		}
	}
	if strings.Contains(strings.ToLower(preview.Body), "recurring pattern") || strings.Contains(strings.ToLower(preview.Body), "systemic") {
		t.Fatalf("issue body claimed recurrence: %s", preview.Body)
	}
}

func TestBuildFixPreviewRejectsMissingRepositoryEvidence(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}}
	service := NewService(cfg, dataDir, AIConfig{})
	_, err := service.PreviewFix(t.Context(), BuildFailureID(detail.JobID, "123"), "token", "")
	if !errors.Is(err, ErrPreviewRejected) || !strings.Contains(err.Error(), "verified local path") {
		t.Fatalf("fix preview error = %v", err)
	}
	if _, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "token", ""); err == nil {
		// No issue repo is configured. This verifies the fix refusal did not mutate the subject.
		t.Fatal("issue preview unexpectedly succeeded without a target repo")
	}
}

func TestBuildSubjectHashChangesWithPublishedAnalysis(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(true)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	subject, err := service.resolveSubject(id)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := subject.ContentHash
	detail.Runs[0].TestCases[0].AIAnalysis.SuggestedFix = "Choose a supported version."
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	if err := service.validateSubjectSnapshot(id, oldHash); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("changed analysis validation = %v", err)
	}
}

func TestBuildFixSnapshotRejectsRemovedSourceEvidenceButIssueRemainsValid(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(true)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	subject, err := service.resolveSubject(id)
	if err != nil {
		t.Fatal(err)
	}
	detail.Runs[0].TestCases[0].AIAnalysis.FileLinks = nil
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	if err := service.validateSubjectSnapshot(id, subject.ContentHash, gfKind); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("old fix snapshot validation = %v", err)
	}
	current, err := service.resolveSubject(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.validateSubjectSnapshot(id, current.ContentHash, gfKind); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("current fix snapshot validation = %v", err)
	}
	if err := service.validateSubjectSnapshot(id, current.ContentHash, "create-issue"); err != nil {
		t.Fatalf("current issue snapshot validation = %v", err)
	}
}

func TestBuildPreviewConfirmUsesTypedSubjectGuard(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "old", Name: "issues"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	preview, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "owner-token", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Issues.Repo = &project.SourceRepo{Owner: "new", Name: "issues"}
	if _, err := service.Confirm(t.Context(), preview.Token, "owner-token"); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("build confirmation error = %v", err)
	}
}

func TestBuildSourceFilesMustMatchFixRepository(t *testing.T) {
	detail := analyzedBuildDetail(true)
	failure := detail.Runs[0].TestCases[0]
	subject := &BuildActionSubject{JobID: detail.JobID, JobName: detail.Name, Build: detail.Runs[0].BuildInfo, Failure: failure, RelevantFiles: failure.AIAnalysis.RelevantFiles}
	if got := verifiedBuildSourceFiles(subject, "example", "repo"); len(got) != 1 || got[0] != "templates/aks.yaml" {
		t.Fatalf("matching source files = %v", got)
	}
	if got := verifiedBuildSourceFiles(subject, "other", "repo"); len(got) != 0 {
		t.Fatalf("cross-repository source files = %v", got)
	}
}

func TestBuildActionsRejectOldCritiqueContract(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(true)
	detail.Runs[0].TestCases[0].AIAnalysis.CritiqueVersion--
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	if _, err := service.resolveSubject(BuildFailureID(detail.JobID, "123")); err == nil || !strings.Contains(err.Error(), "quality gates") {
		t.Fatalf("old critique analysis error = %v", err)
	}
}

type fakeIssuePreviewManager struct {
	specs        []issues.IssueSpec
	forgot       []string
	saved        bool
	url          string
	findURL      string
	saveErr      error
	reconcileErr error
	started      chan struct{}
	release      chan struct{}
}

func (f *fakeIssuePreviewManager) Reconcile(_ context.Context, specs []issues.IssueSpec) (issues.Stats, error) {
	f.specs = append(f.specs, specs...)
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	return issues.Stats{Created: 1}, f.reconcileErr
}
func (f *fakeIssuePreviewManager) TrackedURL(string) (string, bool) { return f.url, f.url != "" }
func (f *fakeIssuePreviewManager) FindOpen(context.Context, string) (string, bool, error) {
	return f.url, f.url != "", nil
}
func (f *fakeIssuePreviewManager) FindAny(context.Context, string) (string, bool, error) {
	return f.findURL, f.findURL != "", nil
}
func (f *fakeIssuePreviewManager) Forget(key string) { f.forgot = append(f.forgot, key) }
func (f *fakeIssuePreviewManager) SaveState() error  { f.saved = true; return f.saveErr }

func TestBuildIssuePreviewToConfirmWritesReviewedDraft(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	manager := &fakeIssuePreviewManager{url: "https://github.com/o/r/issues/7"}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }

	preview, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "owner-token", "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := service.Confirm(t.Context(), preview.Token, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	if url != manager.url || len(manager.specs) != 1 || !manager.saved || len(manager.forgot) != 1 {
		t.Fatalf("confirmation url=%q specs=%d saved=%t forgot=%v", url, len(manager.specs), manager.saved, manager.forgot)
	}
	if manager.specs[0].Body != preview.Body {
		t.Fatal("confirmation did not use the reviewed issue body")
	}
}

func TestBuildIssueConfirmationAdoptsClosedMarkerMatch(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	manager := &fakeIssuePreviewManager{findURL: "https://github.com/o/r/issues/closed"}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	preview, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "owner-token", "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := service.Confirm(t.Context(), preview.Token, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	if url != manager.findURL || len(manager.specs) != 0 {
		t.Fatalf("closed issue adoption url=%q specs=%d", url, len(manager.specs))
	}
}

func TestBuildIssueCleanupFailureStillCommitsConfirmation(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	manager := &fakeIssuePreviewManager{url: "https://github.com/o/r/issues/8", saveErr: errors.New("cleanup failed")}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	preview, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "owner-token", "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := service.Confirm(t.Context(), preview.Token, "owner-token")
	if err != nil || url != manager.url {
		t.Fatalf("confirmation url=%q err=%v", url, err)
	}
}

func TestBuildSourceFilesUseAllAuthoritativeLinks(t *testing.T) {
	detail := analyzedBuildDetail(false)
	failure := detail.Runs[0].TestCases[0]
	failure.AIAnalysis.RelevantFiles = nil
	failure.AIAnalysis.FileLinks = map[string]string{
		"config/versions.yaml": "https://github.com/example/repo/blob/sha/config/versions.yaml",
	}
	subject := &BuildActionSubject{JobID: detail.JobID, JobName: detail.Name, Build: detail.Runs[0].BuildInfo, Failure: failure}
	if got := verifiedBuildSourceFiles(subject, "example", "repo"); len(got) != 1 || got[0] != "config/versions.yaml" {
		t.Fatalf("authoritative source files = %v", got)
	}
}

func TestAsyncBuildIssueLostResponseReconcilesWithoutSecondWrite(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	subject, err := service.resolveSubject(id)
	if err != nil {
		t.Fatal(err)
	}
	spec, targetRepo, err := service.buildIssueSpecForBuild(subject.Build, id)
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeIssuePreviewManager{reconcileErr: fmt.Errorf("%w: connection reset after create", issues.ErrWriteOutcomeUnknown)}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	now := time.Now().UTC()
	service.requests.Requests["request"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "request", FailureID: id, PatternHash: subject.ContentHash, Kind: "create-issue", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
	}, Issue: &spec, TargetRepo: targetRepo, VerificationVersion: sourceVerificationVersion}
	if _, err := service.ConfirmRequest(t.Context(), "request", "alice", "token"); !errors.Is(err, ErrPreviewOutcomeUnknown) {
		t.Fatalf("first confirmation error = %v", err)
	}
	if service.requests.Requests["request"].Status != RequestUnknown || len(manager.specs) != 1 {
		t.Fatalf("unknown request = %+v writes=%d", service.requests.Requests["request"].ActionRequestView, len(manager.specs))
	}
	manager.reconcileErr = nil
	manager.findURL = "https://github.com/o/r/issues/9"
	url, err := service.ConfirmRequest(t.Context(), "request", "alice", "token")
	if err != nil || url != manager.findURL {
		t.Fatalf("reconcile url=%q err=%v", url, err)
	}
	if len(manager.specs) != 1 || service.requests.Requests["request"].Status != RequestConfirmed {
		t.Fatalf("retry wrote again: writes=%d status=%s", len(manager.specs), service.requests.Requests["request"].Status)
	}
}

func TestAsyncBuildIssuePrewriteFailureRemainsRetryable(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	subject, _ := service.resolveSubject(id)
	spec, targetRepo, _ := service.buildIssueSpecForBuild(subject.Build, id)
	manager := &fakeIssuePreviewManager{reconcileErr: errors.New("search unavailable")}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	now := time.Now().UTC()
	service.requests.Requests["request-prewrite"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "request-prewrite", FailureID: id, PatternHash: subject.ContentHash, Kind: "create-issue", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
	}, Issue: &spec, TargetRepo: targetRepo, VerificationVersion: sourceVerificationVersion}
	if _, err := service.ConfirmRequest(t.Context(), "request-prewrite", "alice", "token"); err == nil || errors.Is(err, ErrPreviewOutcomeUnknown) {
		t.Fatalf("prewrite error = %v", err)
	}
	if service.requests.Requests["request-prewrite"].Status != RequestReady {
		t.Fatalf("prewrite status = %s", service.requests.Requests["request-prewrite"].Status)
	}
}

func TestAsyncConfirmationPersistsUnknownBeforeExternalWrite(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	id := BuildFailureID(detail.JobID, "123")
	subject, _ := service.resolveSubject(id)
	spec, targetRepo, _ := service.buildIssueSpecForBuild(subject.Build, id)
	manager := &fakeIssuePreviewManager{started: make(chan struct{}), release: make(chan struct{}), reconcileErr: errors.New("search unavailable")}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	now := time.Now().UTC()
	service.requests.Requests["request-crash"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "request-crash", FailureID: id, PatternHash: subject.ContentHash, Kind: "create-issue", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
	}, Issue: &spec, TargetRepo: targetRepo, VerificationVersion: sourceVerificationVersion}
	done := make(chan error, 1)
	go func() {
		_, err := service.ConfirmRequest(context.Background(), "request-crash", "alice", "token")
		done <- err
	}()
	<-manager.started
	service.rmu.Lock()
	status := service.requests.Requests["request-crash"].Status
	service.rmu.Unlock()
	if status != RequestUnknown {
		t.Fatalf("in-flight status = %s", status)
	}
	close(manager.release)
	if err := <-done; err == nil {
		t.Fatal("confirmation unexpectedly succeeded")
	}
	if service.requests.Requests["request-crash"].Status != RequestReady {
		t.Fatalf("definite failure status = %s", service.requests.Requests["request-crash"].Status)
	}
}

func TestDirectUnknownPreviewReconcilesAfterSubjectLeavesWindow(t *testing.T) {
	dataDir := t.TempDir()
	detail := analyzedBuildDetail(false)
	jobPath := filepath.Join(dataDir, "jobs", models.JobDataFilename(detail.JobID))
	writeJobDetail(t, dataDir, models.JobDataFilename(detail.JobID), detail)
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{})
	manager := &fakeIssuePreviewManager{findURL: "https://github.com/o/r/issues/10"}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	preview, err := service.PreviewIssue(t.Context(), BuildFailureID(detail.JobID, "123"), "owner-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.previewStore.update(func(state *previewState, _ time.Time) (bool, error) {
		record := state.Previews[tokenHash(preview.Token)]
		record.Status = previewStatusUnknown
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jobPath); err != nil {
		t.Fatal(err)
	}
	url, err := service.Confirm(t.Context(), preview.Token, "owner-token")
	if err != nil || url != manager.findURL {
		t.Fatalf("reconcile url=%q err=%v", url, err)
	}
}

func TestPatternIssueAmbiguousWriteUsesOpenOnlyReconciliation(t *testing.T) {
	service, pattern := requestTestService(t)
	manager := &fakeIssuePreviewManager{reconcileErr: fmt.Errorf("%w: lost response", issues.ErrWriteOutcomeUnknown)}
	service.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if _, err := service.ConfirmRequest(t.Context(), ready.ID, "alice", "token"); !errors.Is(err, ErrPreviewOutcomeUnknown) {
		t.Fatalf("ambiguous error = %v", err)
	}
	manager.reconcileErr = nil
	manager.findURL = "https://github.com/o/r/issues/closed"
	manager.url = ""
	if _, err := service.ConfirmRequest(t.Context(), ready.ID, "alice", "token"); !errors.Is(err, ErrPreviewOutcomeUnknown) {
		t.Fatalf("closed issue was incorrectly adopted: %v", err)
	}
	manager.url = "https://github.com/o/r/issues/open"
	url, err := service.ConfirmRequest(t.Context(), ready.ID, "alice", "token")
	if err != nil || url != manager.url {
		t.Fatalf("open reconciliation url=%q err=%v", url, err)
	}
}

type fakeActionSourceReader map[string]string

func (f fakeActionSourceReader) ReadSourceArchive(context.Context) (actionverify.Archive, error) {
	archive := actionverify.Archive{Paths: map[string]bool{}, GoFiles: map[string]string{}}
	for path, content := range f {
		archive.Paths[path] = true
		if strings.HasSuffix(path, ".go") {
			archive.GoFiles[path] = content
		}
	}
	return archive, nil
}

func TestSourcePreflightBlocksAlreadyPresentRemediation(t *testing.T) {
	dataDir := t.TempDir()
	const revision = "0123456789abcdef0123456789abcdef01234567"
	pattern := models.PatternAnalysis{
		JobID: "periodic-capz", Systemic: true,
		SuggestedFix:  "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"sigs.k8s.io/cluster-api/test@v1.13.3/framework/x.go"},
		FileLinks: map[string]string{
			"internal/asomigration/labels.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + revision + "/internal/asomigration/labels.go",
			"test/e2e/capi_test.go":           "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + revision + "/test/e2e/capi_test.go",
		},
		SourceRef: revision,
	}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dataDir, "periodic-capz.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	cfg := &project.Config{
		Branding: project.Branding{SourceRepo: project.SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}},
		Issues:   &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}},
		AI:       &project.AI{SourceRepo: &project.SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}, FixPRs: &project.FixPRs{Enabled: true, Repo: &project.SourceRepo{Owner: "o", Name: "r"}}},
	}
	service := NewService(cfg, dataDir, AIConfig{})
	reader := fakeActionSourceReader{
		"go.mod":                          "module example\n",
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nimport \"example/internal/asomigration\"\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	}
	service.sourceVerifier = func(ctx context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		return actionverify.Verify(ctx, reader, input)
	}
	if _, err := service.PreviewIssue(context.Background(), pattern.ID, "token", ""); !errors.Is(err, ErrRemediationAlreadyPresent) {
		t.Fatalf("issue preview error = %v", err)
	}
	if _, err := service.PreviewFix(context.Background(), pattern.ID, "token", ""); !errors.Is(err, ErrRemediationAlreadyPresent) {
		t.Fatalf("fix preview error = %v", err)
	}
	request, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	view := waitRequest(t, service, request.ID, "alice", RequestFailed)
	if view.Preview != nil || !strings.Contains(view.Error, "already") {
		t.Fatalf("request remained actionable: %+v", view)
	}
}

func TestVerifiedSourceFilesRequirePinnedRevision(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	links := map[string]string{
		"current": "https://github.com/example/repo/blob/" + revision + "/current.go",
		"stale":   "https://github.com/example/repo/blob/fedcba9876543210fedcba9876543210fedcba98/stale.go",
	}
	files := verifiedSourceFiles(links, "example", "repo", revision)
	if len(files) != 1 || files[0] != "current.go" {
		t.Fatalf("verified files = %v", files)
	}
}

func TestContextSourceVerificationDropsPatternPathsFromAnotherRevision(t *testing.T) {
	const oldRevision = "0123456789abcdef0123456789abcdef01234567"
	const newRevision = "fedcba9876543210fedcba9876543210fedcba98"
	pattern := systemicPattern()
	pattern.SuggestedFix = "Implement ExistingFix."
	pattern.SourceRef = "example/repo@" + oldRevision
	pattern.RelevantFiles = []string{"old.go"}
	pattern.FileLinks = map[string]string{
		"old.go": "https://github.com/example/repo/blob/" + oldRevision + "/old.go",
	}
	cfg := &project.Config{AI: &project.AI{
		SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"},
		FixPRs:     &project.FixPRs{Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"}},
	}}
	service := NewService(cfg, t.TempDir(), AIConfig{})
	var got actionverify.Input
	service.sourceVerifier = func(_ context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		got = input
		return actionverify.Result{State: actionverify.StateUnresolved}, nil
	}
	_, _, _ = service.generateFixPreviewForPattern(t.Context(), pattern, "token", "", &fixpr.GenerationContext{
		AssistantAnswer:   "selected answer",
		ArtifactCitations: []fixpr.Evidence{{Path: "build-log.txt", Quote: "failure"}},
		Source: &fixpr.SourceContext{
			Revision:  newRevision,
			Citations: []fixpr.Evidence{{Path: "new.go", LineStart: 1, LineEnd: 1, Quote: "package source"}},
		},
	})
	if len(got.RelevantFiles) != 1 || got.RelevantFiles[0] != "new.go" {
		t.Fatalf("verification files = %v", got.RelevantFiles)
	}
}

func TestBuildSourceVerificationUsesOnlyPinnedLinks(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	detail := analyzedBuildDetail(false)
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:" + revision}
	failure := detail.Runs[0].TestCases[0]
	failure.AIAnalysis.RelevantFiles = []string{"sigs.k8s.io/cluster-api/test@v1.13.3/framework/x.go"}
	failure.AIAnalysis.FileLinks = map[string]string{
		"templates/aks.yaml": "https://github.com/example/repo/blob/" + revision + "/templates/aks.yaml",
	}
	subject := &ActionSubject{Kind: actionSubjectBuild, Build: &BuildActionSubject{
		JobID: detail.JobID, JobName: detail.Name, Build: detail.Runs[0].BuildInfo,
		Failure: failure, RelevantFiles: failure.AIAnalysis.RelevantFiles,
	}}
	service := NewService(&project.Config{AI: &project.AI{
		SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"},
	}}, t.TempDir(), AIConfig{})
	var got actionverify.Input
	service.sourceVerifier = func(_ context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		got = input
		return actionverify.Result{State: actionverify.StateUnresolved}, nil
	}
	if err := service.verifyRemediation(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	if len(got.RelevantFiles) != 1 || got.RelevantFiles[0] != "templates/aks.yaml" {
		t.Fatalf("verification files = %v", got.RelevantFiles)
	}
}

func TestSourcePreflightChecksInstruction(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	pattern := models.PatternAnalysis{
		SuggestedFix: "Implement MissingHelper.", SourceRef: revision,
		FileLinks: map[string]string{"main.go": "https://github.com/example/repo/blob/" + revision + "/main.go"},
	}
	subject := &ActionSubject{Kind: actionSubjectPattern, Pattern: &pattern}
	service := NewService(&project.Config{AI: &project.AI{
		SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"},
	}}, t.TempDir(), AIConfig{})
	reader := fakeActionSourceReader{
		"go.mod":  "module example\n",
		"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
	}
	calls := 0
	service.sourceVerifier = func(ctx context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		calls++
		return actionverify.Verify(ctx, reader, input)
	}
	if err := service.verifyOptionalRemediation(t.Context(), subject, "instead call `ExistingFix`"); !errors.Is(err, ErrRemediationAlreadyPresent) {
		t.Fatalf("instruction preflight error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("verification calls = %d, want 1", calls)
	}
	if err := service.verifyOptionalRemediation(t.Context(), subject, "make the title concise"); err != nil || calls != 1 {
		t.Fatalf("non-remediation instruction error = %v calls = %d", err, calls)
	}
}

func TestIssuePreflightChecksFinalDraft(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	dataDir := t.TempDir()
	pattern := models.PatternAnalysis{
		JobID: "periodic-x", Systemic: true, SuggestedFix: "Implement MissingHelper.", SourceRef: revision,
		FileLinks: map[string]string{"main.go": "https://github.com/example/repo/blob/" + revision + "/main.go"},
	}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	service := NewService(&project.Config{
		Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "example", Name: "issues"}},
		AI:     &project.AI{SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"}},
	}, dataDir, AIConfig{})
	base, targetRepo, err := service.buildIssueSpecForPattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	base.Body = "Instead call `ExistingFix`."
	var proposals []string
	service.sourceVerifier = func(_ context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		proposals = append(proposals, input.Proposal)
		return actionverify.Result{State: actionverify.StateUnresolved}, nil
	}
	if _, _, err := service.generateIssuePreview(
		t.Context(), pattern.ID, "token", "", &base, targetRepo, pattern.ContentHash,
	); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 2 || proposals[0] != pattern.SuggestedFix || !strings.Contains(proposals[1], "ExistingFix") {
		t.Fatalf("verified proposals = %v", proposals)
	}
}

func TestSourcePreflightUsesBrandingRepositoryFallback(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	pattern := models.PatternAnalysis{
		SuggestedFix: "Implement MissingHelper.", SourceRef: revision,
		FileLinks: map[string]string{"main.go": "https://github.com/example/repo/blob/" + revision + "/main.go"},
	}
	subject := &ActionSubject{Kind: actionSubjectPattern, Pattern: &pattern}
	service := NewService(&project.Config{Branding: project.Branding{
		SourceRepo: project.SourceRepo{Owner: "example", Name: "repo"},
	}}, t.TempDir(), AIConfig{})
	called := false
	service.sourceVerifier = func(_ context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		called = true
		if len(input.RelevantFiles) != 1 || input.RelevantFiles[0] != "main.go" {
			t.Fatalf("verification files = %v", input.RelevantFiles)
		}
		return actionverify.Result{State: actionverify.StateUnresolved}, nil
	}
	if err := service.verifyRemediation(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("branding source repository did not enable preflight")
	}
}

func TestPreviewStoreInvalidatesLegacyVerificationVersion(t *testing.T) {
	store := newPreviewStore(t.TempDir())
	state := previewState{Version: 2, Previews: map[string]*persistedPreview{
		"legacy": {
			Owner: "owner", Kind: "issue", FailureID: "failure", PatternHash: "hash",
			TargetRepo: "example/issues", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: previewStatusReady,
			Issue: &issues.IssueSpec{Key: "pattern::legacy", Title: "Legacy", Body: "## Summary\nBody\n\n" + issues.MarkerFor("pattern::legacy")},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(store.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != previewStateVersion || len(loaded.Previews) != 0 {
		t.Fatalf("loaded legacy previews = %+v", loaded)
	}
}

func TestPreviewIssueRecordsUsageOperation(t *testing.T) {
	dataDir := t.TempDir()
	pattern := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pattern}})
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}
	service := NewService(cfg, dataDir, AIConfig{UsageRecorder: usage})
	if _, err := service.PreviewIssue(t.Context(), pattern.ID, "token", ""); err != nil {
		t.Fatal(err)
	}
	snapshot := usage.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.RecentOperations[0].Feature != aiusage.FeatureIssueDraft {
		t.Fatalf("usage = %+v", snapshot)
	}
}
