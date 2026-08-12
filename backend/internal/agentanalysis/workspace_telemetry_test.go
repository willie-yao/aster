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
