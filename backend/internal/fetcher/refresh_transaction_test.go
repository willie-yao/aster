package fetcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type resultLifecycleAnalyzer struct {
	client  *orka.ResultClient
	state   *analysisruntime.ContainerStateStore
	dataDir string
	result  ai.FailureAnalysisResult
	err     error
	calls   atomic.Int64
	mutate  bool
	merge   bool
	merged  chan struct{}
	once    sync.Once
	seeds   atomic.Int64
}

func (a *resultLifecycleAnalyzer) Maintain(context.Context) error { return nil }

func (a *resultLifecycleAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return a.state }

func (a *resultLifecycleAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.calls.Add(1)
	if a.state != nil {
		a.seeds.Add(int64(len(a.state.CacheSeed(request))))
	}
	if a.mutate {
		if err := os.WriteFile(filepath.Join(a.dataDir, ai.CacheFilename), []byte(`{"changed":true}`), 0o600); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
		if err := os.WriteFile(filepath.Join(a.dataDir, "ai_traces.json"), []byte(`{"version":1,"traces":null}`), 0o600); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
	}
	if a.err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, a.err), a.err
	}
	_, ok, err := a.client.Result(ctx, "analysis", "succeeded-task")
	if err != nil {
		err = fmt.Errorf("read succeeded Task result: %w", err)
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if !ok {
		err := fmt.Errorf("succeeded Task result is unavailable")
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if a.merge {
		identity := analysisruntime.NewContainerStateIdentity("analysis", "succeeded-task-"+request.Build.BuildID, request)
		entry := ai.CacheEntry{Key: identity.CacheKey, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"summary":"aborted"}`)}
		if err := a.state.Merge(analysisruntime.ContainerAnalysisState{
			Version: analysisruntime.ContainerStateVersion, TaskNamespace: identity.TaskNamespace,
			TaskName: identity.TaskName, CacheKey: identity.CacheKey,
			CacheEntries: map[string]ai.CacheEntry{identity.CacheKey: entry},
		}); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
		a.once.Do(func() {
			if a.merged != nil {
				close(a.merged)
			}
		})
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

func TestWatchPassAuthorizationFailurePreservesRefreshState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{
		client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir, mutate: true,
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	jobs, err := p.discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := hashFileTree(t, dataDir)
	if err := p.watchPass(t.Context(), jobs); err == nil || !orka.IsResultAuthorizationError(err) {
		t.Fatalf("watchPass error = %v", err)
	}
	if after := hashFileTree(t, dataDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after aborted watch pass\nbefore=%v\nafter=%v", before, after)
	}
}

func TestWatchPassSkipsSideEffects(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	t.Cleanup(func() { newEmailSender = oldEmailSender })
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
	p.enableAI = false
	p.opts.AnalysisRuntime.Type = AnalysisRuntimeInProcess
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

func TestOrkaProjectSetupFailurePreservesRefreshState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	before := hashFileTree(t, dataDir)
	projectDir := t.TempDir()
	projectTarget := filepath.Join(t.TempDir(), "project.yaml")
	writeFixtureFile(t, filepath.Dir(projectTarget), filepath.Base(projectTarget), fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://dashboard.example.test
ai:
  timeout: 5m
  tools: [filesystem]
`, bucketDir))
	if err := os.Symlink(projectTarget, filepath.Join(projectDir, "project.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projectDir, "prompts/system.md", "Investigate build artifacts.\n")

	var patternCalls atomic.Int64
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error {
		patternCalls.Add(1)
		return nil
	}
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	t.Cleanup(func() {
		analyzePatternsAcrossBuilds = oldPatternAnalysis
		newEmailSender = oldEmailSender
	})

	t.Setenv("AI_TOKEN", "dashboard-token")
	t.Setenv("AI_ENDPOINT", "https://model.invalid/v1/chat/completions")
	t.Setenv("AI_MODEL", "test-model")
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	t.Setenv(analysisruntime.ContainerStateKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	opts := validContainerAnalysisOptions()
	opts.ProjectDir = projectDir
	opts.OutDir = dataDir
	opts.BuildsPerJob = 1
	opts.Workers = 1
	opts.Timeout = time.Minute

	err := Run(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "initialize Orka container analysis: validate project bundle source") {
		t.Fatalf("Run error = %v", err)
	}
	if patternCalls.Load() != 0 {
		t.Fatalf("pattern analysis calls = %d, want 0", patternCalls.Load())
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
	after := hashFileTree(t, dataDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after project setup failure\nbefore=%v\nafter=%v", before, after)
	}
}

func TestOrkaProjectSetupFailureAfterSchedulingRollsBackRefresh(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	writeFixtureFile(t, bucketDir, "logs/periodic-test/1/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails-one" classname="suite"><failure message="failed">failed</failure></testcase><testcase name="fails-two" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)
	before := hashFileTree(t, dataDir)
	projectDir := writeSymlinkedFetcherProject(t)
	setupErr := analysisruntime.ValidateProjectBundleSource(projectDir)
	if setupErr == nil || !analysisruntime.IsProjectBundleSourceError(setupErr) {
		t.Fatalf("setup error = %v", setupErr)
	}
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{state: state, dataDir: dataDir, mutate: true, err: setupErr}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	var patternCalls atomic.Int64
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error {
		patternCalls.Add(1)
		return nil
	}
	t.Cleanup(func() {
		newEmailSender = oldEmailSender
		analyzePatternsAcrossBuilds = oldPatternAnalysis
	})

	_, err = p.fullPass(t.Context())
	if err == nil || !analysisruntime.IsProjectBundleSourceError(err) || !strings.Contains(err.Error(), "systemic Orka project setup failure") {
		t.Fatalf("fullPass error = %v", err)
	}
	if analyzer.calls.Load() != 1 {
		t.Fatalf("analyzer calls = %d, want 1", analyzer.calls.Load())
	}
	if patternCalls.Load() != 0 {
		t.Fatalf("pattern analysis calls = %d, want 0", patternCalls.Load())
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
	after := hashFileTree(t, dataDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after scheduled setup failure\nbefore=%v\nafter=%v", before, after)
	}
}

func writeSymlinkedFetcherProject(t *testing.T) string {
	t.Helper()
	projectDir := t.TempDir()
	projectTarget := filepath.Join(t.TempDir(), "project.yaml")
	writeFixtureFile(t, filepath.Dir(projectTarget), filepath.Base(projectTarget), `id: test
name: Test
testgrid:
  dashboard: test
storage:
  provider: local
  base: /fixtures
branding:
  title: Test
  base_path: /
  site_url: https://dashboard.example.test
ai:
  tools: [filesystem]
`)
	if err := os.Symlink(projectTarget, filepath.Join(projectDir, "project.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projectDir, "prompts/system.md", "Investigate build artifacts.\n")
	return projectDir
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
	configureRefreshLifecycleRuntime(t, p, dataDir)
	p.progress = fetchprogress.New(dataDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, _ []models.JobDetail, options patterns.AnalyzeOptions) error {
		options.OnPlan(1)
		options.OnAttempt(patterns.Attempt{Number: 1, FailureCategory: ai.PatternFailureAmbiguous})
		options.OnAttempt(patterns.Attempt{Number: 2, Retry: true, Succeeded: true, Final: true})
		return nil
	}
	t.Cleanup(func() { analyzePatternsAcrossBuilds = oldPatternAnalysis })

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
	patternProgress := p.progress.Snapshot().Patterns
	if patternProgress.Attempts != 2 || patternProgress.Retries != 1 || patternProgress.Completed != 1 || patternProgress.Failed != 0 {
		t.Fatalf("pattern progress = %+v", patternProgress)
	}
}

func TestPatternFailurePreservesRefreshState(t *testing.T) {
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
	configureRefreshLifecycleRuntime(t, p, dataDir)
	p.progress = fetchprogress.New(dataDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	sender := &countingNotifySender{}
	oldEmailSender := newEmailSender
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
	oldPatternAnalysis := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, _ []models.JobDetail, options patterns.AnalyzeOptions) error {
		options.OnPlan(1)
		options.OnAttempt(patterns.Attempt{Number: 1, FailureCategory: ai.PatternFailureRequestTimeout})
		options.OnAttempt(patterns.Attempt{Number: 2, Retry: true, Final: true, FailureCategory: ai.PatternFailureProvider5xx})
		return &ai.PatternProviderError{StatusCode: http.StatusServiceUnavailable}
	}
	t.Cleanup(func() {
		newEmailSender = oldEmailSender
		analyzePatternsAcrossBuilds = oldPatternAnalysis
	})

	if _, err := p.fullPass(t.Context()); err == nil || !strings.Contains(err.Error(), "cross-build pattern analysis") {
		t.Fatalf("fullPass error = %v", err)
	}
	if sender.calls.Load() != 0 {
		t.Fatalf("notification sends = %d, want 0", sender.calls.Load())
	}
	if p.aiRuntime != nil || p.containerAnalyzer != nil {
		t.Fatal("failed pattern refresh retained in-memory AI state")
	}
	patternProgress := p.progress.Snapshot().Patterns
	if patternProgress.Attempts != 2 || patternProgress.Retries != 1 || patternProgress.Completed != 0 || patternProgress.Failed != 1 ||
		patternProgress.FailureCategory != fetchprogress.PatternFailureProvider5xx {
		t.Fatalf("pattern progress = %+v", patternProgress)
	}
	if after := hashFileTree(t, dataDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after pattern failure\nbefore=%v\nafter=%v", before, after)
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

func TestAuthorizationRollbackInvalidatesMergedContainerState(t *testing.T) {
	dataDir, bucketDir := installRefreshLifecycleFixture(t)
	writeFixtureFile(t, bucketDir, "logs/periodic-test/1/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails-a" classname="suite"><failure message="failed">failed</failure></testcase><testcase name="fails-b" classname="suite"><failure message="failed">failed</failure></testcase></testsuite>`)
	before := hashFileTree(t, dataDir)
	merged := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"version":1}`})
			return
		}
		<-merged
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &resultLifecycleAnalyzer{
		client: orka.NewResultClient(server.URL, "token"), state: state, dataDir: dataDir,
		merge: true, merged: merged,
		result: ai.FailureAnalysisResult{
			Summary: &models.AISummary{GeneratedAt: "2026-07-27T00:00:00Z", Summary: "analyzed"},
			Analysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-27T00:00:00Z", RootCause: "configuration drift", Severity: "High",
				SuggestedFix: "update configuration", Mode: "agentic", EvidencePlanCovered: true,
			},
		},
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
	p.opts.AnalysisRuntime.OrkaContainer.MaxConcurrent = 2
	if _, err := p.fullPass(t.Context()); err == nil || !orka.IsResultAuthorizationError(err) {
		t.Fatalf("fullPass error = %v", err)
	}
	if p.containerAnalyzer != nil {
		t.Fatal("aborted container analyzer remained cached")
	}
	if after := hashFileTree(t, dataDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("data directory changed after rollback\nbefore=%v\nafter=%v", before, after)
	}

	nextServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer nextServer.Close()
	nextState, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	nextAnalyzer := &resultLifecycleAnalyzer{client: orka.NewResultClient(nextServer.URL, "token"), state: nextState, dataDir: dataDir}
	p.containerAnalyzer = nextAnalyzer
	if _, err := p.fullPass(t.Context()); err == nil || !orka.IsResultAuthorizationError(err) {
		t.Fatalf("next fullPass error = %v", err)
	}
	if nextAnalyzer.seeds.Load() != 0 {
		t.Fatalf("next pass reused %d aborted cache entries", nextAnalyzer.seeds.Load())
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
