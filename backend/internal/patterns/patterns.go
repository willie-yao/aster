// Package patterns correlates analyzed failures across builds and prepares the
// recurring-pattern data produced by the analysis runtime.
package patterns

import (
	"context"
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

// AnalyzeStats records eligible jobs and completed or failed correlations.
type AnalyzeStats struct {
	Eligible  int
	Completed int
	Failed    int
}

// Analyze correlates eligible jobs and stores each verdict on its JobDetail.
func Analyze(ctx context.Context, analyzer Analyzer, details []models.JobDetail) AnalyzeStats {
	var stats AnalyzeStats
	for i := range details {
		d := &details[i]
		failures := GatherFailures(d)
		if CountFailedBuilds(d) < MinFailedBuilds || len(failures) < 2 {
			continue
		}
		stats.Eligible++
		pa, err := analyzer.AnalyzePattern(ctx, d.JobID, d.Name, failures)
		if err != nil {
			stats.Failed++
			log.Printf("  ⚠ pattern analysis failed for %s: %v", d.Name, err)
			continue
		}
		if applyAnalysis(d, pa) {
			stats.Completed++
		}
	}
	return stats
}

// AnalyzeConcurrent starts every eligible correlation before waiting for
// results, so one slow job cannot consume the finalization budget for the rest.
func AnalyzeConcurrent(ctx context.Context, analyzer Analyzer, details []models.JobDetail) AnalyzeStats {
	type result struct {
		index int
		pa    *models.PatternAnalysis
		err   error
	}
	results := make(chan result, len(details))
	var stats AnalyzeStats
	for i := range details {
		d := &details[i]
		failures := GatherFailures(d)
		if CountFailedBuilds(d) < MinFailedBuilds || len(failures) < 2 {
			continue
		}
		stats.Eligible++
		go func(index int, jobID, subject string, failures []ai.PatternFailure) {
			pa, err := analyzer.AnalyzePattern(ctx, jobID, subject, failures)
			results <- result{index: index, pa: pa, err: err}
		}(i, d.JobID, d.Name, failures)
	}
	for range stats.Eligible {
		result := <-results
		d := &details[result.index]
		if result.err != nil {
			stats.Failed++
			log.Printf("  ⚠ pattern analysis failed for %s: %v", d.Name, result.err)
			continue
		}
		if applyAnalysis(d, result.pa) {
			stats.Completed++
		}
	}
	return stats
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
			details[i].PatternAnalyses[j].ID = models.PatternID(details[i].PatternAnalyses[j])
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
