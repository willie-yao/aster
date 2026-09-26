package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestProviderRejectionClassifiesClientFailures(t *testing.T) {
	tests := []struct {
		name          string
		stdout        string
		wantCode      engineruntime.ExecutionFailureCode
		wantReason    string
		wantStatus    int
		wantMessage   string
		wantProvider  string
		wantErrorCode string
	}{
		{
			name:        "forbidden",
			stdout:      `{"type":"error","error":{"name":"APIError","data":{"message":"Forbidden","statusCode":403,"isRetryable":false}}}`,
			wantCode:    engineruntime.ExecutionFailureProviderCredential,
			wantReason:  "model provider refused the sandbox request (HTTP 403)",
			wantStatus:  403,
			wantMessage: "Forbidden",
		},
		{
			name:        "unauthorized after other events",
			stdout:      "{\"type\":\"text\",\"part\":{\"text\":\"working\"}}\n{\"type\":\"error\",\"error\":{\"name\":\"APIError\",\"data\":{\"message\":\"Unauthorized token=ghp-fixture-secret\",\"statusCode\":401}}}",
			wantCode:    engineruntime.ExecutionFailureProviderCredential,
			wantReason:  "model provider rejected the sandbox credential (HTTP 401)",
			wantStatus:  401,
			wantMessage: "Unauthorized token=[redacted]",
		},
		{
			name:        "first matching event wins",
			stdout:      "{\"type\":\"error\",\"error\":{\"name\":\"APIError\",\"data\":{\"message\":\"Forbidden first\",\"statusCode\":403}}}\n{\"type\":\"error\",\"error\":{\"name\":\"APIError\",\"data\":{\"message\":\"Unauthorized second\",\"statusCode\":401}}}",
			wantCode:    engineruntime.ExecutionFailureProviderCredential,
			wantReason:  "model provider refused the sandbox request (HTTP 403)",
			wantStatus:  403,
			wantMessage: "Forbidden first",
		},
		{
			name:         "provider auth error",
			stdout:       `{"type":"error","error":{"name":"ProviderAuthError","data":{"message":"No provider auth","providerID":"github-copilot"}}}`,
			wantCode:     engineruntime.ExecutionFailureProviderCredential,
			wantReason:   "model provider rejected the sandbox credential",
			wantMessage:  "No provider auth",
			wantProvider: "github-copilot",
		},
		{
			name:          "model not supported",
			stdout:        `{"type":"error","error":{"name":"APIError","data":{"message":"The requested model is not supported.","statusCode":400,"responseBody":"{\"error\":{\"code\":\"model_not_supported\"}}"}}}`,
			wantCode:      engineruntime.ExecutionFailureProviderRequest,
			wantReason:    "model provider rejected the request (HTTP 400 model_not_supported)",
			wantStatus:    400,
			wantMessage:   "The requested model is not supported.",
			wantErrorCode: "model_not_supported",
		},
		{
			name:        "other client error",
			stdout:      `{"type":"error","error":{"name":"APIError","data":{"message":"Unprocessable","statusCode":422}}}`,
			wantCode:    engineruntime.ExecutionFailureProviderRequest,
			wantReason:  "model provider rejected the request (HTTP 422)",
			wantStatus:  422,
			wantMessage: "Unprocessable",
		},
		{name: "timeout", stdout: `{"type":"error","error":{"name":"APIError","data":{"statusCode":408}}}`},
		{name: "rate limited", stdout: `{"type":"error","error":{"name":"APIError","data":{"statusCode":429,"isRetryable":false}}}`},
		{name: "server error", stdout: `{"type":"error","error":{"name":"APIError","data":{"statusCode":503}}}`},
		{name: "context overflow", stdout: `{"type":"error","error":{"name":"ContextOverflowError","data":{}}}`},
		{name: "no error event", stdout: `{"type":"text","part":{"text":"403 forbidden"}}`},
		{name: "empty", stdout: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, reason, detail, rejected := providerRejection(tt.stdout)
			if rejected != (tt.wantReason != "") || reason != tt.wantReason {
				t.Fatalf("reason=%q rejected=%v want=%q", reason, rejected, tt.wantReason)
			}
			if code != tt.wantCode {
				t.Fatalf("code=%q want=%q", code, tt.wantCode)
			}
			if !rejected {
				if detail != nil {
					t.Fatalf("detail = %+v", detail)
				}
				return
			}
			if detail == nil || detail.StatusCode != tt.wantStatus || detail.Message != tt.wantMessage || detail.ProviderID != tt.wantProvider || detail.Code != tt.wantErrorCode {
				t.Fatalf("detail=%+v want status=%d message=%q provider=%q code=%q", detail, tt.wantStatus, tt.wantMessage, tt.wantProvider, tt.wantErrorCode)
			}
		})
	}
}

