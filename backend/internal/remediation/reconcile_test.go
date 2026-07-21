package remediation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakePRClient struct {
	pull ghpr.PullRequest
	err  error
}

func (f fakePRClient) GetPullRequest(context.Context, string, string, int) (ghpr.PullRequest, error) {
	return f.pull, f.err
}

type countingPRClient struct {
	calls int
}

func (c *countingPRClient) GetPullRequest(context.Context, string, string, int) (ghpr.PullRequest, error) {
	c.calls++
	return ghpr.PullRequest{}, nil
}

func TestReconcileStopsOnStateReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReconciler(&countingPRClient{}, dir).Reconcile(
		context.Background(), nil, nil, nil, func(models.PatternAnalysis) string { return "" }); err == nil {
		t.Fatal("reconciliation continued after state read failure")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("state path was overwritten: info=%v err=%v", info, err)
	}
}

func TestReconcileSkipsGitHubRefreshForMergedPullRequest(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job",
		Attempts: []Attempt{{
			URL: "https://github.com/o/r/pull/7", Status: StatusVerifiedFixed, PRState: StatusMerged,
		}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := &countingPRClient{}
	if _, err := NewReconciler(client, dir).Reconcile(context.Background(), nil, nil, nil, func(models.PatternAnalysis) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if client.calls != 0 {
		t.Fatalf("pull request calls = %d, want 0", client.calls)
	}
}

func TestReconcileTracksMergedPullRequest(t *testing.T) {
	dir := t.TempDir()
	mergedAt := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		MergeCommitSHA: "merge", MergedAt: mergedAt,
		Head: ghpr.PullRequestRef{Repo: "fork/r", Ref: "fix", SHA: "head"},
		Base: ghpr.PullRequestRef{Repo: "o/r", Ref: "main", SHA: "base"},
	}}
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "timeout"}
	details := []models.JobDetail{{JobID: "job", Name: "job", JobType: models.JobTypePeriodic}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details,
		map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/7", OpenedAt: "2026-07-20T01:00:00Z"}},
		func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusMerged || attempt.MergeSHA != "merge" || attempt.HeadSHA != "head" || attempt.TargetRepo != "o/r" {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestParsePullRequestURL(t *testing.T) {
	owner, repo, number, err := ParsePullRequestURL("https://github.com/o/r/pull/12")
	if err != nil || owner != "o" || repo != "r" || number != 12 {
		t.Fatalf("owner=%q repo=%q number=%d err=%v", owner, repo, number, err)
	}
}

func TestReconcileRefreshesClosedPullRequest(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job",
		Attempts: []Attempt{{
			Number: 1, PRNumber: 7, URL: "https://github.com/o/r/pull/7",
			Status: StatusClosedUnmerged, PRState: StatusClosedUnmerged,
		}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}

	client := &fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "open",
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	}}
	reconciler := NewReconciler(client, dir)
	state, err := reconciler.Reconcile(context.Background(), nil, nil, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusOpen || attempt.PRState != StatusOpen {
		t.Fatalf("reopened attempt = %+v", attempt)
	}

	client.pull.State = "closed"
	client.pull.Merged = true
	client.pull.MergeCommitSHA = "merge"
	state, err = reconciler.Reconcile(context.Background(), nil, nil, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	attempt = state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusMerged || attempt.PRState != StatusMerged || attempt.MergeSHA != "merge" {
		t.Fatalf("merged attempt = %+v", attempt)
	}
}

func TestApplyPullRequestPreservesPresubmitEvidenceWhenMergeSHAChanges(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{
		Status: StatusPremergeVerified, PRState: StatusOpen, HeadSHA: "head", MergeSHA: "synthetic",
		Outcome: OutcomePassed, OutcomeReason: "matching Prow presubmit passed",
		Observations: []BuildObservation{{BuildID: "10", HeadSHA: "head", Outcome: OutcomePassed}},
	}
	applyPullRequest(entry, attempt, ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		MergeCommitSHA: "final", Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	})
	if attempt.Status != StatusMerged || attempt.Outcome != OutcomePassed || len(attempt.Observations) != 1 {
		t.Fatalf("attempt = %+v", attempt)
	}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("finalized attempt = %+v", attempt)
	}
}

