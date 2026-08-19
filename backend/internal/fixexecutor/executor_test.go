package fixexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func testGatewayProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
}

func testResponsesProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIResponses,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}

func testDirectBearerProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}

func TestExecuteProducesCredentialFreeStagedPatch(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			if spec.Provider.Endpoint != request.ModelProvider.Endpoint || spec.Provider.Model != request.ModelProvider.Model {
				t.Fatalf("provider = %+v", spec.Provider)
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

func TestExecuteRetainsPatchAndCompleteFailedValidationResults(t *testing.T) {
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
	if result.TerminalState != engineruntime.TerminalSucceeded || result.FailureReason != "" {
		t.Fatalf("result = %+v", result)
	}
	if modelRequests != 1 || len(result.CommandResults) != 2 || result.CommandResults[0].ExitCode == 0 || result.CommandResults[0].TimedOut || result.CommandResults[1].ExitCode != 0 {
		t.Fatalf("model requests=%d command results=%+v", modelRequests, result.CommandResults)
	}
	if !equalStrings(result.ChangedFiles, []string{"README"}) || result.Files["README"] != "changed\n" || !strings.Contains(result.Diff, "changed") {
		t.Fatalf("files=%v content=%q diff=%q", result.ChangedFiles, result.Files["README"], result.Diff)
	}
}

func TestExecuteRetainsBoundedValidationTimeout(t *testing.T) {
	repository, sha := fixtureRepository(t)
	binDir := t.TempDir()
	tool := filepath.Join(binDir, "slow-validator-tree")
	childPID := filepath.Join(binDir, "child.pid")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nsleep 5 &\necho $! > \"$1\"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	request := fixtureRequest(repository, sha)
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"slow-validator-tree", childPID}, TimeoutSeconds: 1},
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
	if result.TerminalState != engineruntime.TerminalSucceeded || modelRequests != 1 || len(result.CommandResults) != 2 {
		t.Fatalf("result=%+v model requests=%d", result, modelRequests)
	}
	// The command backgrounds a 5s child, so a duration near its 1s timeout is
	// what proves the timeout bounded the command itself.
	if first := result.CommandResults[0]; !first.TimedOut || first.ExitCode != -1 || first.DurationMs < 900 || first.DurationMs > 2_500 {
		t.Fatalf("timed out command = %+v", first)
	}
	if final := result.CommandResults[1]; final.TimedOut || final.ExitCode != 0 {
		t.Fatalf("final command = %+v", final)
	}
	assertProcessExited(t, childPID)
}

// assertProcessExited fails unless the process recorded in pidFile is gone,
// proving the timeout killed the command's whole process group.
func assertProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	recorded, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read backgrounded child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(recorded)))
	if err != nil {
		t.Fatalf("parse backgrounded child pid %q: %v", recorded, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backgrounded child %d survived the command timeout", pid)
}

func TestExecuteRejectsSignaledValidationCommand(t *testing.T) {
	repository, sha := fixtureRepository(t)
	binDir := t.TempDir()
	tool := filepath.Join(binDir, "crash-validator")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nkill -SEGV $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	request := fixtureRequest(repository, sha)
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"crash-validator"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
	}
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644)
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || len(result.CommandResults) != 1 || result.CommandResults[0].ExitCode != -1 || result.CommandResults[0].TimedOut {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.FailureReason, "could not run") {
		t.Fatalf("failure reason = %q", result.FailureReason)
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
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureRuntime || !strings.Contains(result.FailureReason, `executable "missing-validator-binary" is unavailable`) {
		t.Fatalf("result = %+v", result)
	}
	if modelRequests != 1 || len(result.CommandResults) != 1 || result.CommandResults[0].ExitCode != -1 {
		t.Fatalf("model requests=%d command results=%+v", modelRequests, result.CommandResults)
	}
}

func TestExecuteRejectsValidationWorkspaceMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "tracked file", script: "printf 'validation mutation\\n' > README\n", want: "modified tracked workspace files"},
		{name: "untracked file", script: "touch validation.tmp\n", want: "created untracked workspace files"},
		{name: "staged patch", script: "printf 'validation mutation\\n' > README\ngit add README\n", want: "modified the staged patch"},
		{name: "Git HEAD", script: "printf '0000000000000000000000000000000000000000\\n' > .git/HEAD\n", want: "changed the immutable repository HEAD"},
		{name: "Git remote", script: "git remote add origin https://example.com/repository.git\n", want: "restored or added a Git remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, sha := fixtureRepository(t)
			binDir := t.TempDir()
			tool := filepath.Join(binDir, "mutate-workspace")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nset -eu\n"+tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			request := fixtureRequest(repository, sha)
			request.MaxSteps = 3
			request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
				{Argv: []string{"mutate-workspace"}, TimeoutSeconds: 10},
				{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
			}
			result := Execute(context.Background(), request, Options{
				WorkspaceRoot: t.TempDir(),
				RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
					return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("generated change\n"), 0o644)
				},
			})
			if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureSafetyIntegrity || !strings.Contains(result.FailureReason, tc.want) {
				t.Fatalf("result = %+v, want %q", result, tc.want)
			}
		})
	}
}

