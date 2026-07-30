package fetcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func validContainerAnalysisOptions() Options {
	return Options{
		EnableAI: true,
		AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer,
			OrkaContainer: OrkaContainerAnalysisOptions{
				Namespace: "orka-system", ResultAPI: "http://orka.orka-system.svc.cluster.local:8080", Image: "analyzer:sha-deadbeef",
				ModelSecretName: "model-secret", ModelTokenKey: "token",
				StateSecretName: "state-secret", StateSecretKey: "state-key",
				MaxConcurrent: 2, PollInterval: time.Second, TaskTimeout: 10 * time.Minute, Retries: 1,
				NodeSelector: map[string]string{"agentpool": "nodepool1"},
			},
		},
	}
}

func TestValidateAnalysisRuntimeOptions(t *testing.T) {
	t.Setenv("PROW_AI_STATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	if err := validateAnalysisRuntimeOptions(Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess}}); err != nil {
		t.Fatal(err)
	}
	if err := validateAnalysisRuntimeOptions(validContainerAnalysisOptions()); err != nil {
		t.Fatal(err)
	}
	unknown := Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: "remote"}}
	if err := validateAnalysisRuntimeOptions(unknown); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown runtime error = %v", err)
	}
	gpu := validContainerAnalysisOptions()
	gpu.AnalysisRuntime.OrkaContainer.NodeSelector = map[string]string{"agentpool": "h100"}
	if err := validateAnalysisRuntimeOptions(gpu); err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("GPU placement error = %v", err)
	}
	accelerator := validContainerAnalysisOptions()
	accelerator.AnalysisRuntime.OrkaContainer.NodeSelector["cloud.google.com/gke-accelerator"] = "nvidia-tesla-t4"
	if err := validateAnalysisRuntimeOptions(accelerator); err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("accelerator placement error = %v", err)
	}
	missing := validContainerAnalysisOptions()
	missing.AnalysisRuntime.OrkaContainer.ModelSecretName = ""
	if err := validateAnalysisRuntimeOptions(missing); err == nil || !strings.Contains(err.Error(), "model Secret") {
		t.Fatalf("missing model Secret error = %v", err)
	}
	slowPoll := validContainerAnalysisOptions()
	slowPoll.AnalysisRuntime.OrkaContainer.PollInterval = 30 * time.Second
	if err := validateAnalysisRuntimeOptions(slowPoll); err == nil || !strings.Contains(err.Error(), "less than 30s") {
		t.Fatalf("slow poll error = %v", err)
	}
}

func TestValidateContainerAnalysisStateKey(t *testing.T) {
	t.Setenv("PROW_AI_STATE_KEY", "not-base64")
	err := validateAnalysisRuntimeOptions(validContainerAnalysisOptions())
	if err == nil || !strings.Contains(err.Error(), "state key") {
		t.Fatalf("state key error = %v", err)
	}
}

func TestRunWatchAcceptsContainerAnalysis(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
ai:
  timeout: 30s
  tools: [filesystem]
`, dir)
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_TOKEN", "dashboard-token")
	t.Setenv("AI_API", "chat_completions")
	t.Setenv("AI_ENDPOINT", "https://helm.invalid/v1/chat/completions")
	t.Setenv("AI_MODEL", "helm-model")
	t.Setenv("PROW_AI_STATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	opts := validContainerAnalysisOptions()
	opts.ProjectDir = dir
	opts.OutDir = t.TempDir()
	opts.BuildsPerJob = 1
	opts.Workers = 1
	opts.Timeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunWatch(ctx, opts, time.Second, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunWatch error = %v, want cancellation after valid setup", err)
	}
}

func TestSetupPipelineContainerUsesHelmProviderCoordinates(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: project
ai:
  api: responses
  endpoint: https://project.invalid/v1/responses
  model: project-model
  timeout: 30m
  tools: [filesystem]
`, dir)
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_TOKEN", "dashboard-token")
	t.Setenv("AI_API", "chat_completions")
	t.Setenv("AI_ENDPOINT", "https://helm.invalid/v1/chat/completions")
	t.Setenv("AI_MODEL", "helm-model")
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "128000")
	t.Setenv("PROW_AI_STATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	opts := validContainerAnalysisOptions()
	opts.ProjectDir = dir
	opts.OutDir = t.TempDir()
	opts.BuildsPerJob = 1
	opts.Workers = 1
	opts.Timeout = time.Minute
	opts.AnalysisRuntime.OrkaContainer.TaskTimeout = 32 * time.Minute
	pipeline, err := setupPipeline(opts)
	if err != nil {
		t.Fatal(err)
	}
	provider := pipeline.aiProject.Provider
	if provider.API != "chat_completions" || provider.Endpoint != "https://helm.invalid/v1/chat/completions" || provider.Model != "helm-model" {
		t.Fatalf("provider = %+v", provider)
	}
	if got := pipeline.opts.AnalysisRuntime.OrkaContainer.ContextWindowTokens; got != 128000 {
		t.Fatalf("context window tokens = %d, want 128000", got)
	}
	opts.AnalysisRuntime.OrkaContainer.TaskTimeout = 32*time.Minute - time.Second
	if _, err := setupPipeline(opts); err == nil || !strings.Contains(err.Error(), "ai.timeout") {
		t.Fatalf("short task timeout error = %v", err)
	}
}

