package runtime

import (
	"strings"
	"testing"
)

func executionRequest() ExecutionRequest {
	return ExecutionRequest{
		Version:          ExecutionContractVersion,
		RepositoryURL:    "https://github.com/octocat/Hello-World.git",
		CommitSHA:        "0123456789abcdef0123456789abcdef01234567",
		Prompt:           "make one deterministic change",
		TimeoutSeconds:   60,
		MaxSteps:         2,
		MaxFiles:         1,
		ModelGateway:     ModelGatewayConfig{Endpoint: "https://gateway.internal.example/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"},
		CommandPolicy:    CommandPolicy{Commands: []ExecutionCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30}}},
		ExpectedBaseSHA:  "0123456789abcdef0123456789abcdef01234567",
		OutputLimitBytes: 64 << 10,
	}
}

func executionResult() ExecutionResult {
	return ExecutionResult{
		Version:        ExecutionContractVersion,
		BaseSHA:        "0123456789abcdef0123456789abcdef01234567",
		ChangedFiles:   []string{"fix.txt"},
		Files:          map[string]string{"fix.txt": "fixed\n"},
		Diff:           "diff --git a/fix.txt b/fix.txt\n",
		CommandResults: []CommandResult{{Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 1}},
		TerminalState:  TerminalSucceeded,
		DurationMs:     10,
	}
}

func TestValidateModelGatewayTrust(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.githubcopilot.com/chat/completions",
		"https://api.openai.com/v1/chat/completions",
		"https://my-resource.openai.azure.com/openai/deployments/model/chat/completions",
		"https://integrate.api.nvidia.com/v1/chat/completions",
	} {
		if err := ValidateModelGatewayTrust(endpoint, true); err == nil || !strings.Contains(err.Error(), "non-provider") {
			t.Errorf("documented provider endpoint %q error = %v", endpoint, err)
		}
	}
	if err := ValidateModelGatewayTrust("https://model-gateway.platform.example.com/v1", true); err != nil {
		t.Fatalf("public CA private DNS gateway rejected: %v", err)
	}
	if err := ValidateModelGatewayTrust("https://gateway.platform.svc.cluster.local/v1", false); err != nil {
		t.Fatalf("internal gateway rejected: %v", err)
	}
}

func TestExecutionContractAcceptsCredentialFreeResult(t *testing.T) {
	request := executionRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := executionResult().Validate(request); err != nil {
		t.Fatalf("result: %v", err)
	}
}

