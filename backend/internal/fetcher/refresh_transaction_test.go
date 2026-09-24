package fetcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/resolve"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type countingNotifySender struct {
	calls atomic.Int64
}

func (s *countingNotifySender) Send(context.Context, notify.Message) error {
	s.calls.Add(1)
	return nil
}

func TestWatchPassSkipsSideEffects(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	t.Cleanup(func() { newEmailSender = oldEmailSender })
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
	p.enableAI = false
	jobs, err := p.discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.watchPass(t.Context(), jobs); err != nil {
		t.Fatal(err)
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
}

func TestAIRefreshBackupsRemainPrivate(t *testing.T) {
	dataDir, _ := installRefreshLifecycleFixture(t)
	snapshot, err := captureAIRefreshState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Discard()
	for _, file := range snapshot.files {
		if !file.exists {
			continue
		}
		info, err := os.Stat(file.backupPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("backup mode = %o, want 600", got)
		}
	}
}

func TestMissingIssueTokenDoesNotFailPublishedRefresh(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
	}))
	defer server.Close()

	analyzer := &resultLifecycleAnalyzer{
		result: ai.FailureAnalysisResult{
			Summary: &models.AISummary{GeneratedAt: "2026-08-07T00:00:00Z", Summary: "analyzed"},
			Analysis: &models.AIAnalysis{
				GeneratedAt: "2026-08-07T00:00:00Z", RootCause: "configuration drift", Severity: "High",
				SuggestedFix: "update the configuration", Mode: "agentic", EvidencePlanCovered: true,
			},
		},
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.cfg.Issues = &project.Issues{Enabled: true}
	p.cfg.Branding.SourceRepo = project.SourceRepo{Owner: "example", Name: "repo"}
	configureRefreshLifecycleRuntime(t, p, dataDir)
	p.progress = fetchprogress.New(dataDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	t.Setenv("ISSUE_TOKEN", "")
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	oldIssueFactory := newBatchIssueManager
	newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager {
		t.Fatal("automatic issue reconciliation started without ISSUE_TOKEN")
		return nil
	}
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, _ []models.JobDetail, options patterns.AnalyzeOptions) error {
		options.OnPlan(1)
		options.OnAttempt(patterns.Attempt{Number: 1, Succeeded: true, Final: true})
		return nil
	}
	t.Cleanup(func() {
		newEmailSender = oldEmailSender
		newBatchIssueManager = oldIssueFactory
		analyzePatternsAcrossBuilds = oldPatternAnalysis
	})

	_, err := p.fullPass(t.Context())
	finishProgressPass(p.progress, err, false)
	if err != nil {
		t.Fatalf("fullPass error = %v", err)
	}
	status := p.progress.Snapshot()
	if status.Outcome != fetchprogress.OutcomeSucceeded || status.PublicationPhase != fetchprogress.StageCompleted ||
		status.SideEffectPhase != fetchprogress.StageCompleted || status.LastSuccessfulPublicationAt == nil {
		t.Fatalf("published refresh status = %+v", status)
	}
	if status.FollowUp == nil || status.FollowUp.AutomaticIssues == nil ||
		status.FollowUp.AutomaticIssues.State != fetchprogress.FollowUpSkipped ||
		status.FollowUp.AutomaticIssues.Reason != fetchprogress.FollowUpReasonNotConfigured {
		t.Fatalf("automatic issues follow-up = %+v", status.FollowUp)
	}
}

type failingNotifySender struct {
	calls int
}

func (s *failingNotifySender) Send(context.Context, notify.Message) error {
	s.calls++
	return errors.New("smtp unavailable")
}

