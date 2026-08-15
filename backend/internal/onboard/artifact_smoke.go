package onboard

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// ArtifactSmokeReport records read-only artifact availability for selected jobs.
type ArtifactSmokeReport struct {
	ReadOnly     bool               `json:"read_only"`
	BuildsPerJob int                `json:"builds_per_job"`
	Jobs         []ArtifactJobSmoke `json:"jobs"`
	Warnings     []string           `json:"warnings,omitempty"`
}

// ArtifactJobSmoke summarizes recent artifact availability for one selected job.
type ArtifactJobSmoke struct {
	JobID        string               `json:"job_id"`
	Name         string               `json:"name"`
	JobType      string               `json:"job_type"`
	Repository   string               `json:"repository,omitempty"`
	RecentBuilds int                  `json:"recent_builds"`
	Builds       []ArtifactBuildSmoke `json:"builds,omitempty"`
	Warning      string               `json:"warning,omitempty"`
	Availability ArtifactAvailability `json:"availability"`
}

// ArtifactAvailability counts sampled builds with each expected artifact.
type ArtifactAvailability struct {
	ProwJobJSON ArtifactCount `json:"prowjob_json"`
	StartedJSON ArtifactCount `json:"started_json"`
	BuildLog    ArtifactCount `json:"build_log_txt"`
	JUnit       ArtifactCount `json:"junit"`
	Artifacts   ArtifactCount `json:"artifacts_directory"`
}

// ArtifactCount records how many sampled builds exposed one artifact class.
type ArtifactCount struct {
	Checked   int `json:"checked"`
	Available int `json:"available"`
}

// ArtifactBuildSmoke records availability for one recent build without diagnosing it.
type ArtifactBuildSmoke struct {
	BuildID     string `json:"build_id"`
	PullNumber  string `json:"pull_number,omitempty"`
	ProwJobJSON bool   `json:"prowjob_json"`
	StartedJSON bool   `json:"started_json"`
	BuildLog    bool   `json:"build_log_txt"`
	JUnit       bool   `json:"junit"`
	Artifacts   bool   `json:"artifacts_directory"`
}

func runArtifactSmoke(ctx context.Context, plan *Plan, buildsPerJob int) ArtifactSmokeReport {
	report := ArtifactSmokeReport{ReadOnly: true, BuildsPerJob: buildsPerJob, Jobs: []ArtifactJobSmoke{}}
	if plan == nil || buildsPerJob <= 0 {
		return report
	}
	backend, err := storage.New(plan.Project.StorageConfig(), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		warning := fmt.Sprintf("Artifact smoke check could not initialize storage: %v", err)
		report.Warnings = append(report.Warnings, warning)
		for _, job := range plan.Discovery.Jobs {
			report.Jobs = append(report.Jobs, ArtifactJobSmoke{
				JobID: job.JobID, Name: job.Name, JobType: job.JobType, Repository: job.Repo, Warning: warning,
			})
		}
		return report
	}
	totalJUnitChecked := 0
	totalJUnitAvailable := 0
	for i := range plan.Discovery.Jobs {
		report.Jobs = append(report.Jobs, smokeJobArtifacts(ctx, backend, &plan.Discovery.Jobs[i], buildsPerJob))
		job := report.Jobs[len(report.Jobs)-1]
		totalJUnitChecked += job.Availability.JUnit.Checked
		totalJUnitAvailable += job.Availability.JUnit.Available
		if job.Warning != "" {
			report.Warnings = append(report.Warnings, job.Warning)
		}
	}
	if totalJUnitChecked > 0 && totalJUnitAvailable == 0 {
		report.Warnings = append(report.Warnings, "Artifact smoke check found no JUnit XML in any sampled build across the selected jobs. Diagnostic authoring must account for synthesized build-level failures and unavailable test-level granularity.")
	}
	return report
}

func smokeJobArtifacts(ctx context.Context, backend storage.Backend, job *models.ProwJob, buildsPerJob int) ArtifactJobSmoke {
	result := ArtifactJobSmoke{JobID: job.JobID, Name: job.Name, JobType: job.JobType, Repository: job.Repo}
	builds, err := prowbuild.ListRecentBuilds(ctx, backend, job, buildsPerJob)
	if err != nil {
		result.Warning = fmt.Sprintf("Artifact smoke check could not list recent builds for %s: %v", job.Name, err)
		return result
	}
	result.RecentBuilds = len(builds)
	if len(builds) == 0 {
		result.Warning = fmt.Sprintf("Artifact smoke check found no recent builds for %s.", job.Name)
		return result
	}
	for _, build := range builds {
		loc := prowbuild.BuildLocation{
			JobLocation: prowbuild.JobLocation{JobType: job.JobType, Repo: job.Repo},
			JobName:     job.Name, BuildID: build.ID, PullNumber: build.PullNumber,
		}
		checked := ArtifactBuildSmoke{BuildID: build.ID, PullNumber: build.PullNumber}
		_, err := prowbuild.FetchProwJobMetadata(ctx, backend, loc)
		checked.ProwJobJSON = err == nil
		checked.StartedJSON = artifactObjectAvailable(ctx, backend, loc.BuildPath()+"started.json")
		checked.BuildLog = artifactObjectAvailable(ctx, backend, loc.BuildPath()+"build-log.txt")
		listing, listErr := backend.List(ctx, loc.BuildPath()+"artifacts/")
		checked.Artifacts = listErr == nil && listing != nil && (len(listing.Files) > 0 || len(listing.Dirs) > 0)
		junit, _, _, junitErr := prowbuild.DiscoverJUnitPathsWithStatus(ctx, backend, loc)
		checked.JUnit = junitErr == nil && len(junit) > 0
		result.Builds = append(result.Builds, checked)
		countArtifactAvailability(&result.Availability, checked)
	}
	if result.Availability.JUnit.Checked > 0 && result.Availability.JUnit.Available == 0 {
		result.Warning = fmt.Sprintf("Artifact smoke check found no JUnit XML in %d sampled build(s) for %s; test-level granularity may be unavailable.", result.Availability.JUnit.Checked, job.Name)
	}
	return result
}

func artifactObjectAvailable(ctx context.Context, backend storage.Backend, path string) bool {
	reader, _, err := backend.Open(ctx, path)
	if err != nil {
		return false
	}
	_ = reader.Close()
	return true
}

func countArtifactAvailability(counts *ArtifactAvailability, build ArtifactBuildSmoke) {
	pairs := []struct {
		count     *ArtifactCount
		available bool
	}{
		{&counts.ProwJobJSON, build.ProwJobJSON},
		{&counts.StartedJSON, build.StartedJSON},
		{&counts.BuildLog, build.BuildLog},
		{&counts.JUnit, build.JUnit},
		{&counts.Artifacts, build.Artifacts},
	}
	for _, pair := range pairs {
		pair.count.Checked++
		if pair.available {
			pair.count.Available++
		}
	}
}
