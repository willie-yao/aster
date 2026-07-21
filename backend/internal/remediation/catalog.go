package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// CatalogFileName is the private test-to-presubmit cache.
const CatalogFileName = "remediation_prow_catalog.json"

const coverageCatalogTTL = 24 * time.Hour

// CoverageCatalog maps exact JUnit identities to presubmit jobs.
type CoverageCatalog struct {
	Revision  string                       `json:"revision,omitempty"`
	CreatedAt string                       `json:"created_at"`
	Repos     []string                     `json:"repos,omitempty"`
	Complete  bool                         `json:"complete"`
	Errors    []string                     `json:"errors,omitempty"`
	Jobs      map[string]VerificationJob   `json:"jobs"`
	Tests     map[string][]VerificationJob `json:"tests"`
}

// VerificationJob identifies one presubmit that executed a test.
type VerificationJob struct {
	JobID        string   `json:"job_id"`
	JobName      string   `json:"job_name"`
	Repo         string   `json:"repo"`
	RerunCommand string   `json:"rerun_command,omitempty"`
	Branches     []string `json:"branches,omitempty"`
	SkipBranches []string `json:"skip_branches,omitempty"`
	BuildID      string   `json:"build_id"`
}

// LoadCoverageCatalog returns a current cache for revision when available.
func LoadCoverageCatalog(dir, revision string, repos []string, now time.Time) *CoverageCatalog {
	data, err := os.ReadFile(filepath.Join(dir, CatalogFileName))
	if err != nil {
		return nil
	}
	var catalog CoverageCatalog
	if json.Unmarshal(data, &catalog) != nil || catalog.Revision != revision || !catalog.Complete || catalog.Jobs == nil || catalog.Tests == nil {
		return nil
	}
	if !containsRepos(catalog.Repos, repos) {
		return nil
	}
	created, err := time.Parse(time.RFC3339, catalog.CreatedAt)
	if err != nil || now.Sub(created) > coverageCatalogTTL {
		return nil
	}
	return &catalog
}

// Save writes the private coverage cache.
func (c *CoverageCatalog) Save(dir string) error {
	return statefile.WriteJSON(filepath.Join(dir, CatalogFileName), c)
}

