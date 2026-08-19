package prescalation

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/pullrequest"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// Resolved is everything one escalation needs to run.
type Resolved struct {
	Ref     Ref
	Request ai.FailureAnalysisRequest
	Subject pullrequest.Subject
}

// ChangedFileLister supplies the pull request's changed files.
type ChangedFileLister interface {
	ChangedFiles(ctx context.Context, owner, repo string, number int) (ChangedFileSet, error)
}

// ChangedFileSet is the bounded changed-file view the resolver consumes. It
// mirrors the ghpr shape without importing it, keeping this package free of a
// GitHub client dependency.
type ChangedFileSet struct {
	Files     []ChangedFile
	Truncated bool
	// HeadSHA is the pull request head the diff describes. The published data
	// is a snapshot, so this is the only way to notice a force-push that landed
	// after publication.
	HeadSHA string
}

// ChangedFile is one changed file.
type ChangedFile struct {
	Path      string
	Status    string
	Generated bool
	Patch     string
}

// DataResolver builds analysis inputs from the published pull request files and
// the artifact bucket.
type DataResolver struct {
	// DataDir holds the fetcher output, including pull-requests/<n>.json.
	DataDir string
	// Backend reads build metadata for the escalated build.
	Backend storage.Backend
	// Repo is the "org/repo" whose presubmits produced the build.
	Repo string
	// Owner and Name address the same repository for GitHub calls.
	Owner string
	Name  string
	// Lister supplies changed files. Optional: without it the analysis still
	// runs, just without change context.
	Lister ChangedFileLister
	// CacheGeneration must match the analysis service's generation.
	CacheGeneration string
}

// Resolve loads the failing test, rebuilds its build identity, and assembles
// the analysis request. It refuses failures the deterministic pass explained.
func (r *DataResolver) Resolve(ctx context.Context, ref Ref) (Resolved, error) {
	detail, err := r.loadDetail(ref.PullNumber)
	if err != nil {
		return Resolved{}, err
	}
	check, failure, err := findFailure(detail, ref)
	if err != nil {
		return Resolved{}, err
	}
	if !Eligible(failure.Attribution) {
		return Resolved{}, ErrNotEligible
	}
	if check.Stale {
		// The build tested a different head than the pull request now has, so
		// change context would describe the wrong revision.
		return Resolved{}, fmt.Errorf("%w: the failing build tested an older head", ErrNotEligible)
	}

	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: r.Repo},
		JobName:     check.JobName,
		BuildID:     check.BuildID,
		PullNumber:  fmt.Sprint(ref.PullNumber),
	}
	info, err := prowbuild.FetchBuildInfo(ctx, r.Backend, loc)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: build metadata unavailable", ErrUnavailable)
	}
	if info.Result == "PENDING" {
		// finished.json was absent or unreadable, so the build either has not
		// finished or its metadata could not be read within the budget. Both
		// would have the analysis describe a build state nobody can vouch for,
		// and both clear up on their own, so this is transient rather than a
		// verdict about the failure.
		return Resolved{}, fmt.Errorf("%w: the failing build has no finished metadata", ErrUnavailable)
	}

	subject := pullrequest.Subject{
		Number: detail.Number, HeadSHA: detail.HeadSHA, BaseRef: detail.BaseRef,
	}
	if r.Lister != nil {
		if set, err := r.Lister.ChangedFiles(ctx, r.Owner, r.Name, ref.PullNumber); err == nil {
			// A force-push after publication leaves the stored check looking
			// current while the diff describes a different revision. Comparing
			// against the failing build would be comparing two revisions, so
			// the change context is dropped rather than silently mismatched.
			if set.HeadSHA != "" && detail.HeadSHA != "" && !strings.EqualFold(set.HeadSHA, detail.HeadSHA) {
				log.Printf("⏭ pr #%d moved to %s since publication; escalating without change context",
					ref.PullNumber, set.HeadSHA)
			} else {
				subject.FilesTruncated = set.Truncated
				for _, file := range set.Files {
					subject.Files = append(subject.Files, pullrequest.ChangedFile{
						Path: file.Path, Status: file.Status, Generated: file.Generated, Patch: file.Patch,
					})
				}
			}
		}
	}

	return Resolved{
		Ref: ref,
		Request: ai.FailureAnalysisRequest{
			JobID:           check.JobID,
			BuildPrefix:     loc.BuildPath(),
			Build:           *info,
			TestCase:        failure.TestCase,
			CacheGeneration: r.CacheGeneration,
			ProwJob: &ai.ProwJobContext{
				Name: check.JobName, JobType: models.JobTypePresubmit,
			},
		},
		Subject: subject,
	}, nil
}