type maintenanceOnlyAnalyzer struct {
	called bool
}

func (a *maintenanceOnlyAnalyzer) Maintain(context.Context) error {
	a.called = true
	return nil
}

func (a *maintenanceOnlyAnalyzer) AnalyzeFailure(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	return ai.FailureAnalysisResult{}, nil
}

func (a *maintenanceOnlyAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return nil }

func TestAnalyzeFailuresNoWorkStillRunsContainerMaintenance(t *testing.T) {
	analyzer := &maintenanceOnlyAnalyzer{}
	pipeline := &pipeline{
		opts:              Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeOrkaContainer}},
		containerAnalyzer: analyzer,
	}
	if err := pipeline.analyzeFailuresWithAI(t.Context(), nil, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}
	if !analyzer.called {
		t.Fatal("container maintenance was skipped")
	}
}

type blockingAnalysisAnalyzer struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan struct{}
}

func (a *blockingAnalysisAnalyzer) Maintain(context.Context) error { return nil }

func (a *blockingAnalysisAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.maxActive {
		a.maxActive = a.active
	}
	if a.active == 2 {
		select {
		case a.started <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	<-ctx.Done()
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	return ai.UnavailableFailureAnalysisResult(request.TestCase, ctx.Err()), ctx.Err()
}

func (a *blockingAnalysisAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return nil }

func TestAnalyzeFailuresContainerConcurrencyAndCancellation(t *testing.T) {
	analyzer := &blockingAnalysisAnalyzer{started: make(chan struct{}, 1)}
	p := &pipeline{
		opts: Options{AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 2},
		}},
		cfg:               &project.Config{AI: &project.AI{Concurrency: 5}},
		client:            &http.Client{},
		containerAnalyzer: analyzer,
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{
				{Name: "one", Status: "failed"}, {Name: "two", Status: "failed"}, {Name: "three", Status: "failed"},
				{Name: "four", Status: "failed"}, {Name: "five", Status: "failed"},
			},
		}},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.analyzeFailuresWithAI(ctx, details, models.FlakinessReport{}) }()
	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("two analyses did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("analysis error = %v", err)
	}
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	if analyzer.maxActive != 2 || analyzer.active != 0 {
		t.Fatalf("active=%d max=%d, want active=0 max=2", analyzer.active, analyzer.maxActive)
	}
}

func TestPassExecutionContextsDetachDurableContainerWork(t *testing.T) {
	root := context.Background()
	bounded, cancel := context.WithTimeout(root, time.Second)
	defer cancel()
	analysis, sideEffects := passExecutionContexts(root, bounded, AnalysisRuntimeOrkaContainer)
	if _, ok := analysis.Deadline(); ok {
		t.Fatal("container analysis inherited the fetch deadline")
	}
	if _, ok := sideEffects.Deadline(); ok {
		t.Fatal("container side effects inherited the fetch deadline")
	}
	analysis, sideEffects = passExecutionContexts(root, bounded, AnalysisRuntimeInProcess)
	if _, ok := analysis.Deadline(); !ok {
		t.Fatal("in-process analysis lost the pass deadline")
	}
	if _, ok := sideEffects.Deadline(); !ok {
		t.Fatal("in-process side effects lost the pass deadline")
	}
}

