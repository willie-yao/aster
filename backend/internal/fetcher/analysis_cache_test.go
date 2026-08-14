package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type cachePlanningAnalyzer struct {
	state          *analysisruntime.ContainerStateStore
	maintainCalls  atomic.Int64
	preflightCalls atomic.Int64
	analyzeCalls   atomic.Int64
}

func (a *cachePlanningAnalyzer) Maintain(context.Context) error {
	a.maintainCalls.Add(1)
	return nil
}

func (a *cachePlanningAnalyzer) Preflight(context.Context) error {
	a.preflightCalls.Add(1)
	return nil
}

func (a *cachePlanningAnalyzer) AnalyzeFailure(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.analyzeCalls.Add(1)
	return ai.FailureAnalysisResult{}, fmt.Errorf("unexpected analyzer execution")
}

func (a *cachePlanningAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return a.state }

func TestAnalyzeFailuresContainerPrivateCacheHitSkipsTask(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	dataDir := t.TempDir()
	projectConfig := &project.Config{
		AI:      &project.AI{Concurrency: 1, Agentic: project.Agentic{MinToolCalls: 2, Tools: []string{"filesystem"}}},
		Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: t.TempDir()},
	}
	analysisProject := testCacheAnalysisProject(projectConfig)
	request := testCacheRequest()
	writePrivateAnalysisCache(t, dataDir, request, privateCacheData(t, analysisProject, request, false, ""), time.Now().UTC())
	state, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewLocalBackend(projectConfig.Storage.Base, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &cachePlanningAnalyzer{state: state}
	tracker := fetchprogress.New(dataDir, "sha-new-image")
	tracker.StartPass(fetchprogress.PassInitialWatch)
	p := &pipeline{
		opts: Options{OutDir: dataDir, AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{Image: "dashboard-analyzer:sha-new-image", MaxConcurrent: 1},
		}},
		cfg: projectConfig, client: &http.Client{}, backend: backend, aiProject: analysisProject,
		containerAnalyzer: analyzer, progress: tracker,
	}
	details := testCacheDetails()
	oldPatterns := analyzePatternsAcrossBuilds
	analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error { return nil }
	t.Cleanup(func() { analyzePatternsAcrossBuilds = oldPatterns })

	if err := p.analyzeFailuresWithAI(t.Context(), details, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}
	if analyzer.maintainCalls.Load() != 1 || analyzer.preflightCalls.Load() != 0 || analyzer.analyzeCalls.Load() != 0 {
		t.Fatalf("analyzer calls: maintain=%d preflight=%d analyze=%d", analyzer.maintainCalls.Load(), analyzer.preflightCalls.Load(), analyzer.analyzeCalls.Load())
	}
	got := details[0].Runs[0].TestCases[0]
	if got.AISummary == nil || got.AIAnalysis == nil || !got.AIAnalysis.CacheHit {
		t.Fatalf("cached result was not applied: %+v", got)
	}
	progress := tracker.Snapshot().Analyses
	if progress.LogicalTotal != 1 || progress.AcceptedCacheHits != 1 || progress.Completed != 1 || progress.Queued != 0 || progress.TaskAttempts != 0 || progress.ResultsRetrieved != 0 || !progress.CheckpointCommitted {
		t.Fatalf("analysis progress = %+v", progress)
	}
}

