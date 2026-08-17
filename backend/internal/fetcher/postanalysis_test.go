package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/remediation"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
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
func (f finalizedFakePR) SearchAnyPR(ctx context.Context, owner, repo, token, marker string) (int, string, bool, error) {
	return f.SearchOpenPR(ctx, owner, repo, token, marker)
}

func (finalizedFakePR) ResolveBase(context.Context, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "base-sha", TreeSHA: "tree-sha"}, nil
}

type finalizedFakeAgent struct{}

func (finalizedFakeAgent) Generate(_ context.Context, spec runtime.GenerateSpec) (runtime.GenerateResult, error) {
	results := make([]runtime.CommandResult, 0, len(spec.CommandPolicy.Commands))
	for _, command := range spec.CommandPolicy.Commands {
		results = append(results, runtime.CommandResult{Argv: command.Argv})
	}
	return runtime.GenerateResult{
		Files:          map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff:           "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n",
		CommandResults: results,
		BaseSHA:        spec.Repo.Ref,
	}, nil
}

type recordingScheduledIssueManager struct {
	reconcileCalls int
	saveCalls      int
	reconcileErr   error
	saveErr        error
}

func (m *recordingScheduledIssueManager) Reconcile(context.Context, []issues.IssueSpec) (issues.Stats, error) {
	m.reconcileCalls++
	return issues.Stats{}, m.reconcileErr
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

	if err := processIssues(t.Context(), automaticIssueTestConfig(), models.FlakinessReport{}, nil, "", false, t.TempDir(), nil); err != nil {
		t.Fatalf("processIssues error = %v", err)
	}
}

func TestProcessIssuesRunsWithStaticToken(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{}
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager { return manager }
	t.Cleanup(func() { newBatchIssueManager = oldFactory })

	if err := processIssues(t.Context(), automaticIssueTestConfig(), models.FlakinessReport{}, nil, "", false, t.TempDir(), nil); err != nil {
		t.Fatalf("processIssues error = %v", err)
	}
	if manager.reconcileCalls != 1 || manager.saveCalls != 1 {
		t.Fatalf("issue manager calls = reconcile %d save %d", manager.reconcileCalls, manager.saveCalls)
	}
}

type recordingListBackend struct {
	storage.Backend
	prefixes []string
}

func (b *recordingListBackend) List(ctx context.Context, prefix string) (*storage.Listing, error) {
	b.prefixes = append(b.prefixes, prefix)
	return b.Backend.List(ctx, prefix)
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
      type: agent-sandbox
      allow_bash: false
      output_limit_bytes: 65536
      allowed_commands:
        - argv: [git, diff, --cached, --check]
          timeout: 1m
      model_provider:
        credential_mode: direct
        api: chat_completions
        endpoint: https://models.invalid/v1/chat/completions
        model: test-model
        auth:
          type: bearer
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
			AgentRuntime: &project.FixAgentRuntime{
				Type: "agent-sandbox", OutputLimitBytes: 64 << 10,
				AllowedCommands: []project.FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "1m"}},
			},
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
	changed, err := processFixPRs(context.Background(), cfg, []models.PatternAnalysis{pattern}, "", dataDir, nil)
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

func TestProcessFixPRsSkipsWithoutStaticToken(t *testing.T) {
	zero := 0
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
		Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"},
		AuthorName: "Test", AuthorEmail: "test@example.com", CritiqueRetries: &zero,
		AgentRuntime: &project.FixAgentRuntime{Type: "orka"},
	}}}
	t.Setenv("FIX_TOKEN", "")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) {
		t.Fatal("fix runtime was created without FIX_TOKEN")
		return nil, nil
	}
	newBatchFixManager = func(string, string, fixpr.Options) *fixpr.Manager {
		t.Fatal("fix manager was created without FIX_TOKEN")
		return nil
	}
	t.Cleanup(func() { newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager })

	changed, err := processFixPRs(t.Context(), cfg, []models.PatternAnalysis{{
		ID: "pattern", JobID: "job", Subject: "job", Systemic: true, Confidence: "high",
		SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml",
	}}, "", t.TempDir(), nil)
	if err != nil || changed {
		t.Fatalf("processFixPRs changed=%t error=%v", changed, err)
	}
}

