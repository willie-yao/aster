package fixexecutor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestExecuteProducesCredentialFreeStagedPatch(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			if spec.Gateway.Endpoint != request.ModelGateway.Endpoint || spec.Gateway.Model != request.ModelGateway.Model {
				t.Fatalf("gateway = %+v", spec.Gateway)
			}
			if err := os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("Hello Agent Sandbox!\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return `{"type":"text","part":{"text":"fixture edit complete"}}`, "", nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.BaseSHA != sha {
		t.Fatalf("result = %+v", result)
	}
	if !equalStrings(result.ChangedFiles, []string{"README"}) || result.Files["README"] != "Hello Agent Sandbox!\n" {
		t.Fatalf("files = %v content=%q", result.ChangedFiles, result.Files["README"])
	}
	if len(result.CommandResults) != 1 || result.CommandResults[0].ExitCode != 0 || result.CommandResults[0].TimedOut {
		t.Fatalf("commands = %+v", result.CommandResults)
	}
	if !strings.Contains(result.Diff, "Hello Agent Sandbox!") || !strings.Contains(result.StdoutSummary, "fixture edit complete") {
		t.Fatalf("diff=%q stdout=%q", result.Diff, result.StdoutSummary)
	}
}

func TestExecuteFailsClosedOnUnsafePolicy(t *testing.T) {
	repository, sha := fixtureRepository(t)
	for _, tc := range []struct {
		name string
		edit func(*engineruntime.ExecutionRequest)
		want string
	}{
		{name: "shell", edit: func(r *engineruntime.ExecutionRequest) { r.CommandPolicy.AllowShell = true }, want: "shell execution"},
		{name: "shell executable", edit: func(r *engineruntime.ExecutionRequest) {
			r.MaxSteps = 3
			r.CommandPolicy.Commands = []engineruntime.ExecutionCommand{{Argv: []string{"sh", "-c", "true"}, TimeoutSeconds: 10}, {Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10}}
		}, want: "must not invoke a shell"},
		{name: "busybox shell dispatcher", edit: func(r *engineruntime.ExecutionRequest) {
			r.MaxSteps = 3
			r.CommandPolicy.Commands = []engineruntime.ExecutionCommand{{Argv: []string{"busybox", "sh", "-c", "true"}, TimeoutSeconds: 10}, {Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10}}
		}, want: "command dispatcher"},
		{name: "git alias shell dispatcher", edit: func(r *engineruntime.ExecutionRequest) {
			r.MaxSteps = 3
			r.CommandPolicy.Commands = []engineruntime.ExecutionCommand{{Argv: []string{"git", "-c", "alias.probe=!sh -c true", "probe"}, TimeoutSeconds: 10}, {Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10}}
		}, want: "reserved for the exact final"},
		{name: "no final diff check", edit: func(r *engineruntime.ExecutionRequest) { r.CommandPolicy.Commands[0].Argv = []string{"git", "status"} }, want: "reserved for the exact final"},
		{name: "no agent step", edit: func(r *engineruntime.ExecutionRequest) { r.MaxSteps = 1 }, want: "reserve at least one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fixtureRequest(repository, sha)
			tc.edit(&request)
			result := Execute(context.Background(), request, Options{WorkspaceRoot: t.TempDir(), RunOpenCode: func(context.Context, OpenCodeSpec) (string, string, error) {
				return "", "", nil
			}})
			if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, tc.want) {
				t.Fatalf("result = %+v, want %q", result, tc.want)
			}
		})
	}
}

func TestExecuteMapsValidationFailureAsFailed(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"false"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
	}
	request.MaxSteps = 3
	modelRequests := 0
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			modelRequests++
			if err := os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return "", "", nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.TerminalState == engineruntime.TerminalCancelled {
		t.Fatalf("result = %+v", result)
	}
	if modelRequests != 1 || len(result.CommandResults) != 1 {
		t.Fatalf("model requests=%d command results=%d", modelRequests, len(result.CommandResults))
	}
}

func TestExecuteReportsUnavailableValidationExecutable(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"missing-validator-binary", "argument with spaces"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
	}
	modelRequests := 0
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			modelRequests++
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644)
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, `executable "missing-validator-binary" is unavailable`) {
		t.Fatalf("result = %+v", result)
	}
	if modelRequests != 1 || len(result.CommandResults) != 1 || result.CommandResults[0].ExitCode != -1 {
		t.Fatalf("model requests=%d command results=%+v", modelRequests, result.CommandResults)
	}
}

func TestExecuteRejectsValidationCommandStagedMutation(t *testing.T) {
	repository, sha := fixtureRepository(t)
	binDir := t.TempDir()
	tool := filepath.Join(binDir, "mutate-index")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'validation mutation\n' > README\ngit add README\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	request := fixtureRequest(repository, sha)
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"mutate-index"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
	}
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("generated change\n"), 0o644)
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, "modified the staged patch") {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecuteMapsAgentDeadline(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := Execute(ctx, request, Options{WorkspaceRoot: t.TempDir(), RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (string, string, error) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}})
	if result.TerminalState != engineruntime.TerminalTimedOut || result.FailureReason == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWriteOpenCodeConfigOmitsCredentials(t *testing.T) {
	home := t.TempDir()
	gateway := engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.internal.example/v1/chat/completions", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"}
	if err := writeOpenCodeConfig(home, gateway, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"apiKey", "authorization", "token", "secret"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("config contains %q: %s", forbidden, text)
		}
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	permissions := config["permission"].(map[string]any)
	if permissions["bash"] != "deny" || permissions["webfetch"] != "deny" || permissions["external_directory"] != "deny" {
		t.Fatalf("permissions = %v", permissions)
	}
}

func TestOpenCodeEnvironmentDoesNotInheritCredentials(t *testing.T) {
	t.Setenv("AI_TOKEN", "secret")
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("HTTPS_PROXY", "https://user:secret@proxy.invalid")
	joined := strings.Join(openCodeEnvironment("/home", "/tmp"), "\n")
	for _, forbidden := range []string{"AI_TOKEN", "OPENAI_API_KEY", "HTTPS_PROXY", "secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment contains %q: %s", forbidden, joined)
		}
	}
}

func fixtureRequest(repository, sha string) engineruntime.ExecutionRequest {
	return engineruntime.ExecutionRequest{
		Version: engineruntime.ExecutionContractVersion, RepositoryURL: "file://" + repository,
		CommitSHA: sha, ExpectedBaseSHA: sha, Prompt: "Update README.", TimeoutSeconds: 30,
		MaxSteps: 2, MaxFiles: 1, OutputLimitBytes: 128 << 10,
		ModelGateway: engineruntime.ModelGatewayConfig{
			Endpoint: "https://gateway.internal.example/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1",
		},
		CommandPolicy: engineruntime.CommandPolicy{Commands: []engineruntime.ExecutionCommand{{
			Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10,
		}}},
	}
}

func fixtureRepository(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "config", "user.name", "Fixture")
	runGit(t, dir, "config", "user.email", "fixture@example.test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("Hello World!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README")
	runGit(t, dir, "commit", "-qm", "fixture")
	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	return dir, sha
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
