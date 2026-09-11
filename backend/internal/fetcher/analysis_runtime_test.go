package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/storage"
)

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
		opts: Options{OutDir: dataDir},
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
	if err := p.analyzeFailuresWithAI(ctx, details); !errors.Is(err, context.Canceled) {
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
		AIAnalysis: &models.AIAnalysis{Mode: ai.AgenticMode, CritiquePassed: true, Disposition: models.AnalysisDispositionCitationsVerified},
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{reusable}},
			{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "junit-one", Status: "failed"}, {Name: "junit-two", Status: "failed"}}},
			{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{{Name: "build", Source: models.TestCaseSourceBuild, Status: "failed"}}},
		},
	}}

	work := collectAIWork(details, nil)
	if len(work) != 4 {
		t.Fatalf("work items = %d, want 4", len(work))
	}
	got := []string{work[0].tc.Name, work[1].tc.Name, work[2].tc.Name, work[3].tc.Name}
	want := []string{"build", "junit-one", "junit-two", "reusable"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("work order = %v, want %v", got, want)
	}
}

type namedAnalysisPlanner map[string]bool

func (p namedAnalysisPlanner) NeedsAnalysis(tc *models.TestCase) bool {
	return p[tc.Name]
}

func TestCollectAIWorkUsesCurrentStalenessPlanner(t *testing.T) {
	analysis := &models.AIAnalysis{Mode: ai.AgenticMode, CritiquePassed: true}
	summary := &models.AISummary{Summary: "existing"}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		ConfigFile: "config/jobs/example/periodics.yaml", ConfigRevision: strings.Repeat("b", 40),
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1"},
			TestCases: []models.TestCase{
				{Name: "reusable", Status: "failed", AISummary: summary, AIAnalysis: analysis},
				{Name: "stale", Status: "failed", AISummary: summary, AIAnalysis: analysis},
			},
		}},
	}}
	work := collectAIWork(details, namedAnalysisPlanner{"stale": true})
	if len(work) != 2 || work[0].tc.Name != "stale" || work[1].tc.Name != "reusable" {
		t.Fatalf("work order = %v, %v", work[0].tc.Name, work[1].tc.Name)
	}
	request := work[0].request(2, "")
	if request.ProwJob == nil || request.ProwJob.Name != "job" || request.ProwJob.JobType != models.JobTypePeriodic || request.ProwJob.ConfigFile != "config/jobs/example/periodics.yaml" || request.ProwJob.ConfigRevision != strings.Repeat("b", 40) {
		t.Fatalf("Prow job context = %+v", request.ProwJob)
	}
	if request.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures = %d, want 2", request.ConsecutiveFailures)
	}
}

type progressResultAnalyzer struct {
	cold int
	warm int
}

func (a *progressResultAnalyzer) AnalyzeFailure(_ context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	if request.TestCase.AIAnalysis == nil {
		a.cold++
	} else {
		a.warm++
	}
	return ai.FailureAnalysisResult{
		Summary: &models.AISummary{Summary: "analyzed"},
		Analysis: &models.AIAnalysis{
			Mode: ai.AgenticMode, CritiquePassed: true, Disposition: models.AnalysisDispositionCitationsVerified,
		},
	}, nil
}

