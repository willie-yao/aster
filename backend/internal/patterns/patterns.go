// Package patterns correlates analyzed failures across builds and prepares the
// recurring-pattern data produced by the analysis runtime.
package patterns

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
)

// MinFailedBuilds gates job-level recurring-pattern analysis.
const MinFailedBuilds = 3

// Analyzer correlates representative failures from several builds of one job.
type Analyzer interface {
	AnalyzePattern(ctx context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error)
}

type observedAnalyzer interface {
	AnalyzePatternWithOptions(ctx context.Context, jobID, subject string, failures []ai.PatternFailure, options ai.PatternAnalyzeOptions) (*models.PatternAnalysis, error)
}

type remediationVerifier interface {
	VerifyPatternRemediation(context.Context, models.PatternAnalysis, models.JobDetail) (models.PatternRemediationVerification, error)
}

const maxPatternAttempts = 2

// AnalyzeStats records eligible jobs and completed or failed correlations.
type AnalyzeStats struct {
	Eligible     int
	Completed    int
	Failed       int
	Attempts     int
	Retries      int
	Repairs      int
	CacheHits    int
	Suppressed   int
	FreshRetries int
}

// JobOutcome is one privacy-safe eligible-job correlation result.
type JobOutcome struct {
	JobID           string
	Succeeded       bool
	Systemic        bool
	FailureCategory ai.PatternFailureCategory
	Attempts        int
	Repairs         int
	CacheHits       int
	Suppressed      bool
	FreshRetries    int
}

// AnalyzeResult contains aggregate stats and deterministic job outcomes.
type AnalyzeResult struct {
	Stats    AnalyzeStats
	Outcomes map[string]JobOutcome
}

// Attempt reports one privacy-safe correlation attempt.
type Attempt struct {
	Number          int
	Repair          bool
	CacheHit        bool
	Suppressed      bool
	FreshRetry      bool
	Retry           bool
	Succeeded       bool
	Final           bool
	FailureCategory ai.PatternFailureCategory
}

// AnalyzeOptions reports pattern planning and attempt progress.
type AnalyzeOptions struct {
	OnPlan    func(int)
	OnAttempt func(Attempt)
	OnOutcome func(JobOutcome)
}

type analysisWork struct {
	index    int
	jobID    string
	subject  string
	failures []ai.PatternFailure
}

// Analyze correlates eligible jobs and stores each verdict on its JobDetail.
func Analyze(ctx context.Context, analyzer Analyzer, details []models.JobDetail) (AnalyzeStats, error) {
	return AnalyzeWithOptions(ctx, analyzer, details, AnalyzeOptions{})
}

// AnalyzeWithOptions correlates eligible jobs with bounded per-job retries and
// reports aggregate attempt progress.
func AnalyzeWithOptions(ctx context.Context, analyzer Analyzer, details []models.JobDetail, options AnalyzeOptions) (AnalyzeStats, error) {
	result, err := AnalyzeDetailedWithOptions(ctx, analyzer, details, options)
	return result.Stats, err
}

