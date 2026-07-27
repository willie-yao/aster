package fetcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type resultLifecycleAnalyzer struct {
	client  *orka.ResultClient
	state   *analysisruntime.ContainerStateStore
	dataDir string
	result  ai.FailureAnalysisResult
	calls   atomic.Int64
	mutate  bool
}

func (a *resultLifecycleAnalyzer) Maintain(context.Context) error { return nil }

func (a *resultLifecycleAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return a.state }

func (a *resultLifecycleAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.calls.Add(1)
	if a.mutate {
		if err := os.WriteFile(filepath.Join(a.dataDir, ai.CacheFilename), []byte(`{"changed":true}`), 0o600); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
		if err := os.WriteFile(filepath.Join(a.dataDir, "ai_traces.json"), []byte(`{"version":1,"traces":null}`), 0o600); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
	}
	_, ok, err := a.client.Result(ctx, "analysis", "succeeded-task")
	if err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if !ok {
		err := fmt.Errorf("succeeded Task result is unavailable")
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	return a.result, nil
}

type countingNotifySender struct {
	calls atomic.Int64
}

func (s *countingNotifySender) Send(context.Context, notify.Message) error {
	s.calls.Add(1)
	return nil
}

func TestOrkaAuthorizationFailurePreservesRefreshState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	before := hashFileTree(t, dataDir)

	var resultCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resultCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("private Orka response material"))
	}))
	defer server.Close()

	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{
		client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir, mutate: true,
	}
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	t.Cleanup(func() { newEmailSender = oldEmailSender })

	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	_, err = p.fullPass(t.Context())
	if err == nil || !orka.IsResultAuthorizationError(err) {
		t.Fatalf("fullPass error = %v", err)
	}
	if strings.Contains(err.Error(), "private Orka response material") {
		t.Fatalf("error exposed response body: %v", err)
	}
	if resultCalls.Load() != 1 {
		t.Fatalf("result calls = %d, want 1", resultCalls.Load())
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
	after := hashFileTree(t, dataDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after aborted refresh\nbefore=%v\nafter=%v", before, after)
	}
}

func TestSuccessfulOrkaResultPublishesRefresh(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	before := hashFileTree(t, dataDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
	}))
	defer server.Close()

	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{
		client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir,
		result: ai.FailureAnalysisResult{
			Summary: &models.AISummary{GeneratedAt: "2026-07-27T00:00:00Z", Summary: "analyzed"},
			Analysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-27T00:00:00Z", RootCause: "configuration drift", Severity: "High",
				SuggestedFix: "update the configuration", Mode: "agentic", EvidencePlanCovered: true,
			},
		},
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.opts.SkipSideEffects = true
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	p.aiProject = &analysisruntime.Project{
		Config: p.cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: "http://model.invalid/v1/chat/completions", Model: "test-model",
		},
		SystemPrompt: "test",
	}
	p.aiRuntime, err = analysisruntime.New(t.Context(), analysisruntime.Options{DataDir: dataDir, Project: p.aiProject})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := p.fullPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := hashFileTree(t, dataDir)
	if reflect.DeepEqual(after, before) {
		t.Fatal("successful refresh did not publish output")
	}
	jobData, err := os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(models.JobIDFor(models.JobTypePeriodic, "", "periodic-test"))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jobData), `"root_cause": "configuration drift"`) {
		t.Fatalf("published job detail does not contain successful analysis: %s", jobData)
	}
}

func TestSystemicOrkaAuthorizationStopsScheduling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	dataDir := t.TempDir()
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir}
	p := &pipeline{
		opts: Options{OutDir: dataDir, AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 1},
		}},
		cfg: &project.Config{AI: &project.AI{Concurrency: 1}}, client: &http.Client{}, containerAnalyzer: analyzer,
	}
	details := []models.JobDetail{{Name: "job", JobID: "job", JobType: models.JobTypePeriodic}}
	for i := 0; i < 8; i++ {
		details[0].Runs = append(details[0].Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: fmt.Sprint(i), Result: "FAILURE"},
			TestCases: []models.TestCase{{Name: fmt.Sprintf("test-%d", i), Status: "failed", FailureMessage: "failed"}},
		})
	}
	if err := p.analyzeFailuresWithAI(t.Context(), details, models.FlakinessReport{}); err == nil || !orka.IsResultAuthorizationError(err) {
		t.Fatalf("analysis error = %v", err)
	}
	if got := analyzer.calls.Load(); got != 1 {
		t.Fatalf("scheduled analyses = %d, want 1", got)
	}
}

func TestOrkaNonAuthorizationResultFailureRemainsNonfatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	dataDir := t.TempDir()
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir}
	p := &pipeline{
		opts: Options{OutDir: dataDir, AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 1},
		}},
		cfg: &project.Config{AI: &project.AI{Concurrency: 1}}, client: &http.Client{}, containerAnalyzer: analyzer,
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "failed"}},
		}},
	}}
	if err := p.analyzeFailuresWithAI(t.Context(), details, models.FlakinessReport{}); err != nil {
		t.Fatalf("analysis error = %v", err)
	}
	if details[0].Runs[0].TestCases[0].AISummary == nil {
		t.Fatal("nonfatal unavailable analysis did not retain its summary")
	}
}

func refreshLifecyclePipeline(t *testing.T, dataDir, bucketDir string, analyzer containerFailureAnalyzer) *pipeline {
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
	return &pipeline{
		opts: Options{
			OutDir: dataDir, BuildsPerJob: 1, Workers: 1, Timeout: time.Minute,
			AnalysisRuntime: AnalysisRuntimeOptions{
				Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 1},
			},
		},
		cfg: cfg, client: &http.Client{}, backend: backend, enableAI: true, containerAnalyzer: analyzer,
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
		"dashboard.json":                `{"sentinel":"dashboard"}`,
		"flakiness.json":                `{"sentinel":"flakiness"}`,
		"manifest.json":                 `{"sentinel":"manifest"}`,
		"search-index.json":             `{"sentinel":"search"}`,
		"jobs/old.json":                 `{"job_id":"old","runs":[]}`,
		ai.CacheFilename:                `{}`,
		"ai_traces.json":                `{"version":1,"traces":[]}`,
		"notification_state.json":       `{"sentinel":"notification"}`,
		"remediation_state.json":        `{"sentinel":"remediation"}`,
		".analysis-chat/session-1.json": `{"sentinel":"chat"}`,
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
