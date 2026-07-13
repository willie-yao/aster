package aggregator

import (
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var searchBaseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func searchHoursAgo(h int) time.Time {
	return searchBaseTime.Add(-time.Duration(h) * time.Hour)
}

func makeSearchBuild(buildID, jobName string, started time.Time, passed bool, tests []models.TestCase) models.BuildResult {
	total := len(tests)
	p, f, s := 0, 0, 0
	for _, tc := range tests {
		switch tc.Status {
		case "passed":
			p++
		case "failed":
			f++
		case "skipped":
			s++
		}
	}
	result := "SUCCESS"
	if !passed {
		result = "FAILURE"
	}
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID:         buildID,
			JobName:         jobName,
			Started:         started,
			Finished:        started.Add(300 * time.Second),
			Passed:          passed,
			Result:          result,
			DurationSeconds: 300,
		},
		TestCases:    tests,
		TestsTotal:   total,
		TestsPassed:  p,
		TestsFailed:  f,
		TestsSkipped: s,
	}
}

func searchJobs() []models.ProwJob {
	return []models.ProwJob{
		{Name: "job-alpha", JobID: "job-alpha", JobType: models.JobTypePeriodic, TabName: "Alpha Tab", Branch: "main", Category: "e2e"},
		{Name: "job-beta", JobID: "job-beta", JobType: models.JobTypePeriodic, TabName: "Beta Tab", Branch: "release-1.0", Category: "conformance"},
	}
}