func TestExecuteMapsAgentDeadline(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.TimeoutSeconds = 1
	request.CommandPolicy.Commands[0].TimeoutSeconds = 1
	result := Execute(context.Background(), request, Options{WorkspaceRoot: t.TempDir(), RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (string, string, error) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}})
	if result.TerminalState != engineruntime.TerminalTimedOut || result.FailureReason == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWriteOpenCodeConfigOmitsCredentials(t *testing.T) {
	home := t.TempDir()
	provider := testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture-model")
	if err := writeOpenCodeConfig(home, provider, 4); err != nil {
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
	modelConfig := config["provider"].(map[string]any)["engine"].(map[string]any)["models"].(map[string]any)["fixture-model"].(map[string]any)
	if _, ok := modelConfig["options"]; ok {
		t.Fatalf("empty reasoning effort changed model config: %v", modelConfig)
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
	env, err := openCodeEnvironment("/home", "/tmp", testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture-model"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
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
		ModelProvider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture-model"),
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

func TestWriteOpenCodeConfigReferencesDirectCredentialEnvironment(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	home := t.TempDir()
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortXHigh
	if err := writeOpenCodeConfig(home, provider, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "{env:"+modelprovider.TokenEnv+"}") {
		t.Fatal("config does not reference the fixed provider credential environment")
	}
	if strings.Contains(text, credential) {
		t.Fatal("config serialized the provider credential")
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	modelConfig := config["provider"].(map[string]any)["engine"].(map[string]any)["models"].(map[string]any)["fixture-model"].(map[string]any)
	if modelConfig["options"].(map[string]any)["reasoningEffort"] != "xhigh" {
		t.Fatalf("model config = %v", modelConfig)
	}
	env, err := openCodeEnvironment(home, t.TempDir(), provider)
	if err != nil {
		t.Fatal(err)
	}
	credentialEntries := 0
	for _, value := range env {
		if strings.HasPrefix(value, modelprovider.TokenEnv+"=") {
			credentialEntries++
		}
	}
	if credentialEntries != 1 {
		t.Fatalf("credential environment entries = %d", credentialEntries)
	}
}

func TestExecuteRejectsCredentialBearingOutput(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	for _, tc := range []struct {
		name       string
		run        func(context.Context, OpenCodeSpec) (string, string, error)
		edit       func(*engineruntime.ExecutionRequest, string)
		wantReason string
	}{
		{name: "stdout", run: func(context.Context, OpenCodeSpec) (string, string, error) { return credential, "", nil }},
		{name: "stderr", run: func(context.Context, OpenCodeSpec) (string, string, error) { return "", credential, nil }},
		{name: "error", run: func(context.Context, OpenCodeSpec) (string, string, error) { return "", "", errors.New(credential) }},
		{name: "changed file", run: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte(credential), 0o644)
		}},
		{name: "command output", run: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644)
		}, edit: func(request *engineruntime.ExecutionRequest, binDir string) {
			tool := filepath.Join(binDir, "credential-output")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' '"+credential+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			request.MaxSteps = 3
			request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
				{Argv: []string{"credential-output"}, TimeoutSeconds: 10},
				{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(modelprovider.TokenEnv, credential)
			repository, sha := fixtureRepository(t)
			request := fixtureRequest(repository, sha)
			request.ModelProvider = provider
			binDir := t.TempDir()
			if tc.edit != nil {
				tc.edit(&request, binDir)
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			result := Execute(t.Context(), request, Options{WorkspaceRoot: t.TempDir(), RunOpenCode: tc.run})
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			wantReason := tc.wantReason
			if wantReason == "" {
				wantReason = modelprovider.ErrCredentialExposure.Error()
			}
			if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureSafetyIntegrity || result.FailureReason != wantReason {
				t.Fatalf("credential-bearing result was not rejected or sanitized: state=%s reason=%q", result.TerminalState, result.FailureReason)
			}
			if strings.Contains(string(encoded), credential) {
				t.Fatal("rejected result retained the provider credential")
			}
		})
	}
}

func TestDefaultRunOpenCodeRejectsCredentialOutsideRetainedTail(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			t.Setenv(modelprovider.TokenEnv, credential)
			bin := filepath.Join(t.TempDir(), "opencode")
			redirect := ""
			if stream == "stderr" {
				redirect = "exec 1>&2\n"
			}
			script := "#!/bin/sh\n" + redirect + "printf '%s' \"$" + modelprovider.TokenEnv + "\"\ni=0\nwhile [ $i -lt 70000 ]; do printf x; i=$((i+1)); done\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := defaultRunOpenCode(t.Context(), OpenCodeSpec{
				Bin: bin, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir(),
				Provider: provider, Prompt: "edit", MaxSteps: 2, OutputLimit: maxCapturedStream,
			})
			if !errors.Is(err, modelprovider.ErrCredentialExposure) {
				t.Fatalf("credential-bearing %s error = %v", stream, err)
			}
			if strings.Contains(stdout, credential) || strings.Contains(stderr, credential) {
				t.Fatal("retained output unexpectedly still contained the early credential")
			}
		})
	}
}

