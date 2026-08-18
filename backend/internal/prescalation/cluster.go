package prescalation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/sharedfailure"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// maxClusterIDLength bounds the identifier a client may send. Published ids are
// a fixed-width hash, so anything longer cannot name a real shared failure.
const maxClusterIDLength = 128

// ClusterRef identifies one failure shared across several open pull requests.
// The published id is the whole reference: it already encodes the correlation
// key, and the resolver validates it against the published index rather than
// trusting a caller-supplied job or test name.
type ClusterRef struct {
	ID string `json:"id"`
}

func (c ClusterRef) normalized() (ClusterRef, error) {
	c.ID = strings.TrimSpace(c.ID)
	if c.ID == "" || len(c.ID) > maxClusterIDLength {
		return ClusterRef{}, ErrInvalid
	}
	return c, nil
}

// identity is the stable key for one shared failure. The published id is a hash
// of the correlation key, so it survives membership changes: the same failure
// keeps one analysis as pull requests come and go.
func (c ClusterRef) identity() string { return c.ID }

// ClusterView is one shared failure escalation's public state.
type ClusterView = View[ClusterRef]

// ClusterResolved is everything one shared failure escalation needs to run.
type ClusterResolved struct {
	Ref     ClusterRef
	Request ai.FailureAnalysisRequest
	Subject sharedfailure.Subject
	// Evidence names the member build the artifacts came from. It travels with
	// the result because the newest usable build changes between passes, so the
	// choice cannot be recomputed from published data later.
	Evidence EscalationEvidence
}

// evidence reports the build this work would analyze, so a finished result can
// be revalidated against it rather than pinned to the shared failure forever.
func (r ClusterResolved) evidence() EscalationEvidence { return r.Evidence }

// ClusterResolver builds analysis inputs from the published shared failure
// index, the published pull request details, and the artifact bucket.
type ClusterResolver struct {
	// DataDir holds the fetcher output, including pull-request-failures.json.
	DataDir string
	// Backend reads build metadata for the evidence build.
	Backend storage.Backend
	// Repo is the "org/repo" whose presubmits produced the build.
	Repo string
	// CacheGeneration must match the analysis service's generation.
	CacheGeneration string
}

// Resolve locates the shared failure, picks the build that will supply its
// evidence, and assembles the analysis request. It refuses a failure whose
// members can already be analyzed from their own pull requests, because that
// path is cheaper and already available.
func (r *ClusterResolver) Resolve(ctx context.Context, ref ClusterRef) (ClusterResolved, error) {
	cluster, err := r.loadCluster(ref.ID)
	if err != nil {
		return ClusterResolved{}, err
	}
	if !cluster.Escalatable {
		return ClusterResolved{}, fmt.Errorf(
			"%w: this failure can be analyzed from an affected pull request", ErrNotEligible)
	}

	evidence, ok := evidenceMember(cluster)
	if !ok {
		// Every member build is still running or tested an older head. Both
		// clear up on their own, so this is transient rather than a verdict.
		return ClusterResolved{}, fmt.Errorf(
			"%w: no affected pull request has a finished build on its current head", ErrUnavailable)
	}
	failure, err := r.loadEvidenceFailure(cluster, evidence)
	if err != nil {
		return ClusterResolved{}, err
	}

	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: r.Repo},
		JobName:     cluster.JobName,
		BuildID:     evidence.BuildID,
		PullNumber:  fmt.Sprint(evidence.Number),
	}
	info, err := prowbuild.FetchBuildInfo(ctx, r.Backend, loc)
	if err != nil {
		return ClusterResolved{}, fmt.Errorf("%w: build metadata unavailable", ErrUnavailable)
	}
	if info.Result == "PENDING" {
		// finished.json was absent or unreadable, so the analysis would
		// describe a build state nobody can vouch for.
		return ClusterResolved{}, fmt.Errorf("%w: the evidence build has no finished metadata", ErrUnavailable)
	}

	numbers := make([]int, 0, len(cluster.PullRequests))
	for _, member := range cluster.PullRequests {
		numbers = append(numbers, member.Number)
	}
	return ClusterResolved{
		Ref:      ref,
		Evidence: r.evidenceOf(evidence),
		Request: ai.FailureAnalysisRequest{
			JobID:           cluster.JobID,
			BuildPrefix:     loc.BuildPath(),
			Build:           *info,
			TestCase:        failure.TestCase,
			CacheGeneration: r.CacheGeneration,
			ProwJob: &ai.ProwJobContext{
				Name: cluster.JobName, JobType: models.JobTypePresubmit,
			},
		},
		Subject: sharedfailure.Subject{
			BaseRef:      cluster.BaseRef,
			JobName:      cluster.JobName,
			TestName:     cluster.TestName,
			PullNumbers:  numbers,
			BuildLevel:   cluster.BuildLevel,
			EvidencePull: evidence.Number,
		},
	}, nil
}