func TestFullPassFollowUpFailureKeepsPublishedRefresh(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failEmail        bool
		failIssues       bool
		notifications    fetchprogress.FollowUpState
		notificationCode fetchprogress.FollowUpFailureCode
		issues           fetchprogress.FollowUpState
		issueCode        fetchprogress.FollowUpFailureCode
	}{
		{
			name: "email delivery", failEmail: true,
			notifications: fetchprogress.FollowUpFailed, notificationCode: fetchprogress.FollowUpFailureNotificationDelivery,
			issues: fetchprogress.FollowUpCompleted,
		},
		{
			name: "issue recovery", failIssues: true,
			notifications: fetchprogress.FollowUpCompleted,
			issues:        fetchprogress.FollowUpFailed, issueCode: fetchprogress.FollowUpFailureAutomaticIssues,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, bucketDir := installRefreshLifecycleFixture(t)
			p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
			p.enableAI = false
			p.cfg.Attention = &project.Attention{PersistentAfter: 1}
			p.cfg.Branding.SourceRepo = project.SourceRepo{Owner: "example", Name: "repo"}
			p.cfg.Issues = &project.Issues{Enabled: true, Triggers: []string{project.IssueTriggerPersistent}}
			p.progress = fetchprogress.New(dataDir, "sha-test")
			p.progress.StartPass(fetchprogress.PassOneShot)
			t.Setenv("ISSUE_TOKEN", "test-token")

			successSender := &countingNotifySender{}
			failedSender := &failingNotifySender{}
			oldEmailSender := newEmailSender
			newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) {
				if tc.failEmail {
					return failedSender, nil
				}
				return successSender, nil
			}
			manager := &recordingScheduledIssueManager{}
			if tc.failIssues {
				manager.recoverErr = errors.New("issue recovery unavailable")
			}
			oldIssueManager := newBatchIssueManager
			newBatchIssueManager = func(*issues.Client, string, string, issues.Options) scheduledIssueManager {
				return manager
			}
			t.Cleanup(func() {
				newEmailSender = oldEmailSender
				newBatchIssueManager = oldIssueManager
			})

			jobs, err := p.fullPass(t.Context())
			finishProgressPass(p.progress, err, false)
			if err != nil || len(jobs) != 1 || jobs[0].JobID != "periodic-test" {
				t.Fatalf("published pass jobs=%+v error=%v", jobs, err)
			}
			details, err := loadPublishedJobDetails(dataDir)
			if err != nil || len(details) != 1 || len(details["periodic-test"].Runs) != 1 {
				t.Fatalf("published details=%+v error=%v", details, err)
			}
			for _, name := range []string{"dashboard.json", "flakiness.json", "manifest.json"} {
				if _, err := os.Stat(filepath.Join(dataDir, name)); err != nil {
					t.Fatalf("published %s: %v", name, err)
				}
			}
			status := p.progress.Snapshot()
			if status.Outcome != fetchprogress.OutcomeSucceeded ||
				status.PublicationPhase != fetchprogress.StageCompleted ||
				status.SideEffectPhase != fetchprogress.StageCompleted ||
				status.LastSuccessfulPublicationAt == nil || status.FollowUp == nil ||
				status.FollowUp.Notifications == nil || status.FollowUp.AutomaticIssues == nil {
				t.Fatalf("published pass progress = %+v", status)
			}
			if got := status.FollowUp.Notifications; got.State != tc.notifications || got.Code != tc.notificationCode {
				t.Fatalf("notification follow-up = %+v", got)
			}
			if got := status.FollowUp.AutomaticIssues; got.State != tc.issues || got.Code != tc.issueCode {
				t.Fatalf("issue follow-up = %+v", got)
			}
			if manager.recoverCalls != 1 || manager.saveCalls != 1 || len(manager.activeKeys) != 1 ||
				manager.activeKeys[0] != issues.KeyPrefixPersistent+"periodic-test::fails" {
				t.Fatalf("issue recovery was skipped: %+v", manager)
			}
			if got := successSender.calls.Load() + int64(failedSender.calls); got != 1 {
				t.Fatalf("notification delivery calls = %d, want 1", got)
			}
		})
	}
}