// AnalyzeDetailedWithOptions returns one outcome for every eligible job.
func AnalyzeDetailedWithOptions(ctx context.Context, analyzer Analyzer, details []models.JobDetail, options AnalyzeOptions) (AnalyzeResult, error) {
	work := eligibleWork(details)
	result := AnalyzeResult{Stats: AnalyzeStats{Eligible: len(work)}, Outcomes: map[string]JobOutcome{}}
	if options.OnPlan != nil {
		options.OnPlan(result.Stats.Eligible)
	}
	var errs []error
	for _, item := range work {
		pa, attempts, retries, repairs, cacheHits, suppressed, freshRetries, err := analyzeOne(ctx, analyzer, item, options.OnAttempt)
		result.Stats.Attempts += attempts
		result.Stats.Retries += retries
		result.Stats.Repairs += repairs
		result.Stats.CacheHits += cacheHits
		result.Stats.Suppressed += suppressed
		result.Stats.FreshRetries += freshRetries
		outcome := JobOutcome{JobID: item.jobID, Succeeded: err == nil, Attempts: attempts, Repairs: repairs, CacheHits: cacheHits, Suppressed: suppressed > 0, FreshRetries: freshRetries}
		if pa != nil {
			outcome.Systemic = pa.Systemic
		}
		if err != nil {
			outcome.FailureCategory = ai.PatternFailureCategoryOf(err)
		}
		result.Outcomes[item.jobID] = outcome
		if options.OnOutcome != nil {
			options.OnOutcome(outcome)
		}
		d := &details[item.index]
		if err != nil {
			result.Stats.Failed++
			log.Printf("  ⚠ pattern analysis failed for %s: category=%s", d.Name, outcome.FailureCategory)
			errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
			continue
		}
		applyRemediationVerification(ctx, analyzer, pa, *d)
		if applyAnalysis(d, pa) {
			result.Stats.Completed++
		}
	}
	return result, errors.Join(errs...)
}

// AnalyzeConcurrent starts every eligible correlation before waiting for
// results, so one slow job cannot consume the finalization budget for the rest.
func AnalyzeConcurrent(ctx context.Context, analyzer Analyzer, details []models.JobDetail) AnalyzeStats {
	type result struct {
		index        int
		pa           *models.PatternAnalysis
		err          error
		attempts     int
		retries      int
		repairs      int
		cacheHits    int
		suppressed   int
		freshRetries int
	}
	work := eligibleWork(details)
	results := make(chan result, len(work))
	stats := AnalyzeStats{Eligible: len(work)}
	for _, item := range work {
		go func(item analysisWork) {
			pa, attempts, retries, repairs, cacheHits, suppressed, freshRetries, err := analyzeOne(ctx, analyzer, item, nil)
			results <- result{index: item.index, pa: pa, err: err, attempts: attempts, retries: retries, repairs: repairs, cacheHits: cacheHits, suppressed: suppressed, freshRetries: freshRetries}
		}(item)
	}
	for range stats.Eligible {
		result := <-results
		stats.Attempts += result.attempts
		stats.Retries += result.retries
		stats.Repairs += result.repairs
		stats.CacheHits += result.cacheHits
		stats.Suppressed += result.suppressed
		stats.FreshRetries += result.freshRetries
		d := &details[result.index]
		if result.err != nil {
			stats.Failed++
			log.Printf("  ⚠ pattern analysis failed for %s: category=%s", d.Name, ai.PatternFailureCategoryOf(result.err))
			continue
		}
		applyRemediationVerification(ctx, analyzer, result.pa, *d)
		if applyAnalysis(d, result.pa) {
			stats.Completed++
		}
	}
	return stats
}

// IsEligible reports whether a job qualifies for recurring-pattern correlation.
func IsEligible(detail *models.JobDetail) bool {
	return detail != nil && CountFailedBuilds(detail) >= MinFailedBuilds && len(GatherFailures(detail)) >= 2
}

func eligibleWork(details []models.JobDetail) []analysisWork {
	work := make([]analysisWork, 0, len(details))
	for i := range details {
		d := &details[i]
		failures := GatherFailures(d)
		if !IsEligible(d) {
			continue
		}
		work = append(work, analysisWork{index: i, jobID: d.JobID, subject: d.Name, failures: failures})
	}
	return work
}

