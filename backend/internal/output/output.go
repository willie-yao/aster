// Package output writes pre-processed JSON files for the React frontend.
package output

import (
	"path/filepath"
	"regexp"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

// NonPublishedFiles are operational files written into the output directory that
// must not be served by the API server or deployed to the public Pages site:
// the AI cache and the write-automation dedup/preview state. The frontend never
// reads them; they carry operational metadata (issue numbers, PR URLs) rather
// than dashboard data. resolved.json is intentionally excluded from this list
// because the frontend serves it to render resolved-failure state.
var NonPublishedFiles = []string{
	"ai_cache.json",
	"issue_state.json",
	"fix_pr_state.json",
	"fix_previews.json",
	"notification_state.json",
}

// SanitizeFilename replaces unsafe filename characters with hyphens.
func SanitizeFilename(name string) string {
	return unsafeChars.ReplaceAllString(name, "-")
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
	name := SanitizeFilename(detail.JobID) + ".json"
	return writeJSON(filepath.Join(dir, "jobs", name), detail)
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
	if err := WriteFlakinessReport(dir, flakiness); err != nil {
		return err
	}
	if err := WriteSearchIndex(dir, searchIndex); err != nil {
		return err
	}
	return nil
}
