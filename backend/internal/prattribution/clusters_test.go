package prattribution

import (
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

// clusterDetail is a pull request reporting one failure on one build, with the
// build metadata the cluster view publishes.
func clusterDetail(number int, baseRef, jobName, buildID string, started time.Time, failures ...models.PullRequestFailure) models.PullRequestDetail {
	d := detailOnBase(number, baseRef, jobName, failures...)
	d.Title = "change " + jobName
	d.HTMLURL = "https://github.com/example/project/pull/1"
	d.Checks[0].BuildID = buildID
	d.Checks[0].Started = started
	d.Checks[0].Finished = started.Add(time.Minute)
	return d
}

// sharedDetails is the common fixture: the same failure on three pull requests
// targeting one branch, which is enough for a widespread verdict.
func sharedDetails(t *testing.T) []models.SharedFailure {
	t.Helper()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", base, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", base.Add(time.Hour), failure(testName)),
		clusterDetail(3, "main", e2eJob, "b3", base.Add(2*time.Hour), failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	return Clusters(details)
}

func TestClustersPublishesASharedFailure(t *testing.T) {
	clusters := sharedDetails(t)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	got := clusters[0]
	if got.ID != models.SharedFailureID("main", e2eJob, testName) {
		t.Errorf("id = %q, want the correlation key hash", got.ID)
	}
	if got.BaseRef != "main" || got.JobName != e2eJob || got.TestName != testName {
		t.Errorf("correlation key = %q/%q/%q", got.BaseRef, got.JobName, got.TestName)
	}
	if len(got.PullRequests) != 3 {
		t.Fatalf("expected 3 members, got %d", len(got.PullRequests))
	}
	for i, member := range got.PullRequests {
		if member.Number != i+1 {
			t.Errorf("member %d is #%d, want members ordered by number", i, member.Number)
		}
	}
	if got.PullRequests[0].Verdict != models.AttributionWidespread {
		t.Errorf("member verdict = %q, want the widespread verdict recorded", got.PullRequests[0].Verdict)
	}
}

func TestClustersReportsTheObservedBuildWindow(t *testing.T) {
	clusters := sharedDetails(t)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if !clusters[0].OldestBuildStarted.Equal(base) {
		t.Errorf("oldest = %v, want the earliest member build", clusters[0].OldestBuildStarted)
	}
	if !clusters[0].NewestBuildStarted.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("newest = %v, want the latest member build", clusters[0].NewestBuildStarted)
	}
}

func TestClustersIgnoresZeroBuildStarts(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", time.Time{}, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	clusters := Clusters(details)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if !clusters[0].OldestBuildStarted.Equal(started) {
		t.Errorf("oldest = %v, want the zero start ignored", clusters[0].OldestBuildStarted)
	}
}

func TestClustersNeedsSeveralPullRequests(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	if clusters := Clusters(details); len(clusters) != 0 {
		t.Fatalf("expected no cluster for a failure on one pull request, got %d", len(clusters))
	}
}

func TestClustersSeparateBaseBranches(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName)),
		clusterDetail(2, "release-1.0", e2eJob, "b2", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	if clusters := Clusters(details); len(clusters) != 0 {
		t.Fatalf("pull requests on different base branches must not correlate, got %d clusters", len(clusters))
	}
}

func TestClustersSeparateJobsForBuildLevelFailures(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, buildFailure()),
		clusterDetail(2, "main", "pull-project-unit", "b2", started, buildFailure()),
	}
	Annotate(details, observedBaseline(), Repository{}, nil)
	if clusters := Clusters(details); len(clusters) != 0 {
		t.Fatalf("a build-level failure carries a generic name, so jobs must not correlate; got %d clusters", len(clusters))
	}
}

func TestClustersMarksBuildLevelFailures(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, buildFailure()),
		clusterDetail(2, "main", e2eJob, "b2", started, buildFailure()),
	}
	Annotate(details, observedBaseline(), Repository{}, nil)
	clusters := Clusters(details)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if !clusters[0].BuildLevel {
		t.Error("expected the cluster to be marked build-level")
	}
}

func TestClustersEscalatableOnlyWhenNoMemberCanEscalateAlone(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Three peers make every member widespread, which is exactly the verdict
	// that offers no per-pull-request analysis.
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName)),
		clusterDetail(3, "main", e2eJob, "b3", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	clusters := Clusters(details)
	if len(clusters) != 1 || !clusters[0].Escalatable {
		t.Fatalf("a cluster whose members are all widespread must be escalatable, got %+v", clusters)
	}

	// Two pull requests are mutually uncorroborated, so each keeps a residual
	// verdict it can escalate on its own.
	details = []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	clusters = Clusters(details)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if clusters[0].Escalatable {
		t.Error("a cluster whose members can escalate individually must not offer a second path")
	}
}

func TestClustersStaleMemberDoesNotOfferIndividualEscalation(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName)),
	}
	// Both builds tested an older head, so neither pull request can be
	// escalated even though both verdicts leave room for analysis.
	details[0].Checks[0].Stale = true
	details[1].Checks[0].Stale = true
	Annotate(details, observedBaseline(testName), Repository{}, nil)
	clusters := Clusters(details)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if !clusters[0].Escalatable {
		t.Error("expected the cluster to be escalatable when every member build is stale")
	}
}

func TestClustersRecordOnePullRequestOnce(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "old", started, failure(testName)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName)),
	}
	// A second check reporting the same job name must not make one pull
	// request look like two members.
	newer := details[0].Checks[0]
	newer.BuildID = "new"
	newer.Started = started.Add(time.Hour)
	details[0].Checks = append(details[0].Checks, newer)
	Annotate(details, observedBaseline(testName), Repository{}, nil)

	clusters := Clusters(details)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if len(clusters[0].PullRequests) != 2 {
		t.Fatalf("expected 2 members, got %d", len(clusters[0].PullRequests))
	}
	if got := clusters[0].PullRequests[0].BuildID; got != "new" {
		t.Errorf("member build = %q, want the newest build kept", got)
	}
}

func TestClustersOrderWidestFirst(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	narrow := "[It] deletes a cluster"
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName), failure(narrow)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName), failure(narrow)),
		clusterDetail(3, "main", e2eJob, "b3", started, failure(testName)),
	}
	Annotate(details, observedBaseline(testName, narrow), Repository{}, nil)

	clusters := Clusters(details)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}
	if clusters[0].TestName != testName {
		t.Errorf("first cluster = %q, want the failure hitting the most pull requests", clusters[0].TestName)
	}
}

func TestClustersOrderIsStable(t *testing.T) {
	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	other := "[It] deletes a cluster"
	details := []models.PullRequestDetail{
		clusterDetail(1, "main", e2eJob, "b1", started, failure(testName), failure(other)),
		clusterDetail(2, "main", e2eJob, "b2", started, failure(testName), failure(other)),
	}
	Annotate(details, observedBaseline(testName, other), Repository{}, nil)

	first := Clusters(details)
	for i := 0; i < 20; i++ {
		again := Clusters(details)
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("cluster order changed between passes at %d: %q then %q", j, first[j].ID, again[j].ID)
			}
		}
	}
}
