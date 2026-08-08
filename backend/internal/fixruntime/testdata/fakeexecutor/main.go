package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	requestEnv    = "PROW_AI_FIX_EXECUTION_REQUEST_B64"
	workspaceEnv  = "PROW_AI_FIX_WORKSPACE"
	fixturePrompt = "Create agent-sandbox-spike.txt containing exactly agent-sandbox-v0.5.3, then run the allowed validation command."
	fixtureFile   = "agent-sandbox-spike.txt"
	fixtureText   = "agent-sandbox-v0.5.3\n"
	outputTail    = 32 << 10
)

func main() {
	os.Exit(run())
}

func run() int {
	started := time.Now()
	request, err := readRequest()
	if err != nil {
		return emit(failedResult(engineruntime.ExecutionRequest{}, started, err))
	}
	result := engineruntime.ExecutionResult{
		Version:       engineruntime.ExecutionContractVersion,
		BaseSHA:       request.ExpectedBaseSHA,
		Files:         map[string]string{},
		TerminalState: engineruntime.TerminalFailed,
	}
	finish := func(err error) int {
		result.DurationMs = time.Since(started).Milliseconds()
		if err != nil {
			result.FailureReason = oneLine(err.Error())
		}
		if validateErr := result.Validate(request); validateErr != nil {
			result = failedResult(request, started, fmt.Errorf("invalid executor result: %w", validateErr))
		}
		return emit(result)
	}
	if request.CommandPolicy.AllowShell {
		return finish(fmt.Errorf("shell execution is not supported"))
	}
	if request.Prompt != fixturePrompt {
		return finish(fmt.Errorf("unsupported deterministic prompt"))
	}
	if request.MaxSteps < 1+len(request.CommandPolicy.Commands) {
		return finish(fmt.Errorf("max steps does not cover the edit and validation commands"))
	}
	for _, command := range request.CommandPolicy.Commands {
		if !sameArgv(command.Argv, []string{"git", "diff", "--cached", "--check"}) {
			return finish(fmt.Errorf("command %q is not in the deterministic executor allowlist", strings.Join(command.Argv, " ")))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()
	workspaceRoot := strings.TrimSpace(os.Getenv(workspaceEnv))
	if workspaceRoot == "" {
		workspaceRoot = "/workspace"
	}
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return finish(fmt.Errorf("create workspace root: %w", err))
	}
	work := filepath.Join(workspaceRoot, "repository")
	if err := os.RemoveAll(work); err != nil {
		return finish(fmt.Errorf("reset workspace: %w", err))
	}
	cloneStdout, cloneStderr, err := command(ctx, workspaceRoot, 0, "git", "-c", "credential.helper=", "clone", "--no-checkout", request.RepositoryURL, work)
	result.StdoutSummary = tail(cloneStdout, outputTail)
	result.StderrSummary = tail(cloneStderr, outputTail)
	if err != nil {
		return finish(fmt.Errorf("clone repository: %w", err))
	}
	if _, stderr, err := command(ctx, work, 0, "git", "checkout", "--detach", request.CommitSHA); err != nil {
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		return finish(fmt.Errorf("checkout immutable commit: %w", err))
	}
	head, stderr, err := command(ctx, work, 0, "git", "rev-parse", "HEAD")
	if err != nil {
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		return finish(fmt.Errorf("read checked out commit: %w", err))
	}
	if strings.TrimSpace(head) != request.ExpectedBaseSHA {
		return finish(fmt.Errorf("checked out commit %s does not match expected base %s", strings.TrimSpace(head), request.ExpectedBaseSHA))
	}
	if err := os.WriteFile(filepath.Join(work, fixtureFile), []byte(fixtureText), 0o644); err != nil {
		return finish(fmt.Errorf("write deterministic change: %w", err))
	}

	if _, stderr, err := command(ctx, work, 0, "git", "add", "-A"); err != nil {
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		return finish(fmt.Errorf("stage deterministic change: %w", err))
	}

	for _, allowed := range request.CommandPolicy.Commands {
		commandStarted := time.Now()
		timeout := time.Duration(allowed.TimeoutSeconds) * time.Second
		stdout, stderr, runErr := command(ctx, work, timeout, allowed.Argv...)
		commandResult := engineruntime.CommandResult{
			Argv:       append([]string(nil), allowed.Argv...),
			ExitCode:   exitCode(runErr),
			DurationMs: time.Since(commandStarted).Milliseconds(),
			Stdout:     tail(stdout, outputTail),
			Stderr:     tail(stderr, outputTail),
			TimedOut:   errors.Is(runErr, context.DeadlineExceeded),
		}
		result.CommandResults = append(result.CommandResults, commandResult)
		result.StdoutSummary = tail(result.StdoutSummary+"\n"+stdout, outputTail)
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		if runErr != nil {
			return finish(fmt.Errorf("validation command %q failed: %w", strings.Join(allowed.Argv, " "), runErr))
		}
	}
	changedRaw, stderr, err := command(ctx, work, 0, "git", "diff", "--cached", "--name-only", "--diff-filter=AM")
	if err != nil {
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		return finish(fmt.Errorf("list changed files: %w", err))
	}
	for _, name := range strings.Split(strings.TrimSpace(changedRaw), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(work, name))
		if err != nil {
			return finish(fmt.Errorf("read changed file %s: %w", name, err))
		}
		result.ChangedFiles = append(result.ChangedFiles, name)
		result.Files[name] = string(content)
	}
	sort.Strings(result.ChangedFiles)
	result.Diff, stderr, err = command(ctx, work, 0, "git", "diff", "--cached", "--no-ext-diff", "--no-color", "--binary", "--src-prefix=a/", "--dst-prefix=b/")
	if err != nil {
		result.StderrSummary = tail(result.StderrSummary+"\n"+stderr, outputTail)
		return finish(fmt.Errorf("render unified diff: %w", err))
	}
	result.TerminalState = engineruntime.TerminalSucceeded
	result.FailureReason = ""
	return finish(nil)
}

func readRequest() (engineruntime.ExecutionRequest, error) {
	encoded := strings.TrimSpace(os.Getenv(requestEnv))
	if encoded == "" {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("%s is required", requestEnv)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("decode request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request engineruntime.ExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func failedResult(request engineruntime.ExecutionRequest, started time.Time, err error) engineruntime.ExecutionResult {
	return engineruntime.ExecutionResult{
		Version:       engineruntime.ExecutionContractVersion,
		BaseSHA:       request.ExpectedBaseSHA,
		Files:         map[string]string{},
		TerminalState: engineruntime.TerminalFailed,
		DurationMs:    time.Since(started).Milliseconds(),
		FailureReason: oneLine(err.Error()),
	}
}

func emit(result engineruntime.ExecutionResult) int {
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Printf(`{"version":1,"terminal_state":"failed","failure_reason":"encode result"}`)
		return 1
	}
	fmt.Println(string(data))
	if result.TerminalState == engineruntime.TerminalSucceeded {
		return 0
	}
	return 1
}

func command(parent context.Context, workdir string, timeout time.Duration, argv ...string) (string, string, error) {
	ctx := parent
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME=/tmp/home")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func tail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func sameArgv(left, right []string) bool {
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
