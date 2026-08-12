// Package fixexecutor runs one non-secret code-generation request.
package fixexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	defaultWorkspaceRoot = "/workspace"
	defaultOpenCodeBin   = "opencode"
	maxCapturedStream    = 64 << 10
)

// OpenCodeSpec is the non-secret invocation passed to the coding agent.
type OpenCodeSpec struct {
	Bin         string
	WorkDir     string
	HomeDir     string
	TempDir     string
	Provider    modelprovider.Config
	Prompt      string
	MaxSteps    int
	OutputLimit int64
}

// OpenCodeRunner runs the configured coding agent.
type OpenCodeRunner func(context.Context, OpenCodeSpec) (stdout, stderr string, err error)

// Options configure one executor process.
type Options struct {
	WorkspaceRoot string
	OpenCodeBin   string
	RunOpenCode   OpenCodeRunner
	Now           func() time.Time
}

// Execute clones the pinned repository, runs OpenCode, validates commands, and returns a staged patch.
func Execute(parent context.Context, request engineruntime.ExecutionRequest, opts Options) engineruntime.ExecutionResult {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	result := baseResult(request)
	var credential modelprovider.CredentialGuard
	finish := func(state engineruntime.TerminalState, reason string) engineruntime.ExecutionResult {
		result.TerminalState = state
		result.FailureReason = strings.TrimSpace(credential.SanitizeReason(reason))
		result.DurationMs = max(now().Sub(started).Milliseconds(), 0)
		if state == engineruntime.TerminalSucceeded {
			result.FailureReason = ""
		}
		if err := validateCredentialFreeResult(credential, result); err != nil {
			return compactFailure(request, now().Sub(started), err)
		}
		if err := result.Validate(request); err != nil {
			return compactFailure(request, now().Sub(started), fmt.Errorf("executor result validation: %w", err))
		}
		return result
	}
	if err := request.Validate(); err != nil {
		return compactFailure(request, now().Sub(started), err)
	}
	var err error
	credential, err = modelprovider.NewCredentialGuard(request.ModelProvider, os.LookupEnv)
	if err != nil {
		return compactFailure(request, now().Sub(started), err)
	}
	if request.CommandPolicy.AllowShell {
		return finish(engineruntime.TerminalFailed, "shell execution is not supported")
	}
	if err := validateFinalCommands(request.CommandPolicy.Commands); err != nil {
		return finish(engineruntime.TerminalFailed, err.Error())
	}
	agentSteps := request.MaxSteps - len(request.CommandPolicy.Commands)
	if agentSteps < 1 {
		return finish(engineruntime.TerminalFailed, "max steps must reserve at least one coding-agent step")
	}

	ctx, cancel := context.WithTimeout(parent, time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()
	workspaceRoot := strings.TrimSpace(opts.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("create workspace root: %v", err))
	}
	work := filepath.Join(workspaceRoot, "repository")
	home := filepath.Join(workspaceRoot, "home")
	temp := filepath.Join(workspaceRoot, "tmp")
	for _, path := range []string{work, home, temp} {
		if err := os.RemoveAll(path); err != nil {
			return finish(engineruntime.TerminalFailed, fmt.Sprintf("reset workspace: %v", err))
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return finish(engineruntime.TerminalFailed, fmt.Sprintf("create workspace: %v", err))
		}
	}

	cloneOut, cloneErr, err := runCommand(ctx, workspaceRoot, gitEnvironment(home, temp), maxCapturedStream,
		"git", "-c", "credential.helper=", "clone", "--no-checkout", request.RepositoryURL, work)
	result.StdoutSummary = tail(cloneOut, maxCapturedStream)
	result.StderrSummary = tail(cloneErr, maxCapturedStream)
	if err != nil {
		return finish(stateForContext(ctx), fmt.Sprintf("clone repository: %v", err))
	}
	if _, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "checkout", "--detach", request.CommitSHA); err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(stateForContext(ctx), fmt.Sprintf("checkout immutable commit: %v", err))
	}
	head, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "rev-parse", "HEAD")
	if err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(stateForContext(ctx), fmt.Sprintf("read checked-out commit: %v", err))
	}
	if strings.TrimSpace(head) != request.ExpectedBaseSHA {
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("checked-out commit %s does not match expected base %s", strings.TrimSpace(head), request.ExpectedBaseSHA))
	}
	if _, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "remote", "remove", "origin"); err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("remove repository remote: %v", err))
	}
	if err := setGitMetadataWritable(filepath.Join(work, ".git"), false); err != nil {
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("protect Git metadata: %v", err))
	}

	bin := strings.TrimSpace(opts.OpenCodeBin)
	if bin == "" {
		bin = defaultOpenCodeBin
	}
	runOpenCode := opts.RunOpenCode
	if runOpenCode == nil {
		runOpenCode = defaultRunOpenCode
	}
	stdout, stderr, agentErr := runOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: work, HomeDir: home, TempDir: temp,
		Provider: request.ModelProvider, Prompt: request.Prompt, MaxSteps: agentSteps,
		OutputLimit: request.OutputLimitBytes,
	})
	if err := setGitMetadataWritable(filepath.Join(work, ".git"), true); err != nil {
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("restore Git metadata: %v", err))
	}
	if err := credential.CheckStrings(stdout, stderr); err != nil {
		return compactFailure(request, now().Sub(started), err)
	}
	result.StdoutSummary = appendSummary(result.StdoutSummary, openCodeSummary(stdout))
	result.StderrSummary = appendSummary(result.StderrSummary, stderr)
	if errors.Is(agentErr, modelprovider.ErrCredentialExposure) {
		return compactFailure(request, now().Sub(started), modelprovider.ErrCredentialExposure)
	}
	if agentErr != nil {
		return finish(stateForContext(ctx), safeOpenCodeFailure(agentErr))
	}
	head, stderr, err = runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != request.ExpectedBaseSHA {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, "coding agent changed the immutable repository HEAD")
	}
	remotes, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "remote")
	if err != nil || strings.TrimSpace(remotes) != "" {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, "coding agent restored or added a Git remote")
	}
	if _, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "add", "-A"); err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(stateForContext(ctx), fmt.Sprintf("stage generated patch: %v", err))
	}
	stagedBefore, stderr, err := renderStagedDiff(ctx, work, home, temp, int(request.OutputLimitBytes))
	if err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(stateForContext(ctx), fmt.Sprintf("snapshot generated patch: %v", err))
	}

	for index, command := range request.CommandPolicy.Commands {
		commandStarted := now()
		timeout := time.Duration(command.TimeoutSeconds) * time.Second
		commandCtx := ctx
		commandCancel := func() {}
		if timeout > 0 {
			commandCtx, commandCancel = context.WithTimeout(ctx, timeout)
		}
		stdout, stderr, commandErr := runCommand(commandCtx, work, executionEnvironment(home, temp), commandOutputLimit(request), command.Argv...)
		commandState := stateForContext(commandCtx)
		commandTimedOut := errors.Is(commandCtx.Err(), context.DeadlineExceeded)
		commandCancel()
		commandResult := engineruntime.CommandResult{
			Argv: append([]string(nil), command.Argv...), ExitCode: commandExitCode(commandErr),
			DurationMs: max(now().Sub(commandStarted).Milliseconds(), 0),
			Stdout:     tail(stdout, commandOutputLimit(request)), Stderr: tail(stderr, commandOutputLimit(request)),
			TimedOut: commandTimedOut,
		}
		result.CommandResults = append(result.CommandResults, commandResult)
		result.StdoutSummary = appendSummary(result.StdoutSummary, stdout)
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		if commandErr != nil {
			var executableErr *exec.Error
			if errors.As(commandErr, &executableErr) {
				return finish(engineruntime.TerminalFailed, fmt.Sprintf("validation command %d executable %q is unavailable in the executor image", index+1, command.Argv[0]))
			}
			return finish(commandState, fmt.Sprintf("validation command %d failed: %v", index+1, commandErr))
		}
	}
	if _, _, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "diff", "--quiet"); err != nil {
		return finish(engineruntime.TerminalFailed, "validation commands modified tracked workspace files")
	}
	untracked, stderr, err := runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, fmt.Sprintf("inspect validation workspace: %v", err))
	}
	if strings.TrimSpace(untracked) != "" {
		return finish(engineruntime.TerminalFailed, "validation commands created untracked workspace files")
	}
	head, stderr, err = runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != request.ExpectedBaseSHA {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, "validation commands changed the immutable repository HEAD")
	}
	remotes, stderr, err = runCommand(ctx, work, gitEnvironment(home, temp), maxCapturedStream, "git", "remote")
	if err != nil || strings.TrimSpace(remotes) != "" {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(engineruntime.TerminalFailed, "validation commands restored or added a Git remote")
	}
	stagedAfter, stderr, err := renderStagedDiff(ctx, work, home, temp, int(request.OutputLimitBytes))
	if err != nil {
		result.StderrSummary = appendSummary(result.StderrSummary, stderr)
		return finish(stateForContext(ctx), fmt.Sprintf("verify generated patch: %v", err))
	}
	if stagedAfter != stagedBefore {
		return finish(engineruntime.TerminalFailed, "validation commands modified the staged patch")
	}

	changed, files, diff, err := stagedResult(ctx, work, home, temp, int(request.OutputLimitBytes))
	if err != nil {
		return finish(stateForContext(ctx), err.Error())
	}
	result.ChangedFiles = changed
	result.Files = files
	result.Diff = diff
	return finish(engineruntime.TerminalSucceeded, "")
}

