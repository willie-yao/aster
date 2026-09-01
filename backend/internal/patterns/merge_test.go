package patterns

import (
	"fmt"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestMergeLastGoodMatrix(t *testing.T) {
	priorPattern := models.PatternAnalysis{Subject: "job", JobID: "job", GeneratedAt: "2026-07-28T00:00:00Z", BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"3", "2"}, SuggestedFix: "fix", Summary: "old"}
	models.AssignPatternIdentity(&priorPattern)
	prior := map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1, Suppressed: true}}}
	report, err := MergeLastGood(details, prior, result)
	if err != nil {
		t.Fatal(err)
	}
	retained := details[0].PatternAnalyses[0]
	if report.Retained != 1 || details[0].PatternRefresh.State != models.PatternRefreshRetained || details[0].PatternRefresh.Attempts != 1 ||
		retained.ID != priorPattern.ID || retained.SharedRootCause != priorPattern.SharedRootCause || retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleActive {
		t.Fatalf("report=%+v detail=%+v", report, details[0])
	}
}

func TestMergeLastGoodFreshNonSystemicRemovesPrior(t *testing.T) {
	priorPattern := models.PatternAnalysis{Subject: "job", JobID: "job", Systemic: true, SharedRootCause: "old", SuggestedFix: "fix", Summary: "old"}
	models.AssignPatternIdentity(&priorPattern)
	details := []models.JobDetail{eligibleJob("job")}
	details[0].PatternAnalyses = []models.PatternAnalysis{{Subject: "job", JobID: "job", GeneratedAt: "now", Systemic: false, Confidence: "low", Summary: "unrelated"}}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Succeeded: true}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current != 1 || len(CurrentRecurring(details)) != 0 || details[0].PatternAnalyses[0].ID == priorPattern.ID {
		t.Fatalf("report=%+v detail=%+v", report, details[0])
	}
}

func TestMergeLastGoodRejectsCorruptPriorIdentity(t *testing.T) {
	prior := models.PatternAnalysis{ID: "pattern", ContentHash: "wrong", JobID: "job", Systemic: true}
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{prior}}}, result); err == nil {
		t.Fatal("corrupt prior identity was accepted")
	}
}

func TestMergeLastGoodMarksMissingRetainedEvidence(t *testing.T) {
	prior := models.PatternAnalysis{JobID: "job", GeneratedAt: "old", Systemic: true, SharedBuilds: []string{"999"}, Summary: "old"}
	models.AssignPatternIdentity(&prior)
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{prior}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retained != 1 || details[0].PatternRefresh.EvidenceAvailable {
		t.Fatalf("report=%+v status=%+v", report, details[0].PatternRefresh)
	}
}

func TestMergeLastGoodRejectsCrossJobPriorPattern(t *testing.T) {
	prior := models.PatternAnalysis{JobID: "other-job", Systemic: true, Summary: "old"}
	models.AssignPatternIdentity(&prior)
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{prior}}}, result); err == nil {
		t.Fatal("cross-job prior pattern was accepted")
	}
}

func TestMergeLastGoodRejectsNonCanonicalPriorID(t *testing.T) {
	prior := models.PatternAnalysis{ID: "arbitrary", JobID: "job", Systemic: true, Summary: "old"}
	prior.ContentHash = models.PatternHash(prior)
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{prior}}}, result); err == nil {
		t.Fatal("non-canonical prior ID was accepted")
	}
}

func TestCurrentRecurringExcludesInactiveLifecycle(t *testing.T) {
	details := []models.JobDetail{{
		JobID: "job", PatternRefresh: &models.PatternRefreshStatus{State: models.PatternRefreshCurrent},
		PatternAnalyses: []models.PatternAnalysis{
			{Systemic: true},
			{Systemic: true, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleActive}},
			{Systemic: true, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleRecovered}},
			{Systemic: true, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleObserving}},
			{Systemic: true, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleVerifiedFixed}},
		},
	}}
	got := CurrentRecurring(details)
	if len(got) != 2 {
		t.Fatalf("current recurring = %+v", got)
	}
}

func TestMergeLastGoodRetainedPatternRefreshesObservationRecovery(t *testing.T) {
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-01T00:00:00Z", BuildsAnalyzed: 5,
		Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
		SuggestedFix: "fix", Summary: "old",
	}
	models.ApplyPatternLifecycle(recoveryMergeDetail(0, false), &priorPattern)
	models.AssignPatternIdentity(&priorPattern)
	priorHash := priorPattern.ContentHash
	details := []models.JobDetail{recoveryMergeDetail(3, false)}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}

	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	retained := details[0].PatternAnalyses[0]
	if report.Retained != 1 || retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleRecovered || retained.Lifecycle.RecoveryStreak != 3 || retained.ID != priorPattern.ID || retained.ContentHash == priorHash {
		t.Fatalf("report=%+v retained=%+v", report, retained)
	}
	if got := CollectRecurring(details); len(got) != 0 {
		t.Fatalf("recovered retained pattern surfaced as active: %+v", got)
	}
}

func TestMergeLastGoodRetainedRecoveredPatternReactivatesOnNewFailure(t *testing.T) {
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-01T00:00:00Z", BuildsAnalyzed: 5,
		Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
		SuggestedFix: "fix", Summary: "old",
	}
	models.ApplyPatternLifecycle(recoveryMergeDetail(5, false), &priorPattern)
	models.AssignPatternIdentity(&priorPattern)
	if priorPattern.Lifecycle == nil || priorPattern.Lifecycle.State != models.PatternLifecycleRecovered {
		t.Fatalf("prior lifecycle = %+v", priorPattern.Lifecycle)
	}
	details := []models.JobDetail{recoveryMergeDetail(5, true)}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}

	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result); err != nil {
		t.Fatal(err)
	}
	retained := details[0].PatternAnalyses[0]
	if retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleActive || retained.Lifecycle.RecoveryStreak != 0 || retained.ID != priorPattern.ID {
		t.Fatalf("retained = %+v", retained)
	}
}