func TestApplyPullRequestMovesPremergeVerificationToMerged(t *testing.T) {
	entry := &Remediation{}
	attempt := &Attempt{Status: StatusPremergeVerified, HeadSHA: "head"}
	applyPullRequest(entry, attempt, ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		MergeCommitSHA: "merge", Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	})
	if attempt.Status != StatusMerged || attempt.PRState != StatusMerged {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestReconcileRefreshesFindingIDFromMatchingEvidence(t *testing.T) {
	dir := t.TempDir()
	existing := NewState()
	existing.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job",
		Evidence: Evidence{Tests: []TestEvidence{{Identity: "suite\x00class\x00test", ErrorHash: "hash"}}},
	}
	if err := existing.Save(dir); err != nil {
		t.Fatal(err)
	}
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"1"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "same"}},
	}}}}
	current := EvidenceForPattern(pattern, details)
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	existing = loaded
	existing.Remediations["old"].Evidence.Tests[0].ErrorHash = current.Tests[0].ErrorHash
	if err := existing.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if state.Remediations["old"].FindingID != "new" {
		t.Fatalf("remediation = %+v", state.Remediations["old"])
	}
}

func TestSourceRepoFromBucketMetadata(t *testing.T) {
	detail := models.JobDetail{Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{
		RepoRefs: map[string]string{"example/project": "main"},
	}}}}
	if got := sourceRepoFromDetail(detail, ""); got != "example/project" {
		t.Fatalf("source repo = %q", got)
	}
}

type recoveringPRClient struct {
	fakePRClient
	url string
}

func (c recoveringPRClient) SearchPullRequests(context.Context, string, string, string, string) ([]ghpr.PullRequestSearchResult, error) {
	return []ghpr.PullRequestSearchResult{{Number: 7, HTMLURL: c.url}}, nil
}

func TestReconcileRecoversPullRequestFromMarker(t *testing.T) {
	dir := t.TempDir()
	client := recoveringPRClient{
		fakePRClient: fakePRClient{pull: ghpr.PullRequest{
			Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "open",
			Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
		}},
		url: "https://github.com/o/r/pull/7",
	}
	reconciler := NewReconciler(client, dir)
	reconciler.SetRecovery("o/r", client)
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "timeout"}
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, nil, nil, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Remediations["pattern"]; got == nil || len(got.Attempts) != 1 || got.Attempts[0].PRNumber != 7 {
		t.Fatalf("state = %+v", state)
	}
}

func TestReconcileAppendsRetryToEvidenceMatchedRemediation(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedRootCause: "same", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "2"},
		TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "same"}},
	}}}}
	evidence := EvidenceForPattern(pattern, details)
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", JobName: "job", Evidence: evidence,
		Attempts: []Attempt{{Number: 1, URL: "https://github.com/o/r/pull/1", Status: StatusStillFailingSameCause}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 2, HTMLURL: "https://github.com/o/r/pull/2", State: "open",
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details,
		map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/2"}}, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if state.Remediations["new"] != nil || len(state.Remediations["old"].Attempts) != 2 || state.Remediations["old"].FindingID != "new" {
		t.Fatalf("state = %+v", state)
	}
}

func TestFinalizeMergedPresubmit(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, Outcome: OutcomePassed}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestFinalizeMergedPresubmitWithoutPassIsInconclusive(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestApplyPullRequestClearsStaleEvidenceWhenMergedHeadChanges(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{
		Status: StatusPremergeVerified, HeadSHA: "old", Outcome: OutcomePassed,
		Observations: []BuildObservation{{BuildID: "1", HeadSHA: "old", Outcome: OutcomePassed}},
	}
	applyPullRequest(entry, attempt, ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "new"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	})
	if len(attempt.Observations) != 0 || attempt.Outcome != "" || attempt.Status != StatusMerged {
		t.Fatalf("attempt = %+v", attempt)
	}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestReconcileReopensVerifiedFindingOnNewerFailure(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedBuilds: []string{"20"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "20"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "same"}},
	}}}}
	current := EvidenceForPattern(pattern, details)
	state := NewState()
	previous := current
	previous.BuildWatermark = "10"
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job", JobType: models.JobTypePresubmit, Evidence: previous,
		Attempts: []Attempt{{Status: StatusVerifiedFixed, PRState: StatusMerged, URL: "https://github.com/o/r/pull/7"}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusStillFailingSameCause || state.Remediations["pattern"].Evidence.BuildWatermark != "20" {
		t.Fatalf("attempt=%+v evidence=%+v", attempt, state.Remediations["pattern"].Evidence)
	}
	if len(attempt.Observations) != 1 || attempt.Observations[0].BuildID != "20" || attempt.Observations[0].Outcome != OutcomeSameCause {
		t.Fatalf("observations = %+v", attempt.Observations)
	}
}