func TestExecuteReportsCapturedOpenCodeFailures(t *testing.T) {
	// These events preserve the shape of the three observed failures, with synthetic identifiers.
	const modelNotSupported = `{"type":"error","sessionID":"ses_fixture","error":{"name":"APIError","data":{"message":"The requested model is not supported.","statusCode":400,"responseHeaders":{"x-github-request-id":"fixture-header"},"responseBody":"{\"error\":{\"message\":\"The requested model is not supported.\",\"code\":\"model_not_supported\"}}\n"}}}`
	const emptyTextThenError = "{\"type\":\"text\",\"part\":{\"text\":\"  \"}}\n" + modelNotSupported
	const unsupportedAPI = `{"type":"error","sessionID":"ses_fixture","error":{"name":"APIError","data":{"message":"model \"gpt-6-sol\" is not accessible via the /chat/completions endpoint","statusCode":400,"responseBody":"{\"error\":{\"code\":\"unsupported_api_for_model\"}}\n"}}}`
	const summaryPartsCrash = "{\"type\":\"step_start\",\"part\":{\"type\":\"step-start\"}}\n{\"type\":\"error\",\"error\":{\"name\":\"UnknownError\",\"data\":{\"message\":\"undefined is not an object (evaluating 'A.summaryParts')\"}}}"
	const textThenError = "{\"type\":\"text\",\"part\":{\"text\":\"initial explanation\"}}\n" + summaryPartsCrash
	longToolThenError := `{"type":"tool_use","part":{"text":"` + strings.Repeat("x", maxCapturedStream) + `"}}` + "\n" + summaryPartsCrash
	for _, tt := range []struct {
		name       string
		stdout     string
		stderr     string
		wantCode   engineruntime.ExecutionFailureCode
		wantReason string
		wantDetail string
		wantText   string
	}{
		{name: "retired model", stdout: modelNotSupported, wantCode: engineruntime.ExecutionFailureProviderRequest,
			wantReason: "model provider rejected the request (HTTP 400 model_not_supported)", wantDetail: "model_not_supported"},
		{name: "empty text before provider error", stdout: emptyTextThenError, wantCode: engineruntime.ExecutionFailureProviderRequest,
			wantReason: "model provider rejected the request (HTTP 400 model_not_supported)", wantDetail: "model_not_supported"},
		{name: "wrong API", stdout: unsupportedAPI, wantCode: engineruntime.ExecutionFailureProviderRequest,
			wantReason: "model provider rejected the request (HTTP 400 unsupported_api_for_model)", wantDetail: "unsupported_api_for_model"},
		{name: "coding agent crash", stdout: summaryPartsCrash, wantCode: engineruntime.ExecutionFailureRuntime,
			wantReason: "coding agent failed: UnknownError: undefined is not an object (evaluating 'A.summaryParts')"},
		{name: "text before crash", stdout: textThenError, wantCode: engineruntime.ExecutionFailureRuntime,
			wantReason: "coding agent failed: UnknownError: undefined is not an object (evaluating 'A.summaryParts')", wantText: "initial explanation"},
		{name: "long tool before crash", stdout: longToolThenError, wantCode: engineruntime.ExecutionFailureRuntime,
			wantReason: "coding agent failed: UnknownError: undefined is not an object (evaluating 'A.summaryParts')"},
		{name: "request timeout", stdout: `{"type":"error","error":{"name":"APIError","data":{"message":"request timed out","statusCode":408}}}`,
			wantCode: engineruntime.ExecutionFailureRuntime, wantReason: "coding agent failed: APIError HTTP 408: request timed out"},
		{name: "rate limited", stdout: `{"type":"error","error":{"name":"APIError","data":{"message":"too many requests","statusCode":429}}}`,
			wantCode: engineruntime.ExecutionFailureRuntime, wantReason: "coding agent failed: APIError HTTP 429: too many requests"},
		{name: "silent retries", stderr: "INFO retrying\nERROR provider returned 503 from https://private.example/v1 token=fixture\n",
			wantCode: engineruntime.ExecutionFailureRuntime, wantReason: "coding agent failed: ERROR provider returned 503 from [redacted-url] token=[redacted]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repository, sha := fixtureRepository(t)
			request := fixtureRequest(repository, sha)
			result := Execute(t.Context(), request, Options{
				WorkspaceRoot: t.TempDir(),
				RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
					return tt.stdout, tt.stderr, errors.New("exit status 1")
				},
			})
			if result.FailureCode != tt.wantCode || result.FailureReason != tt.wantReason {
				t.Fatalf("code=%q reason=%q", result.FailureCode, result.FailureReason)
			}
			if tt.wantDetail != "" && (result.ProviderError == nil || result.ProviderError.Code != tt.wantDetail || result.ProviderError.StatusCode != 400) {
				t.Fatalf("provider error = %+v", result.ProviderError)
			}
			if tt.wantText != "" && !strings.Contains(result.StdoutSummary, tt.wantText) {
				t.Fatalf("text summary lost: %q", result.StdoutSummary)
			}
			if tt.wantCode == engineruntime.ExecutionFailureRuntime && result.ProviderError != nil {
				t.Fatalf("runtime failure retained provider rejection: %+v", result.ProviderError)
			}
			if strings.Contains(result.StdoutSummary, "fixture-header") || strings.Contains(result.StdoutSummary, "responseBody") {
				t.Fatalf("raw provider response reached result stdout: %q", result.StdoutSummary)
			}
			encoded, err := json.Marshal(result.ProviderError)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "responseHeaders") || strings.Contains(string(encoded), "responseBody") || strings.Contains(string(encoded), "fixture-header") {
				t.Fatalf("provider metadata retained raw response: %s", encoded)
			}
			if err := result.Validate(request); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestOpenCodeFailureDetailUsesLastErrorOrStderr(t *testing.T) {
	stdout := "{\"type\":\"text\",\"part\":{\"text\":\"working\"}}\n" +
		`{"type":"error","error":{"name":"APIError","data":{"message":"early failure","statusCode":500}}}` + "\n" +
		`{"type":"error","error":{"name":"UnknownError","data":{"message":"later failure"}}}`
	if detail := openCodeFailureDetail(stdout, "ignored"); detail != "UnknownError: later failure" {
		t.Fatalf("last error = %q", detail)
	}
	longTool := `{"type":"tool_use","part":{"text":"` + strings.Repeat("x", maxCapturedStream) + `"}}`
	stdout = longTool + "\n" + `{"type":"error","error":{"name":"APIError","data":{"message":"quota unavailable","statusCode":503,"responseBody":"{\"error\":{\"code\":\"capacity\"}}"}}}`
	if detail := openCodeFailureDetail(stdout, "ignored"); detail != "APIError HTTP 503 capacity: quota unavailable" {
		t.Fatalf("error after long tool = %q", detail)
	}
	if detail := openCodeFailureDetail("", "retrying https://gateway.example/private?token=fixture\n\nERROR: token=fixture unavailable\n\n"); detail != "ERROR: token=[redacted] unavailable" {
		t.Fatalf("stderr fallback = %q", detail)
	}
	longMessage := `{"type":"error","error":{"name":"UnknownError","data":{"message":"` + strings.Repeat("x", 800) + `"}}}`
	if detail := openCodeFailureDetail(longMessage, ""); len(detail) != maxOpenCodeFailureDetailBytes || !strings.HasSuffix(detail, "…") {
		t.Fatalf("detail not bounded: len=%d detail=%q", len(detail), detail)
	}
}

func TestProviderErrorCodeIsBounded(t *testing.T) {
	code := providerErrorCode(fmt.Sprintf(`{"error":{"code":"%s"}}`, strings.Repeat("x", 200)))
	if len(code) != maxProviderErrorCodeBytes || !strings.HasSuffix(code, "…") {
		t.Fatalf("code not bounded: %q", code)
	}
	if code := providerErrorCode(`{"error":{"code":42}}`); code != "" {
		t.Fatalf("non-string provider code = %q", code)
	}
}

func TestExecuteRejectsCredentialInProviderErrorCode(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.ModelProvider = testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	body := fmt.Sprintf(`{"error":{"code":"%s"}}`, credential)
	event, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"name": "APIError", "data": map[string]any{"statusCode": 400, "responseBody": body}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
			return string(event), "", errors.New("exit status 1")
		},
	})
	if result.FailureCode != engineruntime.ExecutionFailureSafetyIntegrity ||
		result.FailureReason != modelprovider.ErrCredentialExposure.Error() || result.ProviderError != nil {
		t.Fatalf("credential-bearing result was not replaced: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), credential) {
		t.Fatal("result leaked provider credential")
	}
}