func recoveryMergeDetail(passingRuns int, newerFailure bool) models.JobDetail {
	detail := models.JobDetail{Name: "job", JobID: "job"}
	if newerFailure {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "new-failure", Result: "FAILURE"}})
	}
	for index := passingRuns; index >= 1; index-- {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
			BuildID: fmt.Sprintf("pass-%d", index), Result: "SUCCESS", Passed: true,
		}})
	}
	for index := 5; index >= 1; index-- {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: fmt.Sprintf("failure-%d", index), Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name: "failed test", Status: "failed",
				AISummary:  &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic", Disposition: models.AnalysisDispositionCitationsVerified},
			}},
		})
	}
	return detail
}

func TestMergeLastGoodKeepsRecoveredPatternHistoryAfterFailuresLeaveWindow(t *testing.T) {
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-01T00:00:00Z", BuildsAnalyzed: 5,
		Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
		SuggestedFix: "fix", Summary: "old",
	}
	models.ApplyPatternLifecycle(recoveryMergeDetail(5, false), &priorPattern)
	models.AssignPatternIdentity(&priorPattern)
	detail := models.JobDetail{Name: "job", JobID: "job"}
	for index := 5; index >= 1; index-- {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
			BuildID: fmt.Sprintf("new-pass-%d", index), Result: "SUCCESS", Passed: true,
		}})
	}
	details := []models.JobDetail{detail}

	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, AnalyzeResult{})
	if err != nil {
		t.Fatal(err)
	}
	retained := details[0].PatternAnalyses[0]
	if report.Retained != 1 || report.NotApplicable != 0 || details[0].PatternRefresh.State != models.PatternRefreshRetained || retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleRecovered || retained.Lifecycle.RecoveryStreak < 5 {
		t.Fatalf("report=%+v retained=%+v", report, retained)
	}
}

func TestMergeLastGoodReactivatesRecoveredHistoryAfterFailuresLeaveWindow(t *testing.T) {
	priorPattern := models.PatternAnalysis{
		Subject: "job", JobID: "job", GeneratedAt: "2026-08-01T00:00:00Z", BuildsAnalyzed: 5,
		Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
		SuggestedFix: "fix", Summary: "old",
	}
	models.ApplyPatternLifecycle(recoveryMergeDetail(5, false), &priorPattern)
	models.AssignPatternIdentity(&priorPattern)
	details := []models.JobDetail{{
		Name: "job", JobID: "job", Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{BuildID: "new-failure", Result: "FAILURE"}}},
	}}

	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, AnalyzeResult{})
	if err != nil {
		t.Fatal(err)
	}
	retained := details[0].PatternAnalyses[0]
	if report.Retained != 1 || retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleActive || retained.Lifecycle.RecoveryStreak != 0 {
		t.Fatalf("report=%+v retained=%+v", report, retained)
	}
	if got := CollectRecurring(details); len(got) != 1 {
		t.Fatalf("reactivated retained pattern was not surfaced: %+v", got)
	}
}

func TestMergeLastGoodRetainedSourceLifecycleReactivatesOnNewFailure(t *testing.T) {
	for _, test := range []struct {
		name          string
		passingBuilds []string
		wantPrior     models.PatternLifecycleState
	}{
		{name: "observing", passingBuilds: []string{"pass-1"}, wantPrior: models.PatternLifecycleObserving},
		{name: "verified fixed", passingBuilds: []string{"pass-1", "pass-2"}, wantPrior: models.PatternLifecycleVerifiedFixed},
	} {
		t.Run(test.name, func(t *testing.T) {
			sharedBuilds := []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"}
			priorPattern := models.PatternAnalysis{
				Subject: "job", JobID: "job", GeneratedAt: "2026-08-01T00:00:00Z", BuildsAnalyzed: 5,
				Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: sharedBuilds,
				SuggestedFix: "fix", Summary: "old",
				RemediationVerification: &models.PatternRemediationVerification{
					State: models.PatternRemediationAlreadyPresent, Repository: "example/repo",
					Revision: "0123456789abcdef0123456789abcdef01234567", FailureState: models.PatternRemediationUnresolved,
					FailureBuilds: append([]string(nil), sharedBuilds...), PassingBuilds: append([]string(nil), test.passingBuilds...),
				},
			}
			models.ApplyPatternLifecycle(recoveryMergeDetail(5, false), &priorPattern)
			models.AssignPatternIdentity(&priorPattern)
			if priorPattern.Lifecycle == nil || priorPattern.Lifecycle.State != test.wantPrior {
				t.Fatalf("prior lifecycle = %+v", priorPattern.Lifecycle)
			}
			details := []models.JobDetail{recoveryMergeDetail(5, true)}
			result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}

			report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
			if err != nil {
				t.Fatal(err)
			}
			retained := details[0].PatternAnalyses[0]
			if report.Retained != 1 || retained.Lifecycle == nil || retained.Lifecycle.State != models.PatternLifecycleActive || retained.Lifecycle.RecoveryStreak != 0 || len(retained.Lifecycle.PassingBuilds) != 0 {
				t.Fatalf("report=%+v retained=%+v", report, retained)
			}
		})
	}
}
