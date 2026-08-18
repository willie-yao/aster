package prescalation

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/sharedfailure"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prattribution"
	"github.com/willie-yao/aster/backend/internal/storage"
)

const (
	clusterJobName = "pull-e2e"
	clusterJobID   = "org/repo/pull-e2e"
	clusterTest    = "[It] creates a cluster"
)

var clusterStart = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// clusterFixture lays out the published shared failure index, the affected
// pull request details, and the build artifacts one escalation resolves
// against.
type clusterFixture struct {
	root    string
	dataDir string
	cluster models.SharedFailure
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	root := t.TempDir()
	f := &clusterFixture{root: root, dataDir: filepath.Join(root, "data")}
	f.cluster = models.SharedFailure{
		ID:          models.SharedFailureID("main", clusterJobName, clusterTest),
		BaseRef:     "main",
		JobName:     clusterJobName,
		JobID:       clusterJobID,
		TestName:    clusterTest,
		Escalatable: true,
		PullRequests: []models.SharedFailureMember{
			f.member(6209, "100", clusterStart),
			f.member(6210, "200", clusterStart.Add(time.Hour)),
		},
	}
	return f
}

func (f *clusterFixture) member(number int, buildID string, started time.Time) models.SharedFailureMember {
	return models.SharedFailureMember{
		Number: number, BuildID: buildID,
		Started: started, Finished: started.Add(time.Minute),
		Verdict: models.AttributionWidespread,
	}
}

// write lays the fixture down on disk: the index, one detail per member, and a
// readable finished build for every member that has one.
func (f *clusterFixture) write(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.dataDir, "pull-requests"), 0o755); err != nil {
		t.Fatalf("data dir: %v", err)
	}
	writeJSON(t, filepath.Join(f.dataDir, output.SharedFailureIndexFilename),
		models.SharedFailureIndex{Repo: "org/repo", Failures: []models.SharedFailure{f.cluster}})

	for _, member := range f.cluster.PullRequests {
		detail := models.PullRequestDetail{
			PullRequestSummary: models.PullRequestSummary{
				Number: member.Number, HeadSHA: "abc123", BaseRef: "main",
			},
			Checks: []models.PullRequestCheck{{
				JobID: clusterJobID, JobName: clusterJobName, BuildID: member.BuildID,
				Failures: []models.PullRequestFailure{{
					TestCase: models.TestCase{Name: clusterTest, FailureMessage: "boom"},
				}},
			}},
		}
		writeJSON(t, filepath.Join(f.dataDir, "pull-requests",
			models.PullRequestDataFilename(member.Number)), detail)
		f.writeBuild(t, member)
	}
}

func (f *clusterFixture) writeBuild(t *testing.T, member models.SharedFailureMember) {
	t.Helper()
	dir := filepath.Join(f.root, "bucket", "pr-logs", "pull", "org_repo",
		strconv.Itoa(member.Number), clusterJobName, member.BuildID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("build dir: %v", err)
	}
	writeJSON(t, filepath.Join(dir, "started.json"), map[string]any{
		"timestamp": 1700000000, "repo-commit": "abc123",
	})
	writeJSON(t, filepath.Join(dir, "finished.json"), map[string]any{
		"timestamp": 1700000900, "passed": false, "result": "FAILURE", "revision": "abc123",
	})
}

func (f *clusterFixture) resolver(t *testing.T) *ClusterResolver {
	t.Helper()
	backend, err := storage.NewLocalBackend(filepath.Join(f.root, "bucket"), "")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return &ClusterResolver{DataDir: f.dataDir, Backend: backend, Repo: "org/repo"}
}

func (f *clusterFixture) ref() ClusterRef { return ClusterRef{ID: f.cluster.ID} }

