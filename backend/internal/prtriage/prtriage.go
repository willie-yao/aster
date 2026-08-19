// Package prtriage builds the pull-request view of presubmit results: which
// presubmits ran on each open pull request, and which tests failed. It reads
// GitHub for pull request identity and the artifact bucket for build outcomes,
// and performs no analysis of its own.
package prtriage

import (
	"context"
	"fmt"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/junit"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/storage"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	// DefaultMaxPullRequests bounds one pass so an unusually busy repository
	// cannot stall a refresh.
	DefaultMaxPullRequests = 100
	// DefaultBuildsPerJob is how many builds are listed per presubmit before the
	// newest is selected.
	DefaultBuildsPerJob = 3
	// DefaultWorkers bounds concurrent per-check bucket reads.
	DefaultWorkers = 8
	// maxFailureBodyBytes bounds a stored failure body so a pathological log
	// dump cannot inflate a pull request detail file.
	maxFailureBodyBytes = 8 * 1024
)

// PullRequestLister enumerates the open pull requests to triage.
type PullRequestLister interface {
	// ListOpenPullRequests returns the open pull requests to triage and whether
	// the listing was cut short, so callers can tell a complete view from a
	// capped one.
	ListOpenPullRequests(ctx context.Context, owner, repo string, limit int) ([]ghpr.PullRequest, bool, error)
}

// Options bound one triage pass.
type Options struct {
	// Owner and Repo name the repository whose pull requests are triaged.
	Owner string
	Repo  string
	// MaxPullRequests, BuildsPerJob, and Workers default when non-positive.
	MaxPullRequests int
	BuildsPerJob    int
	Workers         int
}