// CurrentEvidence reports the build a new escalation of this shared failure
// would read. It reads published state only, with no bucket call, so a status
// poll can use it to notice that a stored analysis describes a build that has
// since moved on.
func (r *ClusterResolver) CurrentEvidence(ref ClusterRef) (EscalationEvidence, bool) {
	cluster, err := r.loadCluster(ref.ID)
	if err != nil {
		return EscalationEvidence{}, false
	}
	member, ok := evidenceMember(cluster)
	if !ok {
		return EscalationEvidence{}, false
	}
	return r.evidenceOf(member), true
}

// evidenceOf addresses one member build the way the artifact store does, by
// repository, pull request, and build.
func (r *ClusterResolver) evidenceOf(member models.SharedFailureMember) EscalationEvidence {
	return EscalationEvidence{Repo: r.Repo, PullNumber: member.Number, BuildID: member.BuildID}
}

// evidenceMember picks the build that supplies the artifacts: the most recent
// finished build that tested its pull request's current head. Any member would
// show the same failure, so recency is the only useful tiebreak.
func evidenceMember(cluster models.SharedFailure) (models.SharedFailureMember, bool) {
	var best models.SharedFailureMember
	found := false
	for _, member := range cluster.PullRequests {
		if member.Stale || member.Finished.IsZero() || member.BuildID == "" {
			continue
		}
		if !found || member.Started.After(best.Started) {
			best, found = member, true
		}
	}
	return best, found
}

func (r *ClusterResolver) loadCluster(id string) (models.SharedFailure, error) {
	path := filepath.Join(r.DataDir, output.SharedFailureIndexFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return models.SharedFailure{}, fmt.Errorf("%w: no shared failures are published", ErrUnavailable)
	}
	var index models.SharedFailureIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return models.SharedFailure{}, fmt.Errorf("%w: the published shared failures are unreadable", ErrUnavailable)
	}
	for _, cluster := range index.Failures {
		if cluster.ID == id {
			return cluster, nil
		}
	}
	return models.SharedFailure{}, fmt.Errorf("%w: no such shared failure", ErrInvalid)
}

// loadEvidenceFailure recovers the failing case from the evidence pull
// request's published detail, so the analysis carries the same failure message
// and body every other analysis of that build would.
func (r *ClusterResolver) loadEvidenceFailure(cluster models.SharedFailure, evidence models.SharedFailureMember) (models.PullRequestFailure, error) {
	path := filepath.Join(r.DataDir, "pull-requests", models.PullRequestDataFilename(evidence.Number))
	data, err := os.ReadFile(path)
	if err != nil {
		return models.PullRequestFailure{}, fmt.Errorf("%w: the evidence pull request is not published", ErrUnavailable)
	}
	var detail models.PullRequestDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return models.PullRequestFailure{}, fmt.Errorf("%w: the evidence pull request is unreadable", ErrUnavailable)
	}
	for _, check := range detail.Checks {
		if check.JobID != cluster.JobID || check.BuildID != evidence.BuildID {
			continue
		}
		for _, failure := range check.Failures {
			if failure.Name == cluster.TestName {
				return failure, nil
			}
		}
	}
	// The published view moved on between the index and this read.
	return models.PullRequestFailure{}, fmt.Errorf("%w: the evidence build no longer reports this failure", ErrUnavailable)
}

// ClusterAnalysisRunner runs one shared failure escalation through the failure
// analyzer.
type ClusterAnalysisRunner struct {
	// NewAnalyzer builds an analyzer bound to the shared failure module for one
	// subject. A fresh analyzer per run keeps the subject out of shared state.
	NewAnalyzer func(sharedfailure.Subject) (ai.FailureAnalyzer, error)
}

// Run performs the analysis and projects it into a public view.
func (r *ClusterAnalysisRunner) Run(ctx context.Context, resolved ClusterResolved) (ClusterView, error) {
	if r.NewAnalyzer == nil {
		return ClusterView{}, ErrUnavailable
	}
	analyzer, err := r.NewAnalyzer(resolved.Subject)
	if err != nil {
		return ClusterView{}, fmt.Errorf("%w: analyzer unavailable", ErrUnavailable)
	}
	view, err := analysisView[ClusterRef](ctx, analyzer, resolved.Request)
	if err != nil {
		return view, err
	}
	evidence := resolved.Evidence
	view.Evidence = &evidence
	return view, nil
}