// BuildCoverageCatalog reads recent completed presubmits and indexes their tests.
func BuildCoverageCatalog(ctx context.Context, b storage.Backend, catalog *jobconfig.Catalog, repos []string) (*CoverageCatalog, error) {
	repos = normalizedRepos(repos)
	out := &CoverageCatalog{
		CreatedAt: nowString(), Repos: repos, Complete: true,
		Jobs: map[string]VerificationJob{}, Tests: map[string][]VerificationJob{},
	}
	if catalog == nil {
		return out, nil
	}
	out.Revision = catalog.Revision
	repoSet := make(map[string]bool, len(repos))
	for _, repo := range repos {
		repoSet[repo] = true
	}
	var definitions []jobconfig.JobDefinition
	seenJobs := map[string]bool{}
	for _, job := range catalog.Jobs {
		jobID := job.ID()
		if job.JobType == models.JobTypePresubmit && repoSet[job.Repo] && !seenJobs[jobID] {
			definitions = append(definitions, job)
			out.Jobs[jobID] = verificationJobFromDefinition(job)
			seenJobs[jobID] = true
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID() < definitions[j].ID() })

	type result struct {
		tests map[string][]VerificationJob
		err   error
	}
	jobs := make(chan jobconfig.JobDefinition)
	results := make(chan result, len(definitions))
	workers := 6
	if len(definitions) < workers {
		workers = len(definitions)
	}
	for range workers {
		go func() {
			for definition := range jobs {
				tests, err := coverageForJob(ctx, b, definition)
				results <- result{tests: tests, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, definition := range definitions {
			select {
			case jobs <- definition:
			case <-ctx.Done():
				return
			}
		}
	}()

	var errs []error
	for range definitions {
		select {
		case item := <-results:
			if item.err != nil {
				out.Complete = false
				out.Errors = append(out.Errors, item.err.Error())
				errs = append(errs, item.err)
			}
			for identity, verificationJobs := range item.tests {
				for _, verification := range verificationJobs {
					out.Tests[identity] = appendVerificationJob(out.Tests[identity], verification)
				}
			}
		case <-ctx.Done():
			out.Complete = false
			out.Errors = append(out.Errors, ctx.Err().Error())
			errs = append(errs, ctx.Err())
			return out, errors.Join(errs...)
		}
	}
	sort.Strings(out.Errors)
	for identity := range out.Tests {
		sort.Slice(out.Tests[identity], func(i, j int) bool {
			return out.Tests[identity][i].JobID < out.Tests[identity][j].JobID
		})
	}
	return out, errors.Join(errs...)
}

func verificationJobFromDefinition(definition jobconfig.JobDefinition) VerificationJob {
	return VerificationJob{
		JobID: definition.ID(), JobName: definition.Name, Repo: definition.Repo,
		RerunCommand: definition.EffectiveRerunCommand(),
		Branches:     append([]string(nil), definition.Branches...),
		SkipBranches: append([]string(nil), definition.SkipBranches...),
	}
}

func coverageForJob(ctx context.Context, b storage.Backend, definition jobconfig.JobDefinition) (map[string][]VerificationJob, error) {
	jobID := definition.ID()
	job := models.ProwJob{Name: definition.Name, JobType: models.JobTypePresubmit, Repo: definition.Repo, JobID: jobID}
	builds, err := prowbuild.ListRecentBuilds(ctx, b, &job, 5)
	if err != nil {
		return nil, fmt.Errorf("coverage list %s: %w", jobID, err)
	}
	var fallback map[string][]VerificationJob
	var scanErrs []error
	for _, build := range builds {
		loc := prowbuild.BuildLocation{
			JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: definition.Repo},
			JobName:     definition.Name, BuildID: build.ID, PullNumber: build.PullNumber,
		}
		info, err := prowbuild.FetchBuildInfo(ctx, b, loc)
		if err != nil || info.Result == "PENDING" {
			continue
		}
		paths, complete, err := prowbuild.DiscoverJUnitPathsWithCompleteness(ctx, b, loc)
		if err != nil {
			scanErrs = append(scanErrs, fmt.Errorf("discover JUnit for %s/%s: %w", jobID, build.ID, err))
			continue
		}
		if !complete {
			scanErrs = append(scanErrs, fmt.Errorf("JUnit artifact listing for %s/%s was incomplete", jobID, build.ID))
		}
		if len(paths) == 0 {
			continue
		}
		tests := map[string][]VerificationJob{}
		for _, path := range paths {
			data, err := storage.ReadAll(ctx, b, path)
			if err != nil {
				scanErrs = append(scanErrs, fmt.Errorf("read JUnit %s: %w", path, err))
				continue
			}
			cases, err := junit.ParseFile(data, filepath.Base(path))
			if err != nil {
				scanErrs = append(scanErrs, fmt.Errorf("parse JUnit %s: %w", path, err))
				continue
			}
			for _, test := range cases {
				if test.Status == "skipped" {
					continue
				}
				verification := verificationJobFromDefinition(definition)
				verification.BuildID = build.ID
				for _, identity := range junit.Identities(test) {
					tests[identity] = appendVerificationJob(tests[identity], verification)
				}
			}
		}
		if len(tests) > 0 {
			if info.Passed {
				return tests, errors.Join(scanErrs...)
			}
			if fallback == nil {
				fallback = tests
			}
		}
	}
	return fallback, errors.Join(scanErrs...)
}

func appendVerificationJob(values []VerificationJob, value VerificationJob) []VerificationJob {
	for _, existing := range values {
		if existing.JobID == value.JobID {
			return values
		}
	}
	return append(values, value)
}

func normalizedRepos(repos []string) []string {
	set := map[string]bool{}
	for _, repo := range repos {
		if repo != "" {
			set[repo] = true
		}
	}
	out := make([]string, 0, len(set))
	for repo := range set {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

func containsRepos(have, want []string) bool {
	set := map[string]bool{}
	for _, repo := range have {
		set[repo] = true
	}
	for _, repo := range want {
		if !set[repo] {
			return false
		}
	}
	return true
}
