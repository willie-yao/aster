// Package fetcher contains the orchestration invoked by cmd/aster:
// loading project config, discovering jobs, fetching builds, running AI
// analysis, and writing dashboard output.
package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/causalcritic"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/fixruntime"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/junit"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/patternstate"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/repotemplate"
	"github.com/willie-yao/aster/backend/internal/resolve"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// Options is the parsed invocation for a single fetcher run.
// cmd/aster constructs it from flags before Run.
const AnalysisRuntimeInProcess = "inprocess"

// ShadowAnalysisOptions configure the private experimental Agent comparison
// path. It runs on Agent Sandbox after authoritative in-process publication and
// never changes public output.
type ShadowAnalysisOptions struct {
	Enabled          bool
	AgentVersion     string
	LedgerPath       string
	MaxPerRun        int
	MaxTurns         int
	Timeout          time.Duration
	Retries          int
	OutputLimitBytes int64
	ModelProvider    modelprovider.Config
}

// CausalCriticOptions configure the private sampled Agent Sandbox review path.
type CausalCriticOptions struct {
	Enabled          bool
	LedgerPath       string
	MaxPerRun        int
	Timeout          time.Duration
	OutputLimitBytes int64
	ModelGateway     runtime.ModelGatewayConfig
}

// AnalysisRuntimeOptions select where single-failure analysis runs.
type AnalysisRuntimeOptions struct {
	Type string
}

type Options struct {
	ProjectDir      string
	OutDir          string
	BuildsPerJob    int
	Workers         int
	Timeout         time.Duration
	AnalysisRuntime AnalysisRuntimeOptions
	ShadowAnalysis  ShadowAnalysisOptions
	CausalCritic    CausalCriticOptions
	// IncludePresubmits fetches presubmit jobs in addition to periodics.
	// It is combined with cfg.Source.IncludePresubmits, so either source can
	// enable presubmits.
	IncludePresubmits bool
	EnableAI          bool
	// SkipSideEffects writes dashboard data without notifications or GitHub writes.
	SkipSideEffects bool
	// Version is the engine version embedded at build time, logged at startup.
	Version string
	// TraceEngine is the build identity persisted with private analysis traces.
	TraceEngine ai.TraceEngine
}

// pipeline holds the resolved, reusable state for a run: config, storage, and
// AI settings. It is built once by setupPipeline and drives one
// or many passes (one-shot Run, or repeated passes in RunWatch).
type pipeline struct {
	opts                 Options
	cfg                  *project.Config
	client               *http.Client
	backend              storage.Backend
	enableAI             bool
	aiToken              string
	aiProject            *analysisruntime.Project
	includePresubmits    bool
	jobCatalog           *jobconfig.Catalog
	aiRuntime            *analysisruntime.Runtime
	usageRecorder        *aiusage.Recorder
	progress             *fetchprogress.Tracker
	aiRefreshTransaction *aiRefreshStateTransaction
	lastPatternOutcomes  map[string]patterns.JobOutcome
	shadowRunner         shadowAnalysisRunner
	shadowFreeze         shadowEvidenceFreezer
	shadowAppend         shadowLedgerAppender
	shadowClaim          shadowLedgerClaimer
	shadowNow            func() time.Time
	shadowAgentNamespace string
	shadowAgentRef       string
	criticReviewer       causalcritic.Reviewer
	criticFreeze         shadowEvidenceFreezer
	criticNow            func() time.Time
}

// refreshResult carries the outputs a pass needs for its side effects.
var writeAllOutput = output.WriteAll

type refreshResult struct {
	details   []models.JobDetail
	flakiness models.FlakinessReport
}

// Run executes the full pipeline once: load, discover, fetch, aggregate,
// analyze, write output, and notify. Per-job fetch errors are logged but do not
// abort.
func Run(ctx context.Context, opts Options) error {
	progress, stopProgress := startFetchProgress(ctx, opts, fetchprogress.PassOneShot)
	defer stopProgress()

	p, err := setupPipeline(opts)
	if err != nil {
		progress.FinishFailure(fetchprogress.FailureSetup)
		return err
	}
	p.progress = progress
	p.configureProgressAnalysisMetadata()
	progress.CompletePhase()
	_, err = p.fullPass(ctx)
	finishProgressPass(progress, err, false)
	if err != nil {
		return err
	}
	log.Println("Done!")
	return nil
}

