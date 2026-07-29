package patterns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type scriptedPatternResult struct {
	raw string
	err error
}

type scriptedPatternAnalyzer struct {
	results map[string][]scriptedPatternResult
	calls   map[string]int
}

func (a *scriptedPatternAnalyzer) AnalyzePattern(_ context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	index := a.calls[jobID]
	a.calls[jobID]++
	results := a.results[jobID]
	if index >= len(results) {
		return nil, errors.New("unexpected pattern attempt")
	}
	result := results[index]
	if result.err != nil {
		return nil, result.err
	}
	return ai.ParsePatternResult(subject, failures, result.raw)
}

func TestAnalyzeRetriesOnlyEligiblePatternFailures(t *testing.T) {
	valid := validPatternResult("3", "2")
	ambiguous := valid + "\n" + strings.Replace(valid, "shared cause", "different cause", 1)
	trailingContract := valid + `\n{"systemic":true}`
	tests := []struct {
		name          string
		results       []scriptedPatternResult
		wantCalls     int
		wantCompleted int
		wantFailed    int
		wantRetries   int
		wantCategory  ai.PatternFailureCategory
	}{
		{
			name: "ambiguous then valid", results: []scriptedPatternResult{{raw: ambiguous}, {raw: valid}},
			wantCalls: 2, wantCompleted: 1, wantRetries: 1,
		},
		{
			name: "ambiguous twice", results: []scriptedPatternResult{{raw: ambiguous}, {raw: ambiguous}},
			wantCalls: 2, wantFailed: 1, wantRetries: 1, wantCategory: ai.PatternFailureAmbiguous,
		},
		{
			name: "request timeout then valid", results: []scriptedPatternResult{{err: &ai.PatternProviderError{StatusCode: 408}}, {raw: valid}},
			wantCalls: 2, wantCompleted: 1, wantRetries: 1,
		},
		{
			name: "request timeout then server failure", results: []scriptedPatternResult{{err: &ai.PatternProviderError{StatusCode: 408}}, {err: &ai.PatternProviderError{StatusCode: 503}}},
			wantCalls: 2, wantFailed: 1, wantRetries: 1, wantCategory: ai.PatternFailureProvider5xx,
		},
		{
			name: "rate limit then valid", results: []scriptedPatternResult{{err: &ai.PatternProviderError{StatusCode: 429}}, {raw: valid}},
			wantCalls: 2, wantCompleted: 1, wantRetries: 1,
		},
		{
			name: "server failure then valid", results: []scriptedPatternResult{{err: &ai.PatternProviderError{StatusCode: 500}}, {raw: valid}},
			wantCalls: 2, wantCompleted: 1, wantRetries: 1,
		},
		{
			name: "nonretryable provider error", results: []scriptedPatternResult{{err: &ai.PatternProviderError{StatusCode: 400}}, {raw: valid}},
			wantCalls: 1, wantFailed: 1, wantCategory: ai.PatternFailureProvider,
		},
		{
			name: "valid first response", results: []scriptedPatternResult{{raw: valid}, {raw: ambiguous}},
			wantCalls: 1, wantCompleted: 1,
		},
		{
			name: "schema invalid", results: []scriptedPatternResult{{raw: `{"systemic":true}`}, {raw: valid}},
			wantCalls: 1, wantFailed: 1, wantCategory: ai.PatternFailureSchema,
		},
		{
			name: "build mismatch", results: []scriptedPatternResult{{raw: validPatternResult("3", "unknown")}, {raw: valid}},
			wantCalls: 1, wantFailed: 1, wantCategory: ai.PatternFailureBuilds,
		},
		{
			name: "trailing contract candidate", results: []scriptedPatternResult{{raw: trailingContract}, {raw: valid}},
			wantCalls: 1, wantFailed: 1, wantCategory: ai.PatternFailureSchema,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			details := []models.JobDetail{eligibleJob("job-a")}
			analyzer := &scriptedPatternAnalyzer{results: map[string][]scriptedPatternResult{"job-a": testCase.results}}
			var planned int
			var attempts []Attempt
			stats, err := AnalyzeWithOptions(t.Context(), analyzer, details, AnalyzeOptions{
				OnPlan:    func(total int) { planned = total },
				OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
			})
			if planned != 1 || stats.Eligible != 1 || stats.Completed != testCase.wantCompleted || stats.Failed != testCase.wantFailed ||
				stats.Attempts != testCase.wantCalls || stats.Retries != testCase.wantRetries {
				t.Fatalf("planned=%d stats=%+v", planned, stats)
			}
			if analyzer.calls["job-a"] != testCase.wantCalls || len(attempts) != testCase.wantCalls {
				t.Fatalf("calls=%d attempts=%+v", analyzer.calls["job-a"], attempts)
			}
			if testCase.wantFailed == 0 {
				if err != nil || len(details[0].PatternAnalyses) != 1 || !attempts[len(attempts)-1].Succeeded || !attempts[len(attempts)-1].Final {
					t.Fatalf("error=%v patterns=%+v attempts=%+v", err, details[0].PatternAnalyses, attempts)
				}
			} else {
				if err == nil || len(details[0].PatternAnalyses) != 0 || ai.PatternFailureCategoryOf(err) != testCase.wantCategory {
					t.Fatalf("error=%v category=%s patterns=%+v", err, ai.PatternFailureCategoryOf(err), details[0].PatternAnalyses)
				}
				if !attempts[len(attempts)-1].Final || attempts[len(attempts)-1].FailureCategory != testCase.wantCategory {
					t.Fatalf("attempts=%+v", attempts)
				}
			}
			if testCase.wantRetries == 1 && (len(attempts) != 2 || attempts[0].Retry || attempts[0].Final || !attempts[1].Retry) {
				t.Fatalf("retry attempt sequence=%+v", attempts)
			}
		})
	}
}

func TestAnalyzeDoesNotRerunSuccessfulJobWhenAnotherFails(t *testing.T) {
	valid := validPatternResult("3", "2")
	ambiguous := valid + "\n" + strings.Replace(valid, "shared cause", "different cause", 1)
	details := []models.JobDetail{eligibleJob("job-a"), eligibleJob("job-b")}
	analyzer := &scriptedPatternAnalyzer{results: map[string][]scriptedPatternResult{
		"job-a": {{raw: valid}},
		"job-b": {{raw: ambiguous}, {raw: ambiguous}},
	}}
	stats, err := Analyze(t.Context(), analyzer, details)
	if err == nil || stats.Completed != 1 || stats.Failed != 1 || stats.Attempts != 3 || stats.Retries != 1 {
		t.Fatalf("stats=%+v error=%v", stats, err)
	}
	if analyzer.calls["job-a"] != 1 || analyzer.calls["job-b"] != 2 {
		t.Fatalf("calls=%v", analyzer.calls)
	}
	if len(details[0].PatternAnalyses) != 1 || len(details[1].PatternAnalyses) != 0 {
		t.Fatalf("job-a patterns=%+v job-b patterns=%+v", details[0].PatternAnalyses, details[1].PatternAnalyses)
	}
}

func validPatternResult(builds ...string) string {
	return fmt.Sprintf(`{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":%s,"suggested_fix":"update configuration","summary":"shared failure"}`,
		mustJSON(builds))
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
