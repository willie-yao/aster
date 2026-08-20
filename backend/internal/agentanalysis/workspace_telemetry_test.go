package agentanalysis

import (
	"strings"
	"testing"
)

func validRequestShapeForTest() WorkspaceOpenCodeRequestShape {
	return WorkspaceOpenCodeRequestShape{
		Available:                  true,
		StreamingMode:              "streaming",
		ModelID:                    "claude-sonnet-4.6",
		SystemPromptBytesAvailable: true,
		SystemPromptBytes:          100,
		UserPromptBytes:            200,
		ToolSchemaAvailable:        true,
		ToolCount:                  5,
		ToolSchemaSHA256:           strings.Repeat("a", 64),
		ResponseSchemaSHA256:       strings.Repeat("b", 64),
		ToolChoiceMode:             "required",
		ContextLimit:               200000,
		OutputTokenLimit:           8192,
		OpenCodeVersion:            "1.18.2",
	}
}

func TestValidateWorkspaceOpenCodeTelemetryAllowsFirstOperationFailure(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Available:                    true,
		Status:                       WorkspaceTelemetryAvailable,
		ProviderCredentialMode:       "direct",
		ProviderAPI:                  "responses",
		ProviderReasoningEffort:      "high",
		EventCount:                   1,
		ProviderRequests:             1,
		ProviderRequestsKnown:        true,
		RequestShape:                 validRequestShapeForTest(),
		Error:                        WorkspaceOpenCodeErrorTelemetry{Available: true, Name: "APIError", HTTPStatusCode: 429, RetryableKnown: true, Retryable: true, Classification: "api_rate_limited"},
		StructuredOutputRetriesKnown: true,
		FailureCode:                  "api_rate_limited",
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkspaceOpenCodeTelemetryAllowsFailureFactsWithoutMessageHistory(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Status:           WorkspaceTelemetryUnavailable,
		ProviderRequests: 1, ProviderRequestsKnown: true,
		RequestShape: validRequestShapeForTest(),
		Error: WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "APIError", RetryableKnown: true, Retryable: true,
			Classification: "header_timeout", MetadataCode: "ProviderHeaderTimeoutError", HeaderTimeout: true,
		},
		StructuredOutputRetriesKnown: true,
		FailureCode:                  "header_timeout",
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkspaceOpenCodeTelemetryAllowsUnknownRequestStage(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Available:        true,
		Status:           WorkspaceTelemetryAvailable,
		EventCount:       1,
		ProviderRequests: 0, ProviderRequestsKnown: false,
		RequestShape: validRequestShapeForTest(),
		Error: WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "UnknownError", Classification: "database",
			MessagePresent: true, MessageBytes: 32, RedactedMessageSHA256: strings.Repeat("c", 64),
		},
		StructuredOutputRetriesKnown: true,
		FailureCode:                  "database",
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkspaceOpenCodeTelemetryRejectsFalseBeforeProviderWithUnknownRequestCount(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Available: true, Status: WorkspaceTelemetryAvailable, EventCount: 1,
		RequestShape: validRequestShapeForTest(), StructuredOutputRetriesKnown: true,
		Error: WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "UnknownError", Classification: "unknown",
			MessagePresent: true, RedactedMessageSHA256: strings.Repeat("e", 64),
			BeforeProviderRequest: boolPointer(false),
		},
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err == nil {
		t.Fatal("contradictory request lifecycle was accepted")
	}
}

