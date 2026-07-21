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
	if got.EventCount != 5 || got.ToolCalls != 1 || !got.TimelineVerified || got.ElapsedMs != 4000 || got.TaskOutcome != "succeeded" {
		t.Fatalf("telemetry = %+v", got)
	}
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.Provider != "openai" || got.Model != "model" || got.ModelRequests != 1 {
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
	valid := withValidation(analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"})
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded"}, "task", 2, 0, "", testValidationKey); err != nil {
		t.Fatalf("valid non-transient analysis rejected: %v", err)
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 1, ValidationPassed: true, TaskOutcome: "succeeded"}, "task", 2, 0, "", testValidationKey); err == nil {
		t.Fatal("analysis below the tool-call floor was accepted")
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, TaskOutcome: "succeeded"}, "task", 2, 0, "", testValidationKey); err == nil {
		t.Fatal("analysis without validate_analysis was accepted")
	}
	valid.IsTransient = &transient
	valid = withValidation(valid)
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded"}, "task", 2, 0, "", testValidationKey); err == nil {
		t.Fatal("transient analysis without verify_timeline was accepted")
	}
	if err := validateAnalysisAcceptance(valid, analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TimelineVerified: true, TaskOutcome: "succeeded"}, "task", 2, 0, "", testValidationKey); err != nil {
		t.Fatalf("timeline-verified transient analysis rejected: %v", err)
	}
}

func TestParseAnalysisRequiresCompleteSchema(t *testing.T) {
	cases := []string{
		`{"root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"}`,
		`{"summary":"summary","root_cause":"cause","severity":"Unknown","is_transient":false,"suggested_fix":"fix"}`,
		`{"summary":"summary","root_cause":"cause","severity":"High","suggested_fix":"fix"}`,
		`{"summary":"summary","root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"}`,
	}
	for _, input := range cases {
		if _, err := parseAnalysis(input); err == nil {
			t.Errorf("incomplete analysis accepted: %s", input)
		}
	}
}

func TestWebhookTelemetryUnavailableIsRetryable(t *testing.T) {
	nonTransient := false
	result := validatedAnalysisJSON(t, analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			http.Error(w, "events warming", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", "v1", 2)
	manifest.ValidationKey = testValidationKey
	s := &webhookServer{client: &orkaClient{base: server.URL, http: server.Client()}, namespace: "orka-system"}
	patch := s.preparePatch(webhookPayload{TaskName: "task", Phase: "Succeeded"}, manifest)
	if !patch.retry || !strings.Contains(patch.reason, "telemetry unavailable") {
		t.Fatalf("patch = %+v, want retryable telemetry error", patch)
	}
}

func TestSummarizeEventsRecordsFailuresRetriesAndContext(t *testing.T) {
	base := time.Date(2026, 7, 18, 1, 0, 0, 0, time.UTC)
	events := []executionEvent{
		{Seq: 1, Type: "TaskStarted", CreatedAt: base},
		{Seq: 2, Type: "ModelRequestFailed", Provider: "openai", Model: "model", InputTokens: 7, StopReason: "provider_error", CreatedAt: base.Add(time.Second)},
		{Seq: 3, Type: "ContextTruncated", CreatedAt: base.Add(2 * time.Second)},
		{Seq: 4, Type: "ToolCallStarted", ToolName: "read-artifact", ToolCallID: "call-1", CreatedAt: base.Add(3 * time.Second)},
		{Seq: 5, Type: "ToolCallFailed", ToolName: "read-artifact", ToolCallID: "call-1", CreatedAt: base.Add(4 * time.Second)},
		{Seq: 6, Type: "TaskStarted", CreatedAt: base.Add(5 * time.Second)},
		{Seq: 7, Type: "ToolCallStarted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2", CreatedAt: base.Add(6 * time.Second)},
		{Seq: 8, Type: "ToolCallCompleted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2", Content: json.RawMessage(`{"resultLength":123}`), CreatedAt: base.Add(7 * time.Second)},
		{Seq: 9, Type: "ModelRequestCompleted", Provider: "openai", Model: "model", InputTokens: 11, OutputTokens: 5, StopReason: "stop", CreatedAt: base.Add(8 * time.Second)},
		{Seq: 10, Type: "TaskSucceeded", CreatedAt: base.Add(9 * time.Second)},
	}
	got := summarizeEvents(events)
	if got.TaskRetries != 1 || got.TaskOutcome != "succeeded" || got.ElapsedMs != 9000 {
		t.Fatalf("task telemetry = %+v", got)
	}
	if got.ToolCalls != 2 || got.ToolFailures != 1 || got.ContextBytes != 123 || got.ContextTruncations != 1 {
		t.Fatalf("tool telemetry = %+v", got)
	}
	if got.ModelRequests != 2 || got.ModelFailures != 1 || got.InputTokens != 18 || got.OutputTokens != 5 || got.StopReason != "stop" {
		t.Fatalf("model telemetry = %+v", got)
	}
}

func TestValidateAnalysisAcceptanceQualityToolPolicy(t *testing.T) {
	transient := false
	a := withValidation(analysis{
		Summary: "summary", RootCause: "cause", Severity: "High",
		IsTransient: &transient, SuggestedFix: "fix",
	})
	advisory := analysisTelemetry{
		EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded",
		qualityToolOutcomes: map[string]string{"recurrence": "failed", "submit_analysis": "completed"},
	}
	if err := validateAnalysisAcceptance(a, advisory, "task", 2, 0, "", testValidationKey); err != nil {
		t.Fatalf("advisory quality-tool failure rejected: %v", err)
	}
	required := advisory
	required.qualityToolOutcomes = map[string]string{
		"required_evidence": "failed",
		"submit_analysis":   "completed",
	}
	if err := validateAnalysisAcceptance(a, required, "task", 2, 0, "skills", testValidationKey); err == nil || !strings.Contains(err.Error(), "required_evidence") {
		t.Fatalf("required quality-tool failure error = %v", err)
	}
}

func TestValidateAnalysisAcceptanceRequiresSucceededEvent(t *testing.T) {
	transient := false
	a := withValidation(analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"})
	base := analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true}
	if err := validateAnalysisAcceptance(a, base, "task", 2, 0, "", testValidationKey); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("missing terminal error = %v", err)
	}
	base.TaskOutcome = "failed"
	if err := validateAnalysisAcceptance(a, base, "task", 2, 0, "", testValidationKey); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failed task error = %v", err)
	}
}