func TestClusterResolveBuildsTheSharedSubject(t *testing.T) {
	f := newClusterFixture(t)
	f.write(t)

	resolved, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Subject.BaseRef != "main" || resolved.Subject.JobName != clusterJobName {
		t.Errorf("subject = %+v", resolved.Subject)
	}
	if len(resolved.Subject.PullNumbers) != 2 {
		t.Errorf("pull numbers = %v, want every affected pull request", resolved.Subject.PullNumbers)
	}
	// The newest finished build supplies the artifacts.
	if resolved.Subject.EvidencePull != 6210 {
		t.Errorf("evidence pull = %d, want the most recent build", resolved.Subject.EvidencePull)
	}
	if resolved.Request.JobID != clusterJobID || resolved.Request.Build.Result != "FAILURE" {
		t.Errorf("request = %+v", resolved.Request)
	}
	// The failing case is recovered from the published detail, so the analysis
	// sees the same failure text every other analysis of that build would.
	if resolved.Request.TestCase.FailureMessage != "boom" {
		t.Errorf("test case = %+v, want the published failure text", resolved.Request.TestCase)
	}
}

func TestClusterResolveRefusesWhenAMemberCanEscalateAlone(t *testing.T) {
	f := newClusterFixture(t)
	f.cluster.Escalatable = false
	f.write(t)

	_, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if !errors.Is(err, ErrNotEligible) {
		t.Fatalf("err = %v, want ErrNotEligible", err)
	}
}