func TestValidateWorkspaceOpenCodeTelemetryRejectsUnsafeOrInconsistentFields(t *testing.T) {
	base := WorkspaceOpenCodeTelemetry{
		Available: true, Status: WorkspaceTelemetryAvailable, EventCount: 1, ProviderRequests: 1, ProviderRequestsKnown: true,
		RequestShape: validRequestShapeForTest(), StructuredOutputRetriesKnown: true,
		Error: WorkspaceOpenCodeErrorTelemetry{Available: true, Name: "APIError", RetryableKnown: true, Classification: "api_error"},
	}
	for name, mutate := range map[string]func(*WorkspaceOpenCodeTelemetry){
		"model oversized":  func(value *WorkspaceOpenCodeTelemetry) { value.RequestShape.ModelID = strings.Repeat("x", 257) },
		"bad tool digest":  func(value *WorkspaceOpenCodeTelemetry) { value.RequestShape.ToolSchemaSHA256 = "secret" },
		"unknown metadata": func(value *WorkspaceOpenCodeTelemetry) { value.Error.MetadataCode = "provider-secret" },
		"unknown cause":    func(value *WorkspaceOpenCodeTelemetry) { value.Error.CauseCode = "PRIVATE_CODE" },
		"message on API error": func(value *WorkspaceOpenCodeTelemetry) {
			value.Error.MessagePresent = true
			value.Error.MessageBytes = 1
			value.Error.RedactedMessageSHA256 = strings.Repeat("d", 64)
		},
		"status out of range": func(value *WorkspaceOpenCodeTelemetry) { value.Error.HTTPStatusCode = 999 },
		"retryable unknown": func(value *WorkspaceOpenCodeTelemetry) {
			value.Error.RetryableKnown = false
			value.Error.Retryable = true
		},
		"context mismatch": func(value *WorkspaceOpenCodeTelemetry) {
			value.Error = WorkspaceOpenCodeErrorTelemetry{Available: true, Name: "ContextOverflowError", Classification: "context_overflow", ContextOverflow: true}
		},
		"request undercount": func(value *WorkspaceOpenCodeTelemetry) { value.StepsUsed = 2 },
		"request count unknown": func(value *WorkspaceOpenCodeTelemetry) {
			value.ProviderRequestsKnown = false
		},
		"invalid reasoning effort":          func(value *WorkspaceOpenCodeTelemetry) { value.ProviderReasoningEffort = "ultra" },
		"reasoning effort without provider": func(value *WorkspaceOpenCodeTelemetry) { value.ProviderReasoningEffort = "high" },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := validateWorkspaceOpenCodeTelemetry(value); err == nil {
				t.Fatalf("invalid telemetry was accepted: %+v", value)
			}
		})
	}
}

func TestValidateWorkspaceOpenCodeTelemetryAllowsRejectedEvidenceDiagnosticsWhenUnavailable(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Status: WorkspaceTelemetryMalformed, RequestShape: validRequestShapeForTest(), StructuredOutputRetriesKnown: true,
		EvidenceHandles: WorkspaceEvidenceHandleDiagnostics{
			Status: WorkspaceEvidenceHandlesRejected, ObservedRangeCount: maxWorkspaceEvidenceRanges + 1,
			DroppedRangeCount: maxWorkspaceEvidenceRanges + 1, Truncated: true, Codes: []string{WorkspaceEvidenceRangeOverflow},
		},
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err != nil {
		t.Fatal(err)
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func TestValidateWorkspaceOpenCodeTelemetryAllowsBoundedEvidenceExhaustion(t *testing.T) {
	telemetry := WorkspaceOpenCodeTelemetry{
		Available: true, Status: WorkspaceTelemetryAvailable, EventCount: 20,
		ProviderRequests: 17, ProviderRequestsKnown: true, StepsUsed: 16,
		RequestShape: validRequestShapeForTest(), StructuredOutputRetriesKnown: true,
		Error: WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "APIError", HTTPStatusCode: 400,
			RetryableKnown: true, Classification: "api_bad_request",
		},
		Tools:                  []WorkspaceToolTelemetry{{Name: "read", Count: 2}, {Name: "StructuredOutput", Count: 1}},
		EvidencePhaseCompleted: true, EvidencePhaseSteps: 15, EvidencePhaseRequests: 16,
		EvidenceStepBudget: 16, EvidenceExhausted: true,
		EvidenceExhaustedSteps: 15, EvidenceExhaustedRequests: 16,
		EvidenceExhaustionClass:   "api_bad_request",
		ArtifactEvidenceToolCalls: 1, SourceEvidenceToolCalls: 1,
		EvidenceReadCalls: 2, DuplicateReadCalls: 1,
		EvidenceHandles: WorkspaceEvidenceHandleDiagnostics{
			Status: WorkspaceEvidenceHandlesAccepted, ObservedRangeCount: 2,
			AcceptedArtifactHandleCount: 1, AcceptedSourceHandleCount: 1,
		},
		FinalizationPhaseCompleted: true, FinalizationPhaseSteps: 1, FinalizationPhaseRequests: 1,
		StructuredOutputToolCalls: 1,
	}
	if err := validateWorkspaceOpenCodeTelemetry(telemetry); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*WorkspaceOpenCodeTelemetry){
		"wrong allocation":     func(value *WorkspaceOpenCodeTelemetry) { value.EvidenceStepBudget-- },
		"wrong classification": func(value *WorkspaceOpenCodeTelemetry) { value.EvidenceExhaustionClass = "api_error" },
		"duplicate overflow":   func(value *WorkspaceOpenCodeTelemetry) { value.DuplicateReadCalls = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			value := telemetry
			mutate(&value)
			if err := validateWorkspaceOpenCodeTelemetry(value); err == nil {
				t.Fatalf("invalid bounded exhaustion telemetry was accepted: %+v", value)
			}
		})
	}
}