func TestValidateAnalysisAcceptanceRequiresConsumerEvidence(t *testing.T) {
	transient := false
	a := withValidation(analysis{Summary: "summary", RootCause: "quota exceeded", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"})
	events := []executionEvent{
		{Seq: 1, Type: "TaskStarted"},
		{Seq: 2, Type: "ToolCallStarted", ToolName: "read-artifact", ToolCallID: "call-1"},
		{Seq: 3, Type: "ToolCallStarted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2"},
		{Seq: 4, Type: "ToolCallCompleted", ToolName: "validate-analysis-bscope", ToolCallID: "call-2"},
		{Seq: 5, Type: "TaskSucceeded"},
	}
	if err := validateAnalysisAcceptance(a, summarizeEvents(events), "task", 2, 0, "skills-hash", testValidationKey); err == nil || !strings.Contains(err.Error(), "required_evidence") {
		t.Fatalf("acceptance error = %v", err)
	}
	events = append(events[:4],
		executionEvent{Seq: 5, Type: "ToolCallStarted", ToolName: "required-evidence-bscope", ToolCallID: "call-3"},
		executionEvent{Seq: 6, Type: "ToolCallCompleted", ToolName: "required-evidence-bscope", ToolCallID: "call-3"},
		executionEvent{Seq: 7, Type: "TaskSucceeded"},
	)
	if err := validateAnalysisAcceptance(a, summarizeEvents(events), "task", 2, 0, "skills-hash", testValidationKey); err != nil {
		t.Fatalf("consumer evidence call rejected: %v", err)
	}
}

func TestValidateAnalysisAcceptanceRejectsMismatchedValidationToken(t *testing.T) {
	transient := false
	a := withValidation(analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"})
	a.RootCause = "changed after validation"
	telemetry := analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded"}
	if err := validateAnalysisAcceptance(a, telemetry, "task", 2, 0, "", testValidationKey); err == nil || !strings.Contains(err.Error(), "validation_token") {
		t.Fatalf("acceptance error = %v", err)
	}
}

func TestValidateAnalysisAcceptanceRejectsTokenRecomputedWithoutKey(t *testing.T) {
	transient := false
	a := analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"}
	zero := 0
	a.GCSBytes = &zero
	a.ValidationToken = orkaapi.AnalysisValidationToken("attacker-key", "task", a.validationInput(), 0)
	telemetry := analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded"}
	if err := validateAnalysisAcceptance(a, telemetry, "task", 2, 0, "", testValidationKey); err == nil || !strings.Contains(err.Error(), "validation_token") {
		t.Fatalf("acceptance error = %v, want keyed validation rejection", err)
	}
}

func TestAnalysisTelemetryRetriesEmptyPageBeforeLatestSequence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": 5, "events": []any{}})
	}))
	defer server.Close()
	client := &orkaClient{base: server.URL, http: server.Client()}
	if _, err := client.analysisTelemetry(context.Background(), "orka-system", "task"); err == nil || !strings.Contains(err.Error(), "not readable yet") {
		t.Fatalf("telemetry error = %v", err)
	}
}

func TestValidateAnalysisAcceptanceEnforcesMinimumGCSBytes(t *testing.T) {
	transient := false
	a := withValidation(analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"})
	telemetry := analysisTelemetry{EventCount: 4, ToolCalls: 2, ValidationPassed: true, TaskOutcome: "succeeded"}
	if err := validateAnalysisAcceptance(a, telemetry, "task", 2, 1, "", testValidationKey); err == nil || !strings.Contains(err.Error(), "GCS byte") {
		t.Fatalf("acceptance error = %v, want minimum GCS byte rejection", err)
	}
}

func TestQualityToolBaseRecognizesTaskScopedValidator(t *testing.T) {
	name := "validate-analysis-az-analysis-3929c1698d436de980b5888747ca973f"
	if got := qualityToolBase(normalizeToolName(name)); got != "validate_analysis" {
		t.Fatalf("qualityToolBase(%q) = %q, want validate_analysis", name, got)
	}
}

func TestSubmitAnalysisCountsAsValidation(t *testing.T) {
	events := []executionEvent{
		{Seq: 1, Type: "ToolCallStarted", ToolName: "submit-analysis-cmp-task", ToolCallID: "call-1"},
		{Seq: 2, Type: "ToolCallCompleted", ToolName: "submit-analysis-cmp-task", ToolCallID: "call-1"},
	}
	got := summarizeEvents(events)
	if !got.ValidationPassed || got.qualityToolOutcomes["submit_analysis"] != "completed" {
		t.Fatalf("submission telemetry = %+v", got)
	}
	if base := qualityToolBase(normalizeToolName("submit-analysis-cmp-task")); base != "submit_analysis" {
		t.Fatalf("qualityToolBase() = %q", base)
	}
}
