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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

var (
	saveContainerAnalysisState = func(store *analysisruntime.ContainerStateStore) error { return store.Save() }
	saveAnalysisRuntimeCache   = func(runtime *analysisruntime.Runtime) error { return runtime.SaveCache() }
	saveAnalysisTraceStore     = func(store *ai.TraceStore, path string) error { return store.Save(path) }
)

type analysisPlanner interface {
	NeedsAnalysis(context.Context, *http.Client, *models.BuildResult, *models.TestCase, int) bool
}

type exactResultReuser interface {
	ReuseExactResult(context.Context, ai.FailureAnalysisRequest, ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error)
}

type compatibleResultReuser interface {
	ReuseCompatibleResult(context.Context, ai.FailureAnalysisRequest, ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error)
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

func planContainerAnalysisWork(ctx context.Context, httpClient *http.Client, work []aiWork, container containerFailureAnalyzer, planner *ai.Service, project *analysisruntime.Project, consecutiveMap map[string]int) ([]aiWork, fetchprogress.AnalysisPlan, error) {
	plan := fetchprogress.AnalysisPlan{LogicalTotal: len(work)}
	cacheGeneration := ""
	if project != nil {
		cacheGeneration = project.CacheGenerationFingerprint
	}
	queued := make([]aiWork, 0, len(work))
	state := container.StateStore()
	var linkResolver *ai.FileLinkResolver
	var sourceOwner, sourceName string
	if project != nil && project.Config != nil {
		sourceRepo := project.AnalysisSource
		if sourceRepo.Owner == "" || sourceRepo.Name == "" {
			sourceRepo = project.Config.EffectiveAnalysisSourceRepo()
		}
		sourceOwner, sourceName = sourceRepo.Owner, sourceRepo.Name
		linkResolver = ai.NewFileLinkResolver(sourceRepo.Owner, sourceRepo.Name, githubReadToken())
	}
	resolveLinks := func(item aiWork) map[string]string {
		if linkResolver == nil || project == nil {
			return map[string]string{}
		}
		if source, ok := ai.ResolveBuildSource(item.run.BuildInfo, sourceOwner, sourceName); ok {
			return linkResolver.ResolveAtRef(ctx, httpClient, item.tc, source.Revision)
		}
		return map[string]string{}
	}
	for _, item := range work {
		buildSubject := item.tc.Source == models.TestCaseSourceBuild
		if buildSubject {
			plan.BuildSubjects.LogicalTotal++
		}
		reason := ai.CacheRejectedMissing
		var result ai.FailureAnalysisResult
		if state != nil && planner != nil {
			var err error
			result, reason, err = state.AcceptCachedFailure(ctx, httpClient, item.request(consecutiveMap[item.jobID+"::"+item.tc.Name], cacheGeneration), planner)
			if err != nil {
				return nil, fetchprogress.AnalysisPlan{}, err
			}
		}
		if reason == ai.CacheAccepted {
			item.tc.AISummary = result.Summary
			item.tc.AIAnalysis = result.Analysis
			if item.tc.AIAnalysis != nil {
				item.tc.AIAnalysis.FileLinks = resolveLinks(item)
			}
			plan.AcceptedCacheHits++
			if buildSubject {
				plan.BuildSubjects.Completed++
				plan.BuildSubjects.AcceptedCacheHits++
			}
			continue
		}
		if reason == ai.CacheRejectedMissing && planner != nil {
			request := item.request(consecutiveMap[item.jobID+"::"+item.tc.Name], cacheGeneration)
			policy, err := analysisruntime.FailureCachePolicy(ctx, httpClient, request, planner)
			if err != nil {
				return nil, fetchprogress.AnalysisPlan{}, err
			}
			if reuser, ok := container.(exactResultReuser); ok {
				result, reused, err := reuser.ReuseExactResult(ctx, request, policy)
				if err != nil {
					return nil, fetchprogress.AnalysisPlan{}, err
				}
				if reused {
					item.tc.AISummary = result.Summary
					item.tc.AIAnalysis = result.Analysis
					if item.tc.AIAnalysis != nil {
						item.tc.AIAnalysis.FileLinks = resolveLinks(item)
					}
					plan.ExactResultsReused++
					if buildSubject {
						plan.BuildSubjects.Completed++
						plan.BuildSubjects.ExactResultsReused++
					}
					continue
				}
			}
			if reuser, ok := container.(compatibleResultReuser); ok {
				result, reused, err := reuser.ReuseCompatibleResult(ctx, request, policy)
				if err != nil {
					return nil, fetchprogress.AnalysisPlan{}, err
				}
				if reused {
					item.tc.AISummary = result.Summary
					item.tc.AIAnalysis = result.Analysis
					if item.tc.AIAnalysis != nil {
						item.tc.AIAnalysis.FileLinks = resolveLinks(item)
					}
					plan.CompatibleResultsReused++
					if buildSubject {
						plan.BuildSubjects.Completed++
					}
					continue
				}
			}
		}
		plan.CacheRejections.Add(string(reason))
		if reason == ai.CacheRejectedMissing {
			plan.NewWork++
		} else {
			plan.StaleWork++
		}
		if buildSubject {
			plan.BuildSubjects.Queued++
		}
		queued = append(queued, item)
	}
	plan.Queued = len(queued)
	return queued, plan, nil
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
	var container containerFailureAnalyzer
	var err error
	if p.opts.AnalysisRuntime.Type == AnalysisRuntimeOrkaContainer {
		container, err = p.ensureContainerAnalyzer()
		if err != nil {
			return fmt.Errorf("container analysis runtime setup: %w", err)
		}
		if err := container.Maintain(ctx); err != nil {
			return err
		}
		var plan fetchprogress.AnalysisPlan
		work, plan, err = planContainerAnalysisWork(ctx, p.client, work, container, planner, p.aiProject, consecutiveMap)
		if err != nil {
			return fmt.Errorf("plan container analysis reuse: %w", err)
		}
		cohorts := analysisFailureCohortStats(work)
		plan.SameFailureGroups = cohorts.Groups
		plan.SameFailureCandidates = cohorts.Candidates
		plan.PotentialTasksSaved = cohorts.PotentialTasksSaved
		plan.LargestSameFailureGroup = cohorts.LargestGroup
		if cohorts.Groups > 0 {
			log.Printf("🧩 same-failure candidates: groups=%d subjects=%d potential_task_savings=%d largest_group=%d",
				cohorts.Groups, cohorts.Candidates, cohorts.PotentialTasksSaved, cohorts.LargestGroup)
		}
		p.planProgressAnalysisWork(plan)
	} else {
		buildSubjects := 0
		for i := range work {
			if work[i].tc.Source == models.TestCaseSourceBuild {
				buildSubjects++
			}
		}
		p.planProgressAnalyses(len(work), buildSubjects)
	}
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
	} else if container != nil {
		if preflight, ok := container.(interface{ Preflight(context.Context) error }); ok {
			if err := preflight.Preflight(ctx); err != nil {
				return err
			}
		}
	}
	if len(work) > 0 {
		log.Printf("🤖 Analyzing %d failures with %s...", len(work), p.opts.AnalysisRuntime.Type)
	}

	var analyzer ai.FailureAnalyzer
	var runtime *analysisruntime.Runtime
	var service *ai.Service
	var traceStore *ai.TraceStore

	if container != nil {
		analyzer = container
	} else {
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
		analyzer = service
	}

	executions := make([]analysisExecution, 0, len(work))
	if container != nil {
		executions = planAnalysisExecutions(work)
	} else {
		for _, item := range work {
			executions = append(executions, analysisExecution{Work: []aiWork{item}})
		}
	}
	concurrency := p.cfg.AnalysisConcurrency()
	if container != nil {
		concurrency = p.opts.AnalysisRuntime.OrkaContainer.MaxConcurrent
	}
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
	var systemicOnce sync.Once
	var systemicErr error
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
	cancelStarted := func(execution analysisExecution) {
		for _, item := range execution.Work {
			p.finishProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild, fetchprogress.OutcomeCancelled)
		}
	}

	var scheduleExecution func(analysisExecution, bool)
	scheduleExecution = func(execution analysisExecution, progressStarted bool) {
		tokenHeld := false
		if !progressStarted {
			select {
			case sem <- struct{}{}:
				tokenHeld = true
			case <-analysisCtx.Done():
				return
			}
			if analysisCtx.Err() != nil {
				<-sem
				return
			}
		}
		wg.Add(1)
		go func(tokenHeld bool) {
			defer wg.Done()
			if !tokenHeld {
				select {
				case sem <- struct{}{}:
					tokenHeld = true
				case <-analysisCtx.Done():
					if progressStarted {
						cancelStarted(execution)
					}
					return
				}
			}
			defer func() { <-sem }()
			if analysisCtx.Err() != nil {
				if progressStarted {
					cancelStarted(execution)
				}
				return
			}
			if !progressStarted {
				for _, item := range execution.Work {
					p.startProgressAnalysis(item.tc.Source == models.TestCaseSourceBuild)
				}
			}

			representative := execution.Work[0]
			before := representative.tc.AISummary
			request := analysisExecutionRequest(execution, consecutiveMap[representative.jobID+"::"+representative.tc.Name], p.cacheGenerationFingerprint())
			result, analyzeErr := analyzer.AnalyzeFailure(analysisCtx, p.client, request)
			if systemic := systemicContainerAnalysisError(analyzeErr); systemic != nil {
				finish(representative, before, result, analyzeErr, true)
				for _, follower := range execution.Work[1:] {
					p.finishProgressAnalysis(follower.tc.Source == models.TestCaseSourceBuild, fetchprogress.OutcomeCancelled)
				}
				systemicOnce.Do(func() {
					systemicErr = systemic
					cancelAnalysis()
				})
				return
			}
			if len(execution.Work) == 1 {
				finish(representative, before, result, analyzeErr, true)
				return
			}
			if errors.Is(analyzeErr, context.Canceled) || errors.Is(analyzeErr, context.DeadlineExceeded) {
				finish(representative, before, result, analyzeErr, true)
				for _, follower := range execution.Work[1:] {
					p.finishProgressAnalysis(follower.tc.Source == models.TestCaseSourceBuild, fetchprogress.OutcomeCancelled)
				}
				return
			}
			if analyzeErr != nil {
				for _, item := range execution.Work {
					scheduleExecution(analysisExecution{Work: []aiWork{item}}, true)
				}
				return
			}

			finish(representative, before, result, nil, true)
			for _, follower := range execution.Work[1:] {
				shared, ok := prepareSameFailureReuse(analysisCtx, p.client, follower, result, container.StateStore(), planner, consecutiveMap[follower.jobID+"::"+follower.tc.Name], p.cacheGenerationFingerprint())
				if ok {
					finish(follower, follower.tc.AISummary, shared, nil, false)
					p.recordProgressSameFailureReuse(1)
					continue
				}
				scheduleExecution(analysisExecution{Work: []aiWork{follower}}, true)
			}
		}(tokenHeld)
	}
	for _, execution := range executions {
		scheduleExecution(execution, false)
	}

	wg.Wait()
	p.cancelQueuedProgressAnalyses()
	p.completeProgressPhase()
	if systemicErr != nil {
		return systemicErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped.Load())
	if n := judgeRan.Load(); n > 0 {
		log.Printf("⚖️ semantic judge: ran on %d, objected on %d, revised %d", n, judgeObjected.Load(), judgeRevised.Load())
	}
	if err := p.persistIndividualAnalysisCheckpoint(container, runtime, traceStore); err != nil {
		return err
	}
	if err := p.commitAnalysisCheckpoint(); err != nil {
		return fmt.Errorf("committing completed analysis checkpoint: %w", err)
	}
	p.markProgressAnalysisCheckpoint()
	p.startProgressPhase(fetchprogress.PhasePatterns)

	if container != nil {
		// Container analysis wrote the checkpoint outside the retained runtime.
		// Reload it before pattern cache writes so new individual entries survive.
		p.aiRuntime = nil
		runtime, err = p.ensureAnalysisRuntime(ctx)
		if err != nil {
			log.Printf("Warning: cross-build analysis runtime setup failed: %v", err)
			p.skipProgressPatterns()
			return nil
		}
		traceStore = container.StateStore().TraceStore()
		service, err = runtime.NewService(analysisruntime.ServiceOptions{
			Backend:             p.backend,
			ConsecutiveFailures: consecutiveMap,
			TraceStore:          traceStore,
			GitHubReadToken:     githubReadToken(),
		})
		if err != nil {
			log.Printf("Warning: cross-build analysis service setup failed: %v", err)
			p.skipProgressPatterns()
			return nil
		}
		runtime.LogConfiguration()
	}

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

