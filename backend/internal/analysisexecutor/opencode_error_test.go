package analysisexecutor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
)

func TestSanitizeOpenCodeAPIErrorClassifications(t *testing.T) {
	for _, test := range []struct {
		status         int
		retryable      bool
		classification string
	}{
		{status: 400, classification: "api_bad_request"},
		{status: 401, classification: "api_unauthorized"},
		{status: 403, classification: "api_forbidden"},
		{status: 408, retryable: true, classification: "api_timeout"},
		{status: 413, classification: "api_request_too_large"},
		{status: 429, retryable: true, classification: "api_rate_limited"},
		{status: 500, retryable: true, classification: "api_server_error"},
		{status: 503, retryable: true, classification: "api_server_error"},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"message":"provider detail","statusCode":%d,"isRetryable":%t}`, test.status, test.retryable))
			got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "APIError", Data: raw})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Available || got.HTTPStatusCode != test.status || !got.RetryableKnown || got.Retryable != test.retryable || got.Classification != test.classification {
				t.Fatalf("telemetry=%+v", got)
			}
		})
	}
}

func TestSanitizeOpenCodeAPIErrorSpecialClassifications(t *testing.T) {
	for _, test := range []struct {
		name           string
		code           string
		classification string
	}{
		{name: "header timeout", code: "ProviderHeaderTimeoutError", classification: "header_timeout"},
		{name: "stream error", code: "ProviderResponseStreamError", classification: "response_stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"message":"private provider message","isRetryable":true,"metadata":{"code":%q,"url":"https://gateway.example/v1?token=secret"},"responseHeaders":{"content-type":"application/json","authorization":"secret"},"responseBody":"private model output"}`, test.code))
			got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "APIError", Data: raw})
			if err != nil {
				t.Fatal(err)
			}
			if got.Classification != test.classification || got.MetadataCode != test.code || !got.ResponseContentTypePresent || !got.ResponseBodyPresent || got.ResponseBodyBytesBounded != len("private model output") || got.ResponseBodySHA256 == "" {
				t.Fatalf("telemetry=%+v", got)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private provider message", "gateway.example", "token=secret", "authorization", "private model output"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("sanitized telemetry leaked %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestSanitizeOpenCodeContextOverflow(t *testing.T) {
	got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{
		Name: "ContextOverflowError",
		Data: json.RawMessage(`{"message":"private overflow detail","responseBody":"private body"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Classification != "context_overflow" || !got.ContextOverflow || !got.ResponseBodyPresent {
		t.Fatalf("telemetry=%+v", got)
	}
}

func TestSanitizeOpenCodeUnknownError(t *testing.T) {
	message := "getaddrinfo ENOTFOUND synthetic.invalid"
	got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "UnknownError", Data: json.RawMessage(`{"message":` + fmt.Sprintf("%q", message) + `}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Name != "UnknownError" || got.Classification != "dns" || got.CauseCode != "ENOTFOUND" || !got.MessagePresent || got.MessageBytes != len(message) || len(got.RedactedMessageSHA256) != 64 {
		t.Fatalf("telemetry=%+v", got)
	}
}

func TestSanitizeOpenCodeUnknownErrorAllowsEmptyMessage(t *testing.T) {
	got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "UnknownError", Data: json.RawMessage(`{"message":""}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !got.MessagePresent || got.MessageBytes != 0 || len(got.RedactedMessageSHA256) != 64 || got.Classification != "unknown" {
		t.Fatalf("telemetry=%+v", got)
	}
}

func TestSanitizeOpenCodeUnknownErrorUsesNestedAllowlistedCause(t *testing.T) {
	raw := json.RawMessage(`{"message":"operation failed","cause":{"name":"Error","cause":{"name":"SystemError","code":"ECONNRESET","message":"private"}}}`)
	got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "UnknownError", Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got.CauseName != "SystemError" || got.CauseCode != "ECONNRESET" || got.Classification != "connection_reset" {
		t.Fatalf("telemetry=%+v", got)
	}
}

func TestSanitizeOpenCodeUnknownErrorAcceptsBoundedOversizedMessage(t *testing.T) {
	message := strings.Repeat("x", maxOpenCodeErrorFieldBytes+1)
	raw, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "UnknownError", Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageBytes != len(message) || len(got.RedactedMessageSHA256) != 64 {
		t.Fatalf("telemetry=%+v", got)
	}
}

func TestSanitizeOpenCodeUnknownErrorRedactsCredentialAndURLBeforeDigest(t *testing.T) {
	sanitize := func(message string) agentanalysis.WorkspaceOpenCodeErrorTelemetry {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"message": message})
		if err != nil {
			t.Fatal(err)
		}
		got, err := sanitizeOpenCodeError(&openCodeErrorEnvelope{Name: "UnknownError", Data: raw})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := sanitize(`request to https://gateway.example/v1?token=first failed token=first-secret`)
	second := sanitize(`request to https://other.invalid/private?token=second failed token=second-secret`)
	if first.RedactedMessageSHA256 != second.RedactedMessageSHA256 {
		t.Fatalf("redacted digests differ: %s %s", first.RedactedMessageSHA256, second.RedactedMessageSHA256)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"gateway.example", "first-secret", "token=first"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("sanitized telemetry leaked %q: %s", secret, encoded)
		}
	}
}

func TestParseOpenCodeUnknownErrorLifecycle(t *testing.T) {
	for _, test := range []struct {
		name                  string
		message               string
		parts                 string
		classification        string
		providerRequests      int
		providerRequestsKnown bool
		beforeProvider        *bool
		beforeTool            *bool
		duringStream          *bool
		duringTool            *bool
		duringSessionPersist  *bool
	}{
		{name: "stream error", message: "ProviderResponseStreamError while reading response stream", parts: `[]`, classification: "response_stream", providerRequests: 1, providerRequestsKnown: true, beforeProvider: openCodeTelemetryBool(false), beforeTool: openCodeTelemetryBool(true), duringStream: openCodeTelemetryBool(true), duringTool: openCodeTelemetryBool(false), duringSessionPersist: openCodeTelemetryBool(false)},
		{name: "DNS error before a persisted step remains unknown", message: "getaddrinfo ENOTFOUND synthetic.invalid", parts: `[]`, classification: "dns"},
		{name: "invalid tool schema before a persisted step remains unknown", message: "invalid tool schema", parts: `[]`, classification: "invalid_tool_schema"},
		{name: "tool permission error", message: "permission denied", parts: `[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"error","error":"permission denied"}}]`, classification: "permission_denied", providerRequests: 1, providerRequestsKnown: true, beforeProvider: openCodeTelemetryBool(false), beforeTool: openCodeTelemetryBool(false)},
		{name: "filesystem tool error", message: "read-only file system EROFS", parts: `[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"error","error":"filesystem error"}}]`, classification: "filesystem", providerRequests: 1, providerRequestsKnown: true, beforeProvider: openCodeTelemetryBool(false), beforeTool: openCodeTelemetryBool(false)},
		{name: "database error before provider request remains unknown", message: "SQLite database is locked", parts: `[]`, classification: "database"},
		{name: "session serialization before provider remains unknown", message: "failed to serialize session persistence record", parts: `[]`, classification: "serialization"},
		{name: "after provider before tool", message: "unexpected stream processing failure", parts: `[{"type":"step-start"}]`, classification: "response_stream", providerRequests: 1, providerRequestsKnown: true, beforeProvider: openCodeTelemetryBool(false), beforeTool: openCodeTelemetryBool(true), duringStream: openCodeTelemetryBool(true), duringTool: openCodeTelemetryBool(false), duringSessionPersist: openCodeTelemetryBool(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`[{"info":{"role":"assistant","error":{"name":"UnknownError","data":{"message":%q}}},"parts":%s}]`, test.message, test.parts))
			usage, telemetry, err := parseOpenCodeTelemetry(raw)
			if err != nil {
				t.Fatal(err)
			}
			got := telemetry.Error
			if usage.Available || usage.Status != "unavailable" || got.Classification != test.classification || telemetry.ProviderRequests != test.providerRequests || telemetry.ProviderRequestsKnown != test.providerRequestsKnown || !optionalBoolEqual(got.BeforeProviderRequest, test.beforeProvider) || !optionalBoolEqual(got.BeforeFirstTool, test.beforeTool) || !optionalBoolEqual(got.DuringStreamProcessing, test.duringStream) || !optionalBoolEqual(got.DuringToolExecution, test.duringTool) || !optionalBoolEqual(got.DuringSessionPersistence, test.duringSessionPersist) {
				t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
			}
		})
	}
}

func optionalBoolEqual(left, right *bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func TestParseOpenCodeUnknownErrorAfterObservedEvidenceKeepsRequestCountInexact(t *testing.T) {
	raw := []byte(`[
		{"info":{"role":"assistant"},"parts":[
			{"type":"step-start"},
			{"type":"tool","tool":"read","state":{"status":"completed"}},
			{"type":"step-finish","cost":0.1,"tokens":{"input":5,"output":2,"cache":{"read":0}}}
		]},
		{"info":{"role":"assistant","error":{"name":"UnknownError","data":{"message":"getaddrinfo ENOTFOUND synthetic.invalid"}}},"parts":[]}
	]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Available || usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || telemetry.ProviderRequests != 1 || telemetry.ProviderRequestsKnown || telemetry.Error.Classification != "dns" {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
	if telemetry.Error.BeforeProviderRequest == nil || *telemetry.Error.BeforeProviderRequest || telemetry.Error.BeforeFirstTool == nil || *telemetry.Error.BeforeFirstTool || telemetry.Error.DuringStreamProcessing != nil || telemetry.Error.DuringToolExecution != nil || telemetry.Error.DuringSessionPersistence != nil {
		t.Fatalf("lifecycle=%+v", telemetry.Error)
	}
}

func TestPromptOpenCodeUnknownErrorDoesNotUseUnpersistedPartsForLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","error":{"name":"UnknownError","data":{"message":"getaddrinfo ENOTFOUND synthetic.invalid"}}},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"running"}}]}`))
	}))
	defer server.Close()
	_, err := promptOpenCode(t.Context(), server.Client(), server.URL, "session-1", OpenCodeSpec{WorkDir: "/workspace", Provider: testOpenCodeProvider("", "test-model")})
	promptErr, ok := err.(*openCodePromptError)
	if !ok {
		t.Fatalf("err=%T %v", err, err)
	}
	got := promptErr.telemetry
	if got.BeforeProviderRequest != nil || got.BeforeFirstTool != nil || got.DuringStreamProcessing != nil || got.DuringToolExecution != nil || got.DuringSessionPersistence != nil {
		t.Fatalf("unpersisted parts affected lifecycle: %+v", got)
	}
}

