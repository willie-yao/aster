package fixexecutor

import (
	"context"
	"errors"
	"strings"
	"testing"

	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestProviderCredentialRejectionRecognizesAuthFailuresOnly(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{
			name:   "forbidden",
			stdout: `{"type":"error","error":{"name":"APIError","data":{"message":"Forbidden","statusCode":403,"isRetryable":false}}}`,
			want:   "model provider rejected the sandbox credential (HTTP 403)",
		},
		{
			name:   "unauthorized after other events",
			stdout: "{\"type\":\"text\",\"part\":{\"text\":\"working\"}}\n{\"type\":\"error\",\"error\":{\"name\":\"APIError\",\"data\":{\"statusCode\":401}}}",
			want:   "model provider rejected the sandbox credential (HTTP 401)",
		},
		{
			name:   "provider auth error",
			stdout: `{"type":"error","error":{"name":"ProviderAuthError","data":{}}}`,
			want:   "model provider rejected the sandbox credential",
		},
		{name: "rate limited", stdout: `{"type":"error","error":{"name":"APIError","data":{"statusCode":429,"isRetryable":false}}}`},
		{name: "server error", stdout: `{"type":"error","error":{"name":"APIError","data":{"statusCode":503}}}`},
		{name: "context overflow", stdout: `{"type":"error","error":{"name":"ContextOverflowError","data":{}}}`},
		{name: "no error event", stdout: `{"type":"text","part":{"text":"403 forbidden"}}`},
		{name: "empty", stdout: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, rejected := providerCredentialRejection(tt.stdout)
			if rejected != (tt.want != "") || reason != tt.want {
				t.Fatalf("reason=%q rejected=%v want=%q", reason, rejected, tt.want)
			}
		})
	}
}

func TestExecuteClassifiesProviderCredentialRejection(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	stdout := `{"type":"error","error":{"name":"APIError","data":{"message":"Forbidden: token not authorized for this integration","statusCode":403,"isRetryable":false}}}`
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
			return stdout, "Cloning into '/workspace/repository'...\n", errors.New("exit status 1")
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureProviderCredential {
		t.Fatalf("result = %+v", result)
	}
	if result.FailureReason != "model provider rejected the sandbox credential (HTTP 403)" {
		t.Fatalf("reason = %q", result.FailureReason)
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

// A timed-out execution cannot carry a failure code, so a provider rejection
// observed while the deadline expires must not break the result contract.
func TestExecuteOmitsProviderCredentialCodeOnDeadline(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.TimeoutSeconds = 1
	request.CommandPolicy.Commands[0].TimeoutSeconds = 1
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (string, string, error) {
			<-ctx.Done()
			return `{"type":"error","error":{"name":"APIError","data":{"statusCode":403}}}`, "", ctx.Err()
		},
	})
	if result.TerminalState != engineruntime.TerminalTimedOut || result.FailureCode != "" {
		t.Fatalf("result = %+v", result)
	}
	if err := result.Validate(request); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