func baseResult(request engineruntime.ExecutionRequest) engineruntime.ExecutionResult {
	return engineruntime.ExecutionResult{
		Version: engineruntime.ExecutionContractVersion, BaseSHA: request.ExpectedBaseSHA,
		Files: map[string]string{}, TerminalState: engineruntime.TerminalFailed,
	}
}

func compactFailure(request engineruntime.ExecutionRequest, duration time.Duration, err error) engineruntime.ExecutionResult {
	return engineruntime.ExecutionResult{
		Version: engineruntime.ExecutionContractVersion, BaseSHA: request.ExpectedBaseSHA,
		Files: map[string]string{}, TerminalState: engineruntime.TerminalFailed,
		DurationMs: max(duration.Milliseconds(), 0), FailureReason: oneLine(err.Error()),
	}
}

func validateCredentialFreeResult(credential modelprovider.CredentialGuard, result engineruntime.ExecutionResult) error {
	if err := credential.CheckStrings(result.Diff, result.StdoutSummary, result.StderrSummary, result.FailureReason); err != nil {
		return err
	}
	for _, name := range result.ChangedFiles {
		if err := credential.CheckStrings(name); err != nil {
			return err
		}
	}
	for name, content := range result.Files {
		if err := credential.CheckStrings(name, content); err != nil {
			return err
		}
	}
	for _, command := range result.CommandResults {
		if err := credential.CheckStrings(command.Stdout, command.Stderr, strings.Join(command.Argv, "\x00")); err != nil {
			return err
		}
	}
	return nil
}