func TestPlanContainerAnalysisWorkUsesStickyCachePolicy(t *testing.T) {
	projectConfig := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2}}}
	analysisProject := testCacheAnalysisProject(projectConfig)
	request := testCacheRequest()
	baseData := privateCacheData(t, analysisProject, request, false, "")
	cases := []struct {
		name         string
		data         json.RawMessage
		consecutive  int
		wantQueued   int
		wantNew      int
		wantAccepted int
		wantMissing  int
	}{
		{name: "missing", wantQueued: 1, wantNew: 1, wantMissing: 1},
		{name: "prompt changed", data: privateCacheData(t, analysisProject, request, false, "old-prompt"), wantAccepted: 1},
		{name: "model endpoint and skill changed", data: mutatePrivateCacheData(t, baseData, map[string]any{
			"skill_set_hash": "old-skills", "model_hash": "old-model",
		}), wantAccepted: 1},
		{name: "transient verdict became persistent", data: privateCacheData(t, analysisProject, request, true, ""), consecutive: 3, wantAccepted: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.data != nil {
				writePrivateAnalysisCache(t, dir, request, tc.data, time.Now().UTC())
			}
			state, err := analysisruntime.NewContainerStateStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			analyzer := &cachePlanningAnalyzer{state: state}
			details := testCacheDetails()
			work := []aiWork{{
				jobID: request.JobID, buildPrefix: request.BuildPrefix, run: &details[0].Runs[0], tc: &details[0].Runs[0].TestCases[0],
			}}
			consecutive := map[string]int{}
			if tc.consecutive > 0 {
				consecutive[request.JobID+"::"+request.TestCase.Name] = tc.consecutive
			}
			planner := analysisruntime.NewReusePlanner(analysisProject)
			queued, plan, err := planContainerAnalysisWork(t.Context(), &http.Client{}, work, analyzer, planner, analysisProject, consecutive)
			if err != nil {
				t.Fatal(err)
			}
			if len(queued) != tc.wantQueued || plan.LogicalTotal != 1 || plan.Queued != tc.wantQueued || plan.NewWork != tc.wantNew ||
				plan.AcceptedCacheHits != tc.wantAccepted || plan.CacheRejections.Missing != tc.wantMissing {
				t.Fatalf("plan = %+v", plan)
			}
			got := details[0].Runs[0].TestCases[0]
			if tc.wantAccepted == 1 {
				if got.AIAnalysis == nil || !got.AIAnalysis.CacheHit || got.AISummary == nil {
					t.Fatalf("accepted cache result was not applied: %+v", got)
				}
			} else if got.AIAnalysis != nil {
				t.Fatal("rejected cache result was applied")
			}
		})
	}
}

func testCacheAnalysisProject(cfg *project.Config) *analysisruntime.Project {
	return &analysisruntime.Project{
		Config: cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model",
		},
		SystemPrompt: "system prompt",
	}
}

func testCacheRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1", Build: models.BuildInfo{BuildID: "1", JobName: "job"},
		TestCase: models.TestCase{Name: "test", Status: "failed", FailureMessage: "failed"},
		ProwJob: &ai.ProwJobContext{
			Name: "job", JobType: models.JobTypePeriodic, ConfigFile: "config/jobs/example/periodics.yaml", ConfigRevision: strings.Repeat("a", 40),
		},
	}
}

func testCacheDetails() []models.JobDetail {
	request := testCacheRequest()
	return []models.JobDetail{{
		Name: "job", JobID: request.JobID, JobType: models.JobTypePeriodic,
		ConfigFile: request.ProwJob.ConfigFile, ConfigRevision: request.ProwJob.ConfigRevision,
		Runs: []models.BuildResult{{BuildInfo: request.Build, TestCases: []models.TestCase{request.TestCase}}},
	}}
}