func TestAnalyzeFailuresInProcessCancellationDoesNotPersistCheckpoint(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		AI:      &project.AI{Agentic: project.Agentic{Tools: []string{"filesystem"}}},
		Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: bucketDir},
	}
	p := &pipeline{
		opts: Options{OutDir: dataDir, AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess}},
		cfg:  cfg, client: &http.Client{}, backend: backend,
		aiProject: &analysisruntime.Project{
			Config: cfg,
			Provider: project.AIProvider{
				API: project.AIAPIChatCompletions, Endpoint: "http://model.invalid/v1/chat/completions", Model: "test-model",
			},
			SystemPrompt: "test prompt",
		},
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "failed"}},
		}},
	}}
	persistCalls := 0
	oldSave := saveAnalysisRuntimeCache
	saveAnalysisRuntimeCache = func(*analysisruntime.Runtime) error {
		persistCalls++
		return nil
	}
	t.Cleanup(func() { saveAnalysisRuntimeCache = oldSave })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.analyzeFailuresWithAI(ctx, details, models.FlakinessReport{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("analysis error = %v", err)
	}
	if persistCalls != 0 {
		t.Fatalf("checkpoint persistence calls = %d, want 0", persistCalls)
	}
}

func TestCollectAIWorkPrioritizesMissingBuildAnalysis(t *testing.T) {
	reusable := models.TestCase{
		Name: "reusable", Status: "failed",
		AISummary:  &models.AISummary{Summary: "cached"},
		AIAnalysis: &models.AIAnalysis{Mode: ai.AgenticMode, CritiquePassed: true},
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{reusable}},
			{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "junit-one", Status: "failed"}, {Name: "junit-two", Status: "failed"}}},
			{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{{Name: "build", Source: models.TestCaseSourceBuild, Status: "failed"}}},
		},
	}}

	work := collectAIWork(details)
	if len(work) != 4 {
		t.Fatalf("work items = %d, want 4", len(work))
	}
	got := []string{work[0].tc.Name, work[1].tc.Name, work[2].tc.Name, work[3].tc.Name}
	want := []string{"build", "junit-one", "junit-two", "reusable"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("work order = %v, want %v", got, want)
	}
}

type priorityBlockingAnalyzer struct {
	started chan ai.FailureAnalysisRequest
	mu      sync.Mutex
	active  int
	max     int
}

func (a *priorityBlockingAnalyzer) Maintain(context.Context) error { return nil }

func (a *priorityBlockingAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.max {
		a.max = a.active
	}
	a.mu.Unlock()
	select {
	case a.started <- request:
	case <-ctx.Done():
	}
	<-ctx.Done()
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	return ai.UnavailableFailureAnalysisResult(request.TestCase, ctx.Err()), ctx.Err()
}

func (a *priorityBlockingAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return nil }

func TestAnalyzeFailuresSubmitsBuildWorkFirstWithoutExtraConcurrency(t *testing.T) {
	analyzer := &priorityBlockingAnalyzer{started: make(chan ai.FailureAnalysisRequest, 1)}
	p := &pipeline{
		opts: Options{AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 1},
		}},
		cfg:               &project.Config{AI: &project.AI{Concurrency: 5}},
		client:            &http.Client{},
		containerAnalyzer: analyzer,
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{
				{Name: "junit", Status: "failed"},
				{Name: "build", Source: models.TestCaseSourceBuild, Status: "failed"},
			},
		}},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.analyzeFailuresWithAI(ctx, details, models.FlakinessReport{}) }()
	select {
	case request := <-analyzer.started:
		if request.TestCase.Source != models.TestCaseSourceBuild {
			t.Fatalf("first submitted source = %q, want build", request.TestCase.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("no analysis started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("analysis error = %v", err)
	}
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	if analyzer.max != 1 || analyzer.active != 0 {
		t.Fatalf("active=%d max=%d, want active=0 max=1", analyzer.active, analyzer.max)
	}
}