// setupPipeline loads config and resolves storage and AI settings.
func setupPipeline(opts Options) (*pipeline, error) {
	if opts.AnalysisRuntime.Type == "" {
		opts.AnalysisRuntime.Type = AnalysisRuntimeInProcess
	}
	normalizeShadowAnalysisOptions(&opts.ShadowAnalysis)
	normalizeCausalCriticOptions(&opts.CausalCritic)
	if err := validateAnalysisRuntimeOptions(opts); err != nil {
		return nil, err
	}
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
		if opts.ShadowAnalysis.Enabled {
			return nil, fmt.Errorf("agent analysis shadow requires AI_TOKEN for authoritative in-process analysis")
		}
		if opts.CausalCritic.Enabled {
			return nil, fmt.Errorf("causal critic shadow requires AI_TOKEN for authoritative in-process analysis")
		}
		log.Println("Warning: -ai enabled but AI_TOKEN is not set, disabling AI analysis")
		enableAI = false
	}
	var aiProject *analysisruntime.Project
	if enableAI {
		fallbacks := analysisruntime.ProviderFallbacks{
			API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"), ReasoningEffort: os.Getenv(project.AIReasoningEffortEnv),
			CacheGeneration: os.Getenv(project.AICacheGenerationEnv),
		}
		aiProject, err = analysisruntime.LoadProject(opts.ProjectDir, cfg, fallbacks)
		if err != nil {
			return nil, err
		}
		log.Printf("Loaded AI skills (profiles=%s engine=%d consumer=%d consumer_bundle=%t hash=%s)",
			aiProject.ProfileSelection.String(), aiProject.SkillSet.EngineCount(), aiProject.SkillSet.ConsumerCount(),
			aiProject.SkillSet.ConsumerBundlePresent(), analysisruntime.ShortHash(aiProject.SkillSet.Hash()))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	backend, err := storage.New(cfg.StorageConfig(), client)
	if err != nil {
		return nil, fmt.Errorf("configuring storage: %w", err)
	}
	usageRecorder, err := analysisruntime.NewUsageRecorder(opts.OutDir, output.AIUsageFetcherFilename, cfg)
	if err != nil {
		return nil, fmt.Errorf("configuring AI usage accounting: %w", err)
	}

	return &pipeline{
		opts:              opts,
		cfg:               cfg,
		client:            client,
		backend:           backend,
		enableAI:          enableAI,
		aiToken:           aiToken,
		aiProject:         aiProject,
		usageRecorder:     usageRecorder,
		includePresubmits: opts.IncludePresubmits || cfg.Source.IncludePresubmits,
	}, nil
}

// fullPass runs discovery, a data refresh, and side effects under the run
// timeout. It returns the discovered jobs so callers can reuse them.
func (p *pipeline) fullPass(ctx context.Context) ([]models.ProwJob, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	p.startProgressPhase(fetchprogress.PhaseDiscovery)
	jobs, err := p.discover(fetchCtx)
	if err != nil {
		return nil, err
	}
	p.completeProgressPhase()
	res, err := p.refreshWithAnalysisContext(fetchCtx, fetchCtx, jobs)
	if err != nil {
		return nil, err
	}
	p.runPullRequestPass(fetchCtx, res)
	defer p.runShadowAnalysis(ctx, res)
	defer p.runCausalCritic(fetchCtx, res)
	if p.opts.SkipSideEffects {
		p.skipProgressSideEffects()
		return jobs, nil
	}
	p.startProgressPhase(fetchprogress.PhaseSideEffects)
	if err := p.runSideEffects(fetchCtx, res); err != nil {
		p.invalidateAnalysisRuntime()
		return nil, err
	}
	p.completeProgressPhase()
	return jobs, nil
}

func validateAnalysisRuntimeOptions(opts Options) error {
	if err := validateShadowAnalysisOptions(opts); err != nil {
		return err
	}
	if err := validateCausalCriticOptions(opts); err != nil {
		return err
	}
	if opts.AnalysisRuntime.Type != AnalysisRuntimeInProcess {
		return fmt.Errorf("unsupported analysis runtime %q", opts.AnalysisRuntime.Type)
	}
	return nil
}

