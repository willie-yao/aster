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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
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
	jobCatalog        *jobconfig.Catalog
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
		if err := project.ValidateAIAPI(aiAPI(cfg)); err != nil {
			return nil, err
		}
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

		// Compose engine profiles with consumer recipes. Parse or regex compile
		// errors are hard startup errors for every selected source.
		set, profileSelection, err := skills.LoadForTools(opts.ProjectDir, cfg.AI.EffectiveAgentic().Tools)
		if err != nil {
			return nil, fmt.Errorf("loading AI skills: %w", err)
		}
		aiSkillSet = set
		log.Printf("Loaded %d AI skill recipe(s) (profiles=%s, hash=%s)",
			len(aiSkillSet.Skills()), profileSelection.String(), shortHash(aiSkillSet.Hash()))
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
		targetRepo := configuredFixRepo(cfg)
		jobs, p.jobCatalog, err = jobconfig.FetchJobConfigsAndCatalog(ctx, p.client, cfg, targetRepo)
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
	if err := clearAnalysisTrace(opts.OutDir); err != nil {
		log.Printf("Warning: failed to clear stale AI traces: %v", err)
	}

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

func clearAnalysisTrace(outDir string) error {
	err := os.Remove(filepath.Join(outDir, output.AITraceFilename))
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
				sender, err := newEmailSender(notify.SMTPConfig{
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

	// Recover existing fix state before issue recovery or new automatic writes.
	remediationReady := true
	if err := p.processRemediations(ctx, flakinessReport.RecurringPatterns, details); err != nil {
		sideEffectErrs = append(sideEffectErrs, err)
		remediationReady = false
	}
	fixStateChanged := false
	if remediationReady {
		ledger, err := remediation.LoadForRepo(opts.OutDir, configuredFixRepo(cfg))
		if err != nil {
			sideEffectErrs = append(sideEffectErrs, fmt.Errorf("load remediation state for fix PRs: %w", err))
		} else {
			fixPatterns := remediation.UntrackedPatterns(ledger, flakinessReport.RecurringPatterns, details)
			if skipped := len(flakinessReport.RecurringPatterns) - len(fixPatterns); skipped > 0 {
				log.Printf("Fix PRs: %d pattern(s) already have remediation history; skipping duplicate proposals", skipped)
			}
			var fixErr error
			fixStateChanged, fixErr = processFixPRs(ctx, cfg, fixPatterns, p.aiToken, opts.OutDir)
			if fixErr != nil {
				sideEffectErrs = append(sideEffectErrs, fixErr)
			}
		}
	}
	// Adopt a PR created in this pass before issues evaluate recovery.
	if fixStateChanged {
		if err := p.processRemediations(ctx, flakinessReport.RecurringPatterns, details); err != nil {
			sideEffectErrs = append(sideEffectErrs, err)
		}
	}
	if err := processIssues(ctx, cfg, flakinessReport, details, p.aiToken, p.enableAI, opts.OutDir); err != nil {
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
			API:          aiAPI(cfg),
			Endpoint:     aiEndpoint(cfg),
			Model:        aiModel(cfg),
			ExtraHeaders: aiHeaders(cfg),
		})
		filler = repotemplate.NewIssueFiller(token, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}

	client := issues.NewClient(token, eff.Repo.Owner, eff.Repo.Name)
	targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
	ledger, err := remediation.LoadForRepo(outDir, configuredFixRepo(cfg))
	if err != nil {
		return fmt.Errorf("load remediation state for issues: %w", err)
	}
	keepOpen, retire := remediationIssueLifecycleKeys(ledger, targetRepo)
	mgr := issues.NewManager(client, filepath.Join(outDir, "issue_state.json"), targetRepo, issues.Options{
		CommentOnRecovery: eff.CommentOnRecovery == nil || *eff.CommentOnRecovery,
		CloseOnRecovery:   eff.CloseOnRecovery,
		MaxNewPerRun:      eff.MaxNewPerRun,
		RecoverPrefixes:   issues.RecoverPrefixesFor(eff.Triggers),
		KeepOpenKeys:      keepOpen,
		RetireKeys:        retire,
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
func processFixPRs(ctx context.Context, cfg *project.Config, patterns []models.PatternAnalysis, aiToken, outDir string) (bool, error) {
	if cfg.AI == nil || cfg.AI.FixPRs == nil || !cfg.AI.FixPRs.Enabled {
		return false, nil
	}
	if len(patterns) == 0 {
		return false, nil
	}
	eff := cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		log.Println("Fix PRs: no source repo resolved (set ai.fix_prs.repo or branding.source_repo); skipping")
		return false, fmt.Errorf("fix PRs: no source repo resolved")
	}
	fixToken := os.Getenv("FIX_TOKEN")
	if fixToken == "" {
		log.Println("Fix PRs: enabled but FIX_TOKEN is unset; skipping")
		return false, fmt.Errorf("fix PRs: FIX_TOKEN is unset")
	}

	provider := cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"))
	if err := project.ValidateAIAPI(provider.API); err != nil {
		return false, fmt.Errorf("fix PRs: %w", err)
	}
	if eff.AgentRuntime.Type != "orka" && provider.API == project.AIAPIResponses {
		return false, fmt.Errorf("fix PRs: local runtime requires chat_completions or an Orka fix runtime")
	}
	var aiClient *ai.Client
	if aiToken != "" && provider.Endpoint != "" && provider.Model != "" {
		aiClient = ai.NewClientWithOptions(ai.Options{Token: aiToken, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model, ExtraHeaders: provider.Headers})
	}
	if eff.AgentRuntime.Type != "orka" && aiClient == nil {
		log.Println("Fix PRs: local runtime requires AI_TOKEN, endpoint, and model; skipping")
		return false, fmt.Errorf("fix PRs: local runtime requires AI_TOKEN, endpoint, and model")
	}

	critique, critiqueRetries, err := fixruntime.Critique(aiClient, eff.CritiqueRetries)
	if err != nil {
		log.Printf("Fix PRs: %v; skipping", err)
		return false, fmt.Errorf("fix PR critique: %w", err)
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
		return false, fmt.Errorf("fix PR runtime: %w", err)
	}
	model := ar.Model
	if model == "" {
		model = aiModel(cfg)
	}
	fixOpts.Agent = &fixpr.AgentConfig{
		Runtime:             agentRuntime,
		API:                 aiAPI(cfg),
		SharedModelEndpoint: ar.Type != "orka",
		Model:               model,
		Endpoint:            aiEndpoint(cfg),
		ModelToken:          aiToken,
		MaxTurns:            ar.MaxTurns,
		AllowBash:           allowBash,
		Timeout:             ar.ParsedTimeout(),
		GitToken:            fixToken,
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
	changed := !eff.DryRun && saveErr == nil && stats.Proposed+stats.Adopted > 0
	return changed, errors.Join(wrapOptional("fix-PR processing", err), wrapOptional("save fix-PR state", saveErr))
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
			// Incomplete JUnit may become available after the build finishes.
			if r.Result != "PENDING" && r.Result != "" && (r.JUnitComplete || r.JUnitTruncated) {
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

	junitPaths, complete, truncated, err := prowbuild.DiscoverJUnitPathsWithStatus(ctx, backend, loc)
	result.JUnitComplete = complete
	result.JUnitTruncated = truncated
	if err != nil {
		result.JUnitComplete = false
		result.JUnitTruncated = false
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
			result.JUnitComplete = false
			result.JUnitTruncated = false
			log.Printf("    ⚠ %s/%s: fetching %s: %v", job.Name, build.ID, path.Base(junitPath), err)
			continue
		}
		testCases, err := junit.ParseFile(junitData, path.Base(junitPath))
		if err != nil {
			result.JUnitComplete = false
			result.JUnitTruncated = false
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

func configuredFixRepo(cfg *project.Config) string {
	if cfg == nil || cfg.AI == nil || cfg.AI.FixPRs == nil {
		return ""
	}
	eff := cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return ""
	}
	return eff.Repo.Owner + "/" + eff.Repo.Name
}

func removeRemediationPublicState(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, remediation.PublicFileName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *pipeline) processRemediations(ctx context.Context, patterns []models.PatternAnalysis, details []models.JobDetail) error {
	targetRepo := configuredFixRepo(p.cfg)
	if targetRepo == "" {
		return removeRemediationPublicState(p.opts.OutDir)
	}
	if p.cfg.EffectiveFixPRs().DryRun {
		return removeRemediationPublicState(p.opts.OutDir)
	}
	fixState := statefile.Load[fixpr.TrackedFix](filepath.Join(p.opts.OutDir, "fix_pr_state.json"), targetRepo, "fix PRs")
	ledger, err := remediation.LoadForRepo(p.opts.OutDir, targetRepo)
	if err != nil {
		return err
	}
	if len(fixState.Tracked) == 0 && len(ledger.Remediations) == 0 && len(patterns) == 0 {
		return ledger.Save(p.opts.OutDir)
	}
	fixes := make(map[string]remediation.FixReference, len(fixState.Tracked))
	for key, fix := range fixState.Tracked {
		if !fix.HasPatternSnapshot() {
			continue
		}
		pattern := fix.Pattern
		fixes[key] = remediation.FixReference{URL: fix.URL, OpenedAt: fix.OpenedAt, Pattern: &pattern}
	}
	if p.jobCatalog == nil && p.cfg.TestGrid.Dashboard != "" {
		_, catalog, err := jobconfig.FetchJobConfigsAndCatalog(ctx, p.client, p.cfg, targetRepo)
		if err != nil {
			log.Printf("Warning: test-infra verification metadata unavailable: %v", err)
		} else {
			p.jobCatalog = catalog
		}
	}
	if p.jobCatalog == nil && p.cfg.EffectiveDiscoverySource() == project.DiscoveryBucket {
		jobs, err := prowbuild.DiscoverJobs(ctx, p.backend, true, nil)
		if err != nil {
			log.Printf("Warning: bucket verification metadata unavailable: %v", err)
		} else {
			p.jobCatalog = jobconfig.CatalogFromJobs(jobs, "bucket")
		}
	}
	var coverage *remediation.CoverageCatalog
	if p.jobCatalog != nil {
		coverageRepos := remediationCoverageRepos(targetRepo, patterns, details, ledger)
		coverage = remediation.LoadCoverageCatalog(p.opts.OutDir, p.jobCatalog.Revision, coverageRepos, time.Now().UTC())
		if coverage == nil {
			built, err := remediation.BuildCoverageCatalog(ctx, p.backend, p.jobCatalog, coverageRepos)
			coverage = built
			if err != nil {
				log.Printf("Warning: Prow verification catalog is partial: %v", err)
			} else if coverage != nil {
				if err := coverage.Save(p.opts.OutDir); err != nil {
					log.Printf("Warning: failed to save Prow verification catalog: %v", err)
				}
			}
		}
	}
	token := os.Getenv("FIX_TOKEN")
	if token == "" {
		token = os.Getenv("BOT_TOKEN")
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		token = os.Getenv("ISSUE_TOKEN")
	}
	client := ghpr.NewClient(p.client, token)
	reconciler := remediation.NewReconciler(client, p.opts.OutDir)
	reconciler.SetVerification(p.backend, p.jobCatalog, coverage, client)
	reconciler.SetRecovery(targetRepo, client)
	issueConfig := p.cfg.EffectiveIssues()
	issueRepo := ""
	if p.cfg.Issues != nil && p.cfg.Issues.Enabled && issueConfig.Repo != nil && issueConfig.Repo.Owner != "" && issueConfig.Repo.Name != "" {
		issueRepo = issueConfig.Repo.Owner + "/" + issueConfig.Repo.Name
		issueState := statefile.Load[issues.TrackedIssue](filepath.Join(p.opts.OutDir, "issue_state.json"), issueRepo, "issues")
		trackedIssues := map[string]remediation.IssueRef{}
		for key, tracked := range issueState.Tracked {
			if !strings.HasPrefix(key, issues.KeyPrefixPattern) {
				continue
			}
			jobID := strings.TrimPrefix(key, issues.KeyPrefixPattern)
			trackedIssues[jobID] = remediation.IssueRef{Number: tracked.Number, URL: tracked.URL, Repo: issueRepo}
		}
		var issueClient remediation.IssueLifecycleClient
		if issueToken := os.Getenv("ISSUE_TOKEN"); issueToken != "" {
			issueClient = issues.NewClient(issueToken, issueConfig.Repo.Owner, issueConfig.Repo.Name)
		}
		reconciler.SetIssues(issueRepo, trackedIssues, issueClient)
	}
	state, err := reconciler.Reconcile(ctx, patterns, details, fixes, fixpr.KeyFor)
	if err != nil {
		log.Printf("Warning: remediation reconciliation failed: %v", err)
	}
	emailErr := p.sendRemediationEmails(ctx, state)
	if emailErr != nil {
		log.Printf("Warning: remediation email failed: %v", emailErr)
	}
	return errors.Join(wrapOptional("remediation reconciliation", err), wrapOptional("remediation email", emailErr))
}

func remediationIssueLifecycleKeys(state *remediation.State, issueRepo string) (map[string]bool, map[string]bool) {
	keepOpen := map[string]bool{}
	retire := map[string]bool{}
	if state == nil || issueRepo == "" {
		return keepOpen, retire
	}
	for _, entry := range state.Remediations {
		if entry == nil || entry.Issue == nil || entry.Issue.Repo != issueRepo || len(entry.Attempts) == 0 {
			continue
		}
		key := issues.KeyPrefixPattern + entry.JobID
		latest := entry.Attempts[len(entry.Attempts)-1]
		if latest.Status == remediation.StatusVerifiedFixed {
			retire[key] = true
			continue
		}
		keepOpen[key] = true
	}
	for key := range keepOpen {
		delete(retire, key)
	}
	return keepOpen, retire
}

func remediationCoverageRepos(targetRepo string, patterns []models.PatternAnalysis, details []models.JobDetail, ledger *remediation.State) []string {
	repos := map[string]bool{targetRepo: targetRepo != ""}
	jobs := map[string]bool{}
	for _, pattern := range patterns {
		jobs[pattern.JobID] = true
	}
	if ledger != nil {
		for _, entry := range ledger.Remediations {
			if entry != nil {
				jobs[entry.JobID] = true
				if entry.SourceRepo != "" {
					repos[entry.SourceRepo] = true
				}
			}
		}
	}
	for _, detail := range details {
		if !jobs[detail.JobID] {
			continue
		}
		if detail.Repo != "" {
			repos[detail.Repo] = true
		}
		for _, run := range detail.Runs {
			for repo := range run.RepoRefs {
				repos[repo] = true
			}
		}
	}
	out := make([]string, 0, len(repos))
	for repo, include := range repos {
		if include && strings.Contains(repo, "/") {
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

var newEmailSender = func(config notify.SMTPConfig) (notify.Sender, error) {
	return notify.NewSMTPSender(config)
}

func (p *pipeline) sendRemediationEmails(ctx context.Context, state *remediation.State) error {
	email, enabled := p.cfg.EffectiveEmailNotifications()
	if !enabled || state == nil {
		return nil
	}
	password := os.Getenv("EMAIL_SMTP_PASSWORD")
	if email.SMTP.Username != "" && password == "" {
		return fmt.Errorf("EMAIL_SMTP_PASSWORD is unset")
	}
	from, recipients, err := notify.ParseAddresses(email.From, email.To)
	if err != nil {
		return err
	}
	sender, err := newEmailSender(notify.SMTPConfig{
		Host: email.SMTP.Host, Port: email.SMTP.Port, Username: email.SMTP.Username,
		Password: password, TLSMode: email.SMTP.TLS,
	})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(state.Remediations))
	for id := range state.Remediations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var errs []error
	changed := false
	for _, id := range ids {
		entry := state.Remediations[id]
		if entry == nil || len(entry.Attempts) == 0 {
			continue
		}
		attempt := &entry.Attempts[len(entry.Attempts)-1]
		if attempt.LastTransition == "" || attempt.TransitionIndex == attempt.LastEmailedTransitionIndex || !remediationEmailStatus(attempt.Status) {
			continue
		}
		dashboardURL := strings.TrimRight(p.cfg.Branding.SiteURL, "/") + "/job/" + url.PathEscape(entry.JobID)
		message := notify.RemediationUpdateMessage(notify.RemediationUpdate{
			From: from, To: recipients, ProjectName: p.cfg.Name, JobName: entry.JobName,
			Status: attempt.Status, Reason: attempt.OutcomeReason, PullURL: attempt.URL,
			DashboardURL: dashboardURL,
		})
		if err := sender.Send(ctx, message); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		attempt.LastEmailedTransition = attempt.LastTransition
		attempt.LastEmailedTransitionIndex = attempt.TransitionIndex
		changed = true
	}
	if changed {
		if err := state.Save(p.opts.OutDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func remediationEmailStatus(status string) bool {
	switch status {
	case remediation.StatusAwaitingPresubmit, remediation.StatusPresubmitRunning,
		remediation.StatusPremergeVerified, remediation.StatusPresubmitFailedSameCause,
		remediation.StatusPresubmitFailedDifferentCause, remediation.StatusObserving,
		remediation.StatusVerifiedFixed, remediation.StatusStillFailingSameCause,
		remediation.StatusFailingDifferentCause, remediation.StatusInconclusive:
		return true
	default:
		return false
	}
}
