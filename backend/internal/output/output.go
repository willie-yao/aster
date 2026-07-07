// Package output writes pre-processed JSON files for the React frontend.
package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

// SanitizeFilename replaces unsafe filename characters with hyphens.
func SanitizeFilename(name string) string {
	return unsafeChars.ReplaceAllString(name, "-")
}

// writeJSON writes indented JSON and creates parent directories as needed. It
// writes to a temp file and renames into place so a concurrent reader (the
// server in Kubernetes-native mode) never observes a half-written file.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Best-effort: some filesystems (SMB/azurefile RWX) don't support chmod and
	// return EPERM, where the mount's file_mode governs readability instead.
	_ = os.Chmod(tmpName, 0o644)
	return os.Rename(tmpName, path)
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