func TestExecuteKeepsFailureDiagnosticWithSmallOutputLimitAndLongLogs(t *testing.T) {
	for _, tt := range []struct {
		name     string
		stdout   string
		wantCode engineruntime.ExecutionFailureCode
		wantText string
	}{
		{
			name:     "provider rejection",
			stdout:   `{"type":"error","error":{"name":"APIError","data":{"message":"The requested model is not supported.","statusCode":400,"responseBody":"{\"error\":{\"code\":\"model_not_supported\"}}"}}}`,
			wantCode: engineruntime.ExecutionFailureProviderRequest,
			wantText: "model_not_supported",
		},
		{
			name:     "coding agent failure",
			stdout:   `{"type":"error","error":{"name":"UnknownError","data":{"message":"stream decode failed"}}}`,
			wantCode: engineruntime.ExecutionFailureRuntime,
			wantText: "stream decode failed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repository, sha := fixtureRepository(t)
			request := fixtureRequest(repository, sha)
			request.OutputLimitBytes = 4096
			result := Execute(t.Context(), request, Options{
				WorkspaceRoot: t.TempDir(),
				RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
					return tt.stdout, strings.Repeat("ERROR: provider retry failed\n", 600), errors.New("exit status 1")
				},
			})
			if result.FailureCode != tt.wantCode || !strings.Contains(result.FailureReason, tt.wantText) {
				t.Fatalf("diagnostic was lost to output limit: code=%q reason=%q", result.FailureCode, result.FailureReason)
			}
			if len(result.StderrSummary) > maxFailureStream {
				t.Fatalf("failure stderr exceeds bound: %d", len(result.StderrSummary))
			}
			if err := result.Validate(request); err != nil {
				t.Fatalf("failure result invalid: %v", err)
			}
		})
	}
}

