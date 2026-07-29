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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// MinFailedBuilds gates job-level recurring-pattern analysis.
const MinFailedBuilds = 3

// Analyzer correlates representative failures from several builds of one job.
type Analyzer interface {
	AnalyzePattern(ctx context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error)
}

const maxPatternAttempts = 2

// AnalyzeStats records eligible jobs and completed or failed correlations.
type AnalyzeStats struct {
	Eligible  int
	Completed int
	Failed    int
	Attempts  int
	Retries   int
}

// Attempt reports one privacy-safe correlation attempt.
type Attempt struct {
	Number          int
	Retry           bool
	Succeeded       bool
	Final           bool
	FailureCategory ai.PatternFailureCategory
}

// AnalyzeOptions reports pattern planning and attempt progress.
type AnalyzeOptions struct {
	OnPlan    func(int)
	OnAttempt func(Attempt)
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
	work := eligibleWork(details)
	stats := AnalyzeStats{Eligible: len(work)}
	if options.OnPlan != nil {
		options.OnPlan(stats.Eligible)
	}
	var errs []error
	for _, item := range work {
		pa, attempts, retries, err := analyzeOne(ctx, analyzer, item, options.OnAttempt)
		stats.Attempts += attempts
		stats.Retries += retries
		d := &details[item.index]
		if err != nil {
			stats.Failed++
			category := ai.PatternFailureCategoryOf(err)
			log.Printf("  ⚠ pattern analysis failed for %s: category=%s", d.Name, category)
			errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
			continue
		}
		if applyAnalysis(d, pa) {
			stats.Completed++
		}
	}
	return stats, errors.Join(errs...)
}

// AnalyzeConcurrent starts every eligible correlation before waiting for
// results, so one slow job cannot consume the finalization budget for the rest.
func AnalyzeConcurrent(ctx context.Context, analyzer Analyzer, details []models.JobDetail) AnalyzeStats {
	type result struct {
		index    int
		pa       *models.PatternAnalysis
		err      error
		attempts int
		retries  int
	}
	work := eligibleWork(details)
	results := make(chan result, len(work))
	stats := AnalyzeStats{Eligible: len(work)}
	for _, item := range work {
		go func(item analysisWork) {
			pa, attempts, retries, err := analyzeOne(ctx, analyzer, item, nil)
			results <- result{index: item.index, pa: pa, err: err, attempts: attempts, retries: retries}
		}(item)
	}
	for range stats.Eligible {
		result := <-results
		stats.Attempts += result.attempts
		stats.Retries += result.retries
		d := &details[result.index]
		if result.err != nil {
			stats.Failed++
			log.Printf("  ⚠ pattern analysis failed for %s: category=%s", d.Name, ai.PatternFailureCategoryOf(result.err))
			continue
		}
		if applyAnalysis(d, result.pa) {
			stats.Completed++
		}
	}
	return stats
}

func eligibleWork(details []models.JobDetail) []analysisWork {
	work := make([]analysisWork, 0, len(details))
	for i := range details {
		d := &details[i]
		failures := GatherFailures(d)
		if CountFailedBuilds(d) < MinFailedBuilds || len(failures) < 2 {
			continue
		}
		work = append(work, analysisWork{index: i, jobID: d.JobID, subject: d.Name, failures: failures})
	}
	return work
}

func analyzeOne(ctx context.Context, analyzer Analyzer, work analysisWork, observe func(Attempt)) (*models.PatternAnalysis, int, int, error) {
	for attempt := 1; attempt <= maxPatternAttempts; attempt++ {
		pa, err := analyzer.AnalyzePattern(ctx, work.jobID, work.subject, work.failures)
		retry := err != nil && attempt < maxPatternAttempts && ai.IsRetryablePatternError(err)
		if observe != nil {
			observe(Attempt{
				Number: attempt, Retry: attempt > 1, Succeeded: err == nil, Final: err == nil || !retry,
				FailureCategory: ai.PatternFailureCategoryOf(err),
			})
		}
		if err == nil {
			return pa, attempt, attempt - 1, nil
		}
		if !retry {
			return nil, attempt, attempt - 1, err
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
	detail.PatternAnalyses = []models.PatternAnalysis{*pa}
	verdict := "not systemic"
	if pa.Systemic {
		verdict = fmt.Sprintf("SYSTEMIC (%s): %s", pa.Confidence, pa.SharedRootCause)
	}
	log.Printf("  🔗 pattern analysis for %s across %d builds: %s", detail.Name, pa.BuildsAnalyzed, verdict)
	return true
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
			if pa.Systemic {
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
	for i := range d.Runs {
		run := &d.Runs[i]
		if run.Passed || run.Result == "PENDING" {
			continue
		}
		var rep *models.TestCase
		for j := range run.TestCases {
			tc := &run.TestCases[j]
			if tc.Status != "failed" || tc.AIAnalysis == nil {
				continue
			}
			if rep == nil || severityRank(tc.AIAnalysis.Severity) > severityRank(rep.AIAnalysis.Severity) {
				rep = tc
			}
		}
		if rep == nil {
			continue
		}
		out = append(out, ai.PatternFailure{
			BuildID:        run.BuildID,
			FailingTest:    rep.Name,
			FailureMessage: rep.FailureMessage,
			RootCause:      rep.AIAnalysis.RootCause,
			SuggestedFix:   rep.AIAnalysis.SuggestedFix,
			RelevantFiles:  rep.AIAnalysis.RelevantFiles,
			LocationFile:   FailureLocationFile(rep.FailureLocation),
			IsTransient:    rep.AISummary != nil && rep.AISummary.IsTransient,
			Severity:       rep.AIAnalysis.Severity,
		})
	}
	return out
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

func severityRank(sev string) int {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "transient-ignore":
		return 1
	default:
		return 0
	}
}
