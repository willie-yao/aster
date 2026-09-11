package fetcher

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type recordingScheduledIssueManager struct {
	recoverCalls int
	saveCalls    int
	recoverErr   error
	saveErr      error
	activeKeys   []string
}

func (m *recordingScheduledIssueManager) Recover(_ context.Context, activeKeys []string) (issues.Stats, error) {
	m.recoverCalls++
	m.activeKeys = activeKeys
	return issues.Stats{}, m.recoverErr
}

func (m *recordingScheduledIssueManager) SaveState() error {
	m.saveCalls++
	return m.saveErr
}

func automaticIssueTestConfig() *project.Config {
	return &project.Config{
		Name: "Test", Branding: project.Branding{
			SiteURL:    "https://dashboard.example.test",
			SourceRepo: project.SourceRepo{Owner: "example", Name: "repo"},
		},
		Issues: &project.Issues{Enabled: true},
	}
}

func TestProcessIssuesSkipsWithoutStaticToken(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "")
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager {
		t.Fatal("issue manager was created without ISSUE_TOKEN")
		return nil
	}
	t.Cleanup(func() { newBatchIssueManager = oldFactory })

	if err := processIssues(t.Context(), automaticIssueTestConfig(), models.FlakinessReport{}, nil, t.TempDir()); err != nil {
		t.Fatalf("processIssues error = %v", err)
	}
}

func TestProcessIssuesRunsWithStaticToken(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{}
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager { return manager }
	t.Cleanup(func() { newBatchIssueManager = oldFactory })

	if err := processIssues(t.Context(), automaticIssueTestConfig(), models.FlakinessReport{}, nil, t.TempDir()); err != nil {
		t.Fatalf("processIssues error = %v", err)
	}
	if manager.recoverCalls != 1 || manager.saveCalls != 1 {
		t.Fatalf("issue manager calls = recover %d save %d", manager.recoverCalls, manager.saveCalls)
	}
}

func TestRunSideEffectsReportsSanitizedAutomaticIssueFailure(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{recoverErr: errors.New("private provider response with token=secret")}
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager { return manager }
	t.Cleanup(func() { newBatchIssueManager = oldFactory })

	dataDir := t.TempDir()
	backend, err := storage.NewLocalBackend(t.TempDir(), "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	tracker := fetchprogress.New(dataDir, "sha-test")
	tracker.StartPass(fetchprogress.PassOneShot)
	tracker.StartPhase(fetchprogress.PhaseSideEffects)
	p := &pipeline{
		cfg: automaticIssueTestConfig(), opts: Options{OutDir: dataDir}, backend: backend,
		client: &http.Client{}, progress: tracker,
	}

	err = p.runSideEffects(t.Context(), &refreshResult{})
	if err == nil || !strings.Contains(err.Error(), "private provider response") {
		t.Fatalf("runSideEffects error = %v", err)
	}
	status := tracker.Snapshot()
	if status.FollowUp == nil || status.FollowUp.AutomaticIssues == nil {
		t.Fatalf("follow-up status = %+v", status.FollowUp)
	}
	issueStatus := status.FollowUp.AutomaticIssues
	if issueStatus.State != fetchprogress.FollowUpFailed || issueStatus.Code != fetchprogress.FollowUpFailureAutomaticIssues ||
		issueStatus.Summary != "Issue recovery reconciliation failed" || strings.Contains(issueStatus.Summary, "secret") {
		t.Fatalf("automatic issue status = %+v", issueStatus)
	}
}

func TestProcessIssuesKeepsCurrentAnalysisOnlyPatternOpen(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{}
	var gotOptions issues.Options
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(_ *issues.Client, _, _ string, opts issues.Options) scheduledIssueManager {
		gotOptions = opts
		return manager
	}
	t.Cleanup(func() { newBatchIssueManager = oldFactory })

	pattern := models.PatternAnalysis{
		JobID: "job", Systemic: true, Recurrence: models.PatternRecurrenceSharedCause,
		SharedRootCause: "active cause", SharedBuilds: []string{"2", "1"}, Summary: "still active",
	}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}}
	details := []models.JobDetail{{
		JobID: "job", PatternRefresh: &models.PatternRefreshStatus{State: models.PatternRefreshCurrent},
	}}
	if err := processIssues(t.Context(), automaticIssueTestConfig(), report, details, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !gotOptions.KeepOpenKeys[issues.KeyPrefixPattern+"job"] {
		t.Fatalf("keep-open keys=%v", gotOptions.KeepOpenKeys)
	}
}