func TestFullPassFollowUpFailureStillPreparesCauses(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	for _, id := range []string{"2", "3"} {
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+id+"/started.json", `{"timestamp":1}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+id+"/finished.json", `{"timestamp":2,"passed":false,"result":"FAILURE"}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+id+"/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)
	}
	analyzer := &resultLifecycleAnalyzer{result: ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: "2026-08-25T00:00:00Z", Summary: "analyzed"},
		Analysis: &models.AIAnalysis{
			GeneratedAt: "2026-08-25T00:00:00Z", RootCause: "controller defect", Severity: "High",
			SuggestedFix: "inspect the controller", Mode: "agentic", EvidencePlanCovered: true,
			Disposition: models.AnalysisDispositionCitationsVerified,
		},
	}}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.opts.BuildsPerJob = 3
	p.opts.PrepareCauseFindings = true
	p.cfg.Attention = &project.Attention{PersistentAfter: 1}
	configureRefreshLifecycleRuntime(t, p, dataDir)
	runtime := p.aiRuntime
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "Inspect the controller.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	installPreparedCauseRunner(t, runner)
	failedSender := &failingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return failedSender, nil }
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, details []models.JobDetail, options patterns.AnalyzeOptions) error {
		seed := preparedCauseJob("periodic-test", true, models.PatternLifecycleActive, "controller defect")
		details[0].Runs[2].TestCases = seed.Runs[0].TestCases
		details[0].PatternAnalyses = seed.PatternAnalyses
		options.OnPlan(1)
		options.OnOutcome(patterns.JobOutcome{JobID: "periodic-test", Succeeded: true, Systemic: true, Attempts: 1})
		options.OnAttempt(patterns.Attempt{Number: 1, Succeeded: true, Final: true})
		return nil
	}
	t.Cleanup(func() {
		newEmailSender = oldEmailSender
		analyzePatternsAcrossBuilds = oldPatternAnalysis
	})
	p.progress = fetchprogress.New(dataDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)

	jobs, err := p.fullPass(t.Context())
	finishProgressPass(p.progress, err, false)
	if err != nil || len(jobs) != 1 || failedSender.calls != 2 {
		t.Fatalf("published jobs=%+v send calls=%d error=%v", jobs, failedSender.calls, err)
	}
	if p.aiRuntime != runtime {
		t.Fatal("follow-up failure discarded the committed analysis runtime")
	}
	details, err := loadPublishedJobDetails(dataDir)
	if err != nil || len(details["periodic-test"].PatternAnalyses) != 1 {
		t.Fatalf("published cause=%+v error=%v", details, err)
	}
	status := p.progress.Snapshot()
	if status.Outcome != fetchprogress.OutcomeSucceeded || status.SideEffectPhase != fetchprogress.StageCompleted ||
		status.FollowUp == nil || status.FollowUp.Notifications == nil ||
		status.FollowUp.Notifications.Code != fetchprogress.FollowUpFailureNotificationDelivery {
		t.Fatalf("follow-up progress = %+v", status)
	}
	state, err := analysischat.LoadPreparedCauseFindings(
		filepath.Join(dataDir, analysischat.PreparedCauseFindingsFilename), analysischat.PreparedCauseGeneration("runtime"))
	if err != nil || runner.calls != 1 || len(state.Findings) != 1 {
		t.Fatalf("prepared causes=%+v runner calls=%d error=%v", state, runner.calls, err)
	}
}

