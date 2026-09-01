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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/junit"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/patternstate"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/recurrenceledger"
	"github.com/willie-yao/aster/backend/internal/resolve"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type Options struct {
	ProjectDir   string
	OutDir       string
	BuildsPerJob int
	Workers      int
	Timeout      time.Duration
	// IncludePresubmits fetches presubmit jobs in addition to periodics.
	// The project discovery policy and this direct CLI override are combined.
	IncludePresubmits    bool
	EnableAI             bool
	PrepareCauseFindings bool
	AIMaxOutputTokens    int
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
}

// refreshResult carries the outputs a pass needs for its side effects.
var writeAllOutput = output.WriteAll

type refreshResult struct {
	details []models.JobDetail
	// flakiness is the published report, ranked and truncated across every
	// published job.
	flakiness models.FlakinessReport
	// baseFlakiness is the same computation over base-branch jobs only. Pull
	// request attribution uses it so publishing presubmits cannot rank a
	// base-branch flake out of the truncated report and change a verdict.
	baseFlakiness models.FlakinessReport
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

	p := &pipeline{
		opts:              opts,
		cfg:               cfg,
		client:            client,
		backend:           backend,
		enableAI:          enableAI,
		aiToken:           aiToken,
		aiProject:         aiProject,
		usageRecorder:     usageRecorder,
		includePresubmits: opts.IncludePresubmits || cfg.Discovery.IncludePresubmits,
	}
	p.warnPullRequestTokenMissing()
	p.warnCommentCredentialsMissing()
	return p, nil
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
	if p.opts.SkipSideEffects {
		p.skipProgressSideEffects()
	} else {
		p.startProgressPhase(fetchprogress.PhaseSideEffects)
		if err := p.runSideEffects(fetchCtx, res); err != nil {
			p.invalidateAnalysisRuntime()
			return nil, err
		}
		p.completeProgressPhase()
	}
	prepareCtx, prepareCancel := context.WithTimeout(ctx, 10*time.Minute)
	p.prepareCauseFindings(prepareCtx, res.details)
	prepareCancel()
	return jobs, nil
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

	// Fetch each job's builds. Cached completed builds are reused.
	priorDetails, err := loadPublishedJobDetails(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("loading prior job details: %w", err)
	}
	cachedJobs := cachedBuildsFromDetails(priorDetails)
	priorHistory := retainedRunsFromDetails(priorDetails)

	type jobResult struct {
		job      models.ProwJob
		runs     []models.BuildResult
		retained []models.BuildResult
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

			results[idx] = jobResult{job: j, runs: runs, retained: selectRetainedRuns(runs, priorHistory[j.JobID])}
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

	// A pass that ran out of time never saw some jobs, and publishing that view
	// would prune the job files it failed to fetch, taking their cached builds
	// and retained pattern analyses with them. Abort so the last good snapshot
	// stands and the next pass republishes from it.
	if err := fetchCtx.Err(); err != nil {
		return nil, fmt.Errorf("fetching builds: %w", err)
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
			RetainedRuns:   r.retained,
		})
	}

	jobResultMap := make(map[string][]models.BuildResult, len(results))
	for _, r := range results {
		if r.job.Name == "" {
			continue
		}
		jobResultMap[r.job.JobID] = r.runs
	}
	aggregatorSettings := attentionSettings(p.cfg)
	flakinessReport := aggregator.ComputeFlakinessReport(jobResultMap, jobs, now, aggregatorSettings)
	baseFlakiness := baseBranchFlakiness(jobResultMap, jobs, flakinessReport, now, aggregatorSettings)
	log.Printf("Flakiness report: %d most flaky, %d persistent, %d recently broken, %d low pass rate",
		len(flakinessReport.MostFlaky), len(flakinessReport.PersistentFailures),
		len(flakinessReport.RecentlyBroken), len(flakinessReport.LowPassRate))

	searchIndex := aggregator.BuildSearchIndex(jobResultMap, jobs, now)
	log.Printf("Search index: %d entries", len(searchIndex.Entries))
	p.completeProgressPhase()

	if p.enableAI {
		p.startProgressPhase(fetchprogress.PhaseAnalysisPlanning)
		if err := p.analyzeFailuresWithAI(analysisCtx, details); err != nil {
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
	// Auto-reopen resolved patterns and causes that have recurred past their
	// watermark, so a fixed-then-flaked failure returns to the active view. The
	// server may also write resolved.json on an admin action; both use atomic
	// writes, and a rare lost update self-heals on the next pass (same trade-off
	// as the other *_state.json files).
	stagedReopened := &resolve.State{Resolved: map[string]resolve.Entry{}, Causes: map[string]resolve.Entry{}}
	if rs := resolve.Load(opts.OutDir); len(rs.Resolved) > 0 || len(rs.Causes) > 0 {
		if pruned, changed := rs.Prune(patterns.CurrentRecurring(details)); changed {
			for id := range rs.Resolved {
				if _, kept := pruned.Resolved[id]; !kept {
					stagedReopened.Resolved[id] = rs.Resolved[id]
				}
			}
			for signature := range rs.Causes {
				if _, kept := pruned.Causes[signature]; !kept {
					stagedReopened.Causes[signature] = rs.Causes[signature]
				}
			}
		}
	}
	reopenedCount := len(stagedReopened.Resolved) + len(stagedReopened.Causes)

	log.Printf("Writing output to %s/ (%d jobs)", opts.OutDir, len(dashboard.Jobs))
	err = patternstate.WithLock(opts.OutDir, func() error {
		// Recurrence is observed before publication so each job is written with
		// the durable history behind the failures it currently shows.
		observeRecurrence(opts.OutDir, details, now)
		if err := writeAllOutput(opts.OutDir, cfg, dashboard, details, flakinessReport, searchIndex); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		if reopenedCount > 0 {
			if err := resolve.RemoveMatching(opts.OutDir, stagedReopened); err != nil {
				log.Printf("Warning: failed to save resolved state after publication: %v", err)
			} else {
				log.Printf("↩ re-opened %d resolved failure(s) after recurrence", reopenedCount)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.markProgressPublished()
	p.completeProgressPhase()

	return &refreshResult{details: details, flakiness: flakinessReport, baseFlakiness: baseFlakiness}, nil
}

// recordRecurrence gives recurring causes memory that outlives the build window,
// so a failure that returns is recognized as the same cause and an
// already-answered investigation is not re-run. Pruning runs before observing, so
// a cause returning after the retention window starts fresh rather than reviving
// a verdict too old to trust. A failure to record never fails the pass: the next
// one re-observes the same causes and the watermark keeps the counts correct, and
// the server independently refuses expired memory on read.
//
// It returns the observed ledger so the pass can publish recurrence from the same
// state it just recorded, or nil when the ledger could not be opened.
func recordRecurrence(outDir string, sightings []recurrenceledger.Sighting, now time.Time) *recurrenceledger.Ledger {
	var observed *recurrenceledger.Ledger
	err := recurrenceledger.Update(outDir, func(ledger *recurrenceledger.Ledger) bool {
		pruned := ledger.Prune(now)
		changed := ledger.Observe(sightings, now)
		observed = ledger
		return pruned || changed
	})
	if err != nil {
		log.Printf("Warning: failed to record recurrence history: %v", err)
	}
	return observed
}

// observeRecurrence records what this pass saw and annotates the details with the
// durable history behind it, so each job is published together with the
// recurrence its current window belongs to.
//
// Two identities are recorded. Causal groups keep their verdict-bearing
// signature, which preserves numbers so a stored conclusion cannot answer a
// materially different failure. Every failed build separately advances a
// recurrence signature, which collapses numbers so a flake whose message carries
// a varying duration or count still accumulates history. Only the recurrence
// identities are published, so a build resolves to exactly one history.
func observeRecurrence(outDir string, details []models.JobDetail, now time.Time) {
	recurrence := make([][]recurrenceledger.Sighting, len(details))
	var all []recurrenceledger.Sighting
	for i := range details {
		recurrence[i] = mergeSightings(failureRecurrenceSightings(&details[i]))
		all = append(all, recurrence[i]...)
		all = append(all, mergeSightings(causalGroupSightings(&details[i]))...)
	}
	applyFailureRecurrence(details, recurrence, recordRecurrence(outDir, all, now))
}

// causalGroupSightings returns one sighting per signed systemic causal group.
// Inactive lifecycle states are included so a recovered cause keeps its history
// and is still recognized if it comes back.
func causalGroupSightings(detail *models.JobDetail) []recurrenceledger.Sighting {
	var out []recurrenceledger.Sighting
	for _, pattern := range detail.PatternAnalyses {
		if !pattern.Systemic {
			continue
		}
		for _, group := range pattern.CausalGroups {
			if group.Signature == "" {
				continue
			}
			out = append(out, recurrenceledger.Sighting{
				Signature: group.Signature, JobID: detail.JobID,
				Subject: detail.Name, Builds: group.Builds,
			})
		}
	}
	return out
}

// failureRecurrenceSightings records every failed build under its recurrence
// signature, whether or not it correlated into a causal group. Correlation needs
// patterns.MinFailedBuilds failures inside a single window, which an infrequent
// flake never reaches, so a build-by-build path is the only way such a failure
// builds any history at all.
func failureRecurrenceSightings(detail *models.JobDetail) []recurrenceledger.Sighting {
	var out []recurrenceledger.Sighting
	for i := range detail.Runs {
		run := &detail.Runs[i]
		if run.Passed || run.Result == "PENDING" {
			continue
		}
		signature := patterns.BuildRecurrenceSignature(*detail, run)
		if signature == "" {
			continue
		}
		out = append(out, recurrenceledger.Sighting{
			Signature: signature, JobID: detail.JobID,
			Subject: detail.Name, Builds: []string{run.BuildID},
		})
	}
	return out
}

// mergeSightings collapses sightings sharing a signature and drops repeated
// builds, so one pass advances a cause's watermark exactly once across every
// build that showed it. Observing a signature twice would otherwise count only
// the first sighting, because the second's builds no longer sit past the
// watermark the first just advanced.
func mergeSightings(sightings []recurrenceledger.Sighting) []recurrenceledger.Sighting {
	out := make([]recurrenceledger.Sighting, 0, len(sightings))
	index := make(map[string]int, len(sightings))
	seen := map[string]bool{}
	for _, sighting := range sightings {
		at, ok := index[sighting.Signature]
		if !ok {
			at = len(out)
			index[sighting.Signature] = at
			out = append(out, recurrenceledger.Sighting{
				Signature: sighting.Signature, JobID: sighting.JobID, Subject: sighting.Subject,
			})
		}
		for _, build := range sighting.Builds {
			build = strings.TrimSpace(build)
			if build == "" || seen[sighting.Signature+"\x00"+build] {
				continue
			}
			seen[sighting.Signature+"\x00"+build] = true
			out[at].Builds = append(out[at].Builds, build)
		}
	}
	return out
}

// applyFailureRecurrence publishes the history behind the failures each job
// currently shows, so a rare flake reveals how long it has been recurring even
// though correlation only ever sees one window.
func applyFailureRecurrence(details []models.JobDetail, recurrence [][]recurrenceledger.Sighting, ledger *recurrenceledger.Ledger) {
	if ledger == nil {
		return
	}
	for i := range details {
		detail := &details[i]
		detail.FailureRecurrence = nil
		for _, sighting := range recurrence[i] {
			entry, ok := ledger.Entries[sighting.Signature]
			if !ok {
				continue
			}
			detail.FailureRecurrence = append(detail.FailureRecurrence, models.FailureRecurrence{
				Signature:   sighting.Signature,
				Occurrences: entry.Occurrences,
				FirstSeen:   entry.FirstSeen,
				LastSeen:    entry.LastSeen,
				Builds:      sighting.Builds,
			})
		}
	}
}

// baseBranchFlakiness recomputes the flakiness report over base-branch jobs// only. The published report ranks and truncates across every published job, so
// presubmits can displace a base-branch flake from it. Attribution needs history
// that does not move when the dashboard starts publishing presubmits.
func baseBranchFlakiness(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, published models.FlakinessReport, now time.Time, settings aggregator.Settings) models.FlakinessReport {
	baseJobs := make([]models.ProwJob, 0, len(jobs))
	for _, job := range jobs {
		if job.JobType != models.JobTypePresubmit {
			baseJobs = append(baseJobs, job)
		}
	}
	if len(baseJobs) == len(jobs) {
		return published
	}
	baseResults := make(map[string][]models.BuildResult, len(baseJobs))
	for _, job := range baseJobs {
		if runs, ok := jobResults[job.JobID]; ok {
			baseResults[job.JobID] = runs
		}
	}
	return aggregator.ComputeFlakinessReport(baseResults, baseJobs, now, settings)
}

// attentionSettings maps the project's attention config onto the aggregator's
// neutral settings. Validate guarantees a configured rule carries a threshold,
// so a nil one here means the consumer left the rule off.
func attentionSettings(cfg *project.Config) aggregator.Settings {
	attention := cfg.EffectiveAttention()
	settings := aggregator.Settings{PersistentAfter: attention.PersistentAfter}
	if rule := attention.LowPassRate; rule != nil && rule.Threshold != nil {
		settings.LowPassRate = &aggregator.LowPassRateRule{
			Threshold:  *rule.Threshold,
			MinRuns:    rule.MinRuns,
			RecentRuns: rule.RecentRuns,
			MaxItems:   rule.MaxItems,
		}
	}
	return settings
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

// runSideEffects handles email notifications and recovery on tracked issues.
// It files no issues and opens no pull requests. These are gated on their own
// env tokens and return joined operational errors.
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

	issuesEnabled := cfg.Issues != nil && cfg.Issues.Enabled
	switch {
	case !issuesEnabled:
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpDisabled, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
	case os.Getenv("ISSUE_TOKEN") == "":
		log.Println("Issues: enabled but ISSUE_TOKEN is unset; skipping recovery reconciliation")
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpSkipped, fetchprogress.FollowUpReasonNotConfigured, fetchprogress.FollowUpFailureNone)
	default:
		p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpRunning, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		if err := processIssues(ctx, cfg, flakinessReport, details, opts.OutDir); err != nil {
			sideEffectErrs = append(sideEffectErrs, err)
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpFailed, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureAutomaticIssues)
		} else {
			p.setProgressFollowUp(fetchprogress.FollowUpAutomaticIssues, fetchprogress.FollowUpCompleted, fetchprogress.FollowUpReasonNone, fetchprogress.FollowUpFailureNone)
		}
	}
	return errors.Join(sideEffectErrs...)
}

// processIssues closes or comments on tracked issues whose finding has
// recovered. It never files new issues: creation is a maintainer-initiated
// server action. Gated on issues.enabled and ISSUE_TOKEN.
func processIssues(ctx context.Context, cfg *project.Config, report models.FlakinessReport, details []models.JobDetail, outDir string) error {
	if cfg.Issues == nil || !cfg.Issues.Enabled {
		return nil
	}
	token := os.Getenv("ISSUE_TOKEN")
	if token == "" {
		log.Println("Issues: enabled but ISSUE_TOKEN is unset; skipping recovery reconciliation")
		return nil
	}
	eff := cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		log.Println("Issues: no target repo resolved (set issues.repo or branding.source_repo); skipping")
		return fmt.Errorf("issues: no target repo resolved")
	}

	// Active keys come from the same builder the server action files against, so
	// a finding that is still failing is never treated as recovered.
	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		JobDetails:   details,
		Triggers:     eff.Triggers,
		Labels:       eff.Labels,
		DashboardURL: cfg.Branding.SiteURL,
	})
	active := make([]string, 0, len(specs))
	for _, spec := range specs {
		active = append(active, spec.Key)
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
	// The server files issues into this same state file, so load, recover, and
	// save must be one locked sequence. Otherwise a recovery pass that loaded
	// first can save over an issue filed meanwhile, dropping it from tracking so
	// it is never closed.
	stateFile := filepath.Join(outDir, "issue_state.json")
	var err, saveErr error
	lockErr := statefile.WithLock(stateFile, func() error {
		mgr := newBatchIssueManager(client, stateFile, targetRepo, issues.Options{
			CommentOnRecovery: eff.CommentOnRecovery == nil || *eff.CommentOnRecovery,
			CloseOnRecovery:   eff.CloseOnRecovery,
			RecoverPrefixes:   issues.RecoverPrefixesFor(eff.Triggers),
			KeepOpenKeys:      keepOpen,
		})
		var stats issues.Stats
		stats, err = mgr.Recover(ctx, active)
		if err != nil {
			log.Printf("Warning: issue recovery failed: %v", err)
		} else if stats.Recovered > 0 {
			log.Printf("🐙 Issues (%s/%s): %d recovered", eff.Repo.Owner, eff.Repo.Name, stats.Recovered)
		}
		saveErr = mgr.SaveState()
		if saveErr != nil {
			log.Printf("Warning: failed to save issue state: %v", saveErr)
		}
		return nil
	})
	if lockErr != nil {
		log.Printf("Warning: issue recovery skipped: %v", lockErr)
		return fmt.Errorf("issue recovery: %w", lockErr)
	}
	return errors.Join(wrapOptional("issue recovery", err), wrapOptional("save issue state", saveErr))
}

type scheduledIssueManager interface {
	Recover(context.Context, []string) (issues.Stats, error)
	SaveState() error
}

var newBatchIssueManager = func(client *issues.Client, stateFile, repo string, opts issues.Options) scheduledIssueManager {
	return issues.NewManager(client, stateFile, repo, opts)
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

// maxRunHistory bounds Runs plus RetainedRuns per job. Retained builds keep only
// their metadata, so the cost is a few hundred bytes each, but the strip stops
// being readable long before the bound and the file should not grow forever.
const maxRunHistory = 40

// retainedRunsFromDetails collects the display history each job carries into the
// next pass: everything it published last time, both the analysis window and the
// prior retention. Test cases are dropped here, so a build that ages out of the
// window costs metadata alone from then on.
func retainedRunsFromDetails(details map[string]models.JobDetail) map[string][]models.BuildResult {
	retained := make(map[string][]models.BuildResult, len(details))
	for jobID, detail := range details {
		history := make([]models.BuildResult, 0, len(detail.Runs)+len(detail.RetainedRuns))
		for _, run := range detail.Runs {
			history = append(history, retainedRun(run))
		}
		for _, run := range detail.RetainedRuns {
			history = append(history, retainedRun(run))
		}
		if len(history) > 0 {
			retained[jobID] = history
		}
	}
	return retained
}

// retainedRun strips a build down to what the run history strip and the run
// metadata panel render. Counts survive; the test cases behind them do not.
func retainedRun(run models.BuildResult) models.BuildResult {
	run.TestCases = nil
	return run
}

// selectRetainedRuns returns the newest history builds that the current window
// does not already carry, bounded so window plus retention stays at maxRunHistory.
// A PENDING build is skipped: it is retained only once it has a final result.
func selectRetainedRuns(window []models.BuildResult, history []models.BuildResult) []models.BuildResult {
	budget := maxRunHistory - len(window)
	if budget <= 0 || len(history) == 0 {
		return nil
	}

	inWindow := make(map[string]bool, len(window))
	for _, run := range window {
		inWindow[run.BuildID] = true
	}

	seen := make(map[string]bool, len(history))
	candidates := make([]models.BuildResult, 0, len(history))
	for _, run := range history {
		if run.BuildID == "" || inWindow[run.BuildID] || seen[run.BuildID] || run.Result == "PENDING" || run.Result == "" {
			continue
		}
		seen[run.BuildID] = true
		candidates = append(candidates, retainedRun(run))
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Started.After(candidates[j].Started)
	})
	if len(candidates) > budget {
		candidates = candidates[:budget]
	}
	return candidates
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
