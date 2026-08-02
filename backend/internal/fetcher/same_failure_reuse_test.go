package fetcher

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func testSameFailureResult(analysisProject *analysisruntime.Project, generation string, toolCalls int) ai.FailureAnalysisResult {
	generatedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	return ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: generatedAt, Summary: "shared DRA failure"},
		Analysis: &models.AIAnalysis{
			GeneratedAt: generatedAt, Mode: ai.AgenticMode, RootCause: "shared DRA cause", Severity: "High", SuggestedFix: "fix DRA setup",
			ToolCalls: toolCalls, CritiqueVersion: ai.CurrentCritiqueVersion(), CritiquePassed: true,
			ModelHash:       ai.ModelFingerprint(analysisProject.Provider.API, analysisProject.Provider.Endpoint, analysisProject.Provider.Model),
			CacheGeneration: generation,
		},
	}
}

func TestPrepareSameFailureReuseStagesPerTestCacheEntry(t *testing.T) {
	retries := 0
	config := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2, Critique: project.AgenticCritique{MaxRetries: &retries}}}}
	analysisProject := testCacheAnalysisProject(config)
	analysisProject.CacheGenerationFingerprint = "0123456789abcdef"
	planner := analysisruntime.NewReusePlanner(analysisProject)
	state, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item := cohortTestWork("job", "1", "DRA test beta", "same DRA test beta failure", "same body")
	result := testSameFailureResult(analysisProject, analysisProject.CacheGenerationFingerprint, 2)
	shared, ok := prepareSameFailureReuse(t.Context(), &http.Client{}, item, result, state, planner, 1, analysisProject.CacheGenerationFingerprint)
	if !ok || shared.Analysis == nil || !shared.Analysis.SameFailureReuse || shared.Analysis.CacheHit {
		t.Fatalf("shared result = %+v ok=%t", shared, ok)
	}
	request := item.request(1, analysisProject.CacheGenerationFingerprint)
	if got := len(state.CacheSeed(request)); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
	accepted, reason, err := state.AcceptCachedFailure(t.Context(), &http.Client{}, request, planner)
	if err != nil || reason != ai.CacheAccepted || accepted.Analysis == nil || !accepted.Analysis.CacheHit || !accepted.Analysis.SameFailureReuse {
		t.Fatalf("accepted=%+v reason=%q error=%v", accepted, reason, err)
	}
}

func TestPrepareSameFailureReuseRejectsBelowFloor(t *testing.T) {
	retries := 0
	config := &project.Config{AI: &project.AI{Agentic: project.Agentic{MinToolCalls: 2, Critique: project.AgenticCritique{MaxRetries: &retries}}}}
	analysisProject := testCacheAnalysisProject(config)
	planner := analysisruntime.NewReusePlanner(analysisProject)
	state, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item := cohortTestWork("job", "1", "DRA test beta", "same DRA test beta failure", "same body")
	if _, ok := prepareSameFailureReuse(t.Context(), &http.Client{}, item, testSameFailureResult(analysisProject, "", 1), state, planner, 1, ""); ok {
		t.Fatal("below-floor representative result was reused")
	}
}

type sameFailureAnalyzer struct {
	state     *analysisruntime.ContainerStateStore
	project   *analysisruntime.Project
	toolCalls int
	firstErr  error
	mu        sync.Mutex
	requests  []ai.FailureAnalysisRequest
}

func (a *sameFailureAnalyzer) Maintain(context.Context) error                   { return nil }
func (a *sameFailureAnalyzer) Preflight(context.Context) error                  { return nil }
func (a *sameFailureAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return a.state }

func (a *sameFailureAnalyzer) AnalyzeFailure(_ context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	call := len(a.requests)
	a.mu.Unlock()
	if call == 1 && a.firstErr != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, a.firstErr), a.firstErr
	}
	result := testSameFailureResult(a.project, request.CacheGeneration, a.toolCalls)
	if a.toolCalls >= a.project.Config.AI.EffectiveAgentic().MinToolCalls {
		entry, err := ai.NewAgenticCacheEntry(analysisruntime.FailureCacheKey(request), result, time.Now().UTC())
		if err != nil {
			return ai.FailureAnalysisResult{}, err
		}
		if err := a.state.StageCacheEntry(entry); err != nil {
			return ai.FailureAnalysisResult{}, err
		}
	}
	return result, nil
}

