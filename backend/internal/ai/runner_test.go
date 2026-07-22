package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestServiceAnalyzeFailureReturnsResult(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"result","is_transient":false,"root_cause":"cause","severity":"Low","suggested_fix":"fix","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	service := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	service.EnableAgentic(AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	}, &fakeFactory{}, registry, enabled)
	request := FailureAnalysisRequest{
		JobID:       "job",
		BuildPrefix: "logs/job/1/",
		Build:       newRun("job", "1").BuildInfo,
		TestCase:    *newFailedTC("Test A", "failure"),
	}

	result, err := service.AnalyzeFailure(context.Background(), &http.Client{}, request)
	if err != nil {
		t.Fatalf("AnalyzeFailure() error = %v", err)
	}
	if result.Summary == nil || result.Summary.Summary != "result" {
		t.Fatalf("summary = %+v", result.Summary)
	}
	if result.Analysis == nil || result.Analysis.Mode != AgenticMode {
		t.Fatalf("analysis = %+v", result.Analysis)
	}
	if request.TestCase.AISummary != nil || request.TestCase.AIAnalysis != nil {
		t.Fatalf("request test case was mutated: %+v", request.TestCase)
	}
}

func TestServiceAnalyzeFailureReturnsUnavailableError(t *testing.T) {
	service := NewService(&Client{}, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	request := FailureAnalysisRequest{
		JobID:       "job",
		BuildPrefix: "logs/job/1/",
		Build:       newRun("job", "1").BuildInfo,
		TestCase:    *newFailedTC("Test A", "failure"),
	}

	result, err := service.AnalyzeFailure(context.Background(), &http.Client{}, request)
	if err == nil || !strings.Contains(err.Error(), "browser factory") {
		t.Fatalf("AnalyzeFailure() error = %v", err)
	}
	if result.Summary == nil || !strings.Contains(result.Summary.Summary, "AI analysis unavailable") {
		t.Fatalf("summary = %+v", result.Summary)
	}
	if result.Analysis != nil {
		t.Fatalf("analysis = %+v, want nil", result.Analysis)
	}
}

func TestServiceAnalyzeFailureClonesCachedResult(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	service := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	service.EnableAgentic(AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	}, &fakeFactory{}, registry, enabled)

	request := FailureAnalysisRequest{
		JobID:       "job",
		BuildPrefix: "logs/job/1/",
		Build: models.BuildInfo{
			JobName: "job", BuildID: "1", JUnitURLs: []string{"junit.xml"}, RepoRefs: map[string]string{"repo": "sha"},
		},
		TestCase: models.TestCase{
			Name: "Test A", Status: "failed", FailureMessage: "failure",
			AISummary: &models.AISummary{Summary: "cached"},
			AIAnalysis: &models.AIAnalysis{
				RootCause: "cached", Mode: AgenticMode,
				PromptHash: PromptFingerprint("sys"), ModelHash: client.modelFingerprint(),
				CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
				RelevantFiles: []string{"a.go"}, FileLinks: map[string]string{"a.go": "https://example.invalid/a.go"},
			},
		},
	}

	result, err := service.AnalyzeFailure(context.Background(), &http.Client{}, request)
	if err != nil {
		t.Fatalf("AnalyzeFailure() error = %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Fatalf("server calls = %d, want 0", got)
	}
	result.Summary.Summary = "changed"
	result.Analysis.RootCause = "changed"
	result.Analysis.RelevantFiles[0] = "changed.go"
	result.Analysis.FileLinks["a.go"] = "changed"
	if request.TestCase.AISummary.Summary != "cached" || request.TestCase.AIAnalysis.RootCause != "cached" {
		t.Fatalf("request output was mutated: %+v %+v", request.TestCase.AISummary, request.TestCase.AIAnalysis)
	}
	if request.TestCase.AIAnalysis.RelevantFiles[0] != "a.go" || request.TestCase.AIAnalysis.FileLinks["a.go"] != "https://example.invalid/a.go" {
		t.Fatalf("request analysis references were mutated: %+v", request.TestCase.AIAnalysis)
	}
}

func TestFailureAnalysisContractJSONRoundTrip(t *testing.T) {
	request := FailureAnalysisRequest{
		JobID:               "periodic-job",
		BuildPrefix:         "logs/periodic-job/1/",
		Build:               models.BuildInfo{JobName: "periodic-job", BuildID: "1"},
		TestCase:            models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "failure"},
		ConsecutiveFailures: 3,
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got FailureAnalysisRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, request) {
		t.Fatalf("request round trip = %+v, want %+v", got, request)
	}

	result := FailureAnalysisResult{
		Summary:  &models.AISummary{Summary: "summary"},
		Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", RelevantFiles: []string{"a.go"}},
	}
	data, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var resultGot FailureAnalysisResult
	if err := json.Unmarshal(data, &resultGot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resultGot, result) {
		t.Fatalf("result round trip = %+v, want %+v", resultGot, result)
	}
}

type contractProbeModule struct {
	consecutive int
}

func (*contractProbeModule) Name() string { return "probe" }

func (m *contractProbeModule) AnalysisPrompt(_ context.Context, _ *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	m.consecutive = consecutive
	run.JUnitURLs[0] = "changed.xml"
	run.RepoRefs["repo"] = "changed"
	tc.Name = "changed"
	return "user"
}

func TestServiceAnalyzeFailureCopiesRequestAndUsesConsecutiveCount(t *testing.T) {
	module := &contractProbeModule{}
	service := NewService(&Client{}, module, "sys", nil)
	request := FailureAnalysisRequest{
		JobID:       "job",
		BuildPrefix: "logs/job/1/",
		Build: models.BuildInfo{
			JobName: "job", BuildID: "1", JUnitURLs: []string{"junit.xml"}, RepoRefs: map[string]string{"repo": "sha"},
		},
		TestCase:            models.TestCase{Name: "Test A", Status: "failed"},
		ConsecutiveFailures: 4,
	}

	_, err := service.AnalyzeFailure(context.Background(), &http.Client{}, request)
	if err == nil || !strings.Contains(err.Error(), "browser factory") {
		t.Fatalf("AnalyzeFailure() error = %v", err)
	}
	if module.consecutive != 4 {
		t.Fatalf("consecutive failures = %d, want 4", module.consecutive)
	}
	if request.Build.JUnitURLs[0] != "junit.xml" || request.Build.RepoRefs["repo"] != "sha" || request.TestCase.Name != "Test A" {
		t.Fatalf("request was mutated: %+v", request)
	}

	module = &contractProbeModule{}
	service = NewService(&Client{}, module, "sys", nil)
	request.ConsecutiveFailures = 0
	_, _ = service.AnalyzeFailure(context.Background(), &http.Client{}, request)
	if module.consecutive != 1 {
		t.Fatalf("default consecutive failures = %d, want 1", module.consecutive)
	}
}

var _ Module = (*contractProbeModule)(nil)