func TestValidateWorkspaceUsageRejectsPartialCost(t *testing.T) {
	usage := WorkspaceUsage{Available: true, Status: WorkspaceTelemetryPartial, ModelRequests: 1, InputTokens: 10, OutputTokens: 2, CostAvailable: true, CostUSD: "0.1"}
	if err := validateWorkspaceUsage(usage); err == nil {
		t.Fatal("partial usage with complete cost was accepted")
	}
	usage.CostAvailable, usage.CostUSD = false, ""
	if err := validateWorkspaceUsage(usage); err != nil {
		t.Fatal(err)
	}
}

func TestValidShadowProvenanceRejectsUnavailableUsageValues(t *testing.T) {
	for _, value := range []Provenance{
		{UsageStatus: WorkspaceTelemetryUnavailable, CostAvailable: true, CostUSD: "0.1"},
		{UsageStatus: WorkspaceTelemetryUnavailable, InputTokens: 10, OutputTokens: 2},
		{UsageStatus: "unknown"},
	} {
		if validShadowProvenance(value) {
			t.Fatalf("invalid provenance accepted: %+v", value)
		}
	}
}

func TestValidateWorkspaceUsageRejectsInconsistentCountsAndCost(t *testing.T) {
	base := WorkspaceUsage{Available: true, Status: WorkspaceTelemetryAvailable, ModelRequests: 1, InputTokens: 10, CachedInputTokens: 2, OutputTokens: 4}
	for name, mutate := range map[string]func(*WorkspaceUsage){
		"negative reasoning":       func(v *WorkspaceUsage) { v.ReasoningTokens = -1 },
		"cached exceeds input":     func(v *WorkspaceUsage) { v.CachedInputTokens = 11 },
		"reasoning exceeds output": func(v *WorkspaceUsage) { v.ReasoningTokens = 5 },
		"invalid cost":             func(v *WorkspaceUsage) { v.CostAvailable = true; v.CostUSD = "nan" },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := validateWorkspaceUsage(value); err == nil {
				t.Fatalf("invalid usage accepted: %+v", value)
			}
		})
	}
	unavailable := WorkspaceUsage{Status: WorkspaceTelemetryUnavailable, ReasoningTokens: 1}
	if err := validateWorkspaceUsage(unavailable); err == nil {
		t.Fatal("unavailable reasoning tokens accepted")
	}
}