func TestClusterResolveRejectsAnUnknownFailure(t *testing.T) {
	f := newClusterFixture(t)
	f.write(t)

	_, err := f.resolver(t).Resolve(context.Background(), ClusterRef{ID: "0000000000000000"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestClusterResolveReportsAMissingIndexAsUnavailable(t *testing.T) {
	f := newClusterFixture(t)
	if err := os.MkdirAll(f.dataDir, 0o755); err != nil {
		t.Fatalf("data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "bucket"), 0o755); err != nil {
		t.Fatalf("bucket dir: %v", err)
	}

	_, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// A stale build tested an older head, and an unfinished one has no outcome to
// read. Neither can serve as evidence, and both clear up on their own.
func TestClusterResolveRefusesWhenNoBuildCanServeAsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*models.SharedFailureMember)
	}{
		{"stale", func(m *models.SharedFailureMember) { m.Stale = true }},
		{"unfinished", func(m *models.SharedFailureMember) { m.Finished = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture(t)
			for i := range f.cluster.PullRequests {
				tc.spoil(&f.cluster.PullRequests[i])
			}
			f.write(t)

			_, err := f.resolver(t).Resolve(context.Background(), f.ref())
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
		})
	}
}

// A usable build must still be chosen when a more recent member is unusable,
// so one stale pull request does not take the whole failure out of reach.
func TestClusterResolveSkipsUnusableMembers(t *testing.T) {
	f := newClusterFixture(t)
	f.cluster.PullRequests[1].Stale = true
	f.write(t)

	resolved, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Subject.EvidencePull != 6209 {
		t.Errorf("evidence pull = %d, want the usable older build", resolved.Subject.EvidencePull)
	}
}

func TestClusterResolveReportsAMissingEvidenceDetailAsUnavailable(t *testing.T) {
	f := newClusterFixture(t)
	f.write(t)
	if err := os.Remove(filepath.Join(f.dataDir, "pull-requests",
		models.PullRequestDataFilename(6210))); err != nil {
		t.Fatalf("remove: %v", err)
	}

	_, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// The index and the details are written together but read separately, so a
// build that no longer reports the failure must not resolve to a wrong case.
func TestClusterResolveReportsAVanishedFailureAsUnavailable(t *testing.T) {
	f := newClusterFixture(t)
	f.write(t)
	detail := models.PullRequestDetail{
		PullRequestSummary: models.PullRequestSummary{Number: 6210, BaseRef: "main"},
		Checks: []models.PullRequestCheck{{
			JobID: clusterJobID, JobName: clusterJobName, BuildID: "200",
			Failures: []models.PullRequestFailure{{TestCase: models.TestCase{Name: "[It] something else"}}},
		}},
	}
	writeJSON(t, filepath.Join(f.dataDir, "pull-requests",
		models.PullRequestDataFilename(6210)), detail)

	_, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestClusterRefNormalization(t *testing.T) {
	if _, err := (ClusterRef{}).normalized(); !errors.Is(err, ErrInvalid) {
		t.Error("an empty id must be rejected")
	}
	if _, err := (ClusterRef{ID: "  "}).normalized(); !errors.Is(err, ErrInvalid) {
		t.Error("a blank id must be rejected")
	}
	long := ClusterRef{ID: string(make([]byte, maxClusterIDLength+1))}
	if _, err := long.normalized(); !errors.Is(err, ErrInvalid) {
		t.Error("an oversized id must be rejected")
	}
	got, err := ClusterRef{ID: " abc123 "}.normalized()
	if err != nil {
		t.Fatalf("normalized: %v", err)
	}
	if got.ID != "abc123" {
		t.Errorf("id = %q, want it trimmed", got.ID)
	}
	if got.identity() != "abc123" {
		t.Errorf("identity = %q", got.identity())
	}
}

// The identity is the correlation key's hash, not the member set, so one
// analysis survives pull requests joining and leaving the failure.
func TestClusterIdentityIsStableAcrossMembershipChanges(t *testing.T) {
	first := models.SharedFailureID("main", clusterJobName, clusterTest)
	second := models.SharedFailureID("main", clusterJobName, clusterTest)
	if first != second {
		t.Fatal("the same correlation key must produce the same id")
	}
	if models.SharedFailureID("release-1.0", clusterJobName, clusterTest) == first {
		t.Error("a different base branch must produce a different id")
	}
	if models.SharedFailureID("main", "other-job", clusterTest) == first {
		t.Error("a different job must produce a different id")
	}
}

func TestClusterRunnerWithoutAnAnalyzerIsUnavailable(t *testing.T) {
	_, err := (&ClusterAnalysisRunner{}).Run(context.Background(), ClusterResolved{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// The index is produced by prattribution and consumed here, so the two must
// agree on the published shape rather than on a hand-written fixture.
func TestClusterResolveReadsWhatAttributionPublishes(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "pull-requests"), 0o755); err != nil {
		t.Fatalf("data dir: %v", err)
	}

	// Three pull requests make every member widespread, which is the verdict
	// that leaves the shared failure as the only way to investigate.
	var details []models.PullRequestDetail
	for i, number := range []int{6209, 6210, 6211} {
		started := clusterStart.Add(time.Duration(i) * time.Hour)
		buildID := strconv.Itoa(100 + i)
		details = append(details, models.PullRequestDetail{
			PullRequestSummary: models.PullRequestSummary{
				Number: number, BaseRef: "main", HeadSHA: "abc123",
			},
			Checks: []models.PullRequestCheck{{
				JobID: clusterJobID, JobName: clusterJobName, BuildID: buildID,
				Started: started, Finished: started.Add(time.Minute),
				Failures: []models.PullRequestFailure{{
					TestCase: models.TestCase{Name: clusterTest, Status: "failed", FailureMessage: "boom"},
				}},
			}},
		})
	}
	baseline := prattribution.BuildBaseline([]models.JobDetail{{
		Name: "periodic-project-e2e", JobID: "periodic-project-e2e", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", Started: clusterStart},
			TestCases: []models.TestCase{{Name: clusterTest, Status: "passed"}},
		}},
	}}, models.FlakinessReport{})
	prattribution.Annotate(details, baseline, prattribution.Repository{}, nil)

	clusters := prattribution.Clusters(details)
	if len(clusters) != 1 || !clusters[0].Escalatable {
		t.Fatalf("expected one escalatable cluster, got %+v", clusters)
	}
	if err := output.WriteSharedFailures(dataDir,
		models.SharedFailureIndex{Repo: "org/repo", Failures: clusters}); err != nil {
		t.Fatalf("WriteSharedFailures: %v", err)
	}

	fixture := &clusterFixture{root: root, dataDir: dataDir, cluster: clusters[0]}
	for i, detail := range details {
		writeJSON(t, filepath.Join(dataDir, "pull-requests",
			models.PullRequestDataFilename(detail.Number)), detail)
		fixture.writeBuild(t, clusters[0].PullRequests[i])
	}

	resolved, err := fixture.resolver(t).Resolve(context.Background(),
		ClusterRef{ID: clusters[0].ID})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Subject.EvidencePull != 6211 {
		t.Errorf("evidence pull = %d, want the newest build", resolved.Subject.EvidencePull)
	}
	if len(resolved.Subject.PullNumbers) != 3 {
		t.Errorf("pull numbers = %v, want every affected pull request", resolved.Subject.PullNumbers)
	}
	if resolved.Request.TestCase.FailureMessage != "boom" {
		t.Errorf("test case = %+v, want the published failure text", resolved.Request.TestCase)
	}
}

// clusterGateRunner blocks in Run so a test can observe how many analyses hold
// the gate at once.
type clusterGateRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r clusterGateRunner) Run(ctx context.Context, _ ClusterResolved) (ClusterView, error) {
	r.started <- struct{}{}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ClusterView{}, ctx.Err()
	}
	return ClusterView{State: StateComplete}, nil
}

type clusterPassthroughResolver struct{}

// staticEvidence stands in for a subject whose evidence build never moves.
func staticEvidence(ClusterRef) (EscalationEvidence, bool) {
	return EscalationEvidence{}, true
}

func (clusterPassthroughResolver) Resolve(context.Context, ClusterRef) (ClusterResolved, error) {
	return ClusterResolved{}, nil
}

// Escalation exists to bound model traffic, so two kinds sharing a server must
// share the slot. Without a shared gate each kind carries its own and their
// analyses run at the same time.
func TestSharedGateSerializesAnalysesAcrossEscalationKinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gate := NewGate(1)
	pullRunner := newFakeRunner()
	pullService, err := New(ctx, &fakeResolver{}, pullRunner, Options[Ref]{Gate: gate})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clusterRunner := clusterGateRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	clusterService, err := New[ClusterRef, ClusterResolved](
		ctx, clusterPassthroughResolver{}, clusterRunner, Options[ClusterRef]{
			Gate:            gate,
			CurrentEvidence: staticEvidence,
		})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := pullService.Start(ctx, testRef("TestA"), "admin", "req-1"); err != nil {
		t.Fatalf("pull Start: %v", err)
	}
	// Wait for the pull request analysis to take the slot.
	select {
	case <-pullRunner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("pull request analysis never started")
	}

	if _, err := clusterService.Start(ctx, ClusterRef{ID: "abc"}, "admin", "req-2"); err != nil {
		t.Fatalf("cluster Start: %v", err)
	}
	select {
	case <-clusterRunner.started:
		t.Fatal("a second analysis ran while the first held the shared gate")
	case <-time.After(200 * time.Millisecond):
	}

	// Releasing the first lets the queued one through.
	close(pullRunner.release)
	select {
	case <-clusterRunner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the queued analysis never started after the gate was released")
	}
	close(clusterRunner.release)
}

