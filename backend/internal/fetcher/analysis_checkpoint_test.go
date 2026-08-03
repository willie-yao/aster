package fetcher

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func TestSuccessfulPatternCacheSurvivesAnotherJobFailure(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["3","2"],"suggested_fix":"update configuration","remediation_targets":[{"intent":"investigate"}],"summary":"shared failure"}`
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": valid}}},
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		AI:      &project.AI{Agentic: project.Agentic{Tools: []string{"filesystem"}}},
		Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: bucketDir},
	}
	aiProject := &analysisruntime.Project{
		Config: cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: server.URL, Model: "checkpoint-test",
		},
		SystemPrompt: "test prompt",
	}
	runtime, err := analysisruntime.New(t.Context(), analysisruntime.Options{Token: "token", DataDir: dataDir, Project: aiProject})
	if err != nil {
		t.Fatal(err)
	}
	traces := ai.NewTraceStore()
	service, err := runtime.NewService(analysisruntime.ServiceOptions{Backend: backend, TraceStore: traces})
	if err != nil {
		t.Fatal(err)
	}
	details := []models.JobDetail{patternCheckpointJob("job-a"), patternCheckpointJob("job-b")}
	stats, patternErr := patterns.AnalyzeWithOptions(t.Context(), service, details, patterns.AnalyzeOptions{})
	if patternErr == nil || stats.Completed != 1 || stats.Failed != 1 {
		t.Fatalf("stats=%+v error=%v", stats, patternErr)
	}
	if err := (&pipeline{opts: Options{OutDir: dataDir}}).persistRuntimeAnalysisState(runtime, traces); err != nil {
		t.Fatal(err)
	}

	callsBeforeReload := calls.Load()
	reloaded, err := analysisruntime.New(t.Context(), analysisruntime.Options{Token: "token", DataDir: dataDir, Project: aiProject})
	if err != nil {
		t.Fatal(err)
	}
	reloadedService, err := reloaded.NewService(analysisruntime.ServiceOptions{Backend: backend, TraceStore: ai.NewTraceStore()})
	if err != nil {
		t.Fatal(err)
	}
	reloadedDetails := []models.JobDetail{patternCheckpointJob("job-a")}
	reloadedStats, err := patterns.AnalyzeWithOptions(t.Context(), reloadedService, reloadedDetails, patterns.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reloadedStats.CacheHits != 1 || reloadedStats.Completed != 1 || calls.Load() != callsBeforeReload {
		t.Fatalf("reloaded stats=%+v calls=%d before=%d", reloadedStats, calls.Load(), callsBeforeReload)
	}
}

func patternCheckpointJob(jobID string) models.JobDetail {
	detail := models.JobDetail{Name: jobID, JobID: jobID, JobType: models.JobTypePeriodic}
	for _, buildID := range []string{"3", "2", "1"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name: "failed test", Status: "failed", FailureMessage: fmt.Sprintf("failure %s", buildID),
				AISummary: &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{
					RootCause: "shared cause", SuggestedFix: "update configuration", Severity: "High", Mode: "agentic",
				},
			}},
		})
	}
	return detail
}
