package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/models"
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
		AIAnalysis: &models.AIAnalysis{Mode: ai.AgenticMode, CritiquePassed: true, Disposition: models.AnalysisDispositionGrounded},
	}
	details := []models.JobDetail{{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{reusable}},
			{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "junit-one", Status: "failed"}, {Name: "junit-two", Status: "failed"}}},
			{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{{Name: "build", Source: models.TestCaseSourceBuild, Status: "failed"}}},
		},
	}}

	work := collectAIWork(t.Context(), nil, details, nil, nil)
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

func (p namedAnalysisPlanner) NeedsAnalysis(_ context.Context, _ *http.Client, _ *models.BuildResult, tc *models.TestCase, _ int) bool {
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
	work := collectAIWork(t.Context(), nil, details, nil, namedAnalysisPlanner{"stale": true})
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