func TestAnalyzeFailuresProgressTracksColdAndWarmResultsAsLogicalCompletions(t *testing.T) {
	p := progressTestPipeline(t)
	analyzer := &progressResultAnalyzer{}
	oldAnalyzer := newAnalysisAnalyzer
	newAnalysisAnalyzer = func(*ai.Service) ai.FailureAnalyzer { return analyzer }
	oldPatterns := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(_ context.Context, _ *ai.Service, _ []models.JobDetail, options patterns.AnalyzeOptions) error {
		options.OnPlan(0)
		return nil
	}
	t.Cleanup(func() {
		newAnalysisAnalyzer = oldAnalyzer
		analyzePatternsAcrossBuilds = oldPatterns
	})

	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{
				{Name: "junit", Status: "failed", FailureMessage: "failed"},
				{Name: "build", Source: models.TestCaseSourceBuild, Status: "failed", FailureMessage: "failed"},
			},
		}},
	}}

	for pass := 0; pass < 2; pass++ {
		p.progress.StartPass(fetchprogress.PassLightweightWatch)
		if err := p.analyzeFailuresWithAI(t.Context(), details); err != nil {
			t.Fatalf("analysis pass %d: %v", pass+1, err)
		}
		status := p.progress.Snapshot()
		want := fetchprogress.AnalysisProgress{
			LogicalTotal: 2, Completed: 2, CheckpointCommitted: true,
			BuildSubjects: fetchprogress.BuildAnalysisProgress{LogicalTotal: 1, Completed: 1},
		}
		if status.Analyses != want {
			t.Fatalf("analysis progress pass %d = %+v, want %+v", pass+1, status.Analyses, want)
		}
	}
	if analyzer.cold != 2 || analyzer.warm != 2 {
		t.Fatalf("analyzer inputs cold=%d warm=%d, want 2 each", analyzer.cold, analyzer.warm)
	}

	data, err := json.Marshal(p.progress.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	analyses := raw["analyses"].(map[string]any)
	for _, retired := range []string{
		"accepted_cache_hits", "compatible_results_reused", "exact_results_reused",
		"same_failure_results_reused", "same_failure_groups", "same_failure_candidates",
		"potential_tasks_saved", "cache_rejections", "task_attempts", "retries",
		"results_retrieved", "fresh_analyses_completed", "result_retrieval_retries",
	} {
		if _, ok := analyses[retired]; ok {
			t.Fatalf("production progress retained unsupported field %q: %s", retired, data)
		}
	}
}

type blockingProgressAnalyzer struct {
	started chan struct{}
}

func (a *blockingProgressAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, _ ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	select {
	case a.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ai.FailureAnalysisResult{}, ctx.Err()
}

func TestAnalyzeFailuresQueuedCancellationRemainsLogicallyConsistent(t *testing.T) {
	p := progressTestPipeline(t)
	analyzer := &blockingProgressAnalyzer{started: make(chan struct{}, 1)}
	oldAnalyzer := newAnalysisAnalyzer
	newAnalysisAnalyzer = func(*ai.Service) ai.FailureAnalyzer { return analyzer }
	t.Cleanup(func() { newAnalysisAnalyzer = oldAnalyzer })

	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
			TestCases: []models.TestCase{
				{Name: "first", Status: "failed", FailureMessage: "failed"},
				{Name: "second", Status: "failed", FailureMessage: "failed"},
			},
		}},
	}}
	p.progress.StartPass(fetchprogress.PassLightweightWatch)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.analyzeFailuresWithAI(ctx, details) }()
	<-analyzer.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("analysis error = %v, want cancellation", err)
	}
	got := p.progress.Snapshot().Analyses
	want := fetchprogress.AnalysisProgress{LogicalTotal: 2, Cancelled: 2}
	if got != want {
		t.Fatalf("cancelled analysis progress = %+v, want %+v", got, want)
	}
}

func progressTestPipeline(t *testing.T) *pipeline {
	t.Helper()
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		AI:      &project.AI{Concurrency: 1, Agentic: project.Agentic{Tools: []string{"filesystem"}}},
		Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: bucketDir},
	}
	aiProject := &analysisruntime.Project{
		Config: cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: "http://model.invalid/v1/chat/completions", Model: "test-model",
		},
		SystemPrompt: "test prompt",
	}
	runtime, err := analysisruntime.New(t.Context(), analysisruntime.Options{DataDir: dataDir, Project: aiProject})
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline{
		opts: Options{OutDir: dataDir}, cfg: cfg, client: &http.Client{}, backend: backend,
		aiProject: aiProject, aiRuntime: runtime, progress: fetchprogress.New(dataDir, "sha-test"),
	}
}
