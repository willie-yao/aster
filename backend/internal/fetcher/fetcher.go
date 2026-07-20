// Package fetcher contains the orchestration invoked by cmd/fetcher:
// loading project config, discovering jobs, fetching builds, running AI
// analysis, and writing dashboard output.
package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Options is the parsed invocation for a single fetcher run.
// cmd/fetcher constructs it from flags before Run.
type Options struct {
	ProjectDir   string
	OutDir       string
	BuildsPerJob int
	Workers      int
	Timeout      time.Duration
	// IncludePresubmits fetches presubmit jobs in addition to periodics.
	// It is combined with cfg.Source.IncludePresubmits, so either source can
	// enable presubmits.
	IncludePresubmits bool
	EnableAI          bool
	// SkipSideEffects writes dashboard data without notifications or GitHub writes.
	SkipSideEffects bool
	// Version is the engine version embedded at build time, logged at startup.
	Version string
}

// pipeline holds the resolved, reusable state for a run: config, storage, and
// AI settings. It is built once by setupPipeline and drives one
// or many passes (one-shot Run, or repeated passes in RunWatch).
type pipeline struct {
	opts              Options
	cfg               *project.Config
	client            *http.Client
	backend           storage.Backend
	enableAI          bool
	aiToken           string
	aiSystemPrompt    string
	aiSkillSet        *skills.Set
	includePresubmits bool
	aiRuntime         *analysisRuntime
}

type analysisRuntime struct {
	client            *ai.Client
	registry          *tools.Registry
	enabledTools      []string
	modelByteBudget   int
	contextByteBudget int
}

// refreshResult carries the outputs a pass needs for its side effects.
type refreshResult struct {
	details   []models.JobDetail
	flakiness models.FlakinessReport
}

// Run executes the full pipeline once: load, discover, fetch, aggregate,
// analyze, write output, and notify. Per-job fetch errors are logged but do not
// abort.
func Run(ctx context.Context, opts Options) error {
	p, err := setupPipeline(opts)
	if err != nil {
		return err
	}
	if _, err := p.fullPass(ctx); err != nil {
		return err
	}
	log.Println("Done!")
	return nil
}

