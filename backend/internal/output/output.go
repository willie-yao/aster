// Package output writes pre-processed JSON files for the React frontend.
package output

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// NonPublishedFiles are operational files written into the output directory that
// must not be served by the API server or deployed to the public Pages site:
// the AI cache, Orka identity manifest, and operational side-effect state. The
// frontend never reads them; they carry operational metadata rather than
// dashboard data. resolved.json is intentionally excluded from this list because
// the frontend serves it to render resolved-failure state.
var NonPublishedFiles = []string{
	"ai_cache.json",
	"issue_state.json",
	"fix_pr_state.json",
	"fix_previews.json",
	"notification_state.json",
	"remediation_state.json",
	"remediation_prow_catalog.json",
	"orka_analysis.json",
	"action_request_state.json",
}

// writeJSON writes indented JSON to path atomically, creating parent
// directories as needed. See statefile.WriteJSON.
func writeJSON(path string, v any) error {
	return statefile.WriteJSON(path, v)
}

// WriteDashboard writes dashboard.json to dir.
func WriteDashboard(dir string, dashboard models.Dashboard) error {
	return writeJSON(filepath.Join(dir, "dashboard.json"), dashboard)
}

// WriteJobDetail writes a per-job detail file under dir/jobs.
// Keying by JobID prevents same-named jobs from overwriting each other.
func WriteJobDetail(dir string, detail models.JobDetail) error {
	return writeJSON(filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), detail)
}

// WriteFlakinessReport writes flakiness.json to dir.
func WriteFlakinessReport(dir string, report models.FlakinessReport) error {
	return writeJSON(filepath.Join(dir, "flakiness.json"), report)
}

// WriteSearchIndex writes search-index.json to dir.
func WriteSearchIndex(dir string, index models.SearchIndex) error {
	return writeJSON(filepath.Join(dir, "search-index.json"), index)
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
	jobsDir := filepath.Join(dir, "jobs")
	entries, err := os.ReadDir(jobsDir)
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
		if err := os.Remove(filepath.Join(jobsDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale job detail %s: %w", entry.Name(), err)
		}
	}
	return nil
}