func TestReconcilePreservesLinkedIssueLifecycleFields(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job",
		Issue: &IssueRef{Number: 9, URL: "old", Repo: "o/r", State: "closed", LastTransition: "observing->verified_fixed"},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(fakePRClient{}, dir)
	reconciler.SetIssues("o/r", map[string]IssueRef{"job": {Number: 9, URL: "new", Repo: "o/r"}}, nil)
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job"}
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, nil, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	issue := state.Remediations["pattern"].Issue
	if issue.URL != "new" || issue.State != "closed" || issue.LastTransition != "observing->verified_fixed" {
		t.Fatalf("issue = %+v", issue)
	}
}

func TestReconcileBackfillsRecoveredJobMetadata(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{
		ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "cause",
		SharedBuilds: []string{"1", "2", "3"},
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "open",
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	}}
	reconciler := NewReconciler(client, dir)
	fixes := map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/7", Pattern: &pattern}}
	state, err := reconciler.Reconcile(context.Background(), nil, nil, fixes, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if state.Remediations["pattern"].JobType != "" {
		t.Fatalf("initial remediation = %+v", state.Remediations["pattern"])
	}

	failure := models.TestCase{Name: "test", Status: "failed", FailureMessage: "boom"}
	details := []models.JobDetail{{
		JobID: "job", Name: "periodic-job", JobType: models.JobTypePeriodic, Repo: "o/r",
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{failure}},
			{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{failure}},
			{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{failure}},
		},
	}}
	state, err = reconciler.Reconcile(context.Background(), nil, details, fixes, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Remediations["pattern"]
	if entry.JobType != models.JobTypePeriodic || entry.JobName != "periodic-job" || entry.SourceRepo != "o/r" {
		t.Fatalf("remediation = %+v", entry)
	}
	if entry.Classification != string(models.ClassificationPersistent) {
		t.Fatalf("classification = %q", entry.Classification)
	}
}

func TestReconcileUsesPatternSnapshotFromTrackedFix(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "cause"}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "open",
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), nil, nil,
		map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/7", Pattern: &pattern}},
		func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if entry := state.Remediations["pattern"]; entry == nil || len(entry.Attempts) != 1 {
		t.Fatalf("state = %+v", state)
	}
}

type multiRecoveringPRClient struct {
	fakePRClient
	results []ghpr.PullRequestSearchResult
}

func (c multiRecoveringPRClient) SearchPullRequests(context.Context, string, string, string, string) ([]ghpr.PullRequestSearchResult, error) {
	return c.results, nil
}

func TestReconcileRecoveryDoesNotAdoptOlderMarkerMatch(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "cause"}
	state := NewStateForRepo("o/r")
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job",
		Attempts: []Attempt{{Number: 1, URL: "https://github.com/o/r/pull/2"}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := multiRecoveringPRClient{
		fakePRClient: fakePRClient{pull: ghpr.PullRequest{
			Number: 2, HTMLURL: "https://github.com/o/r/pull/2", State: "open",
			Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
		}},
		results: []ghpr.PullRequestSearchResult{
			{Number: 2, HTMLURL: "https://github.com/o/r/pull/2"},
			{Number: 1, HTMLURL: "https://github.com/o/r/pull/1"},
		},
	}
	reconciler := NewReconciler(client, dir)
	reconciler.SetRecovery("o/r", client)
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, nil, nil, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Remediations["pattern"].Attempts) != 1 || state.Remediations["pattern"].Attempts[0].URL != "https://github.com/o/r/pull/2" {
		t.Fatalf("attempts = %+v", state.Remediations["pattern"].Attempts)
	}
}

func TestReconcileDoesNotTreatDifferentEvidenceAsSameCause(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedBuilds: []string{"20"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "20"},
		TestCases: []models.TestCase{{Name: "different", Status: "failed", FailureMessage: "different"}},
	}}}}
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job", JobType: models.JobTypePresubmit,
		Evidence: Evidence{BuildWatermark: "10", Tests: []TestEvidence{{Identity: "name\x00old", ErrorHash: "old"}}},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, PRState: StatusMerged, URL: "https://github.com/o/r/pull/7"}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusInconclusive || attempt.Outcome == OutcomeSameCause {
		t.Fatalf("attempt = %+v", attempt)
	}
}
