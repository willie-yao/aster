package aggregator

import (
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

// setupTeardownPrefixes lists test name substrings that indicate
// setup/teardown tests which should be excluded unless they failed.
var setupTeardownPrefixes = []string{
	"SynchronizedBeforeSuite",
	"SynchronizedAfterSuite",
	"BeforeSuite",
	"AfterSuite",
}

// isSetupTeardown returns true if the test name looks like a setup/teardown test.
func isSetupTeardown(name string) bool {
	for _, prefix := range setupTeardownPrefixes {
		if strings.Contains(name, prefix) {
			return true
		}
	}
	return false
}

// BuildSearchIndex creates a searchable index of unique tests across jobs.
// jobResults is keyed by JobID so same-named jobs in different repos do not collide.
func BuildSearchIndex(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, now time.Time) models.SearchIndex {
	jobMeta := make(map[string]models.ProwJob, len(jobs))
	for _, j := range jobs {
		jobMeta[j.JobID] = j
	}

	type testKey struct {
		testName string
		jobID    string
	}
	type testInfo struct {
		latestStatus string
		failures     int
		appearances  int
	}

	seen := make(map[testKey]*testInfo)

	for jobID, runs := range jobResults {
		// Process runs newest-first: the first occurrence sets the latest status.
		for _, run := range runs {
			for _, tc := range run.TestCases {
				if tc.Source == models.TestCaseSourceBuild {
					continue
				}
				key := testKey{testName: tc.Name, jobID: jobID}
				info, ok := seen[key]
				if !ok {
					info = &testInfo{latestStatus: tc.Status}
					seen[key] = info
				}
				if tc.Status == "skipped" {
					continue
				}
				info.appearances++
				if tc.Status == "failed" {
					info.failures++
				}
				// The first non-skipped status we encounter is from the latest run.
				if info.latestStatus == "skipped" {
					info.latestStatus = tc.Status
				}
			}
		}
	}

	var entries []models.SearchEntry

	// Add job entries searchable by job name and tab name.
	for jobID := range jobResults {
		meta := jobMeta[jobID]
		entries = append(entries, models.SearchEntry{
			Kind:     "job",
			JobName:  meta.Name,
			JobID:    jobID,
			JobType:  meta.JobType,
			Repo:     meta.Repo,
			TabName:  meta.TabName,
			Branch:   meta.Branch,
			Category: meta.Category,
			Status:   string(CurrentJobStatus(jobResults[jobID])),
		})
	}

	for key, info := range seen {
		// Skip tests that were only ever skipped.
		if info.appearances == 0 {
			continue
		}

		// Skip setup/teardown tests unless they failed.
		if isSetupTeardown(key.testName) && info.failures == 0 {
			continue
		}

		var failRate float64
		if info.appearances > 0 {
			failRate = float64(info.failures) / float64(info.appearances)
		}

		meta := jobMeta[key.jobID]
		entries = append(entries, models.SearchEntry{
			Kind:     "test",
			TestName: key.testName,
			JobName:  meta.Name,
			JobID:    key.jobID,
			JobType:  meta.JobType,
			Repo:     meta.Repo,
			TabName:  meta.TabName,
			Branch:   meta.Branch,
			Category: meta.Category,
			Status:   info.latestStatus,
			FailRate: failRate,
		})
	}

	// Sort by JobID then test name for deterministic output.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].JobID != entries[j].JobID {
			return entries[i].JobID < entries[j].JobID
		}
		return entries[i].TestName < entries[j].TestName
	})

	return models.SearchIndex{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}