func analyzeOne(ctx context.Context, analyzer Analyzer, work analysisWork, observe func(Attempt)) (*models.PatternAnalysis, int, int, int, int, int, int, error) {
	repairUsed := false
	repairs := 0
	cacheHits := 0
	suppressed := 0
	freshRetries := 0
	for attempt := 1; attempt <= maxPatternAttempts; attempt++ {
		var pa *models.PatternAnalysis
		var err error
		freshRetryThisAttempt := false
		if observed, ok := analyzer.(observedAnalyzer); ok {
			pa, err = observed.AnalyzePatternWithOptions(ctx, work.jobID, work.subject, work.failures, ai.PatternAnalyzeOptions{
				AllowAmbiguityRepair:  !repairUsed,
				AllowValidationRepair: !repairUsed,
				OnCacheHit:            func() { cacheHits++ },
				OnFailureSuppressed: func(PatternFailureCategory ai.PatternFailureCategory) {
					suppressed++
				},
				OnFreshRetry: func() {
					freshRetries++
					freshRetryThisAttempt = true
				},
				OnRepair: func(result ai.PatternRepairAttempt) {
					repairUsed = true
					repairs++
					if observe != nil {
						observe(Attempt{
							Number: repairs, Repair: true, Succeeded: result.Succeeded, Final: true,
							FailureCategory: result.FailureCategory,
						})
					}
				},
			})
		} else {
			pa, err = analyzer.AnalyzePattern(ctx, work.jobID, work.subject, work.failures)
		}
		retry := err != nil && attempt < maxPatternAttempts && ai.IsRetryablePatternError(err)
		if observe != nil {
			observe(Attempt{
				Number: attempt, CacheHit: cacheHits > 0, Suppressed: ai.IsPatternFailureSuppressed(err), FreshRetry: freshRetryThisAttempt,
				Retry: attempt > 1, Succeeded: err == nil, Final: err == nil || !retry,
				FailureCategory: ai.PatternFailureCategoryOf(err),
			})
		}
		if err == nil {
			return pa, attempt, attempt - 1, repairs, cacheHits, suppressed, freshRetries, nil
		}
		if !retry {
			return nil, attempt, attempt - 1, repairs, cacheHits, suppressed, freshRetries, err
		}
		log.Printf("  ↻ retrying pattern analysis for %s: category=%s", work.subject, ai.PatternFailureCategoryOf(err))
	}
	panic("unreachable pattern retry state")
}

func applyAnalysis(detail *models.JobDetail, pa *models.PatternAnalysis) bool {
	if pa == nil {
		return false
	}
	pa.JobID = detail.JobID
	models.ApplyPatternLifecycle(*detail, pa)
	detail.PatternAnalyses = []models.PatternAnalysis{*pa}
	verdict := "not systemic"
	if pa.Systemic {
		verdict = fmt.Sprintf("SYSTEMIC (%s): %s", pa.Confidence, pa.SharedRootCause)
	}
	log.Printf("  🔗 pattern analysis for %s across %d builds: %s", detail.Name, pa.BuildsAnalyzed, verdict)
	return true
}

func applyRemediationVerification(ctx context.Context, analyzer Analyzer, pattern *models.PatternAnalysis, detail models.JobDetail) {
	if pattern == nil || !models.PatternAllowsActions(*pattern) || !pattern.Systemic {
		return
	}
	verifier, ok := analyzer.(remediationVerifier)
	if !ok {
		return
	}
	verification, err := verifier.VerifyPatternRemediation(ctx, *pattern, detail)
	if err != nil {
		pattern.RemediationVerification = &models.PatternRemediationVerification{
			State: models.PatternRemediationInconclusive, Reason: "Pinned source verification could not be completed.",
		}
		return
	}
	pattern.RemediationVerification = &verification
}

// AssignIDs gives every pattern its stable frontend and actions identifier.
func AssignIDs(details []models.JobDetail) {
	for i := range details {
		for j := range details[i].PatternAnalyses {
			models.AssignPatternIdentity(&details[i].PatternAnalyses[j])
		}
	}
}