func TestSanitizeOpenCodeErrorRejectsMalformedAndOversizedData(t *testing.T) {
	for name, input := range map[string]*openCodeErrorEnvelope{
		"missing retryable":  {Name: "APIError", Data: json.RawMessage(`{"message":"x"}`)},
		"invalid status":     {Name: "APIError", Data: json.RawMessage(`{"message":"x","statusCode":99,"isRetryable":false}`)},
		"unknown field":      {Name: "APIError", Data: json.RawMessage(`{"message":"x","isRetryable":false,"providerOutput":"secret"}`)},
		"oversized message":  {Name: "APIError", Data: json.RawMessage(`{"message":"` + strings.Repeat("x", maxOpenCodeErrorFieldBytes+1) + `","isRetryable":false}`)},
		"oversized metadata": {Name: "APIError", Data: json.RawMessage(`{"message":"x","isRetryable":false,"metadata":{"code":"` + strings.Repeat("x", maxOpenCodeErrorFieldBytes+1) + `"}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := sanitizeOpenCodeError(input); err == nil || got.Available {
				t.Fatalf("telemetry=%+v err=%v", got, err)
			}
		})
	}
}

func TestPromptOpenCodeReturnsSanitizedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","error":{"name":"APIError","data":{"message":"provider secret","statusCode":429,"isRetryable":true,"metadata":{"code":"secret-code","url":"https://gateway.example?token=secret"},"responseBody":"model output"}}},"parts":[]}`))
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: "/workspace", Provider: testOpenCodeProvider("", "test-model"), Prompt: "private prompt"}
	_, err := promptOpenCode(t.Context(), server.Client(), server.URL, "session-1", spec)
	if err == nil || err.Error() != "OpenCode structured output failed: APIError" {
		t.Fatalf("err=%v", err)
	}
	if _, ok := err.(*openCodePromptError); !ok {
		t.Fatalf("unexpected error type %T", err)
	}
	if strings.Contains(err.Error(), "provider secret") || strings.Contains(err.Error(), "model output") || strings.Contains(err.Error(), "token=secret") {
		t.Fatalf("prompt error leaked provider data: %v", err)
	}
}

func TestParseOpenCodeTelemetryAcceptsErrorBeforeFirstStep(t *testing.T) {
	raw := []byte(`[{"info":{"role":"assistant","error":{"name":"APIError","data":{"message":"failed before step","statusCode":400,"isRetryable":false}}},"parts":[]}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Available || usage.Status != "unavailable" || !telemetry.Available || telemetry.StepsUsed != 0 || telemetry.ProviderRequests != 1 || telemetry.Error.Classification != "api_bad_request" {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
}

func TestPromptOpenCodePreservesMalformedErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","error":{"name":"APIError","data":{"message":"private detail"}}},"parts":[]}`))
	}))
	defer server.Close()
	_, err := promptOpenCode(t.Context(), server.Client(), server.URL, "session-1", OpenCodeSpec{WorkDir: "/workspace", Provider: testOpenCodeProvider("", "test-model"), Prompt: "private prompt"})
	promptErr, ok := err.(*openCodePromptError)
	if !ok || promptErr.telemetry.Classification != "malformed_error" || promptErr.telemetry.Name != "APIError" || err.Error() != "OpenCode structured output failed: APIError" {
		t.Fatalf("err=%T %v telemetry=%+v", err, err, promptErr)
	}
}
