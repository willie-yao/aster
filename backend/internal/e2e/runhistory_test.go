package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/fetcher"
	"github.com/willie-yao/aster/backend/internal/models"
)

// runPipelineInto runs the pipeline against an existing output dir, so a second
// pass sees the first pass's published job details as prior state.
func runPipelineInto(t *testing.T, projectDir, outDir string, buildsPerJob int) {
	t.Helper()
	for _, k := range []string{"EMAIL_SMTP_PASSWORD", "ISSUE_TOKEN", "GITHUB_TOKEN", "AI_ENDPOINT", "AI_MODEL"} {
		t.Setenv(k, "")
	}
	err := fetcher.Run(context.Background(), fetcher.Options{
		ProjectDir:   projectDir,
		OutDir:       outDir,
		BuildsPerJob: buildsPerJob,
		Workers:      2,
		Timeout:      2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("fetcher.Run: %v", err)
	}
}

func loadOnlyJobDetail(t *testing.T, outDir string) models.JobDetail {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(outDir, "jobs", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("job files = %v, want 1", matches)
	}
	var detail models.JobDetail
	loadJSON(t, matches[0], &detail)
	return detail
}

// TestPipeline_RetainsBuildsPastTheFetchWindow drives two real passes over the
// fixture bucket, the second with a narrower window, and pins that the build the
// window drops is still published for the run history strip while the analysis
// window stays exactly as wide as the fetch depth.
func TestPipeline_RetainsBuildsPastTheFetchWindow(t *testing.T) {
	project := writeProject(t, "")
	out := t.TempDir()

	runPipelineInto(t, project, out, 5)
	first := loadOnlyJobDetail(t, out)
	if len(first.Runs) != 2 {
		t.Fatalf("first pass runs = %d, want 2", len(first.Runs))
	}
	if len(first.RetainedRuns) != 0 {
		t.Fatalf("first pass retained = %d, want 0 when the window covers every build", len(first.RetainedRuns))
	}
	aged := first.Runs[1].BuildID

	// A one-build window pushes the older build out of the analysis window.
	runPipelineInto(t, project, out, 1)
	second := loadOnlyJobDetail(t, out)

	if len(second.Runs) != 1 {
		t.Fatalf("second pass runs = %d, want 1 (retention must not widen the analysis window)", len(second.Runs))
	}
	if len(second.RetainedRuns) != 1 {
		t.Fatalf("second pass retained = %d, want 1", len(second.RetainedRuns))
	}
	retained := second.RetainedRuns[0]
	if retained.BuildID != aged {
		t.Errorf("retained build = %s, want the aged-out %s", retained.BuildID, aged)
	}
	if retained.TestCases != nil {
		t.Errorf("retained build kept %d test cases, want none", len(retained.TestCases))
	}
	if retained.Started.IsZero() || retained.Result == "" {
		t.Errorf("retained build lost the metadata the strip plots: %+v", retained.BuildInfo)
	}
	if retained.BuildID == second.Runs[0].BuildID {
		t.Errorf("build %s published in both the window and retention", retained.BuildID)
	}
}

// TestPipeline_RetainedRunsSurviveRepeatedPasses pins that retention is durable:
// a build that aged out stays plottable across later passes rather than being
// dropped the moment it leaves the window.
func TestPipeline_RetainedRunsSurviveRepeatedPasses(t *testing.T) {
	project := writeProject(t, "")
	out := t.TempDir()

	runPipelineInto(t, project, out, 5)
	runPipelineInto(t, project, out, 1)
	runPipelineInto(t, project, out, 1)

	detail := loadOnlyJobDetail(t, out)
	if len(detail.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(detail.Runs))
	}
	if len(detail.RetainedRuns) != 1 {
		t.Fatalf("retained = %d after three passes, want 1", len(detail.RetainedRuns))
	}
	if _, err := os.Stat(filepath.Join(out, "dashboard.json")); err != nil {
		t.Errorf("missing dashboard.json: %v", err)
	}
}