func privateCacheData(t *testing.T, analysisProject *analysisruntime.Project, request ai.FailureAnalysisRequest, transient bool, promptHash string) json.RawMessage {
	t.Helper()
	if promptHash == "" {
		planner := analysisruntime.NewReusePlanner(analysisProject)
		run := models.BuildResult{BuildInfo: request.Build}
		testCase := request.TestCase
		promptHash = planner.FailureCachePolicy(t.Context(), &http.Client{}, &run, &testCase, max(1, request.ConsecutiveFailures)).PromptHash
	}
	data := map[string]any{
		"summary": "summary", "is_transient": transient, "root_cause": "root cause", "severity": "High", "suggested_fix": "fix", "relevant_files": []string{},
		"tool_calls": 2, "critique_passed": true, "critique_version": 999,
		"model_hash":  ai.ModelFingerprint(analysisProject.Provider.API, analysisProject.Provider.Endpoint, analysisProject.Provider.Model),
		"prompt_hash": promptHash,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutatePrivateCacheData(t *testing.T, raw json.RawMessage, values map[string]any) json.RawMessage {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for key, value := range values {
		data[key] = value
	}
	updated, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func writePrivateAnalysisCache(t *testing.T, dir string, request ai.FailureAnalysisRequest, data json.RawMessage, createdAt time.Time) {
	t.Helper()
	key := analysisruntime.FailureCacheKey(request)
	entries := map[string]ai.CacheEntry{key: {Key: key, CreatedAt: createdAt, Data: data}}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ai.CacheFilename), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanContainerAnalysisWorkAcceptsBuildCache(t *testing.T) {
	projectConfig := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2, MinGCSBytes: 50}}}
	analysisProject := testCacheAnalysisProject(projectConfig)
	request := testCacheRequest()
	request.TestCase.Source = models.TestCaseSourceBuild
	dir := t.TempDir()
	writePrivateAnalysisCache(t, dir, request, privateCacheData(t, analysisProject, request, false, ""), time.Now().UTC())
	state, err := analysisruntime.NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := &cachePlanningAnalyzer{state: state}
	run := models.BuildResult{BuildInfo: request.Build, TestCases: []models.TestCase{request.TestCase}}
	work := []aiWork{{jobID: request.JobID, buildPrefix: request.BuildPrefix, run: &run, tc: &run.TestCases[0]}}
	planner := analysisruntime.NewReusePlanner(analysisProject)
	queued, plan, err := planContainerAnalysisWork(t.Context(), &http.Client{}, work, analyzer, planner, analysisProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 || plan.LogicalTotal != 1 || plan.AcceptedCacheHits != 1 || plan.Queued != 0 ||
		plan.BuildSubjects != (fetchprogress.BuildAnalysisProgress{LogicalTotal: 1, Completed: 1, AcceptedCacheHits: 1}) {
		t.Fatalf("plan = %+v", plan)
	}
	if run.TestCases[0].AIAnalysis == nil || !run.TestCases[0].AIAnalysis.CacheHit {
		t.Fatalf("build cache result was not applied: %+v", run.TestCases[0])
	}
}

type exactPlanningAnalyzer struct {
	*compatiblePlanningAnalyzer
	exactResult ai.FailureAnalysisResult
	exactReused bool
	exactCalls  atomic.Int64
}

func (a *exactPlanningAnalyzer) ReuseExactResult(context.Context, ai.FailureAnalysisRequest, ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error) {
	a.exactCalls.Add(1)
	return a.exactResult, a.exactReused, nil
}

type compatiblePlanningAnalyzer struct {
	*cachePlanningAnalyzer
	result ai.FailureAnalysisResult
	calls  atomic.Int64
}

func (a *compatiblePlanningAnalyzer) ReuseCompatibleResult(context.Context, ai.FailureAnalysisRequest, ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error) {
	a.calls.Add(1)
	return a.result, true, nil
}

func TestPlanContainerAnalysisWorkReusesExactResultBeforeCompatibleFallback(t *testing.T) {
	projectConfig := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2}}}
	analysisProject := testCacheAnalysisProject(projectConfig)
	state, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := testCacheRequest()
	request.TestCase.Source = models.TestCaseSourceBuild
	run := models.BuildResult{BuildInfo: request.Build, TestCases: []models.TestCase{request.TestCase}}
	exactResult := ai.FailureAnalysisResult{
		Summary:  &models.AISummary{Summary: "exact"},
		Analysis: &models.AIAnalysis{Mode: ai.AgenticMode, RootCause: "exact root"},
	}
	compatible := &compatiblePlanningAnalyzer{
		cachePlanningAnalyzer: &cachePlanningAnalyzer{state: state},
		result:                ai.FailureAnalysisResult{Summary: &models.AISummary{Summary: "compatible"}},
	}
	analyzer := &exactPlanningAnalyzer{compatiblePlanningAnalyzer: compatible, exactResult: exactResult, exactReused: true}
	planner := analysisruntime.NewReusePlanner(analysisProject)
	work := []aiWork{{jobID: request.JobID, buildPrefix: request.BuildPrefix, run: &run, tc: &run.TestCases[0]}}
	queued, plan, err := planContainerAnalysisWork(t.Context(), &http.Client{}, work, analyzer, planner, analysisProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 || plan.LogicalTotal != 1 || plan.ExactResultsReused != 1 || plan.CompatibleResultsReused != 0 || plan.NewWork != 0 || plan.Queued != 0 || plan.CacheRejections.Missing != 0 ||
		plan.BuildSubjects != (fetchprogress.BuildAnalysisProgress{LogicalTotal: 1, Completed: 1, ExactResultsReused: 1}) || analyzer.exactCalls.Load() != 1 || compatible.calls.Load() != 0 {
		t.Fatalf("plan=%+v exact calls=%d compatible calls=%d", plan, analyzer.exactCalls.Load(), compatible.calls.Load())
	}
	if run.TestCases[0].AISummary == nil || run.TestCases[0].AISummary.Summary != "exact" {
		t.Fatalf("exact result was not applied: %+v", run.TestCases[0])
	}
}

