package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	orkaapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

func TestAnalysisTelemetryPaginatesAndSummarizes(t *testing.T) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		var events []map[string]any
		switch after {
		case 0:
			events = []map[string]any{
				{"seq": 1, "type": "TaskStarted", "createdAt": base},
				{"seq": 2, "type": "ToolCallStarted", "toolName": "verify-timeline-bscope", "toolCallID": "call-1", "createdAt": base.Add(time.Second)},
			}
		case 2:
			events = []map[string]any{
				{"seq": 3, "type": "ToolCallCompleted", "toolName": "verify-timeline-bscope", "toolCallID": "call-1", "createdAt": base.Add(2 * time.Second)},
				{"seq": 4, "type": "ModelRequestCompleted", "provider": "openai", "model": "model", "inputTokens": 10, "outputTokens": 5, "createdAt": base.Add(3 * time.Second)},
				{"seq": 5, "type": "TaskSucceeded", "createdAt": base.Add(4 * time.Second)},
			}
		default:
			t.Fatalf("unexpected after cursor %d", after)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": 5, "events": events})
	}))
	defer server.Close()

	client := &orkaClient{base: server.URL, http: server.Client()}
	got, err := client.analysisTelemetry(context.Background(), "orka-system", "task")
	if err != nil {
		t.Fatal(err)
	}
	if got.EventCount != 5 || got.ToolCalls != 1 || !got.TimelineVerified || got.ElapsedMs != 4000 {
		t.Fatalf("telemetry = %+v", got)
	}
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.Provider != "openai" || got.Model != "model" {
		t.Fatalf("model telemetry = %+v", got)
	}
}

func TestSummarizeEventsRequiresCompletedValidation(t *testing.T) {
	events := []executionEvent{
		{Seq: 1, Type: "ToolCallStarted", ToolName: "validate-analysis-bscope", ToolCallID: "call-1"},
		{Seq: 2, Type: "ToolCallFailed", ToolName: "validate-analysis-bscope", ToolCallID: "call-1"},
	}
	if got := summarizeEvents(events); got.ValidationPassed {
		t.Fatal("failed validate_analysis call counted as passed")
	}
	events = append(events,
		executionEvent{Seq: 3, Type: "ToolCallStarted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2"},
		executionEvent{Seq: 4, Type: "ToolCallCompleted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2"},
	)
	if got := summarizeEvents(events); !got.ValidationPassed {
		t.Fatal("completed validate_analysis call did not count as passed")
	}
}

func TestValidateAnalysisAcceptance(t *testing.T) {
	transient, nonTransient := true, false
	valid := analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true}, 2); err != nil {
		t.Fatalf("valid non-transient analysis rejected: %v", err)
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 1, ValidationPassed: true}, 2); err == nil {
		t.Fatal("analysis below the tool-call floor was accepted")
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2}, 2); err == nil {
		t.Fatal("analysis without validate_analysis was accepted")
	}
	valid.IsTransient = &transient
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true}, 2); err == nil {
		t.Fatal("transient analysis without verify_timeline was accepted")
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TimelineVerified: true}, 2); err != nil {
		t.Fatalf("timeline-verified transient analysis rejected: %v", err)
	}
}

func TestParseAnalysisRequiresCompleteSchema(t *testing.T) {
	cases := []string{
		`{"root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"}`,
		`{"summary":"summary","root_cause":"cause","severity":"Unknown","is_transient":false,"suggested_fix":"fix"}`,
		`{"summary":"summary","root_cause":"cause","severity":"High","suggested_fix":"fix"}`,
	}
	for _, input := range cases {
		if _, err := parseAnalysis(input); err == nil {
			t.Errorf("incomplete analysis accepted: %s", input)
		}
	}
}

func TestWebhookTelemetryUnavailableIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			http.Error(w, "events warming", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"summary":"summary","root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"}`})
	}))
	defer server.Close()
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", "v1", 2)
	s := &webhookServer{client: &orkaClient{base: server.URL, http: server.Client()}, namespace: "orka-system"}
	patch := s.preparePatch(webhookPayload{TaskName: "task", Phase: "Succeeded"}, manifest)
	if !patch.retry || !strings.Contains(patch.reason, "telemetry unavailable") {
		t.Fatalf("patch = %+v, want retryable telemetry error", patch)
	}
}
