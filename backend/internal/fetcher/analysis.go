package fetcher

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

// analyzeFailuresWithAI runs the agentic AI analysis on every failed test
// case. The agentic tool-calling loop is the only analysis path.
func (p *pipeline) analyzeFailuresWithAI(ctx context.Context, details []models.JobDetail, flakinessReport models.FlakinessReport) {
	tracePath := filepath.Join(p.opts.OutDir, output.AITraceFilename)
	runtime, err := p.ensureAnalysisRuntime(ctx)
	if err != nil {
		log.Printf("⚠ AI runtime setup failed: %v", err)
		return
	}
	cfg := p.cfg
	defer func() {
		if err := runtime.SaveCache(); err != nil {
			log.Printf("Warning: failed to save AI cache: %v", err)
		}
	}()

	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.JobID+"::"+tf.TestName] = tf.ConsecutiveFailures
	}

	traceStore := ai.NewTraceStore()
	service, err := runtime.NewService(analysisruntime.ServiceOptions{
		Backend:             p.backend,
		ConsecutiveFailures: consecutiveMap,
		TraceStore:          traceStore,
		GitHubReadToken:     githubReadToken(),
	})
	if err != nil {
		log.Printf("⚠ AI service setup failed: %v", err)
		return
	}
	defer func() {
		if err := traceStore.Save(tracePath); err != nil {
			log.Printf("Warning: failed to save AI traces: %v", err)
		}
	}()
	runtime.LogConfiguration()

	var totalFailures int
	for _, d := range details {
		for _, run := range d.Runs {
			for _, tc := range run.TestCases {
				if tc.Status == "failed" {
					totalFailures++
				}
			}
		}
	}

	if totalFailures == 0 {
		log.Println("🤖 No failures to analyze")
		return
	}
	log.Printf("🤖 Analyzing %d failures...", totalFailures)

	// Flatten failed test cases into independent work for a bounded worker pool.
	// Shared AI state is internally synchronized; transientSkipped is atomic.
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
			buildPrefix := loc.BuildPath()
			for j := range run.TestCases {
				tc := &run.TestCases[j]
				if tc.Status != "failed" {
					continue
				}
				work = append(work, aiWork{jobID: d.JobID, buildPrefix: buildPrefix, run: run, tc: tc})
			}
		}
	}

	concurrency := cfg.AnalysisConcurrency()
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
			service.Analyze(ctx, p.client, w.jobID, w.buildPrefix, w.run, w.tc)
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

	// Always run the job-level pattern pass. It self-gates on failed build count
	// and is cached.
	analyzePatternsAcrossBuilds(ctx, service, details)
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

// analyzePatternsAcrossBuilds correlates representative failures across failed
// builds into one systemic-vs-transient verdict per job.
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
