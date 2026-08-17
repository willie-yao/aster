// Package output writes pre-processed JSON files for the React frontend.
package output

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// AITraceFilename is the private per-analysis trace snapshot.
const AITraceFilename = "ai_traces.json"

const (
	// AIUsageFetcherFilename is the fetcher-owned private usage ledger.
	AIUsageFetcherFilename = "ai_usage_fetcher.json"
	// AIUsageServerFilename is the server-owned private usage ledger.
	AIUsageServerFilename = "ai_usage_server.json"
)

// NonPublishedFiles are operational files written into the output directory that
// must not be served by the API server or deployed to the public Pages site:
// the AI cache, private analysis state, and operational side-effect state. The
// frontend never reads them; they carry operational metadata rather than
// dashboard data. resolved.json is intentionally excluded from this list because
// the frontend serves it to render resolved-failure state.
var NonPublishedFiles = []string{
	"ai_cache.json",
	AITraceFilename,
	AIUsageFetcherFilename,
	AIUsageServerFilename,
	"issue_state.json",
	"fix_pr_state.json",
	"fix_previews.json",
	"notification_state.json",
	"remediation_state.json",
	"remediation_prow_catalog.json",
	// Retained so a stale file left in an existing data directory by a removed
	// analysis runtime is never published.
	"orka_analysis.json",
	"action_request_state.json",
	"action_preview_state.json",
	"analysis_correction_state.json",
	"pr_escalation_state.json",
}

// writeJSON writes indented JSON to path atomically, creating parent
// directories as needed. See statefile.WriteJSON.
func writeJSON(path string, v any) error {
	return statefile.WriteJSON(path, v)
}

// WriteDashboard writes dashboard.json to dir.
func WriteDashboard(dir string, dashboard models.Dashboard) error {
	if dashboard.Jobs == nil {
		dashboard.Jobs = []models.JobSummary{}
	}
	return writeJSON(filepath.Join(dir, "dashboard.json"), dashboard)
}

// WriteJobDetail writes a per-job detail file under dir/jobs.
// Keying by JobID prevents same-named jobs from overwriting each other.
func WriteJobDetail(dir string, detail models.JobDetail) error {
	detail.PatternAnalyses, _ = models.BackfillPatternIdentities(detail.PatternAnalyses)
	detail.PatternAnalyses = models.WithDefaultPatternRemediationInvestigations(detail.PatternAnalyses)
	return writeJSON(filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), detail)
}

// WriteFlakinessReport writes flakiness.json to dir.
func WriteFlakinessReport(dir string, report models.FlakinessReport) error {
	if report.BuildFailures == nil {
		report.BuildFailures = []models.BuildFailureSummary{}
	}
	report.RecurringPatterns, _ = models.BackfillPatternIdentities(report.RecurringPatterns)
	report.RecurringPatterns = models.WithDefaultPatternRemediationInvestigations(report.RecurringPatterns)
	return writeJSON(filepath.Join(dir, "flakiness.json"), report)
}

// WriteSearchIndex writes search-index.json to dir.
func WriteSearchIndex(dir string, index models.SearchIndex) error {
	return writeJSON(filepath.Join(dir, "search-index.json"), index)
}

// PullRequestIndexFilename is the public open-pull-request index.
const PullRequestIndexFilename = "pull-requests.json"

// pullRequestDir holds the per-pull-request detail files.
const pullRequestDir = "pull-requests"

// WritePullRequestIndex writes pull-requests.json to dir.
func WritePullRequestIndex(dir string, index models.PullRequestIndex) error {
	if index.PullRequests == nil {
		index.PullRequests = []models.PullRequestSummary{}
	}
	return writeJSON(filepath.Join(dir, PullRequestIndexFilename), index)
}

// WritePullRequestDetail writes one pull request's detail file under
// dir/pull-requests.
func WritePullRequestDetail(dir string, detail models.PullRequestDetail) error {
	if detail.Checks == nil {
		detail.Checks = []models.PullRequestCheck{}
	}
	return writeJSON(filepath.Join(dir, pullRequestDir, models.PullRequestDataFilename(detail.Number)), detail)
}

// WritePullRequests writes the index and every detail file, then removes detail
// files for pull requests that are no longer open.
func WritePullRequests(dir string, index models.PullRequestIndex, details []models.PullRequestDetail) error {
	if err := WritePullRequestIndex(dir, index); err != nil {
		return err
	}
	for _, detail := range details {
		if err := WritePullRequestDetail(dir, detail); err != nil {
			return err
		}
	}
	return prunePullRequestDetails(dir, details)
}

func prunePullRequestDetails(dir string, details []models.PullRequestDetail) error {
	expected := make(map[string]bool, len(details))
	for _, detail := range details {
		expected[models.PullRequestDataFilename(detail.Number)] = true
	}
	return pruneStaleJSON(filepath.Join(dir, pullRequestDir), expected, "pull request detail")
}

// WriteManifest writes manifest.json with the resolved project config so the
// frontend knows its title, base path, and repo links at runtime.
func WriteManifest(dir string, cfg *project.Config) error {
	return writeJSON(filepath.Join(dir, "manifest.json"), cfg)
}

// WriteAll writes dashboard.json, all job detail files, flakiness.json,
// search-index.json, and manifest.json. Returns the first error encountered.
func WriteAll(dir string, cfg *project.Config, dashboard models.Dashboard, details []models.JobDetail, flakiness models.FlakinessReport, searchIndex models.SearchIndex) error {
	if err := WriteManifest(dir, cfg); err != nil {
		return err
	}
	if err := WriteDashboard(dir, dashboard); err != nil {
		return err
	}
	for _, d := range details {
		if err := WriteJobDetail(dir, d); err != nil {
			return err
		}
	}
	if err := pruneJobDetails(dir, details); err != nil {
		return err
	}
	if err := WriteFlakinessReport(dir, flakiness); err != nil {
		return err
	}
	if err := WriteSearchIndex(dir, searchIndex); err != nil {
		return err
	}
	return nil
}

func pruneJobDetails(dir string, details []models.JobDetail) error {
	expected := make(map[string]bool, len(details))
	for _, detail := range details {
		expected[models.JobDataFilename(detail.JobID)] = true
	}
	return pruneStaleJSON(filepath.Join(dir, "jobs"), expected, "job detail")
}

// pruneStaleJSON removes .json files in dir that are not in expected. A missing
// directory is not an error because nothing has been written there yet.
func pruneStaleJSON(dir string, expected map[string]bool, label string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || expected[entry.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale %s %s: %w", label, entry.Name(), err)
		}
	}
	return nil
}