// setupPipeline loads config and resolves storage and AI settings.
func setupPipeline(opts Options) (*pipeline, error) {
	cfg, err := project.Load(filepath.Join(opts.ProjectDir, "project.yaml"))
	if err != nil {
		return nil, fmt.Errorf("loading project config: %w", err)
	}
	log.Printf("Project: %s (%s) storage=%s bucket=%s",
		cfg.Name, cfg.DisplayShortName(), cfg.StorageConfig().Provider, cfg.Storage.Bucket)
	if opts.Version != "" {
		log.Printf("Engine version: %s", opts.Version)
	}

	// AI_TOKEN authenticates the configured chat-completions endpoint.
	enableAI := opts.EnableAI
	aiToken := os.Getenv("AI_TOKEN")
	if enableAI && aiToken == "" {
		log.Println("Warning: -ai enabled but AI_TOKEN is not set, disabling AI analysis")
		enableAI = false
	}
	// AI needs an explicit endpoint and model. Fail fast rather than publishing a
	// dashboard with missing analysis.
	if enableAI {
		if aiEndpoint(cfg) == "" || aiModel(cfg) == "" {
			return nil, fmt.Errorf("AI is enabled but no provider is configured: set ai.endpoint and ai.model in project.yaml, or the AI_ENDPOINT and AI_MODEL env vars")
		}
	}

	// Load the prompt and skills only once AI is confirmed enabled and
	// configured, so a config error surfaces before any content errors.
	var aiSystemPrompt string
	var aiSkillSet *skills.Set
	if enableAI {
		prompt, err := project.LoadPrompt(opts.ProjectDir)
		if err != nil {
			return nil, fmt.Errorf("loading AI prompt: %w", err)
		}
		aiSystemPrompt = ai.ComposeSystemPrompt(prompt)

		// Load consumer-owned recipes from <project_dir>/skills/*.yaml.
		// A missing directory returns an empty Set.
		// Parse or regex compile errors are hard startup errors.
		set, err := skills.Load(opts.ProjectDir)
		if err != nil {
			return nil, fmt.Errorf("loading AI skills: %w", err)
		}
		aiSkillSet = set
		if n := len(aiSkillSet.Skills()); n > 0 {
			log.Printf("Loaded %d AI skill recipe(s) from %s/skills/ (hash=%s)",
				n, opts.ProjectDir, shortHash(aiSkillSet.Hash()))
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	backend, err := storage.New(cfg.StorageConfig(), client)
	if err != nil {
		return nil, fmt.Errorf("configuring storage: %w", err)
	}

	return &pipeline{
		opts:              opts,
		cfg:               cfg,
		client:            client,
		backend:           backend,
		enableAI:          enableAI,
		aiToken:           aiToken,
		aiSystemPrompt:    aiSystemPrompt,
		aiSkillSet:        aiSkillSet,
		includePresubmits: opts.IncludePresubmits || cfg.Source.IncludePresubmits,
	}, nil
}

// fullPass runs discovery, a data refresh, and side effects under the run
// timeout. It returns the discovered jobs so callers can reuse them.
func (p *pipeline) fullPass(ctx context.Context) ([]models.ProwJob, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	jobs, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	res, err := p.refresh(ctx, jobs)
	if err != nil {
		return nil, err
	}
	if !p.opts.SkipSideEffects {
		if err := p.runSideEffects(ctx, res); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// discover lists the project's jobs from test-infra or the artifact bucket.
func (p *pipeline) discover(ctx context.Context) ([]models.ProwJob, error) {
	cfg := p.cfg
	var jobs []models.ProwJob
	var err error
	switch cfg.EffectiveDiscoverySource() {
	case project.DiscoveryBucket:
		log.Println("Discovering jobs from the storage bucket...")
		jobs, err = prowbuild.DiscoverJobs(ctx, p.backend, p.includePresubmits, cfg.Discovery.JobFilters)
		if err != nil {
			return nil, fmt.Errorf("discovering jobs from bucket: %w", err)
		}
		// Bucket discovery has no job-config YAML, so assign categories here.
		for i := range jobs {
			jobs[i].Category = cfg.Categorize(jobs[i].Name)
		}
	default:
		log.Println("Fetching job configs from test-infra...")
		jobs, err = jobconfig.FetchJobConfigs(ctx, p.client, cfg)
		if err != nil {
			return nil, fmt.Errorf("fetching job configs: %w", err)
		}
		if !p.includePresubmits {
			var periodic []models.ProwJob
			for _, j := range jobs {
				if j.JobType == models.JobTypePeriodic {
					periodic = append(periodic, j)
				}
			}
			jobs = periodic
		}
	}
	log.Printf("Discovered %d jobs (presubmits=%v)", len(jobs), p.includePresubmits)

	// Derive the display-only short-name prefix from the discovered set so
	// the frontend can render compact job names without consumers having to
	// hand-maintain the prefix.
	cfg.ShortNamePrefix = jobconfig.DerivePeriodicPrefix(jobs)
	return jobs, nil
}

// refresh fetches each job's builds, aggregates, runs AI analysis, and writes
// the output JSON. Completed builds are reused from the on-disk cache, so
// repeated passes only fetch new or still-running builds.
func (p *pipeline) refresh(ctx context.Context, jobs []models.ProwJob) (*refreshResult, error) {
	cfg, opts := p.cfg, p.opts

	// Fetch each job's builds. Cached completed builds are reused.
	cachedJobs := loadCachedJobDetails(opts.OutDir)

	type jobResult struct {
		job  models.ProwJob
		runs []models.BuildResult
	}

	results := make([]jobResult, len(jobs))
	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fetchErrors []error

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j models.ProwJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			runs, err := fetchJobRunsCached(ctx, p.backend, cfg, &j, opts.BuildsPerJob, cachedJobs[j.JobID])
			if err != nil {
				mu.Lock()
				fetchErrors = append(fetchErrors, fmt.Errorf("job %s: %w", j.Name, err))
				mu.Unlock()
				log.Printf("  ⚠ %s: %v", j.Name, err)
				return
			}

			results[idx] = jobResult{job: j, runs: runs}
			passed := 0
			for _, r := range runs {
				if r.Passed {
					passed++
				}
			}
			log.Printf("  ✓ %s: %d runs (%d passed)", j.Name, len(runs), passed)
		}(i, job)
	}
	wg.Wait()

	if len(fetchErrors) > 0 {
		log.Printf("Warning: %d jobs had fetch errors", len(fetchErrors))
	}

	now := time.Now().UTC()
	dashboard := models.Dashboard{GeneratedAt: now}
	var details []models.JobDetail

	for _, r := range results {
		if r.job.Name == "" {
			continue // skipped due to fetch error
		}
		dashboard.Jobs = append(dashboard.Jobs, aggregator.ComputeJobSummary(r.job, r.runs))
		details = append(details, models.JobDetail{
			Name:    r.job.Name,
			JobID:   r.job.JobID,
			JobType: r.job.JobType,
			Repo:    r.job.Repo,
			Runs:    r.runs,
		})
	}

	jobResultMap := make(map[string][]models.BuildResult, len(results))
	for _, r := range results {
		if r.job.Name == "" {
			continue
		}
		jobResultMap[r.job.JobID] = r.runs
	}
	flakinessReport := aggregator.ComputeFlakinessReport(jobResultMap, jobs, now)
	log.Printf("Flakiness report: %d most flaky, %d persistent, %d recently broken",
		len(flakinessReport.MostFlaky), len(flakinessReport.PersistentFailures), len(flakinessReport.RecentlyBroken))

	searchIndex := aggregator.BuildSearchIndex(jobResultMap, jobs, now)
	log.Printf("Search index: %d entries", len(searchIndex.Entries))

	if p.enableAI {
		p.analyzeFailuresWithAI(ctx, details, flakinessReport)
		patterns.AssignIDs(details)
		// Fold systemic job-level verdicts into flakiness.json for the home page.
		flakinessReport.RecurringPatterns = collectRecurringPatterns(details)
		if n := len(flakinessReport.RecurringPatterns); n > 0 {
			log.Printf("🔗 %d systemic recurring pattern(s) surfaced on the home page", n)
		}
	}

	// Auto-reopen resolved patterns that have recurred past their watermark, so
	// a fixed-then-flaked failure returns to the active view. The server may
	// also write resolved.json on an admin action; both use atomic writes, and a
	// rare lost update self-heals on the next pass (same trade-off as the other
	// *_state.json files).
	if rs := resolve.Load(opts.OutDir); len(rs.Resolved) > 0 {
		if pruned, changed := rs.Prune(flakinessReport.RecurringPatterns); changed {
			if err := pruned.Save(opts.OutDir); err != nil {
				log.Printf("Warning: failed to save resolved state: %v", err)
			} else {
				log.Printf("↩ re-opened %d resolved pattern(s) after recurrence", len(rs.Resolved)-len(pruned.Resolved))
			}
		}
	}

	log.Printf("Writing output to %s/ (%d jobs)", opts.OutDir, len(dashboard.Jobs))
	if err := output.WriteAll(opts.OutDir, cfg, dashboard, details, flakinessReport, searchIndex); err != nil {
		return nil, fmt.Errorf("writing output: %w", err)
	}

	return &refreshResult{details: details, flakiness: flakinessReport}, nil
}

// runSideEffects handles notifications, issue filing, and draft PRs. These are
// gated on their own env tokens and return joined operational errors.
func (p *pipeline) runSideEffects(ctx context.Context, res *refreshResult) error {
	cfg, opts := p.cfg, p.opts
	details := res.details
	flakinessReport := res.flakiness
	var sideEffectErrs []error

	if email, enabled := cfg.EffectiveEmailNotifications(); enabled {
		password := os.Getenv("EMAIL_SMTP_PASSWORD")
		if email.SMTP.Username != "" && password == "" {
			log.Println("Notifications: skipped (EMAIL_SMTP_PASSWORD is unset)")
			sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email notifications: EMAIL_SMTP_PASSWORD is unset"))
		} else {
			from, recipients, err := notify.ParseAddresses(email.From, email.To)
			if err != nil {
				log.Printf("Warning: invalid email notification addresses: %v", err)
				sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email addresses: %w", err))
			} else {
				sender, err := notify.NewSMTPSender(notify.SMTPConfig{
					Host:     email.SMTP.Host,
					Port:     email.SMTP.Port,
					Username: email.SMTP.Username,
					Password: password,
					TLSMode:  email.SMTP.TLS,
				})
				if err != nil {
					log.Printf("Warning: invalid email notification config: %v", err)
					sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email config: %w", err))
				} else {
					notifier := notify.NewNotifier(
						sender,
						from,
						recipients,
						filepath.Join(opts.OutDir, "notification_state.json"),
						cfg.Name,
						cfg.Branding.SiteURL,
						p.backend.ProwURL("logs/"),
						email.ActionLinks,
					)
					stats, processErr := notifier.ProcessFailures(ctx, flakinessReport, details)
					log.Printf("📧 Email notifications: %d failure alerts, %d pattern alerts, %d recoveries, %d failed deliveries",
						stats.NewAlerts, stats.PatternAlerts, stats.Recoveries, stats.Failed)
					if processErr != nil {
						log.Printf("Warning: email notification processing failed: %v", processErr)
						sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email notifications: %w", processErr))
					}
					if err := notifier.SaveState(); err != nil {
						log.Printf("Warning: failed to save notification state: %v", err)
						sideEffectErrs = append(sideEffectErrs, err)
					}
				}
			}
		}
	} else {
		log.Println("Notifications: skipped (email disabled)")
	}

	if err := processIssues(ctx, cfg, flakinessReport, details, p.aiToken, p.enableAI, opts.OutDir); err != nil {
		sideEffectErrs = append(sideEffectErrs, err)
	}

	if err := processFixPRs(ctx, cfg, flakinessReport.RecurringPatterns, p.aiToken, opts.OutDir); err != nil {
		sideEffectErrs = append(sideEffectErrs, err)
	}
	return errors.Join(sideEffectErrs...)
}

// RunWatch runs the pipeline continuously as a single writer: a lightweight
// refresh every watchInterval that reuses a cached job list, and a full pass
// (rediscovery plus side effects) every reconcileInterval. Cached completed
// builds mean each refresh only fetches new or still-running builds. It returns
// when ctx is cancelled.
func RunWatch(ctx context.Context, opts Options, watchInterval, reconcileInterval time.Duration) error {
	if watchInterval <= 0 || reconcileInterval <= 0 {
		return fmt.Errorf("watch and reconcile intervals must be positive (got watch=%s reconcile=%s)", watchInterval, reconcileInterval)
	}
	p, err := setupPipeline(opts)
	if err != nil {
		return err
	}

	// Seed the output and the job list with an initial full pass.
	jobs, err := p.fullPass(ctx)
	if err != nil {
		log.Printf("⚠ initial pass failed: %v", err)
	}

	log.Printf("👀 watching: refresh every %s, reconcile every %s", watchInterval, reconcileInterval)
	nextWatch := time.Now().Add(watchInterval)
	nextReconcile := time.Now().Add(reconcileInterval)
	for {
		next := nextWatch
		if nextReconcile.Before(next) {
			next = nextReconcile
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		now := time.Now()
		if !now.Before(nextReconcile) {
			if newJobs, err := p.fullPass(ctx); err != nil {
				log.Printf("⚠ reconcile failed: %v", err)
			} else {
				jobs = newJobs
			}
			nextReconcile = now.Add(reconcileInterval)
			nextWatch = now.Add(watchInterval)
			continue
		}
		if len(jobs) == 0 {
			if newJobs, err := p.fullPass(ctx); err != nil {
				log.Printf("⚠ discovery retry failed: %v", err)
			} else {
				jobs = newJobs
			}
			nextWatch = now.Add(watchInterval)
			continue
		}
		passCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		if _, err := p.refresh(passCtx, jobs); err != nil {
			log.Printf("⚠ refresh failed: %v", err)
		}
		cancel()
		nextWatch = now.Add(watchInterval)
	}
}

// processIssues reconciles the project's highest-signal findings into GitHub
// issues on the configured target repo. Gated on issues.enabled and ISSUE_TOKEN.
func processIssues(ctx context.Context, cfg *project.Config, report models.FlakinessReport, details []models.JobDetail, aiToken string, enableAI bool, outDir string) error {
	if cfg.Issues == nil || !cfg.Issues.Enabled {
		return nil
	}
	token := os.Getenv("ISSUE_TOKEN")
	if token == "" {
		log.Println("Issues: enabled in config but ISSUE_TOKEN is unset; skipping")
		return fmt.Errorf("issues: ISSUE_TOKEN is unset")
	}
	eff := cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		log.Println("Issues: no target repo resolved (set issues.repo or branding.source_repo); skipping")
		return fmt.Errorf("issues: no target repo resolved")
	}

	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		JobDetails:   details,
		Triggers:     eff.Triggers,
		Labels:       eff.Labels,
		DashboardURL: cfg.Branding.SiteURL,
	})

	// When AI is available, reformat issue bodies to follow the target repo's
	// issue template. Falls back to the default body when no template exists.
	var filler issues.TemplateFiller
	if enableAI {
		aiClient := ai.NewClientWithOptions(ai.Options{
			Token:        aiToken,
			Endpoint:     aiEndpoint(cfg),
			Model:        aiModel(cfg),
			ExtraHeaders: aiHeaders(cfg),
		})
		filler = repotemplate.NewIssueFiller(token, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}

	client := issues.NewClient(token, eff.Repo.Owner, eff.Repo.Name)
	targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
	mgr := issues.NewManager(client, filepath.Join(outDir, "issue_state.json"), targetRepo, issues.Options{
		CommentOnRecovery: eff.CommentOnRecovery == nil || *eff.CommentOnRecovery,
		CloseOnRecovery:   eff.CloseOnRecovery,
		MaxNewPerRun:      eff.MaxNewPerRun,
		RecoverPrefixes:   issues.RecoverPrefixesFor(eff.Triggers),
		TemplateFiller:    filler,
	})
	stats, err := mgr.Reconcile(ctx, specs)
	if err != nil {
		log.Printf("Warning: issue processing failed: %v", err)
	} else {
		log.Printf("🐙 Issues (%s/%s): %d filed, %d adopted, %d recovered",
			eff.Repo.Owner, eff.Repo.Name, stats.Created, stats.Adopted, stats.Recovered)
	}
	saveErr := mgr.SaveState()
	if saveErr != nil {
		log.Printf("Warning: failed to save issue state: %v", saveErr)
	}
	return errors.Join(wrapOptional("issue processing", err), wrapOptional("save issue state", saveErr))
}

var newBatchFixRuntime = fixruntime.New
var newBatchFixManager = func(token, stateFile string, opts fixpr.Options) *fixpr.Manager {
	return fixpr.NewManager(fixpr.NewClients(token), stateFile, opts)
}

// processFixPRs drafts minimal fix PRs against the source repo for systemic
// recurring patterns. Gated on ai.fix_prs.enabled and FIX_TOKEN (a CLA-signed
// operator PAT). In dry-run it writes previews instead of opening PRs. Any
// missing piece is a no-op.
func processFixPRs(ctx context.Context, cfg *project.Config, patterns []models.PatternAnalysis, aiToken, outDir string) error {
	if cfg.AI == nil || cfg.AI.FixPRs == nil || !cfg.AI.FixPRs.Enabled {
		return nil
	}
	if len(patterns) == 0 {
		return nil
	}
	eff := cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		log.Println("Fix PRs: no source repo resolved (set ai.fix_prs.repo or branding.source_repo); skipping")
		return fmt.Errorf("fix PRs: no source repo resolved")
	}
	fixToken := os.Getenv("FIX_TOKEN")
	if fixToken == "" {
		log.Println("Fix PRs: enabled but FIX_TOKEN is unset; skipping")
		return fmt.Errorf("fix PRs: FIX_TOKEN is unset")
	}

	provider := cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"))
	var aiClient *ai.Client
	if aiToken != "" && provider.Endpoint != "" && provider.Model != "" {
		aiClient = ai.NewClientWithOptions(ai.Options{Token: aiToken, Endpoint: provider.Endpoint, Model: provider.Model, ExtraHeaders: provider.Headers})
	}
	if eff.AgentRuntime.Type != "orka" && aiClient == nil {
		log.Println("Fix PRs: local runtime requires AI_TOKEN, endpoint, and model; skipping")
		return fmt.Errorf("fix PRs: local runtime requires AI_TOKEN, endpoint, and model")
	}

	critique, critiqueRetries, err := fixruntime.Critique(aiClient, eff.CritiqueRetries)
	if err != nil {
		log.Printf("Fix PRs: %v; skipping", err)
		return fmt.Errorf("fix PR critique: %w", err)
	}
	var prFiller fixpr.PRBodyFiller
	if aiClient != nil {
		prFiller = repotemplate.NewPRFiller(fixToken, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}

	fixOpts := fixpr.Options{
		SourceOwner:     eff.Repo.Owner,
		SourceName:      eff.Repo.Name,
		Fork:            eff.Fork == nil || *eff.Fork,
		AuthorName:      eff.AuthorName,
		AuthorEmail:     eff.AuthorEmail,
		MinConfidence:   eff.MinConfidence,
		MaxFiles:        eff.MaxFiles,
		MaxNewPerRun:    eff.MaxNewPerRun,
		Labels:          eff.Labels,
		DryRun:          eff.DryRun,
		PreviewFile:     filepath.Join(outDir, "fix_previews.json"),
		DashboardURL:    cfg.Branding.SiteURL,
		Critique:        critique,
		CritiqueRetries: critiqueRetries,
		PRFiller:        prFiller,
	}
	if eff.Verify != nil && eff.Verify.Enabled {
		fixOpts.Verify = &fixpr.VerifyConfig{
			Runtime:  runtime.NewLocal(),
			Commands: eff.Verify.ParsedCommands(),
			Timeout:  eff.Verify.ParsedTimeout(),
			Token:    fixToken,
		}
	}
	ar := eff.AgentRuntime
	allowBash := ar.AllowBash == nil || *ar.AllowBash
	agentRuntime, err := newBatchFixRuntime(ar)
	if err != nil {
		log.Printf("Fix PRs: %v; skipping", err)
		return fmt.Errorf("fix PR runtime: %w", err)
	}
	model := ar.Model
	if model == "" {
		model = aiModel(cfg)
	}
	fixOpts.Agent = &fixpr.AgentConfig{
		Runtime:    agentRuntime,
		Model:      model,
		Endpoint:   aiEndpoint(cfg),
		ModelToken: aiToken,
		MaxTurns:   ar.MaxTurns,
		AllowBash:  allowBash,
		Timeout:    ar.ParsedTimeout(),
		GitToken:   fixToken,
	}
	mgr := newBatchFixManager(fixToken, filepath.Join(outDir, "fix_pr_state.json"), fixOpts)
	stats, err := mgr.Reconcile(ctx, patterns)
	if err != nil {
		log.Printf("Warning: fix-PR processing failed: %v", err)
	} else if stats.Proposed+stats.Adopted+stats.Previewed > 0 {
		log.Printf("🛠️ Fix PRs (%s/%s): %d proposed, %d adopted, %d previewed",
			eff.Repo.Owner, eff.Repo.Name, stats.Proposed, stats.Adopted, stats.Previewed)
	}
	// Dry-run keeps no state (it re-previews each run).
	var saveErr error
	if !eff.DryRun {
		saveErr = mgr.SaveState()
		if saveErr != nil {
			log.Printf("Warning: failed to save fix-PR state: %v", saveErr)
		}
	}
	return errors.Join(wrapOptional("fix-PR processing", err), wrapOptional("save fix-PR state", saveErr))
}

func wrapOptional(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// loadCachedJobDetails loads existing per-job JSON files from the output dir.
// The returned map is JobID to build ID to cached BuildResult.
func loadCachedJobDetails(outDir string) map[string]map[string]models.BuildResult {
	cached := make(map[string]map[string]models.BuildResult)
	jobsDir := filepath.Join(outDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return cached
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, e.Name()))
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(data, &detail) != nil || detail.JobID == "" {
			continue
		}
		builds := make(map[string]models.BuildResult, len(detail.Runs))
		for _, r := range detail.Runs {
			// Only cache completed builds.
			if r.Result != "PENDING" && r.Result != "" {
				builds[r.BuildID] = r
			}
		}
		if len(builds) > 0 {
			cached[detail.JobID] = builds
		}
	}
	return cached
}