func TestPatternFailurePreservesRefreshState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	for _, buildID := range []string{"2", "3"} {
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/started.json", `{"timestamp":1}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/finished.json", `{"timestamp":2,"passed":false,"result":"FAILURE"}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)
	}
	priorPattern := models.PatternAnalysis{Subject: "periodic-test", JobID: "periodic-test", GeneratedAt: "2026-07-28T00:00:00Z", BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "last good cause", SharedBuilds: []string{"3", "2"}, SuggestedFix: "keep last good fix", Summary: "last good"}
	models.AssignPatternIdentity(&priorPattern)
	writeFixtureFile(t, dataDir, "jobs/"+models.JobDataFilename("periodic-test"), mustJSON(t, models.JobDetail{JobID: "periodic-test", Name: "periodic-test", JobType: models.JobTypePeriodic, PatternAnalyses: []models.PatternAnalysis{priorPattern}}))
	before := hashFileTree(t, dataDir)
	var resultCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resultCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
	}))
	defer server.Close()
	analyzer := &resultLifecycleAnalyzer{
		result: ai.FailureAnalysisResult{
			Summary: &models.AISummary{GeneratedAt: "2026-07-27T00:00:00Z", Summary: "analyzed"},
			Analysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-27T00:00:00Z", RootCause: "configuration drift", Severity: "High",
				SuggestedFix: "update the configuration", Mode: "agentic", EvidencePlanCovered: true,
			},
		},
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.opts.BuildsPerJob = 3
	p.opts.SkipSideEffects = true
	configureRefreshLifecycleRuntime(t, p, dataDir)
	analysisRuntimeBefore := p.aiRuntime
	var persistedPatternRuntime *analysisruntime.Runtime
	oldSaveRuntime := saveAnalysisRuntimeCache
	saveAnalysisRuntimeCache = func(runtime *analysisruntime.Runtime) error {
		persistedPatternRuntime = runtime
		return oldSaveRuntime(runtime)
	}
	p.progress = fetchprogress.New(dataDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, _ []models.JobDetail, options patterns.AnalyzeOptions) error {
		options.OnPlan(1)
		options.OnOutcome(patterns.JobOutcome{JobID: "periodic-test", Attempts: 2, FailureCategory: ai.PatternFailureProvider5xx})
		options.OnAttempt(patterns.Attempt{Number: 1, FailureCategory: ai.PatternFailureRequestTimeout})
		options.OnAttempt(patterns.Attempt{Number: 2, Retry: true, Final: true, FailureCategory: ai.PatternFailureProvider5xx})
		return &ai.PatternProviderError{StatusCode: http.StatusServiceUnavailable}
	}
	t.Cleanup(func() {
		newEmailSender = oldEmailSender
		analyzePatternsAcrossBuilds = oldPatternAnalysis
		saveAnalysisRuntimeCache = oldSaveRuntime
	})

	if _, err := p.fullPass(t.Context()); err != nil {
		t.Fatalf("fullPass error = %v", err)
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
	if persistedPatternRuntime == nil || persistedPatternRuntime != analysisRuntimeBefore {
		t.Fatal("pattern persistence did not use the retained in-process runtime")
	}
	patternProgress := p.progress.Snapshot().Patterns
	if patternProgress.Attempts != 2 || patternProgress.Retries != 1 || patternProgress.Completed != 0 || patternProgress.Failed != 1 ||
		patternProgress.FailureCategory != fetchprogress.PatternFailureProvider5xx {
		t.Fatalf("pattern progress = %+v", patternProgress)
	}
	after := hashFileTree(t, dataDir)
	if after["dashboard.json"] == before["dashboard.json"] || after[output.AITraceFilename] == before[output.AITraceFilename] {
		t.Fatalf("core publication or checkpoint did not advance: before=%v after=%v", before, after)
	}
	jobData, err := os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename("periodic-test")))
	if err != nil {
		t.Fatal(err)
	}
	var published models.JobDetail
	if err := json.Unmarshal(jobData, &published); err != nil {
		t.Fatal(err)
	}
	if published.PatternRefresh == nil || published.PatternRefresh.State != models.PatternRefreshRetained || len(published.PatternAnalyses) != 1 {
		t.Fatalf("published pattern refresh = %+v patterns=%+v", published.PatternRefresh, published.PatternAnalyses)
	}
	publishedPattern := published.PatternAnalyses[0]
	if publishedPattern.ID != priorPattern.ID || publishedPattern.SharedRootCause != priorPattern.SharedRootCause ||
		publishedPattern.SuggestedFix != priorPattern.SuggestedFix || publishedPattern.ContentHash != models.PatternHash(publishedPattern) ||
		publishedPattern.Lifecycle == nil || publishedPattern.Lifecycle.State != models.PatternLifecycleActive {
		t.Fatalf("retained pattern = %+v", publishedPattern)
	}
	var flakiness models.FlakinessReport
	flakinessData, err := os.ReadFile(filepath.Join(dataDir, "flakiness.json"))
	if err != nil || json.Unmarshal(flakinessData, &flakiness) != nil || flakiness.PatternRefresh.Retained != 1 || len(flakiness.RecurringPatterns) != 1 {
		t.Fatalf("flakiness refresh = %+v error=%v", flakiness, err)
	}
	if !p.progress.Snapshot().Analyses.CheckpointCommitted {
		t.Fatal("analysis checkpoint was not reported")
	}

}

// resultLifecycleAnalyzer is a deterministic in-process analyzer double used to
// drive refresh-transaction failure and rollback paths.
type resultLifecycleAnalyzer struct {
	result ai.FailureAnalysisResult
	err    error
	calls  atomic.Int64
}

func (a *resultLifecycleAnalyzer) AnalyzeFailure(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.calls.Add(1)
	if a.err != nil {
		return ai.FailureAnalysisResult{}, a.err
	}
	return a.result, nil
}

func refreshLifecyclePipeline(t *testing.T, dataDir, bucketDir string, analyzer ai.FailureAnalyzer) *pipeline {
	t.Helper()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		ID: "test", Name: "Test", Discovery: project.Discovery{Source: project.DiscoveryBucket},
		Storage:  project.Storage{Provider: string(storage.ProviderLocal), Base: bucketDir},
		Branding: project.Branding{Title: "Test", BasePath: "/", SiteURL: "https://dashboard.example.test"},
		AI:       &project.AI{Concurrency: 1, Agentic: project.Agentic{Tools: []string{"filesystem"}}},
		Notifications: &project.Notifications{Email: &project.EmailNotifications{
			Enabled: true, From: "dashboard@example.test", To: []string{"owner@example.test"},
			SMTP: project.EmailSMTP{Host: "smtp.example.test", Port: 25, TLS: project.EmailTLSNone},
		}},
	}
	p := &pipeline{
		opts: Options{
			OutDir: dataDir, BuildsPerJob: 1, Workers: 1, Timeout: time.Minute,
		},
		cfg: cfg, client: &http.Client{}, backend: backend, enableAI: true,
	}
	if analyzer != nil {
		old := newAnalysisAnalyzer
		newAnalysisAnalyzer = func(*ai.Service) ai.FailureAnalyzer { return analyzer }
		t.Cleanup(func() { newAnalysisAnalyzer = old })
	}
	return p
}

func configureRefreshLifecycleRuntime(t *testing.T, p *pipeline, dataDir string) {
	t.Helper()
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	p.aiProject = &analysisruntime.Project{
		Config: p.cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: "http://model.invalid/v1/chat/completions", Model: "test-model",
		},
		SystemPrompt: "test",
	}
	var err error
	p.aiRuntime, err = analysisruntime.New(t.Context(), analysisruntime.Options{DataDir: dataDir, Project: p.aiProject})
	if err != nil {
		t.Fatal(err)
	}
}

func installRefreshLifecycleFixture(t *testing.T) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	writeFixtureFile(t, bucketDir, "logs/periodic-test/1/started.json", `{"timestamp":1}`)
	writeFixtureFile(t, bucketDir, "logs/periodic-test/1/finished.json", `{"timestamp":2,"passed":false,"result":"FAILURE"}`)
	writeFixtureFile(t, bucketDir, "logs/periodic-test/1/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)

	files := map[string]string{
		"dashboard.json":               `{"sentinel":"dashboard"}`,
		"flakiness.json":               `{"sentinel":"flakiness"}`,
		"manifest.json":                `{"sentinel":"manifest"}`,
		"search-index.json":            `{"sentinel":"search"}`,
		"jobs/old.json":                `{"job_id":"old","runs":[]}`,
		ai.CacheFilename:               `{}`,
		"ai_traces.json":               `{"version":1,"traces":[]}`,
		"notification_state.json":      `{"sentinel":"notification"}`,
		"remediation_state.json":       `{"sentinel":"remediation"}`,
		"action_request_state.json":    `{"sentinel":"action"}`,
		".analysis-chat/sessions.json": `{"sentinel":"chat"}`,
	}
	for path, body := range files {
		writeFixtureFile(t, dataDir, path, body)
	}
	return dataDir, bucketDir
}

func writeFixtureFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hashFileTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	hashes := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == fetchprogress.StatusDirectory {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hashes[filepath.ToSlash(rel)] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func TestAIRefreshTransactionCommitChangesRollbackBaseline(t *testing.T) {
	dataDir := t.TempDir()
	writeFixtureFile(t, dataDir, ai.CacheFilename, `{"generation":"original"}`)
	writeFixtureFile(t, dataDir, output.AITraceFilename, `{"generation":"original"}`)

	transaction, err := captureAIRefreshState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Discard()
	writeFixtureFile(t, dataDir, ai.CacheFilename, `{"generation":"checkpoint"}`)
	writeFixtureFile(t, dataDir, output.AITraceFilename, `{"generation":"checkpoint"}`)
	if err := transaction.CommitAnalysisCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if !transaction.checkpointCommitted {
		t.Fatal("transaction did not record the checkpoint")
	}

	writeFixtureFile(t, dataDir, ai.CacheFilename, `{"generation":"later"}`)
	writeFixtureFile(t, dataDir, output.AITraceFilename, `{"generation":"later"}`)
	if err := transaction.Restore(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
		data, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"checkpoint"`) {
			t.Fatalf("%s restored data = %s", name, data)
		}
	}
}

