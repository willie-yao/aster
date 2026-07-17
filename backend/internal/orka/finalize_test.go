package orka

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

type fakePatternAnalyzer struct {
	calls int
}

func (f *fakePatternAnalyzer) AnalyzePattern(_ context.Context, _, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	f.calls++
	builds := make([]string, 0, len(failures))
	for _, failure := range failures {
		builds = append(builds, failure.BuildID)
	}
	return &models.PatternAnalysis{
		Subject:         subject,
		GeneratedAt:     "2026-07-17T00:00:00Z",
		BuildsAnalyzed:  len(failures),
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "the controller repeatedly writes a stale configuration",
		SharedBuilds:    builds,
		SuggestedFix:    "serialize the controller update in config/controller.yaml",
		Summary:         "Three recent builds failed through the same controller update path.",
		RelevantFiles:   []string{"config/controller.yaml"},
	}, nil
}

func TestFinalizePatternsWritesJobAndFlakinessPatterns(t *testing.T) {
	dir := t.TempDir()
	detail := models.JobDetail{
		Name:  "periodic-controller",
		JobID: "periodic-controller",
		Runs: []models.BuildResult{
			failedRun("103", "test/e2e/controller.go:44"),
			failedRun("102", "test/e2e/controller.go:44"),
			failedRun("101", "test/e2e/controller.go:44"),
		},
	}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	stale := detail
	stale.Name = "removed-job"
	stale.JobID = "removed-job"
	if err := output.WriteJobDetail(dir, stale); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteDashboard(dir, models.Dashboard{Jobs: []models.JobSummary{{
		ProwJob: models.ProwJob{Name: detail.Name, JobID: detail.JobID},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteFlakinessReport(dir, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	analyzer := &fakePatternAnalyzer{}
	stats, err := FinalizePatterns(context.Background(), dir, analyzer)
	if err != nil {
		t.Fatal(err)
	}
	if analyzer.calls != 1 {
		t.Fatalf("pattern analyzer calls = %d, want 1", analyzer.calls)
	}
	if stats.Jobs != 1 || stats.PatternAnalyses != 1 || stats.RecurringPatterns != 1 {
		t.Fatalf("stats = %+v, want one job, analysis, and recurring pattern", stats)
	}

	jobData, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename("periodic-controller")))
	if err != nil {
		t.Fatal(err)
	}
	var gotJob models.JobDetail
	if err := json.Unmarshal(jobData, &gotJob); err != nil {
		t.Fatal(err)
	}
	if len(gotJob.PatternAnalyses) != 1 {
		t.Fatalf("job patterns = %d, want 1", len(gotJob.PatternAnalyses))
	}
	pattern := gotJob.PatternAnalyses[0]
	if pattern.ID == "" || pattern.JobID != detail.JobID || !pattern.Systemic {
		t.Fatalf("job pattern = %+v, want stable id, job id, and systemic verdict", pattern)
	}

	flakinessData, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report models.FlakinessReport
	if err := json.Unmarshal(flakinessData, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.RecurringPatterns) != 1 || report.RecurringPatterns[0].ID != pattern.ID {
		t.Fatalf("recurring patterns = %+v, want the job pattern", report.RecurringPatterns)
	}
	if report.RecurringPatterns[0].JobID == stale.JobID {
		t.Fatalf("stale job appeared in recurring patterns: %+v", report.RecurringPatterns)
	}
}

func failedRun(buildID, location string) models.BuildResult {
	return models.BuildResult{
		BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
		TestCases: []models.TestCase{{
			Name:            "should reconcile",
			Status:          "failed",
			FailureMessage:  "timed out waiting for controller update",
			FailureLocation: location,
			AISummary:       &models.AISummary{Summary: "stale update", IsTransient: false},
			AIAnalysis: &models.AIAnalysis{
				RootCause:     "the controller wrote a stale configuration",
				Severity:      "High",
				SuggestedFix:  "serialize the controller update",
				RelevantFiles: []string{"config/controller.yaml"},
				Mode:          "agentic",
			},
		}},
	}
}