func TestWriteOpenCodeConfigUsesNativeResponsesProvider(t *testing.T) {
	home := t.TempDir()
	provider := testResponsesProvider("https://provider.example/v1/responses", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	if err := writeOpenCodeConfig(home, provider, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	engine := config["provider"].(map[string]any)["engine"].(map[string]any)
	options := engine["options"].(map[string]any)
	if engine["npm"] != "@ai-sdk/openai" || options["baseURL"] != "https://provider.example/v1" || options["apiKey"] != "{env:"+modelprovider.TokenEnv+"}" {
		t.Fatalf("provider config = %v", engine)
	}
	modelConfig := engine["models"].(map[string]any)["fixture-model"].(map[string]any)
	if modelConfig["options"].(map[string]any)["reasoningEffort"] != "high" {
		t.Fatalf("model config = %v", modelConfig)
	}
	if strings.Contains(string(data), "/responses") {
		t.Fatalf("config retained the operation path: %s", data)
	}
}

func TestValidationCommandsCannotReadProviderOrOpenCodeState(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.ModelProvider = testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	binDir := t.TempDir()
	validator := filepath.Join(binDir, "private-state-check")
	script := `#!/bin/sh
set -eu
test ! -e "$HOME/private-state"
test -z "${PROW_AI_MODEL_PROVIDER_TOKEN:-}"
if [ -r "/proc/$PPID/environ" ]; then
  if tr '\000' '\n' < "/proc/$PPID/environ" | grep -Fq 'fixture-provider-credential-'; then
    echo 'parent credential visible' >&2
    exit 1
  fi
fi
`
	if err := os.WriteFile(validator, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	request.MaxSteps = 3
	request.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"private-state-check"}, TimeoutSeconds: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 10},
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			if err := os.WriteFile(filepath.Join(spec.HomeDir, "private-state"), []byte("selected private evidence\n"), 0o600); err != nil {
				return "", "", err
			}
			return "", "", os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644)
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded {
		t.Fatalf("result = %+v", result)
	}
	if len(result.CommandResults) != 2 || result.CommandResults[0].ExitCode != 0 {
		t.Fatalf("command results = %+v", result.CommandResults)
	}
}

func TestExecuteClassifiesReviewScopeWithoutPatchContent(t *testing.T) {
	repository, sha := fixtureRepository(t)
	request := fixtureRequest(repository, sha)
	request.MaxFiles = 1
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: t.TempDir(),
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (string, string, error) {
			if err := os.WriteFile(filepath.Join(spec.WorkDir, "README"), []byte("changed\n"), 0o644); err != nil {
				return "", "", err
			}
			return "private model output", "private model error", os.WriteFile(filepath.Join(spec.WorkDir, "added.txt"), []byte("added\n"), 0o644)
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureReviewScope {
		t.Fatalf("result = %+v", result)
	}
	if len(result.CommandResults) != len(request.CommandPolicy.Commands) || len(result.Files) != 0 || result.Diff != "" || len(result.ChangedFiles) != 0 {
		t.Fatalf("scope failure retained patch or incomplete command identity: %+v", result)
	}
	for _, command := range result.CommandResults {
		if command.Stdout != "" || command.Stderr != "" {
			t.Fatalf("scope failure retained command output: %+v", command)
		}
	}
}

// OpenCode reaches the provider directly, so it must send the same GitHub
// Copilot integration header the in-process transport sends. Without it Copilot
// rejects the request with an unexplained 403.
func TestWriteOpenCodeConfigSetsCopilotIntegrationHeader(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider modelprovider.Config
		want     bool
	}{
		{"copilot chat completions", testDirectBearerProvider("https://api.githubcopilot.com/chat/completions", "fixture-model"), true},
		{"copilot responses", testResponsesProvider("https://api.githubcopilot.com/responses", "fixture-model"), true},
		{"other provider", testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if err := writeOpenCodeConfig(home, tt.provider, 4); err != nil {
				t.Fatal(err)
			}
			options := readOpenCodeProviderOptions(t, home)
			headers, ok := options["headers"].(map[string]any)
			if !tt.want {
				if ok {
					t.Fatalf("headers = %v, want none for %q", headers, tt.provider.Endpoint)
				}
				return
			}
			if !ok || headers[modelprovider.CopilotIntegrationHeader] != modelprovider.CopilotIntegrationID {
				t.Fatalf("headers = %v, want %s", headers, modelprovider.CopilotIntegrationHeader)
			}
		})
	}
}

func readOpenCodeProviderOptions(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	options, ok := config["provider"].(map[string]any)["engine"].(map[string]any)["options"].(map[string]any)
	if !ok {
		t.Fatalf("provider options missing: %s", data)
	}
	return options
}
