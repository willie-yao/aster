package orka

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
)

// FinalizeStats summarizes the job-level data added after Orka per-test
// analysis completes.
type FinalizeStats struct {
	Jobs              int
	PatternAnalyses   int
	PatternFailures   int
	RecurringPatterns int
}

// FinalizePatterns correlates analyzed failures across builds, writes the
// job-level verdicts, and folds systemic patterns into flakiness.json.
func FinalizePatterns(ctx context.Context, dataDir string, analyzer patterns.Analyzer) (FinalizeStats, error) {
	if analyzer == nil {
		return FinalizeStats{}, fmt.Errorf("orka finalization: pattern analyzer is required")
	}
	details, err := loadJobDetails(dataDir)
	if err != nil {
		return FinalizeStats{}, err
	}

	analyzeStats := patterns.AnalyzeConcurrent(ctx, analyzer, details)
	patterns.AssignIDs(details)
	recurring := patterns.CollectRecurring(details)

	for _, detail := range details {
		if err := output.WriteJobDetail(dataDir, detail); err != nil {
			return FinalizeStats{}, fmt.Errorf("orka finalization: write job %s: %w", detail.JobID, err)
		}
	}

	report, err := loadFlakinessReport(dataDir)
	if err != nil {
		return FinalizeStats{}, err
	}
	report.RecurringPatterns = recurring
	if err := output.WriteFlakinessReport(dataDir, report); err != nil {
		return FinalizeStats{}, fmt.Errorf("orka finalization: write flakiness: %w", err)
	}

	if state := resolve.Load(dataDir); len(state.Resolved) > 0 {
		if pruned, changed := state.Prune(recurring); changed {
			if err := pruned.Save(dataDir); err != nil {
				return FinalizeStats{}, fmt.Errorf("orka finalization: save resolved state: %w", err)
			}
		}
	}

	stats := FinalizeStats{
		Jobs: len(details), PatternFailures: analyzeStats.Failed,
		RecurringPatterns: len(recurring),
	}
	for i := range details {
		stats.PatternAnalyses += len(details[i].PatternAnalyses)
	}
	return stats, nil
}

func loadJobDetails(dataDir string) ([]models.JobDetail, error) {
	active, err := ActiveJobIDs(dataDir)
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dataDir, "jobs", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("orka finalization: list jobs: %w", err)
	}
	sort.Strings(paths)
	details := make([]models.JobDetail, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("orka finalization: read %s: %w", path, err)
		}
		var detail models.JobDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			return nil, fmt.Errorf("orka finalization: parse %s: %w", path, err)
		}
		if detail.JobID == "" {
			return nil, fmt.Errorf("orka finalization: %s has no job_id", path)
		}
		if !active[detail.JobID] {
			continue
		}
		details = append(details, detail)
	}
	return details, nil
}

// ActiveJobIDs returns the jobs present in the current dashboard snapshot.
func ActiveJobIDs(dataDir string) (map[string]bool, error) {
	path := filepath.Join(dataDir, "dashboard.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orka finalization: read dashboard: %w", err)
	}
	var dashboard models.Dashboard
	if err := json.Unmarshal(data, &dashboard); err != nil {
		return nil, fmt.Errorf("orka finalization: parse dashboard: %w", err)
	}
	active := make(map[string]bool, len(dashboard.Jobs))
	for _, job := range dashboard.Jobs {
		if job.JobID != "" {
			active[job.JobID] = true
		}
	}
	return active, nil
}

func loadFlakinessReport(dataDir string) (models.FlakinessReport, error) {
	path := filepath.Join(dataDir, "flakiness.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return models.FlakinessReport{}, fmt.Errorf("orka finalization: read flakiness: %w", err)
	}
	var report models.FlakinessReport
	if err := json.Unmarshal(data, &report); err != nil {
		return models.FlakinessReport{}, fmt.Errorf("orka finalization: parse flakiness: %w", err)
	}
	return report, nil
}
