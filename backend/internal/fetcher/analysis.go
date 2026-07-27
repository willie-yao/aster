package fetcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
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
	}
	if len(work) == 0 {
		log.Println("🤖 No failures to analyze")
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
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(w aiWork) {
			defer wg.Done()
			defer func() { <-sem }()
			before := w.tc.AISummary
			result, analyzeErr := analyzer.AnalyzeFailure(ctx, p.client, ai.FailureAnalysisRequest{
				JobID:               w.jobID,
				BuildPrefix:         w.buildPrefix,
				Build:               w.run.BuildInfo,
				TestCase:            *w.tc,
				ConsecutiveFailures: consecutiveMap[w.jobID+"::"+w.tc.Name],
			})
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
		}(w)
	}
	wg.Wait()
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped.Load())
	if n := judgeRan.Load(); n > 0 {
		log.Printf("⚖️ semantic judge: ran on %d, objected on %d, revised %d", n, judgeObjected.Load(), judgeRevised.Load())
	}

	if container != nil {
		warnOnAnalysisPersistence("container analysis state", container.StateStore().Save)
		runtime, err = p.ensureAnalysisRuntime(ctx)
		if err != nil {
			log.Printf("Warning: cross-build analysis runtime setup failed: %v", err)
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
			return nil
		}
		runtime.LogConfiguration()
	}

	analyzePatternsAcrossBuilds(ctx, service, details)
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
	})
	if err != nil {
		return nil, err
	}
	p.containerAnalyzer = container
	return p.containerAnalyzer, nil
}

func analyzePatternsAcrossBuilds(ctx context.Context, service *ai.Service, details []models.JobDetail) {
	patterns.Analyze(ctx, service, details)
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