// fetchJobRunsCached discovers recent builds and reuses cached data for
// completed builds. Per-build fetch errors are logged but do not abort.
func fetchJobRunsCached(ctx context.Context, backend storage.Backend, cfg *project.Config, job *models.ProwJob, count int, cachedBuilds map[string]models.BuildResult) ([]models.BuildResult, error) {
	builds, err := prowbuild.ListRecentBuilds(ctx, backend, job, count)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	fetched, reused := 0, 0
	for _, b := range builds {
		if cached, ok := cachedBuilds[b.ID]; ok {
			runs = append(runs, cached)
			reused++
			continue
		}
		result, err := fetchBuildResult(ctx, backend, job, b)
		if err != nil {
			log.Printf("    ⚠ %s/%s: %v", job.Name, b.ID, err)
			continue
		}
		runs = append(runs, *result)
		fetched++
	}

	if reused > 0 {
		log.Printf("    💾 %s: %d cached, %d fetched", job.Name, reused, fetched)
	}

	return runs, nil
}

// fetchBuildResult fetches metadata and JUnit XML for a single build.
func fetchBuildResult(ctx context.Context, backend storage.Backend, job *models.ProwJob, build prowbuild.Build) (*models.BuildResult, error) {
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: job.JobType, Repo: job.Repo},
		JobName:     job.Name,
		BuildID:     build.ID,
		PullNumber:  build.PullNumber,
	}

	info, err := prowbuild.FetchBuildInfo(ctx, backend, loc)
	if err != nil {
		return nil, fmt.Errorf("fetching build info: %w", err)
	}

	result := &models.BuildResult{BuildInfo: *info, TestCases: []models.TestCase{}}

	junitPaths, err := prowbuild.DiscoverJUnitPaths(ctx, backend, loc)
	if err != nil {
		log.Printf("    ⚠ %s/%s: discovering junit files: %v", job.Name, build.ID, err)
		return result, nil
	}
	if len(junitPaths) == 0 {
		return result, nil
	}

	for _, junitPath := range junitPaths {
		result.JUnitURLs = append(result.JUnitURLs, backend.WebURL(junitPath))
		junitData, err := storage.ReadAll(ctx, backend, junitPath)
		if err != nil {
			log.Printf("    ⚠ %s/%s: fetching %s: %v", job.Name, build.ID, path.Base(junitPath), err)
			continue
		}
		testCases, err := junit.ParseFile(junitData, path.Base(junitPath))
		if err != nil {
			log.Printf("    ⚠ %s/%s: parsing %s: %v", job.Name, build.ID, path.Base(junitPath), err)
			continue
		}
		result.TestCases = append(result.TestCases, testCases...)
	}

	for _, tc := range result.TestCases {
		result.TestsTotal++
		switch tc.Status {
		case "passed":
			result.TestsPassed++
		case "failed":
			result.TestsFailed++
		case "skipped":
			result.TestsSkipped++
		}
	}

	return result, nil
}
