package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// TestRefreshData_TimedOutPassPreservesPublishedJobs pins that a pass which ran
// out of time does not publish. Publication prunes job files absent from the
// refresh, so a timed-out pass that published its partial view would delete the
// jobs it never reached, discarding their cached builds and the pattern analyses
// retention depends on.
func TestRefreshData_TimedOutPassPreservesPublishedJobs(t *testing.T) {
	dataDir := t.TempDir()
	jobsDir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	published := models.JobDetail{
		JobID: "job", Name: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{
			BuildID: "1", Started: time.Unix(1000, 0).UTC(), Passed: true,
			Result: "SUCCESS", JUnitComplete: true,
		}}},
	}
	if err := statefile.WriteJSON(filepath.Join(jobsDir, models.JobDataFilename("job")), published); err != nil {
		t.Fatal(err)
	}

	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline{
		opts:    Options{OutDir: dataDir, BuildsPerJob: 1, Workers: 1},
		cfg:     &project.Config{Branding: project.Branding{Title: "T", BasePath: "/", SiteURL: "https://example.test"}},
		backend: backend,
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = p.refreshDataWithAnalysisContext(expired, expired,
		[]models.ProwJob{{Name: "job", JobID: "job", JobType: models.JobTypePeriodic}})
	if err == nil {
		t.Fatal("a timed-out pass reported success and published its partial view")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a cancellation", err)
	}

	data, readErr := os.ReadFile(filepath.Join(jobsDir, models.JobDataFilename("job")))
	if readErr != nil {
		t.Fatalf("published job detail was deleted by a timed-out pass: %v", readErr)
	}
	var reloaded models.JobDetail
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Runs) != 1 || reloaded.Runs[0].BuildID != "1" {
		t.Errorf("published runs = %+v, want the untouched build 1", reloaded.Runs)
	}
}
