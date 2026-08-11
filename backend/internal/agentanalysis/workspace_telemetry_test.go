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
		EventCount:                   1,
		ProviderRequests:             1,
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
		ProviderRequests: 1,
		RequestShape:     validRequestShapeForTest(),
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

func TestValidateWorkspaceOpenCodeTelemetryRejectsUnsafeOrInconsistentFields(t *testing.T) {
	base := WorkspaceOpenCodeTelemetry{
		Available: true, Status: WorkspaceTelemetryAvailable, EventCount: 1, ProviderRequests: 1,
		RequestShape: validRequestShapeForTest(), StructuredOutputRetriesKnown: true,
		Error: WorkspaceOpenCodeErrorTelemetry{Available: true, Name: "APIError", RetryableKnown: true, Classification: "api_error"},
	}
	for name, mutate := range map[string]func(*WorkspaceOpenCodeTelemetry){
		"model oversized":     func(value *WorkspaceOpenCodeTelemetry) { value.RequestShape.ModelID = strings.Repeat("x", 257) },
		"bad tool digest":     func(value *WorkspaceOpenCodeTelemetry) { value.RequestShape.ToolSchemaSHA256 = "secret" },
		"unknown metadata":    func(value *WorkspaceOpenCodeTelemetry) { value.Error.MetadataCode = "provider-secret" },
		"status out of range": func(value *WorkspaceOpenCodeTelemetry) { value.Error.HTTPStatusCode = 999 },
		"retryable unknown": func(value *WorkspaceOpenCodeTelemetry) {
			value.Error.RetryableKnown = false
			value.Error.Retryable = true
		},
		"context mismatch": func(value *WorkspaceOpenCodeTelemetry) {
			value.Error = WorkspaceOpenCodeErrorTelemetry{Available: true, Name: "ContextOverflowError", Classification: "context_overflow", ContextOverflow: true}
		},
		"request undercount": func(value *WorkspaceOpenCodeTelemetry) { value.StepsUsed = 2 },
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