func safeOpenCodeFailure(err error) string {
	if err == nil {
		return "coding agent failed"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("coding agent exited with status %d", exitErr.ExitCode())
	}
	return "coding agent failed"
}

func validateFinalCommands(commands []engineruntime.ExecutionCommand) error {
	if len(commands) == 0 {
		return fmt.Errorf("at least one final validation command is required")
	}
	for index, command := range commands {
		if err := engineruntime.ValidateExecutionCommandArgv(command.Argv); err != nil {
			return fmt.Errorf("validation command %d: %w", index+1, err)
		}
	}
	last := commands[len(commands)-1].Argv
	if !equalStrings(last, []string{"git", "diff", "--cached", "--check"}) {
		return fmt.Errorf("the final validation command must be git diff --cached --check")
	}
	return nil
}

func stagedResult(ctx context.Context, work, home, temp string, outputLimit int) ([]string, map[string]string, string, error) {
	env := gitEnvironment(home, temp)
	status, stderr, err := runCommand(ctx, work, env, maxCapturedStream, "git", "diff", "--cached", "--name-status")
	if err != nil {
		return nil, nil, "", fmt.Errorf("read staged status: %v: %s", err, oneLine(stderr))
	}
	var changed []string
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			return nil, nil, "", fmt.Errorf("malformed staged status %q", line)
		}
		switch parts[0][0] {
		case 'A', 'M':
		default:
			return nil, nil, "", fmt.Errorf("unsupported staged change %q", line)
		}
		changed = append(changed, parts[len(parts)-1])
	}
	sort.Strings(changed)
	files := make(map[string]string, len(changed))
	for _, name := range changed {
		content, stderr, err := runCommandExact(ctx, work, env, outputLimit, "git", "show", ":"+name)
		if err != nil {
			return nil, nil, "", fmt.Errorf("read staged file %s: %v: %s", name, err, oneLine(stderr))
		}
		if !utf8.ValidString(content) {
			return nil, nil, "", fmt.Errorf("staged file %s is not valid UTF-8", name)
		}
		files[name] = content
	}
	diff, stderr, err := renderStagedDiff(ctx, work, home, temp, outputLimit)
	if err != nil {
		return nil, nil, "", fmt.Errorf("render staged diff: %v: %s", err, oneLine(stderr))
	}
	return changed, files, diff, nil
}

