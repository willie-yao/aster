package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
)

var (
	saveAnalysisRuntimeCache = func(runtime *analysisruntime.Runtime) error { return runtime.SaveCache() }
	saveAnalysisTraceStore   = func(store *ai.TraceStore, path string) error { return store.Save(path) }
	// newAnalysisAnalyzer selects the analyzer one pass runs against. Tests replace it.
	newAnalysisAnalyzer = func(service *ai.Service) ai.FailureAnalyzer { return service }
)

type analysisPlanner interface {
	NeedsAnalysis(context.Context, *http.Client, *models.BuildResult, *models.TestCase, int) bool
}

type aiWork struct {
	jobID       string
	buildPrefix string
	run         *models.BuildResult
	tc          *models.TestCase
	prowJob     ai.ProwJobContext
	priority    aiWorkPriority
}

func (w aiWork) request(consecutiveFailures int, cacheGeneration string) ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID:               w.jobID,
		BuildPrefix:         w.buildPrefix,
		Build:               w.run.BuildInfo,
		TestCase:            *w.tc,
		ProwJob:             ai.CanonicalProwJobContext(&w.prowJob),
		ConsecutiveFailures: consecutiveFailures,
		CacheGeneration:     cacheGeneration,
	}
}

type aiWorkPriority uint8

const (
	aiWorkBuildMissing aiWorkPriority = iota
	aiWorkJUnitMissing
	aiWorkReusable
)

func collectAIWork(ctx context.Context, httpClient *http.Client, details []models.JobDetail, consecutiveMap map[string]int, planner analysisPlanner) []aiWork {
	var work []aiWork
	for i := range details {
		d := &details[i]
		jobLoc := prowbuild.JobLocation{JobType: d.JobType, Repo: d.Repo}
		for ri := range d.Runs {
			run := &d.Runs[ri]
			loc := prowbuild.BuildLocation{
				JobLocation: jobLoc,
				JobName:     d.Name,
				BuildID:     run.BuildID,
				PullNumber:  run.PullNumber,
			}
			for j := range run.TestCases {
				tc := &run.TestCases[j]
				if tc.Status != "failed" {
					continue
				}
				consecutive := consecutiveMap[d.JobID+"::"+tc.Name]
				item := aiWork{
					jobID: d.JobID, buildPrefix: loc.BuildPath(), run: run, tc: tc,
					prowJob: ai.ProwJobContext{
						Name: d.Name, JobType: d.JobType, ConfigFile: d.ConfigFile, ConfigRevision: d.ConfigRevision,
					},
				}
				item.priority = classifyAIWork(ctx, httpClient, item, consecutive, planner)
				work = append(work, item)
			}
		}
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].priority < work[j].priority })
	return work
}

func classifyAIWork(ctx context.Context, httpClient *http.Client, item aiWork, consecutive int, planner analysisPlanner) aiWorkPriority {
	needsWork := analysisNeedsWork(item.tc)
	if planner != nil {
		needsWork = planner.NeedsAnalysis(ctx, httpClient, item.run, item.tc, max(1, consecutive))
	}
	if needsWork {
		if item.tc.Source == models.TestCaseSourceBuild {
			return aiWorkBuildMissing
		}
		return aiWorkJUnitMissing
	}
	return aiWorkReusable
}

func analysisNeedsWork(tc *models.TestCase) bool {
	return tc.AISummary == nil || tc.AIAnalysis == nil || tc.AIAnalysis.Mode != ai.AgenticMode || !tc.AIAnalysis.CritiquePassed
}

func (p *pipeline) cacheGenerationFingerprint() string {
	if p == nil || p.aiProject == nil {
		return ""
	}
	return p.aiProject.CacheGenerationFingerprint
}