// CollectRecurring gathers systemic verdicts, ordered by confidence and span.
func CollectRecurring(details []models.JobDetail) []models.PatternAnalysis {
	var out []models.PatternAnalysis
	for i := range details {
		for _, pa := range details[i].PatternAnalyses {
			if pa.Systemic && models.PatternIsActive(pa) {
				out = append(out, pa)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := confidenceRank(out[i].Confidence), confidenceRank(out[j].Confidence)
		if ri != rj {
			return ri > rj
		}
		return out[i].BuildsAnalyzed > out[j].BuildsAnalyzed
	})
	return out
}

// CountFailedBuilds counts a job's completed failed builds.
func CountFailedBuilds(d *models.JobDetail) int {
	n := 0
	for i := range d.Runs {
		run := &d.Runs[i]
		if !run.Passed && run.Result != "PENDING" {
			n++
		}
	}
	return n
}

// GatherFailures picks the most severe analyzed failure from each failed build.
func GatherFailures(d *models.JobDetail) []ai.PatternFailure {
	var out []ai.PatternFailure
	recentRuns := make([]ai.PatternRun, 0, len(d.Runs))
	for i := range d.Runs {
		run := &d.Runs[i]
		if run.Result == "PENDING" {
			continue
		}
		recentRuns = append(recentRuns, ai.PatternRun{
			BuildID: run.BuildID, Result: run.Result, Passed: run.Passed,
			StartedAt: run.Started, SourceRevision: patternRunSourceRevision(run.BuildInfo),
		})
	}
	sort.SliceStable(recentRuns, func(i, j int) bool {
		if !recentRuns[i].StartedAt.Equal(recentRuns[j].StartedAt) {
			if recentRuns[i].StartedAt.IsZero() {
				return false
			}
			if recentRuns[j].StartedAt.IsZero() {
				return true
			}
			return recentRuns[i].StartedAt.After(recentRuns[j].StartedAt)
		}
		return recentRuns[i].BuildID > recentRuns[j].BuildID
	})
	for i := range d.Runs {
		run := &d.Runs[i]
		if run.Passed || run.Result == "PENDING" {
			continue
		}
		rep := RepresentativeAnalyzedFailure(run)
		if rep == nil {
			continue
		}
		out = append(out, ai.PatternFailure{
			BuildID:            run.BuildID,
			FailingTest:        rep.Name,
			FailureMessage:     rep.FailureMessage,
			RootCause:          rep.AIAnalysis.RootCause,
			SuggestedFix:       rep.AIAnalysis.SuggestedFix,
			RelevantFiles:      rep.AIAnalysis.RelevantFiles,
			CauseLocation:      rep.AIAnalysis.CauseLocation,
			LocationFile:       FailureLocationFile(rep.FailureLocation),
			ProwJobName:        d.Name,
			ProwConfigFile:     d.ConfigFile,
			ProwConfigRevision: d.ConfigRevision,
			IsTransient:        rep.AISummary != nil && rep.AISummary.IsTransient,
			Severity:           rep.AIAnalysis.Severity,
			RecentRuns:         recentRuns,
		})
	}
	return out
}

// RepresentativeAnalyzedFailure returns the exact failure used for causal correlation.
func RepresentativeAnalyzedFailure(run *models.BuildResult) *models.TestCase {
	if run == nil {
		return nil
	}
	var representative *models.TestCase
	for index := range run.TestCases {
		testCase := &run.TestCases[index]
		if testCase.Status != "failed" || testCase.AIAnalysis == nil || !ai.IsGroundedAnalysis(testCase.AIAnalysis) {
			continue
		}
		if representative == nil || models.SeverityRank(testCase.AIAnalysis.Severity) > models.SeverityRank(representative.AIAnalysis.Severity) {
			representative = testCase
		}
	}
	return representative
}

func patternRunSourceRevision(build models.BuildInfo) string {
	if revision := strings.TrimSpace(build.Revision); revision != "" {
		return revision
	}
	if commit := strings.TrimSpace(build.Commit); commit != "" {
		return commit
	}
	if len(build.RepoRefs) == 1 {
		for _, revision := range build.RepoRefs {
			return strings.TrimSpace(revision)
		}
	}
	return ""
}

func confidenceRank(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// FailureLocationFile strips a trailing line and column from a failure location.
func FailureLocationFile(loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return ""
	}
	file, _, _ := strings.Cut(loc, ":")
	return file
}