func TestPlanContainerAnalysisWorkFallsBackFromExactToCompatibleResult(t *testing.T) {
	projectConfig := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2}}}
	analysisProject := testCacheAnalysisProject(projectConfig)
	state, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := testCacheRequest()
	run := models.BuildResult{BuildInfo: request.Build, TestCases: []models.TestCase{request.TestCase}}
	compatibleResult := ai.FailureAnalysisResult{
		Summary:  &models.AISummary{Summary: "compatible"},
		Analysis: &models.AIAnalysis{Mode: ai.AgenticMode, RootCause: "compatible root"},
	}
	compatible := &compatiblePlanningAnalyzer{cachePlanningAnalyzer: &cachePlanningAnalyzer{state: state}, result: compatibleResult}
	analyzer := &exactPlanningAnalyzer{compatiblePlanningAnalyzer: compatible}
	planner := analysisruntime.NewReusePlanner(analysisProject)
	work := []aiWork{{jobID: request.JobID, buildPrefix: request.BuildPrefix, run: &run, tc: &run.TestCases[0]}}
	queued, plan, err := planContainerAnalysisWork(t.Context(), &http.Client{}, work, analyzer, planner, analysisProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 || plan.ExactResultsReused != 0 || plan.CompatibleResultsReused != 1 || analyzer.exactCalls.Load() != 1 || compatible.calls.Load() != 1 {
		t.Fatalf("plan=%+v exact calls=%d compatible calls=%d", plan, analyzer.exactCalls.Load(), compatible.calls.Load())
	}
}

func TestPlanContainerAnalysisWorkCountsCompatibleResultReuse(t *testing.T) {
	projectConfig := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2}}}
	analysisProject := testCacheAnalysisProject(projectConfig)
	state, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := testCacheRequest()
	run := models.BuildResult{BuildInfo: request.Build, TestCases: []models.TestCase{request.TestCase}}
	result := ai.FailureAnalysisResult{
		Summary:  &models.AISummary{Summary: "compatible"},
		Analysis: &models.AIAnalysis{Mode: ai.AgenticMode, RootCause: "root"},
	}
	analyzer := &compatiblePlanningAnalyzer{cachePlanningAnalyzer: &cachePlanningAnalyzer{state: state}, result: result}
	planner := analysisruntime.NewReusePlanner(analysisProject)
	work := []aiWork{{jobID: request.JobID, buildPrefix: request.BuildPrefix, run: &run, tc: &run.TestCases[0]}}
	queued, plan, err := planContainerAnalysisWork(t.Context(), &http.Client{}, work, analyzer, planner, analysisProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 || plan.LogicalTotal != 1 || plan.CompatibleResultsReused != 1 || plan.AcceptedCacheHits != 0 || plan.NewWork != 0 || plan.Queued != 0 || plan.CacheRejections.Missing != 0 || analyzer.calls.Load() != 1 {
		t.Fatalf("plan=%+v calls=%d", plan, analyzer.calls.Load())
	}
	if run.TestCases[0].AISummary == nil || run.TestCases[0].AISummary.Summary != "compatible" {
		t.Fatalf("compatible result was not applied: %+v", run.TestCases[0])
	}
}