func TestBuildSearchIndex_Deduplication(t *testing.T) {
	// Same test name across runs of one job yields one entry.
	jobs := searchJobs()[:1]
	jobResults := map[string][]models.BuildResult{
		"job-alpha": {
			makeSearchBuild("2", "job-alpha", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestDup", "passed", 1.0, ""),
			}),
			makeSearchBuild("1", "job-alpha", searchHoursAgo(2), false, []models.TestCase{
				makeTC("TestDup", "failed", 1.0, "err"),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	count := 0
	for _, e := range idx.Entries {
		if e.TestName == "TestDup" && e.JobName == "job-alpha" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 entry for TestDup/job-alpha, got %d", count)
	}
}

func TestBuildSearchIndex_StatusFromLatestRun(t *testing.T) {
	// Latest run passed, so status should be "passed".
	jobs := searchJobs()[:1]
	jobResults := map[string][]models.BuildResult{
		"job-alpha": {
			makeSearchBuild("2", "job-alpha", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestStatus", "passed", 1.0, ""),
			}),
			makeSearchBuild("1", "job-alpha", searchHoursAgo(2), false, []models.TestCase{
				makeTC("TestStatus", "failed", 1.0, "err"),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	var found *models.SearchEntry
	for i := range idx.Entries {
		if idx.Entries[i].TestName == "TestStatus" {
			found = &idx.Entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("TestStatus entry not found")
	}
	if found.Status != "passed" {
		t.Errorf("Status = %q, want %q", found.Status, "passed")
	}
	// fail_rate is 1 failure over 2 appearances.
	if found.FailRate != 0.5 {
		t.Errorf("FailRate = %f, want 0.5", found.FailRate)
	}
}

func TestBuildSearchIndex_SkippedOnlyExclusion(t *testing.T) {
	// Test that appears only as "skipped" should be excluded.
	jobs := searchJobs()[:1]
	jobResults := map[string][]models.BuildResult{
		"job-alpha": {
			makeSearchBuild("2", "job-alpha", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestSkipOnly", "skipped", 0, ""),
				makeTC("TestReal", "passed", 1.0, ""),
			}),
			makeSearchBuild("1", "job-alpha", searchHoursAgo(2), true, []models.TestCase{
				makeTC("TestSkipOnly", "skipped", 0, ""),
				makeTC("TestReal", "passed", 1.0, ""),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	for _, e := range idx.Entries {
		if e.TestName == "TestSkipOnly" {
			t.Error("skipped-only test TestSkipOnly should be excluded from index")
		}
	}
	// TestReal should still be present.
	found := false
	for _, e := range idx.Entries {
		if e.TestName == "TestReal" {
			found = true
		}
	}
	if !found {
		t.Error("TestReal should be in the index")
	}
}

func TestBuildSearchIndex_SetupTeardownExclusion(t *testing.T) {
	// Setup/teardown tests that never failed should be excluded.
	// Setup/teardown tests that failed should be included.
	jobs := searchJobs()[:1]
	jobResults := map[string][]models.BuildResult{
		"job-alpha": {
			makeSearchBuild("2", "job-alpha", searchHoursAgo(1), false, []models.TestCase{
				makeTC("[SynchronizedBeforeSuite] setup", "passed", 1.0, ""),
				makeTC("[BeforeSuite] init", "failed", 1.0, "setup failed"),
				makeTC("[AfterSuite] cleanup", "passed", 1.0, ""),
				makeTC("TestNormal", "passed", 1.0, ""),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	names := make(map[string]bool)
	for _, e := range idx.Entries {
		names[e.TestName] = true
	}

	// Passing SynchronizedBeforeSuite is excluded.
	if names["[SynchronizedBeforeSuite] setup"] {
		t.Error("passing SynchronizedBeforeSuite should be excluded")
	}
	// Passing AfterSuite is excluded.
	if names["[AfterSuite] cleanup"] {
		t.Error("passing AfterSuite should be excluded")
	}
	// Failed BeforeSuite is included.
	if !names["[BeforeSuite] init"] {
		t.Error("failed BeforeSuite should be included")
	}
	// Normal test is included.
	if !names["TestNormal"] {
		t.Error("TestNormal should be included")
	}
}

func TestBuildSearchIndex_SortOrder(t *testing.T) {
	jobs := []models.ProwJob{
		{Name: "job-beta", JobID: "job-beta", JobType: models.JobTypePeriodic},
		{Name: "job-alpha", JobID: "job-alpha", JobType: models.JobTypePeriodic},
	}
	jobResults := map[string][]models.BuildResult{
		"job-beta": {
			makeSearchBuild("1", "job-beta", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestZ", "passed", 1.0, ""),
				makeTC("TestA", "passed", 1.0, ""),
			}),
		},
		"job-alpha": {
			makeSearchBuild("1", "job-alpha", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestM", "passed", 1.0, ""),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	if len(idx.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(idx.Entries))
	}
	var testEntries []models.SearchEntry
	for _, e := range idx.Entries {
		if e.Kind == "test" {
			testEntries = append(testEntries, e)
		}
	}
	if len(testEntries) != 3 {
		t.Fatalf("expected 3 test entries, got %d", len(testEntries))
	}
	if testEntries[0].JobName != "job-alpha" || testEntries[0].TestName != "TestM" {
		t.Errorf("testEntries[0] = %s/%s, want job-alpha/TestM", testEntries[0].JobName, testEntries[0].TestName)
	}
	if testEntries[1].JobName != "job-beta" || testEntries[1].TestName != "TestA" {
		t.Errorf("testEntries[1] = %s/%s, want job-beta/TestA", testEntries[1].JobName, testEntries[1].TestName)
	}
	if testEntries[2].JobName != "job-beta" || testEntries[2].TestName != "TestZ" {
		t.Errorf("testEntries[2] = %s/%s, want job-beta/TestZ", testEntries[2].JobName, testEntries[2].TestName)
	}
}

func TestBuildSearchIndex_JobMetadata(t *testing.T) {
	jobs := searchJobs()
	jobResults := map[string][]models.BuildResult{
		"job-alpha": {
			makeSearchBuild("1", "job-alpha", searchHoursAgo(1), true, []models.TestCase{
				makeTC("TestMeta", "passed", 1.0, ""),
			}),
		},
	}

	idx := BuildSearchIndex(jobResults, jobs, searchBaseTime)

	// 1 job entry plus 1 test entry.
	if len(idx.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(idx.Entries))
	}
	var e models.SearchEntry
	for _, entry := range idx.Entries {
		if entry.Kind == "test" {
			e = entry
			break
		}
	}
	if e.TabName != "Alpha Tab" {
		t.Errorf("TabName = %q, want %q", e.TabName, "Alpha Tab")
	}
	if e.Branch != "main" {
		t.Errorf("Branch = %q, want %q", e.Branch, "main")
	}
	if e.Category != "e2e" {
		t.Errorf("Category = %q, want %q", e.Category, "e2e")
	}
}

func TestBuildSearchIndex_GeneratedAt(t *testing.T) {
	idx := BuildSearchIndex(nil, nil, searchBaseTime)

	expected := searchBaseTime.UTC().Format(time.RFC3339)
	if idx.GeneratedAt != expected {
		t.Errorf("GeneratedAt = %q, want %q", idx.GeneratedAt, expected)
	}
}
