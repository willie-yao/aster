package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type failedListingsBackend struct {
	storage.Backend
	jobs map[string]bool
}

func (b failedListingsBackend) List(ctx context.Context, prefix string) (*storage.Listing, error) {
	for job := range b.jobs {
		if prefix == "logs/"+job+"/" {
			return nil, errors.New("listing unavailable: private-token")
		}
	}
	return b.Backend.List(ctx, prefix)
}

func TestRefreshDataPublishesJobFetchFreshness(t *testing.T) {
	cases := []struct {
		name     string
		failures map[string]bool
		noPrior  bool
		failed   int
		missing  int
	}{
		{"mixed", map[string]bool{"outage": true, "new": true}, false, 2, 1},
		{"all failed with prior data", map[string]bool{"outage": true, "healthy": true, "new": true}, false, 3, 1},
		{"all failed without prior data", map[string]bool{"outage": true, "healthy": true, "new": true}, true, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, bucketDir, prior := listingFailureFixture(t)
			backend := p.backend.(failingJobListBackend).Backend
			p.backend = failedListingsBackend{Backend: backend, jobs: tc.failures}
			writeFixtureFile(t, bucketDir, "logs/new/1/started.json", `{"timestamp":1}`)
			lastSuccess := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
			prior.Fetch = &models.JobFetchStatus{State: models.JobFetchFresh, CheckedAt: lastSuccess, LastSuccessAt: &lastSuccess}
			if err := statefile.WriteJSON(filepath.Join(p.opts.OutDir, "jobs", models.JobDataFilename("outage")), prior); err != nil {
				t.Fatal(err)
			}
			if tc.noPrior {
				for _, name := range []string{"outage", "healthy"} {
					if err := os.Remove(filepath.Join(p.opts.OutDir, "jobs", models.JobDataFilename(name))); err != nil {
						t.Fatal(err)
					}
				}
			}
			p.progress = fetchprogress.New(p.opts.OutDir, "sha-test")
			jobs, err := p.discover(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				p.progress.StartPass(fetchprogress.PassOneShot)
				started := time.Now().UTC()
				res, err := p.refreshWithAnalysisContext(t.Context(), t.Context(), jobs)
				if err != nil {
					t.Fatal(err)
				}
				finishProgressPass(p.progress, nil, false)
				completed := time.Now().UTC()
				want := &models.JobFetchSummary{Total: 3, Failed: tc.failed, Missing: tc.missing}
				var dashboard models.Dashboard
				readFreshnessJSON(t, filepath.Join(p.opts.OutDir, "dashboard.json"), &dashboard)
				var report models.FlakinessReport
				readFreshnessJSON(t, filepath.Join(p.opts.OutDir, "flakiness.json"), &report)
				if !reflect.DeepEqual(dashboard.Fetch, want) || !reflect.DeepEqual(report.Fetch, want) || !reflect.DeepEqual(res.flakiness.Fetch, want) {
					t.Fatalf("fetch summary dashboard=%+v report=%+v result=%+v, want %+v", dashboard.Fetch, report.Fetch, res.flakiness.Fetch, want)
				}
				if len(dashboard.Jobs) != 3-tc.missing || dashboard.GeneratedAt.Before(started) || dashboard.GeneratedAt.After(completed) {
					t.Fatalf("published dashboard = %+v", dashboard)
				}
				details, err := loadPublishedJobDetails(p.opts.OutDir)
				if err != nil {
					t.Fatal(err)
				}
				for _, job := range dashboard.Jobs {
					detail := details[job.JobID]
					status := job.Fetch
					if status == nil || !reflect.DeepEqual(status, detail.Fetch) || status.CheckedAt.Before(started) || status.CheckedAt.After(completed) {
						t.Fatalf("job fetch summary=%+v detail=%+v", status, detail.Fetch)
					}
					if tc.failures[job.JobID] {
						if status.State != models.JobFetchCarriedForward {
							t.Fatalf("failed job fetch = %+v", status)
						}
						if job.JobID == "outage" {
							if status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(lastSuccess) || job.CurrentStatus != models.JobCurrentFailing || len(detail.Runs) != len(prior.Runs) {
								t.Fatalf("carried job changed observations: job=%+v detail=%+v", job, detail)
							}
						} else if status.LastSuccessAt != nil {
							t.Fatalf("invented prior success: %+v", status)
						}
					} else if status.State != models.JobFetchFresh || status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(status.CheckedAt) {
						t.Fatalf("fresh job fetch = %+v", status)
					}
				}
				status, err := fetchprogress.Read(fetchprogress.Path(p.opts.OutDir))
				if err != nil {
					t.Fatal(err)
				}
				if status.Outcome != fetchprogress.OutcomeDegraded || status.Phase != fetchprogress.PhaseComplete || status.Jobs != (fetchprogress.JobProgress{Total: 3, Completed: 3, Failed: tc.failed}) || status.LastSuccessfulPublicationAt == nil {
					t.Fatalf("degraded status = %+v", status)
				}
				for _, name := range []string{"dashboard.json", "flakiness.json"} {
					data, err := os.ReadFile(filepath.Join(p.opts.OutDir, name))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "listing unavailable") || strings.Contains(string(data), "private-token") {
						t.Fatalf("public metadata leaked raw error: %s", data)
					}
				}
			}
			p.backend = backend
			p.progress.StartPass(fetchprogress.PassOneShot)
			res, err := p.refreshWithAnalysisContext(t.Context(), t.Context(), jobs)
			if err != nil {
				t.Fatal(err)
			}
			finishProgressPass(p.progress, nil, false)
			if got := p.progress.Snapshot(); got.Outcome != fetchprogress.OutcomeSucceeded || got.Jobs.Failed != 0 {
				t.Fatalf("recovered status = %+v", got)
			}
			if !reflect.DeepEqual(res.flakiness.Fetch, &models.JobFetchSummary{Total: 3}) {
				t.Fatalf("recovered summary = %+v", res.flakiness.Fetch)
			}
			for _, detail := range res.details {
				if detail.Fetch == nil || detail.Fetch.State != models.JobFetchFresh || detail.Fetch.LastSuccessAt == nil || !detail.Fetch.LastSuccessAt.Equal(detail.Fetch.CheckedAt) {
					t.Fatalf("recovered detail = %+v", detail.Fetch)
				}
			}
		})
	}
}

func readFreshnessJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
