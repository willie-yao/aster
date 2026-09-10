package patterns

import (
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestMergeLastGoodRetainsPersistedVerifiedFixedLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	current := eligibleJob("job")
	for i := range current.Runs {
		current.Runs[i].Commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		current.Runs[i].Started = base.Add(time.Duration(i) * time.Hour)
	}
	for index, buildID := range []string{"pass-1", "pass-2"} {
		current.Runs = append(current.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
			BuildID: buildID, Commit: "0123456789abcdef0123456789abcdef01234567", Result: "SUCCESS", Passed: true, Started: base.Add(time.Duration(index+4) * time.Hour),
		}})
	}
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-10T00:00:00Z", BuildsAnalyzed: 3,
		Systemic: true, Confidence: "high", SharedRootCause: "cause", SharedBuilds: []string{"3", "2"}, SuggestedFix: "fix",
		RemediationVerification: &models.PatternRemediationVerification{
			State: models.PatternRemediationAlreadyPresent, Reason: "present", Repository: "example/repo",
			Revision: "0123456789abcdef0123456789abcdef01234567", FailureState: models.PatternRemediationUnresolved,
			FailureBuilds: []string{"3", "2"}, PassingBuilds: []string{"pass-1", "pass-2"},
		},
	}
	models.ApplyPatternLifecycle(current, &priorPattern)
	models.AssignPatternIdentity(&priorPattern)

	details := []models.JobDetail{current}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	pattern := details[0].PatternAnalyses[0]
	if report.Retained != 1 || pattern.RemediationVerification == nil || pattern.Lifecycle == nil || pattern.Lifecycle.State != models.PatternLifecycleVerifiedFixed {
		t.Fatalf("report=%+v pattern=%+v", report, pattern)
	}
}

func TestMergeLastGoodRetainsPersistedInconclusiveLifecycle(t *testing.T) {
	current := eligibleJob("job")
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-10T00:00:00Z", BuildsAnalyzed: 3,
		Systemic: true, Confidence: "high", SharedRootCause: "cause", SharedBuilds: []string{"3", "2"}, SuggestedFix: "fix",
		RemediationVerification: &models.PatternRemediationVerification{
			State: models.PatternRemediationInconclusive, Reason: "Pinned source verification could not be completed.",
		},
	}
	models.ApplyPatternLifecycle(current, &priorPattern)
	models.AssignPatternIdentity(&priorPattern)

	details := []models.JobDetail{current}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	pattern := details[0].PatternAnalyses[0]
	if report.Retained != 1 || pattern.RemediationVerification == nil || pattern.Lifecycle == nil || pattern.Lifecycle.State != models.PatternLifecycleActive {
		t.Fatalf("report=%+v pattern=%+v", report, pattern)
	}
}