func TestProcessFixPRsRejectsInvalidAIAPI(t *testing.T) {
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
		Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"},
		AgentRuntime: &project.FixAgentRuntime{Type: "orka"},
	}}}
	t.Setenv("FIX_TOKEN", "test-token")
	t.Setenv("AI_API", "invalid")
	pattern := models.PatternAnalysis{ID: "pattern", Systemic: true, Confidence: "high"}
	changed, err := processFixPRs(context.Background(), cfg, []models.PatternAnalysis{pattern}, "", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), `AI API "invalid" is invalid`) {
		t.Fatalf("err = %v, want invalid AI API error", err)
	}
	if changed {
		t.Fatal("invalid AI API changed fix state")
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
      type: agent-sandbox
      allow_bash: false
      output_limit_bytes: 65536
      allowed_commands:
        - argv: [git, diff, --cached, --check]
          timeout: 1m
      model_provider:
        credential_mode: direct
        api: chat_completions
        endpoint: https://models.invalid/v1/chat/completions
        model: test-model
        auth:
          type: bearer
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
	storageDir := t.TempDir()
	buildDir := filepath.Join(storageDir, "logs", "exact-job", "1")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "started.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	localBackend, err := storage.NewLocalBackend(storageDir, "")
	if err != nil {
		t.Fatal(err)
	}
	recordingBackend := &recordingListBackend{Backend: localBackend}
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
		cfg: &project.Config{
			Discovery: project.Discovery{Source: project.DiscoveryBucket, ExactJobs: []string{"exact-job"}},
			AI: &project.AI{FixPRs: &project.FixPRs{
				Repo: &project.SourceRepo{Owner: "o", Name: "r"},
			}},
		},
		backend: recordingBackend,
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
	for _, prefix := range recordingBackend.prefixes {
		if prefix == "logs/" || prefix == "pr-logs/directory/" {
			t.Fatalf("exact remediation discovery enumerated bucket root %q", prefix)
		}
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

func TestRunSideEffectsReportsSanitizedAutomaticIssueFailure(t *testing.T) {
	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{reconcileErr: errors.New("private provider response with token=secret")}
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
		issueStatus.Summary != "Automatic issue reconciliation failed" || strings.Contains(issueStatus.Summary, "secret") {
		t.Fatalf("automatic issue status = %+v", issueStatus)
	}
}

type countingRoundTripper struct {
	calls atomic.Int64
}

func (t *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected network request")
}

func TestRunSideEffectsSkipsFixAutomationWithoutStaticToken(t *testing.T) {
	t.Setenv("FIX_TOKEN", "")
	zero := 0
	cfg := &project.Config{
		Name: "Test", Branding: project.Branding{SiteURL: "https://dashboard.example.test"},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"},
			AuthorName: "Test", AuthorEmail: "test@example.com", CritiqueRetries: &zero,
			AgentRuntime: &project.FixAgentRuntime{
				Type: "agent-sandbox", OutputLimitBytes: 64 << 10,
				AllowedCommands: []project.FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "1m"}},
			},
		}},
	}
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) {
		t.Fatal("fix runtime was created without FIX_TOKEN")
		return nil, nil
	}
	newBatchFixManager = func(string, string, fixpr.Options) *fixpr.Manager {
		t.Fatal("fix manager was created without FIX_TOKEN")
		return nil
	}
	t.Cleanup(func() { newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager })

	dataDir := t.TempDir()
	backend, err := storage.NewLocalBackend(t.TempDir(), "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	transport := &countingRoundTripper{}
	tracker := fetchprogress.New(dataDir, "sha-test")
	tracker.StartPass(fetchprogress.PassOneShot)
	tracker.StartPhase(fetchprogress.PhaseSideEffects)
	p := &pipeline{
		cfg: cfg, opts: Options{OutDir: dataDir}, backend: backend,
		client: &http.Client{Transport: transport}, progress: tracker,
	}
	res := &refreshResult{flakiness: models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{{
		ID: "pattern", JobID: "job", Subject: "job", Systemic: true, Confidence: "high",
		SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml",
	}}}}

	if err := p.runSideEffects(t.Context(), res); err != nil {
		t.Fatalf("runSideEffects error = %v", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("network calls = %d, want 0", transport.calls.Load())
	}
	status := tracker.Snapshot()
	if status.FollowUp == nil || status.FollowUp.AutomaticFixPRs == nil || status.FollowUp.Remediation == nil ||
		status.FollowUp.AutomaticFixPRs.State != fetchprogress.FollowUpSkipped ||
		status.FollowUp.AutomaticFixPRs.Reason != fetchprogress.FollowUpReasonNotConfigured ||
		status.FollowUp.Remediation.State != fetchprogress.FollowUpSkipped {
		t.Fatalf("follow-up status = %+v", status.FollowUp)
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
	if err := processIssues(t.Context(), automaticIssueTestConfig(), report, details, "", false, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if !gotOptions.KeepOpenKeys[issues.KeyPrefixPattern+"job"] {
		t.Fatalf("keep-open keys=%v", gotOptions.KeepOpenKeys)
	}
}

type agentSandboxBatchAgent struct {
	spec runtime.GenerateSpec
}

func (a *agentSandboxBatchAgent) Generate(_ context.Context, spec runtime.GenerateSpec) (runtime.GenerateResult, error) {
	a.spec = spec
	results := make([]runtime.CommandResult, len(spec.CommandPolicy.Commands))
	for i, command := range spec.CommandPolicy.Commands {
		results[i] = runtime.CommandResult{Argv: append([]string(nil), command.Argv...), ExitCode: 0, DurationMs: 1}
	}
	return runtime.GenerateResult{
		BaseSHA: "base-sha", Files: map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff: "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n", CommandResults: results,
	}, nil
}

func TestProcessFixPRsAgentSandboxUsesExecutorVerification(t *testing.T) {
	t.Setenv("FIX_TOKEN", "write-token")
	t.Setenv(runtime.TrustedLocalRuntimeEnv, "")
	zero := 0
	cfg := &project.Config{
		Name: "Test", Branding: project.Branding{SiteURL: "https://dashboard.example.test"},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"}, DryRun: true,
			AuthorName: "Test", AuthorEmail: "test@example.com", CritiqueRetries: &zero,
			AgentRuntime: &project.FixAgentRuntime{
				Type: "agent-sandbox", MaxTurns: 3, Timeout: "1m", OutputLimitBytes: 65536,
				AllowedCommands: []project.FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}},
			},
		}},
	}
	agent := &agentSandboxBatchAgent{}
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	var captured fixpr.Options
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return agent, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		captured = opts
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() { newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager })

	pattern := models.PatternAnalysis{
		ID: "pattern", JobID: "job", Subject: "job", Systemic: true, Confidence: "high",
		SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml",
	}
	if changed, err := processFixPRs(t.Context(), cfg, []models.PatternAnalysis{pattern}, "", t.TempDir(), nil); err != nil || changed {
		t.Fatalf("changed=%t error=%v", changed, err)
	}
	if captured.Verify != nil || captured.Agent == nil || !captured.Agent.RequireCommandResults {
		t.Fatalf("captured options = %+v", captured)
	}
	if captured.Agent.GitToken != "" || agent.spec.Repo.Token != "" {
		t.Fatalf("dashboard token entered Sandbox: config=%q spec=%q", captured.Agent.GitToken, agent.spec.Repo.Token)
	}
}