func TestAnalysisCheckpointPersistenceFailureRestoresRefreshState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	before := hashFileTree(t, dataDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
	}))
	defer server.Close()
	analyzer := &resultLifecycleAnalyzer{
		result: ai.FailureAnalysisResult{
			Summary: &models.AISummary{GeneratedAt: "2026-07-29T00:00:00Z", Summary: "analyzed"},
			Analysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-29T00:00:00Z", RootCause: "configuration drift", Severity: "High",
				SuggestedFix: "update configuration", Mode: "agentic", EvidencePlanCovered: true,
			},
		},
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	configureRefreshLifecycleRuntime(t, p, dataDir)
	want := errors.New("checkpoint disk full")
	oldSave := saveAnalysisRuntimeCache
	saveAnalysisRuntimeCache = func(*analysisruntime.Runtime) error { return want }
	t.Cleanup(func() { saveAnalysisRuntimeCache = oldSave })

	if _, err := p.fullPass(t.Context()); !errors.Is(err, want) || !strings.Contains(err.Error(), "persisting AI cache") {
		t.Fatalf("fullPass error = %v", err)
	}
	if p.aiRuntime != nil {
		t.Fatal("checkpoint failure retained in-memory AI state")
	}
	if after := hashFileTree(t, dataDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("checkpoint failure changed refresh state\nbefore=%v\nafter=%v", before, after)
	}
}

