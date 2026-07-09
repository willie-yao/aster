package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
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

	s := NewService(cfg, dataDir, AIConfig{}) // empty AI config
	_, err := s.PreviewFix(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want AI-not-configured error, got %v", err)
	}
}

func TestPreviewCache_TokenOwnershipAndConsumption(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	tok := s.stash("owner-token", &previewEntry{kind: "issue"})

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
	tok := s.stash("owner-token", &previewEntry{kind: "issue"})
	// Age the entry past the TTL.
	s.pmu.Lock()
	s.previews[tok].createdAt = time.Now().Add(-previewTTL - time.Minute)
	s.pmu.Unlock()
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