func (p *pipeline) setJobCatalog(catalog *jobconfig.Catalog) {
	p.jobCatalog = catalog
	if catalog != nil && p.cfg.EffectiveDiscoverySource() == project.DiscoveryTestGrid {
		p.cfg.Discovery.ResolvedTestInfraRevision = catalog.Revision
	}
}

// discover lists the project's jobs from test-infra or the artifact bucket.
func (p *pipeline) discover(ctx context.Context) ([]models.ProwJob, error) {
	cfg := p.cfg
	var jobs []models.ProwJob
	var err error
	switch cfg.EffectiveDiscoverySource() {
	case project.DiscoveryBucket:
		log.Println("Discovering jobs from the storage bucket...")
		if len(cfg.Discovery.ExactJobs) > 0 {
			jobs, err = prowbuild.DiscoverExactJobs(ctx, p.backend, p.includePresubmits, cfg.Discovery.ExactJobs)
		} else {
			jobs, err = prowbuild.DiscoverJobs(ctx, p.backend, p.includePresubmits, cfg.Discovery.JobFilters)
		}
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
		var catalog *jobconfig.Catalog
		jobs, catalog, err = jobconfig.FetchJobConfigsAndCatalog(ctx, p.client, cfg, targetRepo)
		if err != nil {
			return nil, fmt.Errorf("fetching job configs: %w", err)
		}
		p.setJobCatalog(catalog)
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

// refreshWithAnalysisContext fetches builds, runs analysis, and writes output.
func (p *pipeline) refreshWithAnalysisContext(fetchCtx, analysisCtx context.Context, jobs []models.ProwJob) (*refreshResult, error) {
	p.startProgressPhase(fetchprogress.PhaseArtifacts)
	p.setProgressJobs(len(jobs))
	var transaction *aiRefreshStateTransaction
	var err error
	if p.enableAI {
		transaction, err = captureAIRefreshState(p.opts.OutDir)
		if err != nil {
			return nil, fmt.Errorf("snapshotting AI refresh state: %w", err)
		}
		p.aiRefreshTransaction = transaction
		defer func() { p.aiRefreshTransaction = nil }()
	}
	result, err := p.refreshDataWithAnalysisContext(fetchCtx, analysisCtx, jobs)
	if err == nil {
		if transaction != nil {
			transaction.Discard()
		}
		return result, nil
	}
	if transaction == nil {
		return result, err
	}
	return nil, p.rollbackAIRefresh(transaction, err)
}

func (p *pipeline) rollbackAIRefresh(transaction *aiRefreshStateTransaction, refreshErr error) error {
	p.invalidateAnalysisRuntime()
	if restoreErr := transaction.Restore(); restoreErr != nil {
		return errors.Join(refreshErr, fmt.Errorf("restoring AI refresh state: %w", restoreErr))
	}
	return refreshErr
}

func (p *pipeline) invalidateAnalysisRuntime() {
	p.aiRuntime = nil
}

func (p *pipeline) refreshDataWithAnalysisContext(fetchCtx, analysisCtx context.Context, jobs []models.ProwJob) (*refreshResult, error) {
	cfg, opts := p.cfg, p.opts
	if err := clearAnalysisTrace(opts.OutDir); err != nil {
		log.Printf("Warning: failed to clear stale AI traces: %v", err)
	}

	// Fetch each job's builds. Cached completed builds are reused.
	priorDetails, err := loadPublishedJobDetails(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("loading prior job details: %w", err)
	}
	cachedJobs := cachedBuildsFromDetails(priorDetails)

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

			runs, stats, err := fetchJobRunsCachedWithStats(fetchCtx, p.backend, cfg, &j, opts.BuildsPerJob, cachedJobs[j.JobID])
			defer p.finishProgressJob(stats.cached, stats.fetched)
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
	p.markProgressChecked()
	p.completeProgressPhase()
	p.startProgressPhase(fetchprogress.PhaseAggregation)

	now := time.Now().UTC()
	dashboard := models.Dashboard{GeneratedAt: now}
	var details []models.JobDetail

	configRevision := ""
	if p.jobCatalog != nil {
		configRevision = p.jobCatalog.Revision
	}
	for _, r := range results {
		if r.job.Name == "" {
			continue // skipped due to fetch error
		}
		summary := aggregator.ComputeJobSummary(r.job, r.runs)
		dashboard.Jobs = append(dashboard.Jobs, summary)
		details = append(details, models.JobDetail{
			Name:           r.job.Name,
			JobID:          r.job.JobID,
			JobType:        r.job.JobType,
			Repo:           r.job.Repo,
			ConfigFile:     r.job.ConfigFile,
			ConfigRevision: configRevision,
			CurrentStatus:  summary.CurrentStatus,
			PassRateRecent: summary.PassRateRecent,
			Runs:           r.runs,
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
	p.completeProgressPhase()

	if p.enableAI {
		p.startProgressPhase(fetchprogress.PhaseAnalysisPlanning)
		if err := p.analyzeFailuresWithAI(analysisCtx, details, flakinessReport); err != nil {
			return nil, err
		}
		p.completeProgressPhase()
	} else {
		p.lastPatternOutcomes = map[string]patterns.JobOutcome{}
		p.skipProgressPatterns()
	}
	refreshReport, err := patterns.MergeLastGood(details, priorDetails, patterns.AnalyzeResult{Outcomes: p.lastPatternOutcomes})
	if err != nil {
		return nil, fmt.Errorf("merging pattern refresh results: %w", err)
	}
	flakinessReport.PatternRefresh = refreshReport
	if p.progress != nil {
		p.progress.SetPatternRefreshCounts(refreshReport.Current, refreshReport.Retained, refreshReport.Unavailable)
	}
	flakinessReport.RecurringPatterns = collectRecurringPatterns(details)
	flakinessReport.BuildFailures = aggregator.CollectBuildFailures(details)
	if n := len(flakinessReport.RecurringPatterns); n > 0 {
		log.Printf("🔗 %d systemic recurring pattern(s) surfaced on the home page", n)
	}

	p.startProgressPhase(fetchprogress.PhasePublication)
	// Auto-reopen resolved patterns that have recurred past their watermark, so
	// a fixed-then-flaked failure returns to the active view. The server may
	// also write resolved.json on an admin action; both use atomic writes, and a
	// rare lost update self-heals on the next pass (same trade-off as the other
	// *_state.json files).
	stagedReopened := map[string]resolve.Entry{}
	if rs := resolve.Load(opts.OutDir); len(rs.Resolved) > 0 {
		if pruned, changed := rs.Prune(patterns.CurrentRecurring(details)); changed {
			for id := range rs.Resolved {
				if _, kept := pruned.Resolved[id]; !kept {
					stagedReopened[id] = rs.Resolved[id]
				}
			}
		}
	}

	log.Printf("Writing output to %s/ (%d jobs)", opts.OutDir, len(dashboard.Jobs))
	err = patternstate.WithLock(opts.OutDir, func() error {
		if err := writeAllOutput(opts.OutDir, cfg, dashboard, details, flakinessReport, searchIndex); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		if len(stagedReopened) > 0 {
			if err := resolve.RemoveMatching(opts.OutDir, stagedReopened); err != nil {
				log.Printf("Warning: failed to save resolved state after publication: %v", err)
			} else {
				log.Printf("↩ re-opened %d resolved pattern(s) after recurrence", len(stagedReopened))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.markProgressPublished()
	p.completeProgressPhase()

	return &refreshResult{details: details, flakiness: flakinessReport}, nil
}

type aiRefreshFileSnapshot struct {
	path       string
	backupPath string
	mode       os.FileMode
	exists     bool
}

type aiRefreshStateTransaction struct {
	outDir              string
	files               []aiRefreshFileSnapshot
	checkpointCommitted bool
}

func captureAIRefreshState(outDir string) (*aiRefreshStateTransaction, error) {
	snapshot := &aiRefreshStateTransaction{outDir: outDir}
	for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
		path := filepath.Join(outDir, name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			snapshot.files = append(snapshot.files, aiRefreshFileSnapshot{path: path})
			continue
		}
		if err != nil {
			snapshot.Discard()
			return nil, err
		}
		if !info.Mode().IsRegular() {
			snapshot.Discard()
			return nil, fmt.Errorf("%s is not a regular file", name)
		}
		source, err := os.Open(path)
		if err != nil {
			snapshot.Discard()
			return nil, err
		}
		backup, err := os.CreateTemp("", "prow-ai-refresh-state-*")
		if err != nil {
			source.Close()
			snapshot.Discard()
			return nil, err
		}
		backupPath := backup.Name()
		_, copyErr := io.Copy(backup, source)
		closeSourceErr := source.Close()
		closeBackupErr := backup.Close()
		if err := errors.Join(copyErr, closeSourceErr, closeBackupErr); err != nil {
			_ = os.Remove(backupPath)
			snapshot.Discard()
			return nil, err
		}
		snapshot.files = append(snapshot.files, aiRefreshFileSnapshot{
			path: path, backupPath: backupPath, mode: info.Mode().Perm(), exists: true,
		})
	}
	return snapshot, nil
}

// CommitAnalysisCheckpoint makes the current private generation the rollback baseline.
func (s *aiRefreshStateTransaction) CommitAnalysisCheckpoint() error {
	if s == nil {
		return nil
	}
	next, err := captureAIRefreshState(s.outDir)
	if err != nil {
		return err
	}
	s.Discard()
	s.files = next.files
	s.checkpointCommitted = true
	return nil
}

func (s *aiRefreshStateTransaction) Restore() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, file := range s.files {
		if !file.exists {
			if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if err := restoreAIRefreshFile(file); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func restoreAIRefreshFile(snapshot aiRefreshFileSnapshot) error {
	backup, err := os.Open(snapshot.backupPath)
	if err != nil {
		return err
	}
	defer backup.Close()
	if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(snapshot.path), filepath.Base(snapshot.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// Some RWX filesystems use mount-level modes and reject chmod.
	_ = tmp.Chmod(snapshot.mode)
	if _, err := io.Copy(tmp, backup); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, snapshot.path); err != nil {
		return err
	}
	return os.Remove(snapshot.backupPath)
}

func (s *aiRefreshStateTransaction) Discard() {
	if s == nil {
		return
	}
	for _, file := range s.files {
		if file.backupPath != "" {
			_ = os.Remove(file.backupPath)
		}
	}
}

func (p *pipeline) commitAnalysisCheckpoint() error {
	if p == nil || p.aiRefreshTransaction == nil {
		return nil
	}
	return p.aiRefreshTransaction.CommitAnalysisCheckpoint()
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
		p.setProgressFollowUp(fetchprogress.FollowUpNotifications, fetchprogress.FollowUpRunning, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		failureCode := fetchprogress.FollowUpFailureNone
		password := os.Getenv("EMAIL_SMTP_PASSWORD")
		if email.SMTP.Username != "" && password == "" {
			log.Println("Notifications: skipped (EMAIL_SMTP_PASSWORD is unset)")
			sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email notifications: EMAIL_SMTP_PASSWORD is unset"))
			failureCode = fetchprogress.FollowUpFailureNotificationCredentials
		} else {
			from, recipients, err := notify.ParseAddresses(email.From, email.To)
			if err != nil {
				log.Printf("Warning: invalid email notification addresses: %v", err)
				sideEffectErrs = append(sideEffectErrs, fmt.Errorf("email addresses: %w", err))
				failureCode = fetchprogress.FollowUpFailureNotificationConfiguration
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
					failureCode = fetchprogress.FollowUpFailureNotificationConfiguration
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
						failureCode = fetchprogress.FollowUpFailureNotificationDelivery
					}
					if err := notifier.SaveState(); err != nil {
						log.Printf("Warning: failed to save notification state: %v", err)
						sideEffectErrs = append(sideEffectErrs, err)
						failureCode = fetchprogress.FollowUpFailureNotificationStatePersistence
					}
				}
			}
		}
		if failureCode == fetchprogress.FollowUpFailureNone {
			p.setProgressFollowUp(fetchprogress.FollowUpNotifications, fetchprogress.FollowUpCompleted, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		} else {
			p.setProgressFollowUp(fetchprogress.FollowUpNotifications, fetchprogress.FollowUpFailed, fetchprogress.FollowUpReasonNone, failureCode)
		}
	} else {
		log.Println("Notifications: skipped (email disabled)")
		p.setProgressFollowUp(fetchprogress.FollowUpNotifications, fetchprogress.FollowUpDisabled, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
	}

	fixEnabled := cfg.AI != nil && cfg.AI.FixPRs != nil && cfg.AI.FixPRs.Enabled
	switch {
	case !fixEnabled:
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpDisabled, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
	case os.Getenv("FIX_TOKEN") == "":
		log.Println("Fix PRs: enabled but FIX_TOKEN is unset; skipping automatic generation")
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpSkipped, fetchprogress.FollowUpReasonNotConfigured, fetchprogress.FollowUpFailureNone)
	default:
		fixPatterns := currentActionablePatterns(flakinessReport.RecurringPatterns, details)
		if len(fixPatterns) == 0 {
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpSkipped, fetchprogress.FollowUpReasonNoWork, fetchprogress.FollowUpFailureNone)
			break
		}
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpRunning, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		if _, err := processFixPRs(ctx, cfg, fixPatterns, p.aiToken, opts.OutDir, p.usageRecorder); err != nil {
			sideEffectErrs = append(sideEffectErrs, err)
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpFailed, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureAutomaticFixPRs)
		} else {
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticFixPRs, fetchprogress.FollowUpCompleted, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		}
	}

	issuesEnabled := cfg.Issues != nil && cfg.Issues.Enabled
	switch {
	case !issuesEnabled:
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpDisabled, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
	case os.Getenv("ISSUE_TOKEN") == "":
		log.Println("Issues: enabled but ISSUE_TOKEN is unset; skipping automatic reconciliation")
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpSkipped, fetchprogress.FollowUpReasonNotConfigured, fetchprogress.FollowUpFailureNone)
	default:
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpRunning, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		if err := processIssues(ctx, cfg, flakinessReport, details, p.aiToken, p.enableAI, opts.OutDir, p.usageRecorder); err != nil {
			sideEffectErrs = append(sideEffectErrs, err)
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpFailed, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureAutomaticIssues)
		} else {
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpCompleted, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		}
	}
	return errors.Join(sideEffectErrs...)
}

func repositoryToken(runtimeType, token string) string {
	if runtimeType == "agent-sandbox" {
		return ""
	}
	return token
}

func fetcherUsageOutcome(err error) aiusage.Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return aiusage.OutcomeCancelled
	}
	if err != nil {
		return aiusage.OutcomeError
	}
	return aiusage.OutcomeSuccess
}

// processIssues reconciles the project's highest-signal findings into GitHub
// issues on the configured target repo. Gated on issues.enabled and ISSUE_TOKEN.
func processIssues(ctx context.Context, cfg *project.Config, report models.FlakinessReport, details []models.JobDetail, aiToken string, enableAI bool, outDir string, usageRecorder *aiusage.Recorder) error {
	if cfg.Issues == nil || !cfg.Issues.Enabled {
		return nil
	}
	token := os.Getenv("ISSUE_TOKEN")
	if token == "" {
		log.Println("Issues: enabled but ISSUE_TOKEN is unset; skipping automatic reconciliation")
		return nil
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
		provider := cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv))
		aiClient := ai.NewClientWithOptions(ai.Options{
			Token:           aiToken,
			API:             provider.API,
			Endpoint:        provider.Endpoint,
			Model:           provider.Model,
			ReasoningEffort: provider.ReasoningEffort,
			ExtraHeaders:    provider.Headers,
		})
		filler = repotemplate.NewIssueFiller(token, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}

	client := issues.NewClient(token, eff.Repo.Owner, eff.Repo.Name)
	targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
	keepOpen := map[string]bool{}
	for _, pattern := range report.RecurringPatterns {
		if models.PatternAllowsActions(pattern) || !pattern.Systemic || pattern.JobID == "" || !models.PatternIsCurrent(details, pattern.JobID) {
			continue
		}
		keepOpen[issues.KeyPrefixPattern+pattern.JobID] = true
	}
	for _, detail := range details {
		if detail.PatternRefresh == nil || detail.PatternRefresh.State == models.PatternRefreshCurrent ||
			detail.PatternRefresh.State == models.PatternRefreshNotApplicable {
			continue
		}
		keepOpen[issues.KeyPrefixPattern+detail.JobID] = true
	}
	mgr := newBatchIssueManager(client, filepath.Join(outDir, "issue_state.json"), targetRepo, issues.Options{
		CommentOnRecovery: eff.CommentOnRecovery == nil || *eff.CommentOnRecovery,
		CloseOnRecovery:   eff.CloseOnRecovery,
		MaxNewPerRun:      eff.MaxNewPerRun,
		RecoverPrefixes:   issues.RecoverPrefixesFor(eff.Triggers),
		KeepOpenKeys:      keepOpen,
		TemplateFiller:    filler,
	})
	ctx, usageOperation := aiusage.Begin(ctx, usageRecorder, aiusage.Metadata{
		LogicalID: "scheduled-issues", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureIssueDraft,
	})
	stats, err := mgr.Reconcile(ctx, specs)
	usageOperation.Finish(fetcherUsageOutcome(err))
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

type scheduledIssueManager interface {
	Reconcile(context.Context, []issues.IssueSpec) (issues.Stats, error)
	SaveState() error
}

var newBatchIssueManager = func(client *issues.Client, stateFile, repo string, opts issues.Options) scheduledIssueManager {
	return issues.NewManager(client, stateFile, repo, opts)
}

// processFixPRs drafts minimal fix PRs against the source repo for systemic
// recurring patterns. Gated on ai.fix_prs.enabled and FIX_TOKEN (a CLA-signed
// operator PAT). In dry-run it writes previews instead of opening PRs. Any
// missing piece is a no-op.
func processFixPRs(ctx context.Context, cfg *project.Config, patterns []models.PatternAnalysis, aiToken, outDir string, usageRecorder *aiusage.Recorder) (bool, error) {
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
		log.Println("Fix PRs: enabled but FIX_TOKEN is unset; skipping automatic generation")
		return false, nil
	}

	provider := cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv))
	if err := project.ValidateAIProvider(provider); err != nil {
		return false, fmt.Errorf("fix PRs: %w", err)
	}
	var aiClient *ai.Client
	if aiToken != "" && provider.Endpoint != "" && provider.Model != "" {
		aiClient = ai.NewClientWithOptions(ai.Options{Token: aiToken, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model, ReasoningEffort: provider.ReasoningEffort, ExtraHeaders: provider.Headers})
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
	if eff.Verify != nil && eff.Verify.Enabled && eff.AgentRuntime.Type != "agent-sandbox" {
		trusted, err := runtime.TrustedLocalRuntimeEnabled()
		if err != nil {
			return false, err
		}
		if !trusted {
			return false, fmt.Errorf("fix PRs: local verification requires %s=true on a trusted development or CI host", runtime.TrustedLocalRuntimeEnv)
		}
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
	commands, err := ar.RuntimeCommands(ar.ParsedTimeout())
	if err != nil {
		return false, fmt.Errorf("fix PR command policy: %w", err)
	}
	fixOpts.Agent = &fixpr.AgentConfig{
		Runtime:               agentRuntime,
		API:                   aiAPI(cfg),
		Model:                 aiModel(cfg),
		Endpoint:              aiEndpoint(cfg),
		ModelToken:            aiToken,
		MaxTurns:              ar.MaxTurns,
		MaxFiles:              eff.MaxFiles,
		ModelProvider:         ar.ModelProvider.RuntimeConfig(),
		OutputLimitBytes:      ar.OutputLimitBytes,
		AllowBash:             allowBash,
		CommandPolicy:         runtime.CommandPolicy{AllowShell: allowBash, Commands: commands},
		RequireCommandResults: true,
		Timeout:               ar.ParsedTimeout(),
		GitToken:              repositoryToken(ar.Type, fixToken),
	}
	mgr := newBatchFixManager(fixToken, filepath.Join(outDir, "fix_pr_state.json"), fixOpts)
	ctx, usageOperation := aiusage.Begin(ctx, usageRecorder, aiusage.Metadata{
		LogicalID: "scheduled-fix-prs", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureFixPreview,
	})
	stats, err := mgr.Reconcile(ctx, patterns)
	usageOperation.Finish(fetcherUsageOutcome(err))
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
func loadPublishedJobDetails(outDir string) (map[string]models.JobDetail, error) {
	details := map[string]models.JobDetail{}
	jobsDir := filepath.Join(outDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if os.IsNotExist(err) {
		return details, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var detail models.JobDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if detail.JobID == "" {
			return nil, fmt.Errorf("job detail %s is missing job_id", entry.Name())
		}
		if _, duplicate := details[detail.JobID]; duplicate {
			return nil, fmt.Errorf("duplicate job detail for %s", detail.JobID)
		}
		details[detail.JobID] = detail
	}
	return details, nil
}

func cachedBuildsFromDetails(details map[string]models.JobDetail) map[string]map[string]models.BuildResult {
	cached := make(map[string]map[string]models.BuildResult, len(details))
	for jobID, detail := range details {
		builds := make(map[string]models.BuildResult, len(detail.Runs))
		for _, run := range detail.Runs {
			cacheableJUnit := run.JUnitComplete || (run.JUnitTruncated && len(run.JUnitURLs) > 0)
			if run.Result != "PENDING" && run.Result != "" && cacheableJUnit {
				builds[run.BuildID] = run
			}
		}
		if len(builds) > 0 {
			cached[jobID] = builds
		}
	}
	return cached
}

func loadCachedJobDetails(outDir string) map[string]map[string]models.BuildResult {
	details, err := loadPublishedJobDetails(outDir)
	if err != nil {
		return map[string]map[string]models.BuildResult{}
	}
	return cachedBuildsFromDetails(details)
}

type buildFetchStats struct {
	cached  int
	fetched int
}

// fetchJobRunsCachedWithStats discovers recent builds and reuses cached data.
func fetchJobRunsCachedWithStats(ctx context.Context, backend storage.Backend, cfg *project.Config, job *models.ProwJob, count int, cachedBuilds map[string]models.BuildResult) ([]models.BuildResult, buildFetchStats, error) {
	builds, err := prowbuild.ListRecentBuilds(ctx, backend, job, count)
	if err != nil {
		return nil, buildFetchStats{}, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	stats := buildFetchStats{}
	for _, b := range builds {
		if cached, ok := cachedBuilds[b.ID]; ok {
			normalizeBuildResult(&cached)
			runs = append(runs, cached)
			stats.cached++
			continue
		}
		result, err := fetchBuildResult(ctx, backend, job, b)
		if err != nil {
			log.Printf("    ⚠ %s/%s: %v", job.Name, b.ID, err)
			continue
		}
		runs = append(runs, *result)
		stats.fetched++
	}

	if stats.cached > 0 {
		log.Printf("    💾 %s: %d cached, %d fetched", job.Name, stats.cached, stats.fetched)
	}

	return runs, stats, nil
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
		normalizeBuildResult(result)
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

	normalizeBuildResult(result)
	return result, nil
}

func normalizeBuildResult(result *models.BuildResult) {
	if result == nil {
		return
	}
	if eligibleForBuildFailure(result) {
		result.TestCases = append(result.TestCases, newBuildFailure(result))
	}

	result.TestsTotal = 0
	result.TestsPassed = 0
	result.TestsFailed = 0
	result.TestsSkipped = 0
	for _, tc := range result.TestCases {
		if tc.Source == models.TestCaseSourceBuild {
			continue
		}
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
}

func eligibleForBuildFailure(result *models.BuildResult) bool {
	if result == nil || result.Passed || result.Result == "PENDING" || !result.JUnitComplete || result.JUnitTruncated {
		return false
	}
	for i := range result.TestCases {
		if result.TestCases[i].Status == "failed" {
			return false
		}
	}
	return true
}

func newBuildFailure(result *models.BuildResult) models.TestCase {
	return models.NewProwJobExecutionFailure(result.DurationSeconds)
}

// currentActionablePatterns keeps recurring patterns that may start an action
// and whose job produced a fresh pattern result this pass.
func currentActionablePatterns(patterns []models.PatternAnalysis, details []models.JobDetail) []models.PatternAnalysis {
	out := make([]models.PatternAnalysis, 0, len(patterns))
	for _, pattern := range patterns {
		if !models.PatternAllowsActions(pattern) {
			continue
		}
		if len(details) > 0 && !models.PatternIsCurrent(details, pattern.JobID) {
			continue
		}
		out = append(out, pattern)
	}
	return out
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

var newEmailSender = func(config notify.SMTPConfig) (notify.Sender, error) {
	return notify.NewSMTPSender(config)
}