func TestRollbackFailureInvalidatesInMemoryAnalysisState(t *testing.T) {
	p := &pipeline{aiRuntime: &analysisruntime.Runtime{}}
	transaction := &aiRefreshStateTransaction{files: []aiRefreshFileSnapshot{{
		path: filepath.Join(t.TempDir(), ai.CacheFilename), backupPath: filepath.Join(t.TempDir(), "missing-backup"), exists: true,
	}}}
	refreshErr := errors.New("refresh failed")
	err := p.rollbackAIRefresh(transaction, refreshErr)
	if !errors.Is(err, refreshErr) || !strings.Contains(err.Error(), "restoring AI refresh state") {
		t.Fatalf("rollback error = %v", err)
	}
	if p.aiRuntime != nil {
		t.Fatal("rollback failure retained in-memory AI state")
	}
}

func TestOutputFailureLeavesResolvedStateUnchanged(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	for _, buildID := range []string{"2", "3"} {
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/started.json", `{"timestamp":1}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/finished.json", `{"timestamp":2,"passed":false,"result":"FAILURE"}`)
		writeFixtureFile(t, bucketDir, "logs/periodic-test/"+buildID+"/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)
	}
	prior := models.PatternAnalysis{Subject: "periodic-test", JobID: "periodic-test", GeneratedAt: "2026-07-28T00:00:00Z", BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "cause", SharedBuilds: []string{"1"}, SuggestedFix: "fix", Summary: "old"}
	models.AssignPatternIdentity(&prior)
	writeFixtureFile(t, dataDir, "jobs/"+models.JobDataFilename("periodic-test"), mustJSON(t, models.JobDetail{JobID: "periodic-test", Name: "periodic-test", JobType: models.JobTypePeriodic, PatternAnalyses: []models.PatternAnalysis{prior}}))
	resolved := &resolve.State{Resolved: map[string]resolve.Entry{prior.ID: {Watermark: "1"}}}
	if err := resolved.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dataDir, resolve.FileName))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
	}))
	defer server.Close()
	analyzer := &resultLifecycleAnalyzer{result: ai.FailureAnalysisResult{Summary: &models.AISummary{Summary: "analyzed"}, Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", SuggestedFix: "fix", Mode: "agentic", EvidencePlanCovered: true}}}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.opts.BuildsPerJob = 3
	p.opts.SkipSideEffects = true
	configureRefreshLifecycleRuntime(t, p, dataDir)
	oldPatterns, oldWrite := analyzePatternsAcrossBuilds, writeAllOutput
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, details []models.JobDetail, options patterns.AnalyzeOptions) error {
		fresh := prior
		fresh.GeneratedAt = "2026-07-29T00:00:00Z"
		fresh.SharedBuilds = []string{"3", "2"}
		details[0].PatternAnalyses = []models.PatternAnalysis{fresh}
		options.OnOutcome(patterns.JobOutcome{JobID: "periodic-test", Succeeded: true, Systemic: true, Attempts: 1})
		return nil
	}
	writeAllOutput = func(string, *project.Config, models.Dashboard, []models.JobDetail, models.FlakinessReport, models.SearchIndex) error {
		return errors.New("output failed")
	}
	t.Cleanup(func() { analyzePatternsAcrossBuilds, writeAllOutput = oldPatterns, oldWrite })
	if _, err := p.fullPass(t.Context()); err == nil || !strings.Contains(err.Error(), "output failed") {
		t.Fatalf("fullPass error = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dataDir, resolve.FileName))
	if !bytes.Equal(before, after) {
		t.Fatalf("resolved state changed on output failure\nbefore=%s\nafter=%s", before, after)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