func TestExecutionRequestRejectsMutableOrCredentialedInput(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ExecutionRequest)
		want string
	}{
		{name: "mutable ref", edit: func(r *ExecutionRequest) { r.CommitSHA = "main" }, want: "40 lowercase"},
		{name: "base mismatch", edit: func(r *ExecutionRequest) { r.ExpectedBaseSHA = strings.Repeat("a", 40) }, want: "must equal"},
		{name: "credentialed URL", edit: func(r *ExecutionRequest) { r.RepositoryURL = "https://token@example.test/repo.git" }, want: "credentials"},
		{name: "internal repository", edit: func(r *ExecutionRequest) { r.RepositoryURL = "https://git.internal/repo.git" }, want: "public host"},
		{name: "private repository IP", edit: func(r *ExecutionRequest) { r.RepositoryURL = "https://10.0.0.10/repo.git" }, want: "public host"},
		{name: "hosted file URL", edit: func(r *ExecutionRequest) { r.RepositoryURL = "file://host/tmp/repo" }, want: "without a host"},
		{name: "unbounded prompt", edit: func(r *ExecutionRequest) { r.Prompt = strings.Repeat("x", maxExecutionPromptBytes+1) }, want: "prompt"},
		{name: "gateway credentials", edit: func(r *ExecutionRequest) { r.ModelGateway.Endpoint = "https://token@gateway.internal.example/v1" }, want: "credentials"},
		{name: "gateway protocol", edit: func(r *ExecutionRequest) { r.ModelGateway.ProtocolVersion = "responses-v1" }, want: "protocol"},
		{name: "file bound", edit: func(r *ExecutionRequest) { r.MaxFiles = 0 }, want: "max files"},
		{name: "zero command timeout", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].TimeoutSeconds = 0 }, want: "timeout must be positive"},
		{name: "command timeout exceeds execution", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].TimeoutSeconds = 61 }, want: "execution timeout"},
		{name: "no commands", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands = nil }, want: "at least one"},
		{name: "no coding-agent step", edit: func(r *ExecutionRequest) { r.MaxSteps = 1 }, want: "reserve at least one"},
		{name: "shell policy", edit: func(r *ExecutionRequest) { r.CommandPolicy.AllowShell = true }, want: "shell execution"},
		{name: "shell executable", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"sh", "-c", "true"} }, want: "must not invoke a shell"},
		{name: "busybox dispatcher", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"busybox", "sh", "-c", "true"} }, want: "command dispatcher"},
		{name: "env dispatcher", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"env", "sh", "-c", "true"} }, want: "command dispatcher"},
		{name: "coding agent", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"opencode", "run"} }, want: "coding agent"},
		{name: "path executable", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv[0] = "/usr/bin/git" }, want: "PATH-resolved"},
		{name: "wrong final command", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"git", "diff", "--check"} }, want: "reserved for the exact final"},
		{name: "git alias shell", edit: func(r *ExecutionRequest) {
			r.CommandPolicy.Commands[0].Argv = []string{"git", "-c", "alias.probe=!sh -c true", "probe"}
		}, want: "reserved for the exact final"},
		{name: "empty argv", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv[1] = "" }, want: "is empty"},
		{name: "multiline argv", edit: func(r *ExecutionRequest) { r.CommandPolicy.Commands[0].Argv[1] = "diff\nstatus" }, want: "single-line"},
		{name: "oversized argv", edit: func(r *ExecutionRequest) {
			r.CommandPolicy.Commands[0].Argv[1] = strings.Repeat("x", maxExecutionSingleArgBytes+1)
		}, want: "exceeds"},
		{name: "too many commands", edit: func(r *ExecutionRequest) {
			r.MaxSteps = 1
			r.CommandPolicy.Commands = append(r.CommandPolicy.Commands, ExecutionCommand{Argv: []string{"git", "status"}})
		}, want: "reserve at least one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := executionRequest()
			tc.edit(&request)
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExecutionRequestPreservesArgumentWithSpaces(t *testing.T) {
	request := executionRequest()
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []ExecutionCommand{
		{Argv: []string{"validator", "argument with spaces"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := request.CommandPolicy.Commands[0].Argv[1]; got != "argument with spaces" {
		t.Fatalf("argv = %q", got)
	}
}

func TestExecutionResultRejectsSuccessfulCommandDurationBeyondTimeout(t *testing.T) {
	request := executionRequest()
	result := executionResult()
	result.CommandResults[0].DurationMs = request.CommandPolicy.Commands[0].TimeoutSeconds*1000 + 1
	if err := result.Validate(request); err == nil || !strings.Contains(err.Error(), "successful command") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutionResultBoundsFailedCommandCleanupGrace(t *testing.T) {
	request := executionRequest()
	result := executionResult()
	result.TerminalState = TerminalFailed
	result.FailureReason = "validation failed"
	result.CommandResults[0].ExitCode = 1
	result.CommandResults[0].DurationMs = request.CommandPolicy.Commands[0].TimeoutSeconds*1000 + maxExecutionCommandDurationGraceMs + 1
	if err := result.Validate(request); err == nil || !strings.Contains(err.Error(), "cleanup grace") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutionResultAllowsBoundedAdapterResourceMetadata(t *testing.T) {
	request := executionRequest()
	request.OutputLimitBytes = 4096
	result := executionResult()
	for {
		result.StdoutSummary += "x"
		if err := result.Validate(request); err != nil {
			result.StdoutSummary = result.StdoutSummary[:len(result.StdoutSummary)-1]
			break
		}
	}
	result.Resources = ResourceMetadata{
		Backend: "agent-sandbox", Namespace: strings.Repeat("n", 63), Name: strings.Repeat("s", 63),
		PodName: strings.Repeat("p", 63), NodeName: strings.Repeat("d", 253), RuntimeClassName: strings.Repeat("r", 63),
		CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralStorage: "256Mi",
	}
	if err := result.Validate(request); err != nil {
		t.Fatalf("bounded adapter metadata rejected: %v", err)
	}
	result.Resources.NodeName = strings.Repeat("x", maxExecutionResourceMetadataBytes)
	if err := result.Validate(request); err == nil || !strings.Contains(err.Error(), "resource metadata") {
		t.Fatalf("oversized resource metadata error = %v", err)
	}
}

func TestExecutionResultRejectsMismatchedOutput(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ExecutionResult)
		want string
	}{
		{name: "wrong base", edit: func(r *ExecutionResult) { r.BaseSHA = strings.Repeat("a", 40) }, want: "base SHA"},
		{name: "unsafe path", edit: func(r *ExecutionResult) {
			r.ChangedFiles = []string{"../fix.txt"}
			r.Files = map[string]string{"../fix.txt": "x"}
		}, want: "unsafe"},
		{name: "file mismatch", edit: func(r *ExecutionResult) { r.Files = map[string]string{} }, want: "differ"},
		{name: "too many files", edit: func(r *ExecutionResult) {
			r.ChangedFiles = []string{"a", "b"}
			r.Files = map[string]string{"a": "a", "b": "b"}
		}, want: "max_files"},
		{name: "command mismatch", edit: func(r *ExecutionResult) { r.CommandResults[0].Argv = []string{"sh", "-c", "true"} }, want: "allowed argv"},
		{name: "missing command result", edit: func(r *ExecutionResult) { r.CommandResults = nil }, want: "every allowed command"},
		{name: "failed command on success", edit: func(r *ExecutionResult) { r.CommandResults[0].ExitCode = 1 }, want: "failed command"},
		{name: "changed file without diff", edit: func(r *ExecutionResult) { r.Diff = "" }, want: "unified diff"},
		{name: "failed without reason", edit: func(r *ExecutionResult) { r.TerminalState = TerminalFailed }, want: "failure reason"},
		{name: "oversized output", edit: func(r *ExecutionResult) { r.StdoutSummary = strings.Repeat("x", 65<<10) }, want: "output limit"},
		{name: "escaped output", edit: func(r *ExecutionResult) { r.Files["fix.txt"] = strings.Repeat("\x01", 40<<10) }, want: "output limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := executionResult()
			tc.edit(&result)
			if err := result.Validate(executionRequest()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