func renderStagedDiff(ctx context.Context, work, home, temp string, outputLimit int) (string, string, error) {
	return runCommandExact(ctx, work, gitEnvironment(home, temp), outputLimit,
		"git", "diff", "--cached", "--no-ext-diff", "--no-color", "--binary", "--src-prefix=a/", "--dst-prefix=b/")
}

func defaultRunOpenCode(ctx context.Context, spec OpenCodeSpec) (string, string, error) {
	if err := writeOpenCodeConfig(spec.HomeDir, spec.Provider, spec.MaxSteps); err != nil {
		return "", "", err
	}
	bin, err := exec.LookPath(spec.Bin)
	if err != nil {
		return "", "", fmt.Errorf("opencode executable: %w", err)
	}
	argv := []string{bin, "run", "--dir", spec.WorkDir, "--format", "json", "--agent", "build", "--model", "engine/" + spec.Provider.Model, spec.Prompt}
	env, err := openCodeEnvironment(spec.HomeDir, spec.TempDir, spec.Provider)
	if err != nil {
		return "", "", err
	}
	credential, err := modelprovider.NewCredentialGuard(spec.Provider, os.LookupEnv)
	if err != nil {
		return "", "", err
	}
	return runOpenCodeCommand(ctx, spec.WorkDir, env, credential, min(int(spec.OutputLimit), maxCapturedStream), argv...)
}

func writeOpenCodeConfig(home string, provider modelprovider.Config, maxSteps int) error {
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	adapter, err := modelprovider.OpenCodeAdapterFor(provider)
	if err != nil {
		return err
	}
	providerOptions := map[string]any{"baseURL": adapter.BaseURL}
	if provider.Auth.Type == modelprovider.AuthTypeBearer {
		providerOptions["apiKey"] = "{env:" + modelprovider.TokenEnv + "}"
	}
	modelOptions := map[string]any{"limit": map[string]any{"context": 128000, "output": 8192}}
	if provider.ReasoningEffort != "" {
		modelOptions["options"] = map[string]any{"reasoningEffort": string(provider.ReasoningEffort)}
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "share": "disabled", "autoupdate": false, "snapshot": false,
		"provider": map[string]any{"engine": map[string]any{
			"npm": adapter.NPM, "name": "engine",
			"options": providerOptions,
			"models":  map[string]any{provider.Model: modelOptions},
		}},
		"agent": map[string]any{"build": map[string]any{"steps": maxSteps}},
		"permission": map[string]any{
			"edit": "allow", "bash": "deny", "webfetch": "deny", "task": "deny", "skill": "deny", "external_directory": "deny",
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "opencode.json"), data, 0o600)
}

