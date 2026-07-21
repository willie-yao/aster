package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

func TestRunFinalizedSideEffectsLoadsFinalizedOutput(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	storageDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + storageDir + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: repo
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "jobs", models.JobDataFilename("job")), models.JobDetail{JobID: "job", Name: "Job"}); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	if err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFinalizedDataRejectsMalformedJob(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "jobs", "bad.json"), []byte(`{"job_id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadFinalizedData(dataDir)
	if err == nil || !strings.Contains(err.Error(), "parse finalized job") {
		t.Fatalf("error = %v, want malformed job error", err)
	}
}

type finalizedFakePR struct{}

func (finalizedFakePR) OpenPR(context.Context, ghpr.Request) (string, error) {
	return "https://github.com/example/repo/pull/7", nil
}
func (finalizedFakePR) SearchOpenPR(context.Context, string, string, string, string) (int, string, bool, error) {
	return 0, "", false, nil
}
func (finalizedFakePR) ResolveBase(context.Context, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "base-sha", TreeSHA: "tree-sha"}, nil
}

type finalizedFakeAgent struct{}

func (finalizedFakeAgent) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{
		Files: map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff:  "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n",
	}, nil
}

func TestRunFinalizedSideEffectsProducesFixPreview(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	storageDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + storageDir + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo: {owner: example, name: repo}
ai:
  fix_prs:
    enabled: true
    author_name: Test
    author_email: test@example.com
    dry_run: true
    critique_retries: 0
    agent_runtime:
      type: orka
      agent_ref: test-fixer
      api: http://orka.test
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{
		RecurringPatterns: []models.PatternAnalysis{{
			ID: "pattern", Subject: "job", Systemic: true, Confidence: "high",
			SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml", Summary: "recurring",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIX_TOKEN", "test-token")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return finalizedFakeAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() {
		newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager
	})

	if err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}
	var previews []fixpr.Preview
	data, err := os.ReadFile(filepath.Join(dataDir, "fix_previews.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &previews); err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || !strings.Contains(previews[0].Diff, "fixed: true") {
		t.Fatalf("previews = %+v", previews)
	}
}

func TestProcessFixPRsReportsPersistedReference(t *testing.T) {
	zero := 0
	cfg := &project.Config{
		Name:     "Test Project",
		Branding: project.Branding{SiteURL: "https://example.test"},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"},
			AuthorName: "Test", AuthorEmail: "test@example.com", CritiqueRetries: &zero,
			AgentRuntime: &project.FixAgentRuntime{Type: "orka"},
		}},
	}
	t.Setenv("FIX_TOKEN", "test-token")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return finalizedFakeAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() {
		newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager
	})
	dataDir := t.TempDir()
	pattern := models.PatternAnalysis{
		ID: "pattern", JobID: "job", Subject: "job", Systemic: true, Confidence: "high",
		SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml", Summary: "recurring",
	}
	changed, err := processFixPRs(context.Background(), cfg, []models.PatternAnalysis{pattern}, "", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("persisted fix reference was not reported")
	}
	state := statefile.Load[fixpr.TrackedFix](filepath.Join(dataDir, "fix_pr_state.json"), "example/repo", "fix PRs")
	if len(state.Tracked) != 1 {
		t.Fatalf("tracked fixes = %+v", state.Tracked)
	}
}

type failingFinalizedAgent struct{}

func (failingFinalizedAgent) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{}, errors.New("generation failed")
}

func TestProcessFixPRsPropagatesPatternFailure(t *testing.T) {
	projectDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + t.TempDir() + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo: {owner: example, name: repo}
ai:
  fix_prs:
    enabled: true
    author_name: Test
    author_email: test@example.com
    dry_run: true
    critique_retries: 0
    agent_runtime:
      type: orka
      agent_ref: test-fixer
      api: http://orka.test
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIX_TOKEN", "test-token")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return failingFinalizedAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() {
		newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager
	})

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{
		RecurringPatterns: []models.PatternAnalysis{{
			ID: "pattern", Subject: "job", Systemic: true, Confidence: "high",
			SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "generation failed") {
		t.Fatalf("error = %v, want per-pattern generation failure", err)
	}
}

func TestProcessRemediationsRemovesPublicStateWhenInactive(t *testing.T) {
	tests := []struct {
		name string
		cfg  *project.Config
	}{
		{name: "removed", cfg: &project.Config{}},
		{name: "dry run", cfg: &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
			DryRun: true, Repo: &project.SourceRepo{Owner: "o", Name: "r"},
		}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			state := remediation.NewStateForRepo("o/r")
			state.Remediations["pattern"] = &remediation.Remediation{ID: "pattern", FindingID: "pattern"}
			if err := state.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			p := &pipeline{cfg: tt.cfg, opts: Options{OutDir: dataDir}}
			if err := p.processRemediations(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, remediation.PublicFileName)); !os.IsNotExist(err) {
				t.Fatalf("public state still exists: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, remediation.FileName)); err != nil {
				t.Fatalf("private state was removed: %v", err)
			}
		})
	}
}

func TestProcessRemediationsClearsStateForChangedRepo(t *testing.T) {
	dataDir := t.TempDir()
	old := remediation.NewStateForRepo("old/r")
	old.Remediations["pattern"] = &remediation.Remediation{ID: "pattern", FindingID: "pattern"}
	if err := old.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	p := &pipeline{
		opts: Options{OutDir: dataDir},
		cfg: &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
			Repo: &project.SourceRepo{Owner: "new", Name: "r"},
		}}},
	}
	if err := p.processRemediations(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	state, err := remediation.LoadForRepo(dataDir, "new/r")
	if err != nil {
		t.Fatal(err)
	}
	if state.Repo != "new/r" || len(state.Remediations) != 0 {
		t.Fatalf("state = %+v", state)
	}
	data, err := os.ReadFile(filepath.Join(dataDir, remediation.PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pattern") {
		t.Fatalf("public remediation state was not cleared: %s", data)
	}
}

func TestProcessRemediationsSkipsFixWithoutPatternSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	fixState := statefile.State[fixpr.TrackedFix]{
		Repo: "o/r",
		Tracked: map[string]fixpr.TrackedFix{
			"legacy": {URL: "https://github.com/o/r/pull/7"},
		},
	}
	if err := fixState.Save(filepath.Join(dataDir, "fix_pr_state.json")); err != nil {
		t.Fatal(err)
	}
	p := &pipeline{
		opts: Options{OutDir: dataDir},
		cfg: &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
			Repo: &project.SourceRepo{Owner: "o", Name: "r"},
		}}},
	}
	if err := p.processRemediations(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := remediation.LoadForRepo(dataDir, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Remediations) != 0 {
		t.Fatalf("remediations = %+v", got.Remediations)
	}
}

type recordingRemediationSender struct {
	messages []notify.Message
	err      error
}

func (s *recordingRemediationSender) Send(_ context.Context, message notify.Message) error {
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, message)
	return nil
}

func remediationEmailTestConfig() *project.Config {
	return &project.Config{
		Name:     "Test Project",
		Branding: project.Branding{SiteURL: "https://dashboard.test"},
		Notifications: &project.Notifications{Email: &project.EmailNotifications{
			Enabled: true, From: "dashboard@example.test", To: []string{"maintainer@example.test"},
			SMTP: project.EmailSMTP{Host: "smtp.example.test", Port: 25, TLS: project.EmailTLSNone},
		}},
	}
}

func remediationEmailTestState(t *testing.T, dir string) *remediation.State {
	t.Helper()
	state := remediation.NewState()
	state.Remediations["pattern"] = &remediation.Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job", JobName: "job",
		Attempts: []remediation.Attempt{{
			Number: 1, Status: remediation.StatusAwaitingPresubmit,
			URL: "https://github.com/o/r/pull/7", OutcomeReason: "waiting for Prow",
			LastTransition: "open->awaiting_presubmit", TransitionIndex: 1,
		}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSendRemediationEmailsPersistsSuccessfulDelivery(t *testing.T) {
	dir := t.TempDir()
	state := remediationEmailTestState(t, dir)
	sender := &recordingRemediationSender{}
	oldFactory := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	t.Cleanup(func() { newEmailSender = oldFactory })
	p := &pipeline{cfg: remediationEmailTestConfig(), opts: Options{OutDir: dir}}

	if err := p.sendRemediationEmails(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(sender.messages))
	}
	reloaded, err := remediation.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	attempt := reloaded.Remediations["pattern"].Attempts[0]
	if attempt.LastEmailedTransitionIndex != 1 || attempt.LastEmailedTransition != attempt.LastTransition {
		t.Fatalf("attempt = %+v", attempt)
	}
	if err := p.sendRemediationEmails(context.Background(), reloaded); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages after reload = %d, want 1", len(sender.messages))
	}
}

func TestSendRemediationEmailsRetriesFailedDeliveryAfterReload(t *testing.T) {
	dir := t.TempDir()
	state := remediationEmailTestState(t, dir)
	failedSender := &recordingRemediationSender{err: errors.New("delivery failed")}
	oldFactory := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return failedSender, nil }
	t.Cleanup(func() { newEmailSender = oldFactory })
	p := &pipeline{cfg: remediationEmailTestConfig(), opts: Options{OutDir: dir}}

	if err := p.sendRemediationEmails(context.Background(), state); err == nil {
		t.Fatal("failed delivery must return an error")
	}
	if state.Remediations["pattern"].Attempts[0].LastEmailedTransitionIndex != 0 {
		t.Fatalf("attempt advanced after failure: %+v", state.Remediations["pattern"].Attempts[0])
	}
	reloaded, err := remediation.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Remediations["pattern"].Attempts[0].LastEmailedTransitionIndex != 0 {
		t.Fatalf("reloaded attempt advanced after failure: %+v", reloaded.Remediations["pattern"].Attempts[0])
	}

	retrySender := &recordingRemediationSender{}
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return retrySender, nil }
	if err := p.sendRemediationEmails(context.Background(), reloaded); err != nil {
		t.Fatal(err)
	}
	if len(retrySender.messages) != 1 || reloaded.Remediations["pattern"].Attempts[0].LastEmailedTransitionIndex != 1 {
		t.Fatalf("messages = %d, attempt = %+v", len(retrySender.messages), reloaded.Remediations["pattern"].Attempts[0])
	}
}

func TestRemediationIssueLifecycleKeys(t *testing.T) {
	state := remediation.NewState()
	state.Remediations["closed"] = &remediation.Remediation{
		JobID: "closed", Issue: &remediation.IssueRef{Number: 7, Repo: "o/r"},
		Attempts: []remediation.Attempt{{Status: remediation.StatusClosedUnmerged}},
	}
	state.Remediations["verified"] = &remediation.Remediation{
		JobID: "verified", Issue: &remediation.IssueRef{Number: 8, Repo: "o/r"},
		Attempts: []remediation.Attempt{{Status: remediation.StatusVerifiedFixed}},
	}
	state.Remediations["older-verified"] = &remediation.Remediation{
		JobID: "closed", Issue: &remediation.IssueRef{Number: 7, Repo: "o/r"},
		Attempts: []remediation.Attempt{{Status: remediation.StatusVerifiedFixed}},
	}
	state.Remediations["other-repo"] = &remediation.Remediation{
		JobID: "other", Issue: &remediation.IssueRef{Number: 9, Repo: "other/r"},
		Attempts: []remediation.Attempt{{Status: remediation.StatusOpen}},
	}
	state.Remediations["unlinked"] = &remediation.Remediation{
		JobID: "unlinked", Attempts: []remediation.Attempt{{Status: remediation.StatusOpen}},
	}

	keepOpen, retire := remediationIssueLifecycleKeys(state, "o/r")
	if !keepOpen[issues.KeyPrefixPattern+"closed"] {
		t.Fatalf("keepOpen = %+v", keepOpen)
	}
	if !retire[issues.KeyPrefixPattern+"verified"] {
		t.Fatalf("retire = %+v", retire)
	}
	if len(keepOpen) != 1 || len(retire) != 1 {
		t.Fatalf("keepOpen = %+v, retire = %+v", keepOpen, retire)
	}
}