// Eligible reports whether a failure's deterministic verdict leaves room for
// analysis. Escalation exists only for the residual set.
func Eligible(attribution *models.FailureAttribution) bool {
	return attribution.NeedsInvestigation()
}

func (r *DataResolver) loadDetail(number int) (models.PullRequestDetail, error) {
	path := filepath.Join(r.DataDir, "pull-requests", models.PullRequestDataFilename(number))
	data, err := os.ReadFile(path)
	if err != nil {
		return models.PullRequestDetail{}, fmt.Errorf("%w: pull request is not published", ErrInvalid)
	}
	var detail models.PullRequestDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return models.PullRequestDetail{}, fmt.Errorf("%w: published pull request is unreadable", ErrUnavailable)
	}
	return detail, nil
}

// findFailure locates the exact check and failing case a Ref names.
func findFailure(detail models.PullRequestDetail, ref Ref) (models.PullRequestCheck, models.PullRequestFailure, error) {
	for _, check := range detail.Checks {
		if check.JobID != ref.JobID || check.BuildID != ref.BuildID {
			continue
		}
		for _, failure := range check.Failures {
			if failure.Name == ref.TestName {
				return check, failure, nil
			}
		}
	}
	return models.PullRequestCheck{}, models.PullRequestFailure{},
		fmt.Errorf("%w: no such failing test on this pull request", ErrInvalid)
}

// AnalysisRunner runs one escalation through the failure analyzer.
type AnalysisRunner struct {
	// NewAnalyzer builds an analyzer bound to the pull request module for one
	// subject. A fresh analyzer per run keeps the subject out of shared state.
	NewAnalyzer func(pullrequest.Subject) (ai.FailureAnalyzer, error)
}

// Run performs the analysis and projects it into a public view.
func (r *AnalysisRunner) Run(ctx context.Context, resolved Resolved) (View[Ref], error) {
	if r.NewAnalyzer == nil {
		return View[Ref]{}, ErrUnavailable
	}
	analyzer, err := r.NewAnalyzer(resolved.Subject)
	if err != nil {
		return View[Ref]{}, fmt.Errorf("%w: analyzer unavailable", ErrUnavailable)
	}
	return analysisView[Ref](ctx, analyzer, resolved.Request)
}

// analysisView runs one analysis and projects its result into a public view.
// Both escalation kinds run the ordinary agentic analysis under their own
// module, so only the subject differs and the projection is shared.
func analysisView[R any](ctx context.Context, analyzer ai.FailureAnalyzer, request ai.FailureAnalysisRequest) (View[R], error) {
	result, err := analyzer.AnalyzeFailure(ctx, nil, request)
	if err != nil {
		return View[R]{}, err
	}
	if result.Analysis == nil {
		return View[R]{State: StateFailed, Error: "the analysis produced no result"}, nil
	}
	return View[R]{
		State:        StateComplete,
		RootCause:    strings.TrimSpace(result.Analysis.RootCause),
		Severity:     result.Analysis.Severity,
		SuggestedFix: strings.TrimSpace(result.Analysis.SuggestedFix),
		Citations:    result.Analysis.EvidenceCitations,
	}, nil
}