func (p *pipeline) persistIndividualAnalysisCheckpoint(container containerFailureAnalyzer, runtime *analysisruntime.Runtime, traces *ai.TraceStore) error {
	if container != nil {
		container.StateStore().SetTraceEngine(p.opts.TraceEngine)
		if err := saveContainerAnalysisState(container.StateStore()); err != nil {
			return fmt.Errorf("persisting completed container analysis: %w", err)
		}
		return nil
	}
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

func (p *pipeline) ensureContainerAnalyzer() (containerFailureAnalyzer, error) {
	if p.containerAnalyzer != nil {
		return p.containerAnalyzer, nil
	}
	stateKey, err := analysisruntime.ParseContainerStateKey(os.Getenv(analysisruntime.ContainerStateKeyEnv))
	if err != nil {
		return nil, err
	}
	cfg := p.opts.AnalysisRuntime.OrkaContainer
	container, err := orka.NewContainerAnalyzer(orka.ContainerAnalyzerOptions{
		Namespace:           cfg.Namespace,
		OrkaAPI:             cfg.ResultAPI,
		OrkaAPIToken:        os.Getenv("ORKA_ANALYSIS_API_TOKEN"),
		Image:               cfg.Image,
		ProjectDir:          p.opts.ProjectDir,
		DataDir:             p.opts.OutDir,
		API:                 p.aiProject.Provider.API,
		Endpoint:            p.aiProject.Provider.Endpoint,
		Model:               p.aiProject.Provider.Model,
		CacheGeneration:     p.aiProject.CacheGeneration,
		ModelSecretName:     cfg.ModelSecretName,
		ModelTokenKey:       cfg.ModelTokenKey,
		GitHubSecretName:    cfg.GitHubSecretName,
		GitHubTokenKey:      cfg.GitHubTokenKey,
		StateSecretName:     cfg.StateSecretName,
		StateSecretKey:      cfg.StateSecretKey,
		StateKey:            stateKey,
		ContextWindowTokens: cfg.ContextWindowTokens,
		AnalysisTimeout:     p.aiProject.Config.AI.EffectiveAgentic().Timeout,
		TaskTimeout:         cfg.TaskTimeout,
		PollInterval:        cfg.PollInterval,
		MaxRetries:          cfg.Retries,
		MaxConcurrentTasks:  cfg.MaxConcurrent,
		NodeSelector:        cfg.NodeSelector,
		Tolerations:         cfg.Tolerations,
		Affinity:            cfg.Affinity,
		Progress:            p.progress,
		UsageRecorder:       p.usageRecorder,
	})
	if err != nil {
		return nil, err
	}
	container.StateStore().SetTraceEngine(p.opts.TraceEngine)
	p.containerAnalyzer = container
	return p.containerAnalyzer, nil
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
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).API
}

// aiEndpoint returns the configured AI chat-completions URL.
// project.yaml wins over AI_ENDPOINT.
func aiEndpoint(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Endpoint
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
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Model
}

// aiHeaders returns the extra HTTP headers to attach to AI provider requests.
func aiHeaders(cfg *project.Config) map[string]string {
	return cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Headers
}
