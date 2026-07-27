package patterns

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type concurrentAnalyzer struct {
	started chan string
	release chan struct{}
}

type failingAnalyzer struct{}

func (failingAnalyzer) AnalyzePattern(context.Context, string, string, []ai.PatternFailure) (*models.PatternAnalysis, error) {
	return nil, errors.New("response validation failed (schema)")
}

func TestAnalyzeReturnsPatternFailures(t *testing.T) {
	details := []models.JobDetail{eligibleJob("job-a")}
	stats, err := Analyze(t.Context(), failingAnalyzer{}, details)
	if stats.Eligible != 1 || stats.Completed != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if err == nil || !strings.Contains(err.Error(), "job-a") {
		t.Fatalf("error = %v", err)
	}
	if len(details[0].PatternAnalyses) != 0 {
		t.Fatalf("patterns = %+v", details[0].PatternAnalyses)
	}
}

func (a *concurrentAnalyzer) AnalyzePattern(_ context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	a.started <- jobID
	<-a.release
	return &models.PatternAnalysis{
		Subject: subject, BuildsAnalyzed: len(failures), Systemic: true,
		Confidence: "high", SharedRootCause: "shared cause", Summary: "shared failure",
	}, nil
}

func TestAnalyzeConcurrentStartsAllEligibleJobs(t *testing.T) {
	analyzer := &concurrentAnalyzer{started: make(chan string, 2), release: make(chan struct{})}
	details := []models.JobDetail{eligibleJob("job-a"), eligibleJob("job-b")}
	done := make(chan AnalyzeStats, 1)
	go func() {
		done <- AnalyzeConcurrent(context.Background(), analyzer, details)
	}()

	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case jobID := <-analyzer.started:
			started[jobID] = true
		case <-time.After(time.Second):
			close(analyzer.release)
			t.Fatalf("started jobs = %v, want both jobs before either completes", started)
		}
	}
	close(analyzer.release)
	stats := <-done
	if stats.Eligible != 2 || stats.Completed != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for i := range details {
		if len(details[i].PatternAnalyses) != 1 {
			t.Fatalf("job %s patterns = %d, want 1", details[i].JobID, len(details[i].PatternAnalyses))
		}
	}
}

func eligibleJob(jobID string) models.JobDetail {
	detail := models.JobDetail{Name: jobID, JobID: jobID}
	for _, buildID := range []string{"3", "2", "1"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
			TestCases: []models.TestCase{{
				Name: "failed test", Status: "failed",
				AISummary:  &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic"},
			}},
		})
	}
	return detail
}

func TestAssignIDsBindsPatternContent(t *testing.T) {
	details := []models.JobDetail{{
		JobID: "periodic-x",
		PatternAnalyses: []models.PatternAnalysis{{
			JobID: "periodic-x", SharedRootCause: "retry failure", SuggestedFix: "bound retries",
		}},
	}}
	AssignIDs(details)
	pattern := details[0].PatternAnalyses[0]
	if pattern.ID != models.PatternID(pattern) || pattern.ContentHash != models.PatternHash(pattern) {
		t.Fatalf("assigned pattern identity = %+v", pattern)
	}
}
