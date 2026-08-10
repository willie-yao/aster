package patterns

import (
	"context"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type lifecycleAnalyzer struct {
	verification models.PatternRemediationVerification
	err          error
}

func (a *lifecycleAnalyzer) AnalyzePattern(_ context.Context, _ string, subject string, _ []ai.PatternFailure) (*models.PatternAnalysis, error) {
	return &models.PatternAnalysis{
		Subject: subject, Systemic: true, Confidence: "high", BuildsAnalyzed: 3,
		SharedRootCause: "cause", SharedBuilds: []string{"3", "2"}, SuggestedFix: "fix",
		RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentAddSymbol, Symbol: "Fix", Path: "fix.go"}},
		SourceRef:          "example/repo@0123456789abcdef0123456789abcdef01234567",
	}, nil
}

func (a *lifecycleAnalyzer) VerifyPatternRemediation(context.Context, models.PatternAnalysis, models.JobDetail) (models.PatternRemediationVerification, error) {
	return a.verification, a.err
}

func TestAnalyzeAppliesVerifiedFixedLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	detail := eligibleJob("job")
	for i := range detail.Runs {
		detail.Runs[i].Commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		detail.Runs[i].Started = base.Add(time.Duration(i) * time.Hour)
	}
	for index, buildID := range []string{"pass-1", "pass-2"} {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
			BuildID: buildID, Commit: "0123456789abcdef0123456789abcdef01234567", Passed: true, Started: base.Add(time.Duration(index+4) * time.Hour),
		}})
	}
	analyzer := &lifecycleAnalyzer{verification: models.PatternRemediationVerification{
		State: models.PatternRemediationAlreadyPresent, Reason: "present", Repository: "example/repo", Revision: "0123456789abcdef0123456789abcdef01234567", FailureState: models.PatternRemediationUnresolved, FailureBuilds: []string{"3", "2"}, PassingBuilds: []string{"pass-1", "pass-2"},
	}}
	details := []models.JobDetail{detail}
	if _, err := Analyze(t.Context(), analyzer, details); err != nil {
		t.Fatal(err)
	}
	pattern := details[0].PatternAnalyses[0]
	if pattern.RemediationVerification == nil || pattern.Lifecycle == nil || pattern.Lifecycle.State != models.PatternLifecycleVerifiedFixed {
		t.Fatalf("pattern = %+v", pattern)
	}
}

func TestAnalyzeVerificationFailureRemainsActive(t *testing.T) {
	details := []models.JobDetail{eligibleJob("job")}
	analyzer := &lifecycleAnalyzer{err: context.DeadlineExceeded}
	if _, err := Analyze(t.Context(), analyzer, details); err != nil {
		t.Fatal(err)
	}
	pattern := details[0].PatternAnalyses[0]
	if pattern.RemediationVerification == nil || pattern.RemediationVerification.State != models.PatternRemediationInconclusive || pattern.Lifecycle == nil || pattern.Lifecycle.State != models.PatternLifecycleActive {
		t.Fatalf("pattern = %+v", pattern)
	}
}