func TestAnalyzeFailuresReusesSameFailureRepresentativeAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolCalls  int
		firstErr   error
		wantCalls  int
		wantReused int
	}{
		{name: "representative fanout", toolCalls: 2, wantCalls: 1, wantReused: 1},
		{name: "below-floor fallback", toolCalls: 1, wantCalls: 2},
		{name: "representative error fallback", toolCalls: 2, firstErr: errors.New("representative unavailable"), wantCalls: 2, wantReused: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
			dataDir := t.TempDir()
			retries := 0
			config := &project.Config{
				AI:      &project.AI{Concurrency: 2, Agentic: project.Agentic{MinToolCalls: 2, Tools: []string{"filesystem"}, Critique: project.AgenticCritique{MaxRetries: &retries}}},
				Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: t.TempDir()},
			}
			analysisProject := testCacheAnalysisProject(config)
			analysisProject.CacheGenerationFingerprint = "0123456789abcdef"
			state, err := analysisruntime.NewContainerStateStore(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			backend, err := storage.NewLocalBackend(config.Storage.Base, "https://prow.example.test")
			if err != nil {
				t.Fatal(err)
			}
			analyzer := &sameFailureAnalyzer{state: state, project: analysisProject, toolCalls: tc.toolCalls, firstErr: tc.firstErr}
			tracker := fetchprogress.New(dataDir, "sha-test")
			tracker.StartPass(fetchprogress.PassInitialWatch)
			p := &pipeline{
				opts: Options{OutDir: dataDir, AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeOrkaContainer, OrkaContainer: OrkaContainerAnalysisOptions{MaxConcurrent: 2}}},
				cfg:  config, client: &http.Client{}, backend: backend, aiProject: analysisProject, aiToken: "token",
				containerAnalyzer: analyzer, progress: tracker,
			}
			details := []models.JobDetail{{
				Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
				Runs: []models.BuildResult{{
					BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
					TestCases: []models.TestCase{
						*cohortTestWork("job", "1", "DRA test alpha", "same DRA test alpha failure", "same body").tc,
						*cohortTestWork("job", "1", "DRA test beta", "same DRA test beta failure", "same body").tc,
					},
				}},
			}}
			oldPatterns := analyzePatternsAcrossBuilds
			analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error { return nil }
			t.Cleanup(func() { analyzePatternsAcrossBuilds = oldPatterns })

			if err := p.analyzeFailuresWithAI(t.Context(), details, models.FlakinessReport{}); err != nil {
				t.Fatal(err)
			}
			if len(analyzer.requests) != tc.wantCalls {
				t.Fatalf("analyzer calls = %d, want %d", len(analyzer.requests), tc.wantCalls)
			}
			if tc.wantReused > 0 {
				if analyzer.requests[0].FailureCohort == nil || analyzer.requests[0].FailureCohort.Count != 2 {
					t.Fatalf("representative request = %+v", analyzer.requests[0])
				}
				if !details[0].Runs[0].TestCases[1].AIAnalysis.SameFailureReuse {
					t.Fatalf("follower analysis = %+v", details[0].Runs[0].TestCases[1].AIAnalysis)
				}
				for i := range details[0].Runs[0].TestCases {
					item := aiWork{jobID: "job", run: &details[0].Runs[0], tc: &details[0].Runs[0].TestCases[i]}
					if got := len(state.CacheSeed(item.request(1, analysisProject.CacheGenerationFingerprint))); got != 1 {
						t.Fatalf("cache seed %d entries = %d", i, got)
					}
				}
			}
			progress := tracker.Snapshot().Analyses
			if progress.Completed != 2 || progress.SameFailureReused != tc.wantReused || progress.Failed != 0 || progress.Cancelled != 0 {
				t.Fatalf("progress = %+v", progress)
			}
		})
	}
}
