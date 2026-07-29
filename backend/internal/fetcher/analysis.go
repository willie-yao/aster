package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

// analyzeFailuresWithAI runs the dashboard-owned analyzer on every failed test.
func (p *pipeline) analyzeFailuresWithAI(ctx context.Context, details []models.JobDetail, flakinessReport models.FlakinessReport) error {
	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.JobID+"::"+tf.TestName] = tf.ConsecutiveFailures
	}

	type aiWork struct {
		jobID       string
		buildPrefix string
		run         *models.BuildResult
		tc          *models.TestCase
	}
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
				if tc.Status == "failed" {
					work = append(work, aiWork{jobID: d.JobID, buildPrefix: loc.BuildPath(), run: run, tc: tc})
				}
			}
		}
	}
	p.planProgressAnalyses(len(work))
	p.completeProgressPhase()
	p.startProgressPhase(fetchprogress.PhaseAnalysis)

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
		if preflight, ok := container.(interface{ Preflight(context.Context) error }); ok {
			if err := preflight.Preflight(ctx); err != nil {
				return err
			}
		}
	}
	if len(work) == 0 {
		log.Println("🤖 No failures to analyze")
		p.completeProgressPhase()
		p.startProgressPhase(fetchprogress.PhasePatterns)
		p.skipProgressPatterns()
		return nil
	}
	log.Printf("🤖 Analyzing %d failures with %s...", len(work), p.opts.AnalysisRuntime.Type)

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

schedule:
	for _, w := range work {
		select {
		case sem <- struct{}{}:
		case <-analysisCtx.Done():
			break schedule
		}
		if analysisCtx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(w aiWork) {
			defer wg.Done()
			defer func() { <-sem }()
			p.startProgressAnalysis()
			before := w.tc.AISummary
			result, analyzeErr := analyzer.AnalyzeFailure(analysisCtx, p.client, ai.FailureAnalysisRequest{
				JobID:               w.jobID,
				BuildPrefix:         w.buildPrefix,
				Build:               w.run.BuildInfo,
				TestCase:            *w.tc,
				ConsecutiveFailures: consecutiveMap[w.jobID+"::"+w.tc.Name],
			})
			if analysisruntime.IsProjectBundleSourceError(analyzeErr) {
				p.finishProgressAnalysis(fetchprogress.OutcomeFailed)
				systemicOnce.Do(func() {
					systemicErr = fmt.Errorf("systemic Orka project setup failure: %w", analyzeErr)
					cancelAnalysis()
				})
				return
			}
			if orka.IsResultAuthorizationError(analyzeErr) {
				p.finishProgressAnalysis(fetchprogress.OutcomeFailed)
				systemicOnce.Do(func() {
					systemicErr = fmt.Errorf("systemic Orka result API authorization failure: %w", analyzeErr)
					cancelAnalysis()
				})
				return
			}
			w.tc.AISummary = result.Summary
			w.tc.AIAnalysis = result.Analysis
			if analyzeErr != nil {
				log.Printf("  ⚠ analysis unavailable for %s/%s: %v", w.jobID, w.tc.Name, analyzeErr)
			}
			if before == nil && w.tc.AISummary != nil && w.tc.AISummary.IsTransient && w.tc.AIAnalysis == nil {
				transientSkipped.Add(1)
			}
			if a := w.tc.AIAnalysis; a != nil {
				if a.JudgeRan {
					judgeRan.Add(1)
				}
				if a.JudgeObjected {
					judgeObjected.Add(1)
				}
				if a.JudgeRevised {
					judgeRevised.Add(1)
				}
			}
			outcome := fetchprogress.OutcomeSucceeded
			if analyzeErr != nil {
				outcome = fetchprogress.OutcomeFailed
				if errors.Is(analyzeErr, context.Canceled) || errors.Is(analyzeErr, context.DeadlineExceeded) {
					outcome = fetchprogress.OutcomeCancelled
				}
			}
			p.finishProgressAnalysis(outcome)
		}(w)
	}
	wg.Wait()
	p.cancelQueuedProgressAnalyses()
	p.completeProgressPhase()
	if systemicErr != nil {
		return systemicErr
	}
	if container != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped.Load())
	if n := judgeRan.Load(); n > 0 {
		log.Printf("⚖️ semantic judge: ran on %d, objected on %d, revised %d", n, judgeObjected.Load(), judgeRevised.Load())
	}
	p.startProgressPhase(fetchprogress.PhasePatterns)

	if container != nil {
		warnOnAnalysisPersistence("container analysis state", container.StateStore().Save)
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
		OnAttempt: func(attempt patterns.Attempt) {
			if p.progress != nil {
				p.progress.RecordPatternAttempt(
					attempt.Retry,
					attempt.Succeeded,
					attempt.Final,
					fetchprogress.PatternFailureCategory(attempt.FailureCategory),
				)
			}
		},
	}
	if err := analyzePatternsAcrossBuilds(ctx, service, details, patternOptions); err != nil {
		return fmt.Errorf("cross-build pattern analysis: %w", err)
	}
	warnOnAnalysisPersistence("AI cache", runtime.SaveCache)
	warnOnAnalysisPersistence("AI traces", func() error {
		return traceStore.Save(filepath.Join(p.opts.OutDir, output.AITraceFilename))
	})
	return nil
}

func warnOnAnalysisPersistence(name string, save func() error) {
	if err := save(); err != nil {
		log.Printf("Warning: failed to save %s: %v", name, err)
	}
}

func (p *pipeline) ensureAnalysisRuntime(ctx context.Context) (*analysisruntime.Runtime, error) {
	if p.aiRuntime != nil {
		return p.aiRuntime, nil
	}
	runtime, err := analysisruntime.New(ctx, analysisruntime.Options{
		Token: p.aiToken, DataDir: p.opts.OutDir, Project: p.aiProject,
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
		ModelSecretName:     cfg.ModelSecretName,
		ModelTokenKey:       cfg.ModelTokenKey,
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
	})
	if err != nil {
		return nil, err
	}
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

// githubReadToken returns the token for read-only GitHub API access (the
// pattern agent's recursive repo-tree listing). GITHUB_READ_TOKEN is preferred;
// FIX_TOKEN then GITHUB_TOKEN are reused as fallbacks so a deploy that already
// has a fix-PR token or the Actions-provided token grounds the pattern agent
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
