package remediation

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeCompare struct {
	contains bool
	status   string
	err      error
}

func (f fakeCompare) CompareCommits(context.Context, string, string, string, string) (bool, string, error) {
	return f.contains, f.status, f.err
}

type countingCompare struct {
	calls int
}

func (f *countingCompare) CompareCommits(context.Context, string, string, string, string) (bool, string, error) {
	f.calls++
	return false, "behind", nil
}

func periodicEvidence(failure string) Evidence {
	test := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class"}
	return Evidence{BuildWatermark: "10", Tests: []TestEvidence{{
		Identity: junit.Identity(test), ErrorHash: aggregator.HashError(aggregator.NormalizeErrorMessage(failure)),
	}}}
}

func TestObservePeriodic_VerifiesTwoCleanRuns(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "12", Commit: "c2", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed || attempt.Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicCachesNegativeAncestry(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom"),
	}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Commit: "before-merge", Result: "SUCCESS", JUnitComplete: true},
	}}}}
	client := &countingCompare{}
	for range 2 {
		if err := ObservePeriodic(context.Background(), client, remediation, attempt, details, 2); err != nil {
			t.Fatal(err)
		}
	}
	if client.calls != 1 {
		t.Fatalf("compare calls = %d, want 1", client.calls)
	}
	if attempt.IneligibleCommits["11"] != "before-merge" {
		t.Fatalf("ineligible commits = %+v", attempt.IneligibleCommits)
	}
}

func TestObservePeriodicNewerDifferentCauseOverridesOlderPasses(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom"),
	}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	different := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "different"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "13", Commit: "c3", Result: "FAILURE", JUnitComplete: true}, TestCases: []models.TestCase{different}},
		{BuildInfo: models.BuildInfo{BuildID: "12", Commit: "c2", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusFailingDifferentCause {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicVerifiesCleanRunsAfterOlderFailure(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom"),
	}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	different := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "different"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "13", Commit: "c3", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
		{BuildInfo: models.BuildInfo{BuildID: "12", Commit: "c2", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass}},
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "FAILURE", JUnitComplete: true}, TestCases: []models.TestCase{different}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodic_DetectsSameCause(t *testing.T) {
	failure := "boom 42"
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence(failure)}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	failed := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: failure}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "FAILURE", JUnitComplete: true}, TestCases: []models.TestCase{failed}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusStillFailingSameCause {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodic_RejectsTargetMismatch(t *testing.T) {
	remediation := &Remediation{JobType: models.JobTypePeriodic, SourceRepo: "upstream/repo"}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "fork/repo"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Outcome != OutcomeTargetRepoNotTested {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicPreservesVerifiedOutcomeWhenJobDataIsMissing(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom"),
	}
	attempt := &Attempt{
		Status: StatusVerifiedFixed, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r",
		Outcome: OutcomePassed, OutcomeReason: "2 clean post-merge runs",
	}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed || attempt.Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicPreservesVerifiedOutcomeWhenOldRunsAgeOut(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{
		Status: StatusVerifiedFixed, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r",
		Observations: []BuildObservation{
			{BuildID: "11", JobType: models.JobTypePeriodic, Outcome: OutcomePassed},
			{BuildID: "12", JobType: models.JobTypePeriodic, Outcome: OutcomePassed},
		},
	}
	details := []models.JobDetail{{JobID: "job", Name: "job"}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicMissingSourceCommitIsInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Result: "SUCCESS", Passed: true},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive || attempt.Outcome != OutcomeInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicInconclusiveObservationStaysInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Commit: "new", Result: "SUCCESS", Passed: true},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicMissingJobDetailIsInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r"}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicRejectsNonPrimaryRepoCommit(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "secondary/repo", CommitRepo: "primary/repo",
	}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "secondary/repo"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive || !strings.Contains(attempt.OutcomeReason, "primary/repo") {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicReclassifiesInconclusiveBuild(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{
		Status: StatusObserving, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r",
		Observations: []BuildObservation{{
			BuildID: "11", JobName: "job", JobType: models.JobTypePeriodic,
			SourceCommit: "new", Outcome: OutcomeInconclusive,
		}},
	}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Commit: "new", Result: "SUCCESS", Passed: true, JUnitComplete: true}, TestCases: []models.TestCase{pass},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 1); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed || attempt.Observations[0].Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicIncompleteJUnitCannotPassWithExpectedTestPresent(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Commit: "new", Result: "SUCCESS", Passed: true, JUnitComplete: false},
		TestCases: []models.TestCase{pass},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 1); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}
