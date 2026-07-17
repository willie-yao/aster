package fetcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/universal"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

// Budget auto-sizing factors. The agentic loop needs client-side byte budgets
// before endpoints with hard context limits fail on overflow. Budgets derive
// from the reported context window and are not deployment-configurable.
const (
	// Conservative bytes/token: CI logs (long paths, hex IDs, timestamps,
	// stack traces) tokenize densely, so a low estimate underfills the token
	// window and keeps the byte budget inside the model's hard limit. A higher
	// value overshoots a small window and the request 400s on overflow.
	avgBytesPerToken       = 3
	modelBudgetWindowPct   = 50 // evidence-gathering cap ~= half the window
	contextBudgetWindowPct = 75 // compaction guard ~= 3/4 the window for response headroom

	// fallbackModelByteBudget is used when the endpoint does not report a
	// context window. The compaction guard stays off in that case.
	fallbackModelByteBudget = 300_000

	// gcsByteBudget is the fixed aggregate ceiling on bytes fetched from GCS
	// across one analysis. It is a runaway-fetch safety cap, not a tuning knob.
	gcsByteBudget = 1_000_000_000
)

// analyzeFailuresWithAI runs the agentic AI analysis on every failed test
// case. The agentic tool-calling loop is the only analysis path.
func (p *pipeline) analyzeFailuresWithAI(ctx context.Context, details []models.JobDetail, flakinessReport models.FlakinessReport) {
	runtime, err := p.ensureAnalysisRuntime(ctx)
	if err != nil {
		log.Printf("⚠ AI runtime setup failed: %v", err)
		return
	}
	cfg := p.cfg
	aiClient := runtime.client
	defer func() {
		if err := aiClient.Cache().Save(); err != nil {
			log.Printf("Warning: failed to save AI cache: %v", err)
		}
	}()

	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.JobID+"::"+tf.TestName] = tf.ConsecutiveFailures
	}

	module := universal.New()
	service := ai.NewService(aiClient, module, p.aiSystemPrompt, consecutiveMap)
	// Resolve and verify repo-relative file citations against branding.source_repo.
	service.SetSourceRepo(cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name)
	// Ground the recurring-pattern agent on the real source tree when a repo is
	// configured, so it verifies file/config paths instead of guessing. The
	// token clears GitHub's anonymous trees-API rate limit; file reads use the
	// raw CDN and need none. Without a repo or token it falls back to the
	// tool-free correlation call plus the path-verification guard.
	if cfg.Branding.SourceRepo.Owner != "" && cfg.Branding.SourceRepo.Name != "" {
		if ghToken := githubReadToken(); ghToken != "" {
			service.SetPatternRepoReader(ai.NewGitHubRepoReader(
				cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name, "", ghToken))
			log.Printf("🔎 Pattern agent grounded on %s/%s source tree",
				cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name)
		}
	}

	eff := cfg.AI.EffectiveAgentic()
	factory := artifacts.NewBackendFactory(p.backend, cfg.Storage.Bucket)
	service.EnableAgentic(ai.AgenticOptions{
		MaxIters:           eff.MaxIters,
		ModelByteBudget:    runtime.modelByteBudget,
		GCSByteBudget:      gcsByteBudget,
		Timeout:            eff.Timeout,
		ContextByteBudget:  runtime.contextByteBudget,
		MinToolCalls:       eff.MinToolCalls,
		MinGCSBytes:        eff.MinGCSBytes,
		CritiqueMaxRetries: *eff.Critique.MaxRetries,
		SingleToolCall:     eff.SingleToolCall,
		SemanticJudge:      true,
	}, factory, runtime.registry, runtime.enabledTools)
	// nil is safe; without recipes the service skips skill matching.
	service.SetSkills(p.aiSkillSet)
	skillsLog := "off"
	if p.aiSkillSet != nil && len(p.aiSkillSet.Skills()) > 0 {
		skillsLog = fmt.Sprintf("on/%d", len(p.aiSkillSet.Skills()))
	}
	log.Printf("🤖 Agentic AI enabled (%d iters, %dKB model, %dMB gcs, %s timeout, min_tools=%d, min_gcs_kb=%d, critique=on/%d, skills=%s, tools=%v)",
		eff.MaxIters, runtime.modelByteBudget/1024, gcsByteBudget/1024/1024, eff.Timeout, eff.MinToolCalls, eff.MinGCSBytes/1024, *eff.Critique.MaxRetries, skillsLog, runtime.enabledTools)
	// The endpoint and model are deliberately kept out of published data files;
	// in the pages deployment the fetcher's stdout is a public Actions build log,
	// so only disclose them when an operator opts in.
	if os.Getenv("AI_LOG_ENDPOINT") == "1" {
		log.Printf("Using AI endpoint: %s, model: %s", aiClient.Endpoint(), aiClient.ModelName())
	} else {
		log.Printf("AI client configured (set AI_LOG_ENDPOINT=1 to log endpoint and model)")
	}

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

func (p *pipeline) ensureAnalysisRuntime(ctx context.Context) (*analysisRuntime, error) {
	if p.aiRuntime != nil {
		return p.aiRuntime, nil
	}
	client := ai.NewClientWithOptions(ai.Options{
		Token:        p.aiToken,
		CacheDir:     p.opts.OutDir,
		Endpoint:     aiEndpoint(p.cfg),
		Model:        aiModel(p.cfg),
		ExtraHeaders: aiHeaders(p.cfg),
	})
	modelByteBudget := fallbackModelByteBudget
	contextByteBudget := 0
	if tokens, ok := client.DetectContextWindowTokens(ctx); ok {
		windowBytes := tokens * avgBytesPerToken
		modelByteBudget = windowBytes * modelBudgetWindowPct / 100
		contextByteBudget = windowBytes * contextBudgetWindowPct / 100
		log.Printf("🪟 detected context window: %d tokens (~%d KB); model_byte_budget=%d KB, context_byte_budget=%d KB",
			tokens, windowBytes/1024, modelByteBudget/1024, contextByteBudget/1024)
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	toolNames := p.cfg.AI.EffectiveAgentic().Tools
	if len(toolNames) == 0 {
		toolNames = []string{"filesystem", "k8s"}
	}
	enabled, err := registry.Enable(toolNames)
	if err != nil {
		return nil, fmt.Errorf("AI tool configuration: %w", err)
	}
	p.aiRuntime = &analysisRuntime{
		client:            client,
		registry:          registry,
		enabledTools:      enabled,
		modelByteBudget:   modelByteBudget,
		contextByteBudget: contextByteBudget,
	}
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

// aiEndpoint returns the configured AI chat-completions URL.
// project.yaml wins over AI_ENDPOINT.
func aiEndpoint(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Endpoint
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

// shortHash returns a short SkillSet hash prefix for startup logs.
func shortHash(h string) string {
	if len(h) == 0 {
		return ""
	}
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// aiModel returns the configured AI model identifier.
// project.yaml wins over AI_MODEL.
func aiModel(cfg *project.Config) string {
	return cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Model
}

// aiHeaders returns the extra HTTP headers to attach to AI provider requests.
func aiHeaders(cfg *project.Config) map[string]string {
	return cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")).Headers
}