func openCodeEnvironment(home, temp string, provider modelprovider.Config) ([]string, error) {
	credential, err := modelprovider.NewCredentialGuard(provider, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	env := append(baseEnvironment(home, temp),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"OPENCODE_CONFIG="+filepath.Join(home, ".config", "opencode", "opencode.json"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=true", "OPENCODE_DISABLE_AUTOUPDATE=true", "OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
	)
	return append(env, credential.Environment()...), nil
}

func gitEnvironment(home, temp string) []string {
	return append(baseEnvironment(home, temp), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
}

func executionEnvironment(home, temp string) []string { return baseEnvironment(home, temp) }

func baseEnvironment(home, temp string) []string {
	env := []string{"HOME=" + home, "TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func runOpenCodeCommand(ctx context.Context, dir string, env []string, credential modelprovider.CredentialGuard, limit int, argv ...string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	stdout := newTailBuffer(limit)
	stderr := newTailBuffer(limit)
	stdoutCredential := credential.NewDetector()
	stderrCredential := credential.NewDetector()
	cmd.Stdout = io.MultiWriter(stdout, stdoutCredential)
	cmd.Stderr = io.MultiWriter(stderr, stderrCredential)
	err := cmd.Run()
	if stdoutCredential.Detected() || stderrCredential.Detected() {
		return stdout.String(), stderr.String(), modelprovider.ErrCredentialExposure
	}
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

func runCommand(ctx context.Context, dir string, env []string, limit int, argv ...string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	stdout := newTailBuffer(limit)
	stderr := newTailBuffer(limit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

func runCommandExact(ctx context.Context, dir string, env []string, limit int, argv ...string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	stdout := &exactBuffer{limit: max(limit, 1)}
	stderr := newTailBuffer(maxCapturedStream)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	if stdout.exceeded {
		return stdout.String(), stderr.String(), fmt.Errorf("command output exceeds %d bytes", stdout.limit)
	}
	return stdout.String(), stderr.String(), err
}

func setGitMetadataWritable(root string, writable bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.IsDir() {
			mode = 0o555
		}
		if writable {
			mode = 0o644
			if info.IsDir() {
				mode = 0o755
			}
		}
		return os.Chmod(path, mode)
	})
}

func openCodeSummary(output string) string {
	var summaries []string
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Type string `json:"type"`
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "text" && strings.TrimSpace(event.Part.Text) != "" {
			summaries = append(summaries, strings.TrimSpace(event.Part.Text))
		}
	}
	if len(summaries) == 0 {
		return tail(output, maxCapturedStream)
	}
	return strings.Join(summaries, "\n")
}

func stateForContext(ctx context.Context) engineruntime.TerminalState {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return engineruntime.TerminalTimedOut
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return engineruntime.TerminalCancelled
	}
	return engineruntime.TerminalFailed
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func commandOutputLimit(request engineruntime.ExecutionRequest) int {
	limit := int(request.OutputLimitBytes) / max(len(request.CommandPolicy.Commands)+4, 1)
	return min(max(limit, 1024), maxCapturedStream)
}

func appendSummary(current, next string) string {
	return tail(strings.TrimSpace(current+"\n"+next), maxCapturedStream)
}

func tail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type exactBuffer struct {
	limit    int
	buf      []byte
	exceeded bool
}

func (b *exactBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		if len(value) > remaining {
			b.buf = append(b.buf, value[:remaining]...)
		} else {
			b.buf = append(b.buf, value...)
		}
	}
	if original > remaining {
		b.exceeded = true
	}
	return original, nil
}

func (b *exactBuffer) String() string { return string(bytes.Clone(b.buf)) }

type tailBuffer struct {
	limit int
	buf   []byte
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: max(limit, 1)} }

func (b *tailBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) >= b.limit {
		b.buf = append(b.buf[:0], value[len(value)-b.limit:]...)
		return original, nil
	}
	if len(b.buf)+len(value) > b.limit {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)+len(value)-b.limit:]...)
	}
	b.buf = append(b.buf, value...)
	return original, nil
}

func (b *tailBuffer) String() string { return string(bytes.Clone(b.buf)) }