// analyzeFailuresWithAI runs the dashboard-owned analyzer on every failed test.
func (p *pipeline) analyzeFailuresWithAI(ctx context.Context, details []models.JobDetail, flakinessReport models.FlakinessReport) error {
	p.lastPatternOutcomes = map[string]patterns.JobOutcome{}
	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.JobID+"::"+tf.TestName] = tf.ConsecutiveFailures
	}

	planner := analysisruntime.NewReusePlanner(p.aiProject)
	work := collectAIWork(ctx, p.client, details, consecutiveMap, planner)
	logicalTotal := len(work)
	var err error
	buildSubjects := 0
	for i := range work {
		if work[i].tc.Source == models.TestCaseSourceBuild {
			buildSubjects++
		}
	}
	p.planProgressAnalyses(len(work), buildSubjects)
	p.completeProgressPhase()
	p.startProgressPhase(fetchprogress.PhaseAnalysis)
	if len(work) == 0 {
		if logicalTotal == 0 {
			log.Println("🤖 No failures to analyze")
			p.completeProgressPhase()
			p.startProgressPhase(fetchprogress.PhasePatterns)
			p.skipProgressPatterns()
			return nil
		}
		log.Println("🤖 All failure analysis results are ready from reuse")
	}
	if len(work) > 0 {
		log.Printf("🤖 Analyzing %d failures with %s...", len(work), p.opts.AnalysisRuntime.Type)
	}

	var analyzer ai.FailureAnalyzer
	var runtime *analysisruntime.Runtime
	var service *ai.Service
	var traceStore *ai.TraceStore

	{
		runtime, err = p.ensureAnalysisRuntime(ctx)
		if err != nil {
			log.Printf("⚠ AI runtime setup failed: %v", err)
			p.skipProgressAnalysis()
			p.completeProgressPhase()
			p.startProgressPhase(fetchprogress.PhasePatterns)
			p.skipProgressPatterns()
			return nil
		}
		traceStore = ai.NewTraceStore()
		service, err = runtime.NewService(analysisruntime.ServiceOptions{
			Backend:             p.backend,
			ConsecutiveFailures: consecutiveMap,
			TraceStore:          traceStore,
			GitHubReadToken:     githubReadToken(),
		})
		if err != nil {
			log.Printf("⚠ AI service setup failed: %v", err)
			p.skipProgressAnalysis()
			p.completeProgressPhase()
			p.startProgressPhase(fetchprogress.PhasePatterns)
			p.skipProgressPatterns()
			return nil
		}
		runtime.LogConfiguration()
		analyzer = newAnalysisAnalyzer(service)
	}

	concurrency := p.cfg.AnalysisConcurrency()
	if concurrency > len(work) {
		concurrency = len(work)
	}
	if concurrency > 1 {
		log.Printf("🤖 analyzing with concurrency=%d", concurrency)
	}

	var transientSkipped atomic.Int64
	var judgeRan, judgeObjected, judgeRevised atomic.Int64
	analysisCtx, cancelAnalysis := context.WithCancel(ctx)
	defer cancelAnalysis()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	finish := func(item aiWork, before *models.AISummary, result ai.FailureAnalysisResult, analyzeErr error, recordJudge bool) {
		item.tc.AISummary = result.Summary
		item.tc.AIAnalysis = result.Analysis
		if analyzeErr != nil {
			log.Printf("  ⚠ analysis unavailable for %s/%s: %v", item.jobID, item.tc.Name, analyzeErr)
		}
		if before == nil && item.tc.AISummary != nil && item.tc.AISummary.IsTransient && item.tc.AIAnalysis == nil {
			transientSkipped.Add(1)
		}
		if recordJudge {
			if analysis := item.tc.AIAnalysis; analysis != nil {
				if analysis.JudgeRan {
					judgeRan.Add(1)
				}
				if analysis.JudgeObjected {
					judgeObjected.Add(1)
				}
				if analysis.JudgeRevised {
					judgeRevised.Add(1)
				}
			}
		}
		outcome := fetchprogress.OutcomeSucceeded
		if analyzeErr != nil {
			outcome = fetchprogress.OutcomeFailed
			if errors.Is(analyzeErr, context.Canceled) || errors.Is(analyzeErr, context.DeadlineExceeded) {
				outcome = fetchprogress.OutcomeCancelled
			}
		}
		p.finishProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild, outcome)
	}
	scheduleWork := func(item aiWork) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-analysisCtx.Done():
				p.finishProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild, fetchprogress.OutcomeCancelled)
				return
			}
			defer func() { <-sem }()
			if analysisCtx.Err() != nil {
				p.finishProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild, fetchprogress.OutcomeCancelled)
				return
			}
			p.startProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild)
			before := item.tc.AISummary
			request := item.request(consecutiveMap[item.jobID+"::"+item.tc.Name], p.cacheGenerationFingerprint())
			result, analyzeErr := analyzer.AnalyzeFailure(analysisCtx, p.client, request)
			finish(item, before, result, analyzeErr, true)
		}()
	}
	for _, item := range work {
		scheduleWork(item)
	}

	wg.Wait()
	p.cancelQueuedProgressAnalyses()
	p.completeProgressPhase()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped.Load())
	if n := judgeRan.Load(); n > 0 {
		log.Printf("⚖️ semantic judge: ran on %d, objected on %d, revised %d", n, judgeObjected.Load(), judgeRevised.Load())
	}
	if err := p.persistIndividualAnalysisCheckpoint(runtime, traceStore); err != nil {
		return err
	}
	if err := p.commitAnalysisCheckpoint(); err != nil {
		return fmt.Errorf("committing completed analysis checkpoint: %w", err)
	}
	p.markProgressAnalysisCheckpoint()
	p.startProgressPhase(fetchprogress.PhasePatterns)

	patternOptions := patterns.AnalyzeOptions{
		OnPlan: func(total int) {
			if p.progress != nil {
				p.progress.PlanPatterns(total)
			}
		},
		OnOutcome: func(outcome patterns.JobOutcome) {
			p.lastPatternOutcomes[outcome.JobID] = outcome
		},
		OnAttempt: func(attempt patterns.Attempt) {
			if p.progress != nil {
				p.progress.RecordPatternAttempt(
					attempt.CacheHit,
					attempt.Repair,
					attempt.Retry,
					attempt.Succeeded,
					attempt.Final,
					fetchprogress.PatternFailureCategory(attempt.FailureCategory),
				)
				if attempt.Suppressed {
					p.progress.RecordPatternSuppressed()
				}
				if attempt.FreshRetry {
					p.progress.RecordPatternFreshRetry()
				}
			}
		},
	}
	patternErr := analyzePatternsAcrossBuilds(ctx, service, details, patternOptions)
	persistErr := p.persistRuntimeAnalysisState(runtime, traceStore)
	if persistErr == nil {
		persistErr = p.commitAnalysisCheckpoint()
		if persistErr != nil {
			persistErr = fmt.Errorf("committing completed pattern checkpoint: %w", persistErr)
		}
	}
	if persistErr != nil {
		return errors.Join(patternErr, persistErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if patternErr != nil {
		log.Printf("Warning: cross-build pattern analysis incomplete: %v", patternErr)
	}
	return nil
}

func (p *pipeline) persistIndividualAnalysisCheckpoint(runtime *analysisruntime.Runtime, traces *ai.TraceStore) error {
	return p.persistRuntimeAnalysisState(runtime, traces)
}

func (p *pipeline) persistRuntimeAnalysisState(runtime *analysisruntime.Runtime, traces *ai.TraceStore) error {
	if err := saveAnalysisRuntimeCache(runtime); err != nil {
		return fmt.Errorf("persisting AI cache: %w", err)
	}
	if traces == nil {
		return fmt.Errorf("persisting AI traces: trace store is unavailable")
	}
	traces.SetEngine(p.opts.TraceEngine)
	if err := saveAnalysisTraceStore(traces, filepath.Join(p.opts.OutDir, output.AITraceFilename)); err != nil {
		return fmt.Errorf("persisting AI traces: %w", err)
	}
	return nil
}

func (p *pipeline) ensureAnalysisRuntime(ctx context.Context) (*analysisruntime.Runtime, error) {
	if p.aiRuntime != nil {
		return p.aiRuntime, nil
	}
	runtime, err := analysisruntime.New(ctx, analysisruntime.Options{
		Token: p.aiToken, DataDir: p.opts.OutDir, Project: p.aiProject,
		UsageRecorder: p.usageRecorder, UsageOrigin: aiusage.OriginFetcher,
	})
	if err != nil {
		return nil, err
	}
	p.aiRuntime = runtime
	return p.aiRuntime, nil
}

var analyzePatternsAcrossBuilds = func(ctx context.Context, service *ai.Service, details []models.JobDetail, options patterns.AnalyzeOptions) error {
	_, err := patterns.AnalyzeWithOptions(ctx, service, details, options)
	return err
}

func collectRecurringPatterns(details []models.JobDetail) []models.PatternAnalysis {
	return patterns.CollectRecurring(details)
}

func gatherPatternFailures(d *models.JobDetail) []ai.PatternFailure {
	return patterns.GatherFailures(d)
}

func failureLocationFile(loc string) string {
	return patterns.FailureLocationFile(loc)
}

// aiAPI returns the configured model API. project.yaml wins over AI_API.
func aiAPI(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv)).API
}

// aiEndpoint returns the configured AI chat-completions URL.
// project.yaml wins over AI_ENDPOINT.
func aiEndpoint(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv)).Endpoint
}

// githubReadToken returns the token for read-only GitHub source access.
// GITHUB_READ_TOKEN is preferred;
// FIX_TOKEN then GITHUB_TOKEN are reused as fallbacks so a deploy that already
// has a fix-PR token or the Actions-provided token enables authenticated reads
// without extra configuration.
func githubReadToken() string {
	for _, name := range []string{"GITHUB_READ_TOKEN", "FIX_TOKEN", "GITHUB_TOKEN"} {
		if t := os.Getenv(name); t != "" {
			return t
		}
	}
	return ""
}

// aiModel returns the configured AI model identifier.
// project.yaml wins over AI_MODEL.
func aiModel(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv)).Model
}