func TestExecuteKeepsProviderRejectionWhenLogsExpandOnJSONEncoding(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.OutputLimitBytes = 4096
	message := "Request refused: " + strings.Repeat("<tr><td></td></tr>", 40)
	textEvent, err := json.Marshal(map[string]any{
		"type": "text", "part": map[string]any{"text": strings.Repeat("Inspecting <table> markup. ", 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	errorEvent, err := json.Marshal(map[string]any{
		"type": "error", "error": map[string]any{
			"name": "APIError", "data": map[string]any{"statusCode": 403, "message": message},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
			return string(textEvent) + "\n" + string(errorEvent), strings.Repeat("ERROR "+message+"\n", 40), errors.New("exit status 1")
		},
	})
	if result.FailureCode != engineruntime.ExecutionFailureProviderCredential ||
		result.FailureReason != "model provider refused the sandbox request (HTTP 403)" ||
		result.ProviderError == nil || result.ProviderError.StatusCode != 403 ||
		!strings.Contains(result.ProviderError.Message, "Request refused:") {
		t.Fatalf("provider diagnosis lost to JSON encoding: %+v", result)
	}
	if err := result.Validate(request); err != nil {
		t.Fatalf("failure result invalid: %v", err)
	}
}

func TestExecuteClassifiesProviderCredentialRejection(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	stdout := `{"type":"error","error":{"name":"APIError","data":{"message":"Forbidden: token=ghp-fixture-secret","statusCode":403,"isRetryable":false}}}`
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
			return stdout, "Cloning into '/workspace/repository'...\n", errors.New("exit status 1")
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureProviderCredential {
		t.Fatalf("result = %+v", result)
	}
	if result.FailureReason != "model provider refused the sandbox request (HTTP 403)" {
		t.Fatalf("reason = %q", result.FailureReason)
	}
	if result.ProviderError == nil || result.ProviderError.StatusCode != 403 ||
		result.ProviderError.Message != "Forbidden: token=[redacted]" ||
		result.ProviderError.Endpoint != "" || result.ProviderError.Model != "" {
		t.Fatalf("provider error = %+v", result.ProviderError)
	}
	if strings.Contains(result.ProviderError.Message, "ghp-fixture-secret") {
		t.Fatalf("provider error disclosed token: %+v", result.ProviderError)
	}
	if err := result.Validate(request); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestExecuteKeepsRuntimeCodeForNonCredentialAgentFailure(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
			return `{"type":"error","error":{"name":"APIError","data":{"statusCode":500}}}`, "", errors.New("exit status 1")
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureRuntime {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(result.FailureReason, "credential") {
		t.Fatalf("reason = %q", result.FailureReason)
	}
}

func TestExecuteOmitsProviderCodeOnDeadline(t *testing.T) {
	for _, status := range []int{400, 403} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			repository, sha := fixtureRepository(t)
			request := fixtureRequest(repository, sha)
			request.TimeoutSeconds = 1
			request.CommandPolicy.Commands[0].TimeoutSeconds = 1
			result := Execute(context.Background(), request, Options{
				WorkspaceRoot: t.TempDir(),
				RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (string, string, error) {
					<-ctx.Done()
					return fmt.Sprintf(`{"type":"error","error":{"name":"APIError","data":{"statusCode":%d}}}`, status), "", ctx.Err()
				},
			})
			if result.TerminalState != engineruntime.TerminalTimedOut || result.FailureCode != "" || result.ProviderError != nil {
				t.Fatalf("result = %+v", result)
			}
			if err := result.Validate(request); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}