// The newest usable build changes between passes while the shared failure id
// stays the same, so a stored result must carry the build it actually read
// instead of leaving the reader to recompute a choice that has since moved.
func TestClusterResolveRecordsTheEvidenceBuild(t *testing.T) {
	f := newClusterFixture(t)
	f.write(t)

	resolved, err := f.resolver(t).Resolve(context.Background(), f.ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Evidence.PullNumber != 6210 || resolved.Evidence.BuildID != "200" {
		t.Fatalf("evidence = %+v, want the newest usable member build", resolved.Evidence)
	}

	runner := &ClusterAnalysisRunner{
		NewAnalyzer: func(sharedfailure.Subject) (ai.FailureAnalyzer, error) {
			return stubAnalyzer{}, nil
		},
	}
	view, err := runner.Run(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if view.Evidence == nil || view.Evidence.PullNumber != 6210 || view.Evidence.BuildID != "200" {
		t.Fatalf("view evidence = %+v, want the analyzed build recorded", view.Evidence)
	}
}

// stubAnalyzer returns a fixed analysis so the runner's projection can be
// checked without a model.
type stubAnalyzer struct{}

func (stubAnalyzer) AnalyzeFailure(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	return ai.FailureAnalysisResult{
		Analysis: &models.AIAnalysis{RootCause: "quota exhausted", Severity: "high"},
	}, nil
}

// countingClusterRunner reports how many analyses actually ran and echoes the
// evidence the resolver chose, the way the real runner does.
type countingClusterRunner struct {
	mu   sync.Mutex
	runs int
}

func (c *countingClusterRunner) Run(_ context.Context, resolved ClusterResolved) (ClusterView, error) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	evidence := resolved.Evidence
	return ClusterView{State: StateComplete, RootCause: "read " + evidence.BuildID, Evidence: &evidence}, nil
}

func (c *countingClusterRunner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// movingResolver stands in for a shared failure whose newest usable build
// changes between passes while its identity stays the same.
type movingResolver struct {
	mu      sync.Mutex
	buildID string
}

func (m *movingResolver) Resolve(context.Context, ClusterRef) (ClusterResolved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ClusterResolved{Evidence: EscalationEvidence{PullNumber: 1, BuildID: m.buildID}}, nil
}

// current mirrors the resolver's choice for status reads.
func (m *movingResolver) current(ClusterRef) (EscalationEvidence, bool) {
	return m.currentEvidence(), true
}

func (m *movingResolver) currentEvidence() EscalationEvidence {
	m.mu.Lock()
	defer m.mu.Unlock()
	return EscalationEvidence{PullNumber: 1, BuildID: m.buildID}
}

func (m *movingResolver) moveTo(buildID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buildID = buildID
}

func completedCluster(t *testing.T, service *Service[ClusterRef, ClusterResolved], requestID string) ClusterView {
	t.Helper()
	if _, err := service.Start(context.Background(), ClusterRef{ID: "abc"}, "admin", requestID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		view, err := service.Get(ClusterRef{ID: "abc"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if view.State == StateComplete {
			return view
		}
		select {
		case <-deadline:
			t.Fatalf("escalation never completed, state = %q", view.State)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A shared failure keeps one identity while the build under it moves on, so a
// finished analysis must not answer a later failure of the same test.
func TestClusterAnalysisIsRerunWhenTheEvidenceBuildMoves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resolver := &movingResolver{buildID: "100"}
	runner := &countingClusterRunner{}
	service, err := New[ClusterRef, ClusterResolved](ctx, resolver, runner, Options[ClusterRef]{
		CurrentEvidence: resolver.current,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := completedCluster(t, service, "req-1")
	if first.RootCause != "read 100" || runner.count() != 1 {
		t.Fatalf("first analysis = %+v after %d runs", first, runner.count())
	}

	// Same evidence build: the stored result still describes it, so no repeat.
	again := completedCluster(t, service, "req-2")
	if again.RootCause != "read 100" || runner.count() != 1 {
		t.Fatalf("an unchanged evidence build must reuse the result, got %+v after %d runs", again, runner.count())
	}

	// The failure recurs on a newer build, which the old analysis never read.
	resolver.moveTo("300")
	fresh := completedCluster(t, service, "req-3")
	if runner.count() != 2 {
		t.Fatalf("a moved evidence build must be analyzed again, runs = %d", runner.count())
	}
	if fresh.RootCause != "read 300" {
		t.Fatalf("result = %+v, want the analysis of the current build", fresh)
	}
	if fresh.Evidence == nil || fresh.Evidence.BuildID != "300" {
		t.Fatalf("evidence = %+v, want the current build recorded", fresh.Evidence)
	}
}

// A pull request reference names its own build, so its finished result cannot
// describe anything else and must stay pinned.
func TestPullRequestAnalysisIsNotRerun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runner := newFakeRunner()
	close(runner.release)
	service, err := New(ctx, &fakeResolver{}, runner, Options[Ref]{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ref := testRef("TestA")
	if _, err := service.Start(ctx, ref, "admin", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, service, ref, StateComplete)
	before := len(runner.started)

	if _, err := service.Start(ctx, ref, "admin", "req-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	view, err := service.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateComplete {
		t.Fatalf("state = %q, want the stored result returned unchanged", view.State)
	}
	if len(runner.started) != before {
		t.Error("a pull request result must not be analyzed again")
	}
}

// The panel reads status on mount and only offers the button for a subject
// that is not started or failed, so a completed result that Get keeps serving
// is one no maintainer can ever ask to re-analyze. This walks that path rather
// than calling Start directly.
func TestStaleClusterResultIsReportedAsNeverStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resolver := &movingResolver{buildID: "100"}
	runner := &countingClusterRunner{}
	service, err := New[ClusterRef, ClusterResolved](ctx, resolver, runner, Options[ClusterRef]{
		CurrentEvidence: resolver.current,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	completedCluster(t, service, "req-1")
	// While the build is unchanged the result is terminal, so no button shows.
	view, err := service.Get(ClusterRef{ID: "abc"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateComplete {
		t.Fatalf("state = %q, want a current result still served", view.State)
	}

	resolver.moveTo("300")
	view, err = service.Get(ClusterRef{ID: "abc"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateNotStarted {
		t.Fatalf("state = %q, want a moved evidence build to offer a fresh analysis", view.State)
	}

	// And that offer actually reaches a new analysis of the current build.
	fresh := completedCluster(t, service, "req-2")
	if fresh.RootCause != "read 300" || runner.count() != 2 {
		t.Fatalf("result = %+v after %d runs, want the current build analyzed", fresh, runner.count())
	}
}

// A transient failure to determine the current build must not make a stored
// result vanish, which would look like the analysis was lost.
func TestUnknownCurrentEvidenceKeepsServingTheStoredResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resolver := &movingResolver{buildID: "100"}
	service, err := New[ClusterRef, ClusterResolved](ctx, resolver, &countingClusterRunner{}, Options[ClusterRef]{
		CurrentEvidence: func(ClusterRef) (EscalationEvidence, bool) { return EscalationEvidence{}, false },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	completedCluster(t, service, "req-1")
	view, err := service.Get(ClusterRef{ID: "abc"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateComplete {
		t.Fatalf("state = %q, want the stored result kept when the current build is unknown", view.State)
	}
}

// A build id is unique within one repository's pull request, not across a data
// directory that outlives a change of project, so the whole address is compared.
func TestEvidenceComparesTheWholeBuildAddress(t *testing.T) {
	base := EscalationEvidence{Repo: "org/repo", PullNumber: 7, BuildID: "100"}
	if !base.sameBuild(EscalationEvidence{Repo: "org/repo", PullNumber: 7, BuildID: "100"}) {
		t.Error("the same address must compare equal")
	}
	for _, other := range []EscalationEvidence{
		{Repo: "org/other", PullNumber: 7, BuildID: "100"},
		{Repo: "org/repo", PullNumber: 8, BuildID: "100"},
		{Repo: "org/repo", PullNumber: 7, BuildID: "200"},
	} {
		if base.sameBuild(other) {
			t.Errorf("%+v must not compare equal to %+v", other, base)
		}
	}
}

// A kind whose evidence can move but that cannot report its current build
// would strand finished results, so the wiring mistake is refused outright
// rather than left to look like a working service.
func TestClusterServiceRequiresCurrentEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, err := New[ClusterRef, ClusterResolved](
		ctx, &movingResolver{}, &countingClusterRunner{}, Options[ClusterRef]{})
	if err == nil {
		t.Fatal("a revalidating subject with no CurrentEvidence must be refused")
	}

	// The pull request kind names its own build, so it needs no such hook.
	if _, err := New(ctx, &fakeResolver{}, newFakeRunner(), Options[Ref]{}); err != nil {
		t.Fatalf("the pull request kind must not require CurrentEvidence: %v", err)
	}
}

// phasedClusterRunner completes its first analysis and blocks in its second, so
// a test can hold a record in the running state.
type phasedClusterRunner struct {
	mu      sync.Mutex
	runs    int
	started chan struct{}
	release chan struct{}
}

func (p *phasedClusterRunner) Run(ctx context.Context, resolved ClusterResolved) (ClusterView, error) {
	p.mu.Lock()
	p.runs++
	n := p.runs
	p.mu.Unlock()
	evidence := resolved.Evidence
	done := ClusterView{State: StateComplete, RootCause: "read " + evidence.BuildID, Evidence: &evidence}
	if n == 1 {
		return done, nil
	}
	p.started <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ClusterView{}, ctx.Err()
	}
	return done, nil
}

// The staleness check runs without the lock, so a start can replace the record
// underneath it. Reporting the stale verdict then would hide an analysis that
// is already running, and the panel stops polling on a not_started state.
func TestGetDoesNotHideAnAnalysisStartedDuringTheEvidenceLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resolver := &movingResolver{buildID: "100"}
	runner := &phasedClusterRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(runner.release) })

	var service *Service[ClusterRef, ClusterResolved]
	var mu sync.Mutex
	armed := false
	service, err := New[ClusterRef, ClusterResolved](ctx, resolver, runner, Options[ClusterRef]{
		CurrentEvidence: func(ref ClusterRef) (EscalationEvidence, bool) {
			mu.Lock()
			fire := armed
			armed = false
			mu.Unlock()
			// A start lands while this lookup is in flight, which is exactly
			// the window the record lock is released for.
			if fire {
				if _, startErr := service.Start(context.Background(), ref, "admin", "req-2"); startErr != nil {
					t.Errorf("Start during lookup: %v", startErr)
				} else {
					<-runner.started
				}
			}
			return resolver.currentEvidence(), true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	completedCluster(t, service, "req-1")

	// From here the stored result describes a build that has moved on, and the
	// next status read is the one that races a start.
	resolver.moveTo("300")
	mu.Lock()
	armed = true
	mu.Unlock()

	view, err := service.Get(ClusterRef{ID: "abc"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State == StateNotStarted {
		t.Fatal("a running analysis was reported as never started, so the caller would stop polling")
	}
}

// waitClusterState polls the record directly, so a test can wait for an
// escalation without going through Get and its evidence lookup.
func waitClusterState(t *testing.T, service *Service[ClusterRef, ClusterResolved], identity, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		service.mu.Lock()
		rec := service.records[identity]
		var state string
		if rec != nil {
			state = rec.view.State
		}
		service.mu.Unlock()
		if state == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("record never reached %q, state = %q", want, state)
		case <-time.After(time.Millisecond):
		}
	}
}

// A replacement can itself already be stale: it resolved one build while the
// published evidence moved to another. The panel never polls a completed
// state, so returning it unvalidated would leave a stale analysis on screen
// with no way to ask again.
func TestGetRevalidatesAReplacementThatIsAlreadyStale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resolver := &movingResolver{buildID: "100"}
	runner := &countingClusterRunner{}
	var service *Service[ClusterRef, ClusterResolved]
	var mu sync.Mutex
	reported := "100"
	armed := false
	ref := ClusterRef{ID: "abc"}

	service, err := New[ClusterRef, ClusterResolved](ctx, resolver, runner, Options[ClusterRef]{
		CurrentEvidence: func(ClusterRef) (EscalationEvidence, bool) {
			mu.Lock()
			fire := armed
			armed = false
			mu.Unlock()
			if fire {
				// A start lands and completes against build 200 while this
				// lookup is in flight, and the evidence moves on again to 300.
				resolver.moveTo("200")
				if _, startErr := service.Start(context.Background(), ref, "admin", "req-2"); startErr != nil {
					t.Errorf("Start during lookup: %v", startErr)
				}
				waitClusterState(t, service, ref.identity(), StateComplete)
				mu.Lock()
				reported = "300"
				mu.Unlock()
			}
			mu.Lock()
			defer mu.Unlock()
			return EscalationEvidence{PullNumber: 1, BuildID: reported}, true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	completedCluster(t, service, "req-1")
	mu.Lock()
	armed = true
	reported = "999"
	mu.Unlock()

	view, err := service.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateNotStarted {
		t.Fatalf("state = %q (%q), want a replacement that is itself stale to offer a fresh analysis",
			view.State, view.RootCause)
	}
}