func (o Options) withDefaults() Options {
	if o.MaxPullRequests <= 0 {
		o.MaxPullRequests = DefaultMaxPullRequests
	}
	if o.BuildsPerJob <= 0 {
		o.BuildsPerJob = DefaultBuildsPerJob
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	return o
}

// fullRepo returns the "org/repo" the pass triages.
func (o Options) fullRepo() string { return o.Owner + "/" + o.Repo }

// Collect returns the pull request index and one detail per open pull request.
// Per-check bucket errors are logged and skipped so one unreadable build does
// not fail the pass.
// Result is one triage pass's output.
type Result struct {
	Index   models.PullRequestIndex
	Details []models.PullRequestDetail
	// Truncated reports that more open pull requests exist than were triaged,
	// so the published pages do not cover every open pull request.
	Truncated bool
}

func Collect(ctx context.Context, backend storage.Backend, lister PullRequestLister, catalog *jobconfig.Catalog, opts Options) (Result, error) {
	opts = opts.withDefaults()
	if opts.Owner == "" || opts.Repo == "" {
		return Result{}, fmt.Errorf("prtriage: owner and repo are required")
	}
	if backend == nil {
		return Result{}, fmt.Errorf("prtriage: storage backend is required")
	}
	if lister == nil {
		return Result{}, fmt.Errorf("prtriage: pull request lister is required")
	}

	pulls, truncated, err := lister.ListOpenPullRequests(ctx, opts.Owner, opts.Repo, opts.MaxPullRequests)
	if err != nil {
		return Result{}, fmt.Errorf("prtriage: %w", err)
	}
	presubmits := presubmitsForRepo(catalog, opts.fullRepo())
	log.Printf("🔀 Triaging %d open pull requests against %d presubmits in %s",
		len(pulls), len(presubmits), opts.fullRepo())

	checks := collectChecks(ctx, backend, pulls, presubmits, opts)

	details := make([]models.PullRequestDetail, 0, len(pulls))
	summaries := make([]models.PullRequestSummary, 0, len(pulls))
	for i, pull := range pulls {
		detail := buildDetail(pull, opts.fullRepo(), checks[i])
		details = append(details, detail)
		summaries = append(summaries, detail.PullRequestSummary)
	}
	return Result{
		Index:     models.PullRequestIndex{Repo: opts.fullRepo(), PullRequests: summaries},
		Details:   details,
		Truncated: truncated,
	}, nil
}

// presubmitsForRepo returns the catalog's presubmits for repo, ordered by name
// so output stays stable across passes.
func presubmitsForRepo(catalog *jobconfig.Catalog, repo string) []jobconfig.JobDefinition {
	if catalog == nil {
		return nil
	}
	var out []jobconfig.JobDefinition
	for _, job := range catalog.Jobs {
		if job.JobType != models.JobTypePresubmit || !strings.EqualFold(job.Repo, repo) {
			continue
		}
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkTask is one (pull request, presubmit) pair to observe.
type checkTask struct {
	pullIndex int
	job       jobconfig.JobDefinition
}

// collectChecks observes every applicable (pull request, presubmit) pair with a
// bounded worker pool. The result is indexed by position in pulls.
func collectChecks(ctx context.Context, backend storage.Backend, pulls []ghpr.PullRequest, presubmits []jobconfig.JobDefinition, opts Options) [][]models.PullRequestCheck {
	out := make([][]models.PullRequestCheck, len(pulls))
	var tasks []checkTask
	for i, pull := range pulls {
		for _, job := range presubmits {
			applies, err := job.AppliesToBranch(pull.Base.Ref)
			if err != nil {
				log.Printf("    ⚠ pr #%d: %v", pull.Number, err)
				continue
			}
			if applies {
				tasks = append(tasks, checkTask{pullIndex: i, job: job})
			}
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Workers)
	for _, task := range tasks {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(task checkTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pull := pulls[task.pullIndex]
			check, err := newestCheck(ctx, backend, pull, task.job, opts)
			if err != nil {
				log.Printf("    ⚠ pr #%d %s: %v", pull.Number, task.job.Name, err)
				return
			}
			if check == nil {
				return
			}
			mu.Lock()
			out[task.pullIndex] = append(out[task.pullIndex], *check)
			mu.Unlock()
		}(task)
	}
	wg.Wait()
	return out
}

// newestCheck observes the newest build of one presubmit on one pull request.
// A nil check means the presubmit has not run on that pull request.
func newestCheck(ctx context.Context, backend storage.Backend, pull ghpr.PullRequest, job jobconfig.JobDefinition, opts Options) (*models.PullRequestCheck, error) {
	pullNumber := strconv.Itoa(pull.Number)
	builds, err := prowbuild.ListPullBuilds(ctx, backend, job.Repo, pullNumber, job.Name, opts.BuildsPerJob)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	if len(builds) == 0 {
		return nil, nil
	}
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: job.Repo},
		JobName:     job.Name,
		BuildID:     builds[0].ID,
		PullNumber:  pullNumber,
	}
	info, err := prowbuild.FetchBuildInfo(ctx, backend, loc)
	if err != nil {
		return nil, fmt.Errorf("fetching build info: %w", err)
	}

	check := models.PullRequestCheck{
		JobName:     job.Name,
		JobID:       job.ID(),
		Optional:    job.Optional,
		BuildID:     info.BuildID,
		Passed:      info.Passed,
		Result:      info.Result,
		Started:     info.Started,
		Finished:    info.Finished,
		WebURL:      info.WebURL,
		BuildLogURL: info.BuildLogURL,
	}
	if tested, ok := prowbuild.PullHeadRevision(info.RepoRefs, job.Repo, pullNumber); ok {
		check.TestedSHA = tested
		check.Stale = pull.Head.SHA != "" && !strings.EqualFold(tested, pull.Head.SHA)
	}
	if check.Passed || running(check) {
		return &check, nil
	}
	failures, err := failingCases(ctx, backend, loc)
	if err != nil {
		// A build whose JUnit is unreadable is still a reportable failure.
		log.Printf("    ⚠ pr #%d %s/%s: %v", pull.Number, job.Name, info.BuildID, err)
		return &check, nil
	}
	// A job can fail without any failing JUnit case, for example a build or
	// verify step. Stand in a build-level failure so the check still names a
	// subject, matching how the job pipeline reports the same situation.
	if failures.total == 0 && failures.complete && !failures.truncated {
		failures.cases = []models.PullRequestFailure{{TestCase: models.NewProwJobExecutionFailure(info.DurationSeconds)}}
		failures.total = 1
	}
	check.TestsFailed = failures.total
	check.Failures = failures.cases
	check.FailuresTruncated = failures.total > len(failures.cases)
	return &check, nil
}

// buildFailures is one build's failing cases plus the discovery status needed
// to tell "no failures" apart from "failures could not be read".
type buildFailures struct {
	cases     []models.PullRequestFailure
	total     int
	complete  bool
	truncated bool
}

// failingCases returns the build's failing cases capped for storage, alongside
// the true failing count before the cap.
func failingCases(ctx context.Context, backend storage.Backend, loc prowbuild.BuildLocation) (buildFailures, error) {
	paths, complete, truncated, err := prowbuild.DiscoverJUnitPathsWithStatus(ctx, backend, loc)
	if err != nil {
		return buildFailures{}, fmt.Errorf("discovering junit files: %w", err)
	}
	out := buildFailures{complete: complete, truncated: truncated}
	for _, junitPath := range paths {
		data, err := storage.ReadAll(ctx, backend, junitPath)
		if err != nil {
			out.complete = false
			continue
		}
		cases, err := junit.ParseFile(data, path.Base(junitPath))
		if err != nil {
			out.complete = false
			continue
		}
		for _, tc := range cases {
			if tc.Status != "failed" {
				continue
			}
			out.total++
			if len(out.cases) >= models.PullRequestCheckFailureCap {
				continue
			}
			tc.FailureBody = textutil.Truncate(tc.FailureBody, maxFailureBodyBytes)
			out.cases = append(out.cases, models.PullRequestFailure{TestCase: tc})
		}
	}
	return out, nil
}

// running reports whether the build has not finished yet.
func running(check models.PullRequestCheck) bool { return check.Finished.IsZero() }

// buildDetail assembles one pull request's detail file and its derived summary.
func buildDetail(pull ghpr.PullRequest, repo string, checks []models.PullRequestCheck) models.PullRequestDetail {
	sortChecks(checks)
	summary := models.PullRequestSummary{
		Number:    pull.Number,
		Title:     pull.Title,
		Author:    pull.Author,
		Repo:      repo,
		BaseRef:   pull.Base.Ref,
		HeadSHA:   pull.Head.SHA,
		HTMLURL:   pull.HTMLURL,
		CreatedAt: pull.CreatedAt,
		UpdatedAt: pull.UpdatedAt,
		CIState:   models.PullRequestCIUnknown,
	}
	pending := false
	for _, check := range checks {
		summary.ChecksObserved++
		summary.FailingTests += check.TestsFailed
		switch {
		case running(check):
			pending = true
		case !check.Passed:
			summary.ChecksFailing++
		}
	}
	switch {
	case summary.ChecksFailing > 0:
		summary.CIState = models.PullRequestCIFailing
	case pending:
		summary.CIState = models.PullRequestCIPending
	case summary.ChecksObserved > 0:
		summary.CIState = models.PullRequestCIPassing
	}
	if checks == nil {
		checks = []models.PullRequestCheck{}
	}
	return models.PullRequestDetail{PullRequestSummary: summary, Checks: checks}
}

// sortChecks orders failing checks first, then still-running, then passing,
// each group by job name so output is stable.
func sortChecks(checks []models.PullRequestCheck) {
	rank := func(check models.PullRequestCheck) int {
		switch {
		case running(check):
			return 1
		case !check.Passed:
			return 0
		default:
			return 2
		}
	}
	sort.Slice(checks, func(i, j int) bool {
		if ri, rj := rank(checks[i]), rank(checks[j]); ri != rj {
			return ri < rj
		}
		return checks[i].JobName < checks[j].JobName
	})
}
