// Package analysisexecutor runs one credential-free OpenCode failure analysis.
package analysisexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	defaultWorkspaceRoot = "/workspace"
	defaultTempRoot      = "/tmp"
	defaultOpenCodeBin   = "opencode"
)

// OpenCodeSpec is the non-secret analyzer invocation.
type OpenCodeSpec struct {
	Bin      string
	WorkDir  string
	HomeDir  string
	TempDir  string
	Gateway  engineruntime.ModelGatewayConfig
	Prompt   string
	MaxSteps int
}

// OpenCodeRunner runs one native OpenCode session.
type OpenCodeRunner func(context.Context, OpenCodeSpec) error

// Options configure one executor process.
type Options struct {
	WorkspaceRoot string
	TempRoot      string
	OpenCodeBin   string
	RunOpenCode   OpenCodeRunner
	Now           func() time.Time
}

// Execute validates a sealed workspace, runs OpenCode once, and returns one analysis.
func Execute(parent context.Context, request agentanalysis.WorkspaceExecutionRequest, opts Options) agentanalysis.WorkspaceExecutionResult {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	result := agentanalysis.WorkspaceExecutionResult{
		Version: agentanalysis.WorkspaceResultVersion, ContractVersion: agentanalysis.WorkspaceContractVersion,
		RequestHash: request.Hash, Usage: agentanalysis.WorkspaceUsage{},
	}
	fail := func(state engineruntime.TerminalState, reason string) agentanalysis.WorkspaceExecutionResult {
		result.TerminalState = state
		result.FailureReason = boundedReason(reason)
		result.Analysis = nil
		result.DurationMs = max(now().Sub(started).Milliseconds(), 0)
		return result
	}
	if err := agentanalysis.ValidateWorkspaceExecutionRequest(request); err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()

	workspaceRoot := strings.TrimSpace(opts.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	sourceRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir)
	artifactRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)
	resultRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceResultDir)
	if err := prepareResultRoot(resultRoot); err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	if err := verifyInputs(ctx, request, sourceRoot, artifactRoot); err != nil {
		return fail(stateForContext(ctx), err.Error())
	}

	tempRoot := strings.TrimSpace(opts.TempRoot)
	if tempRoot == "" {
		tempRoot = defaultTempRoot
	}
	runtimeRoot, err := os.MkdirTemp(filepath.Clean(tempRoot), "prow-ai-analysis-")
	if err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("create analyzer runtime: %v", err))
	}
	defer os.RemoveAll(runtimeRoot)
	home := filepath.Join(runtimeRoot, "home")
	temp := filepath.Join(runtimeRoot, "tmp")
	for _, path := range []string{home, temp} {
		if err := os.Mkdir(path, 0o700); err != nil {
			return fail(engineruntime.TerminalFailed, fmt.Sprintf("create analyzer runtime directory: %v", err))
		}
	}
	prompt, err := agentanalysis.WorkspaceInstruction(request, workspaceRoot)
	if err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	run := opts.RunOpenCode
	if run == nil {
		run = defaultRunOpenCode
	}
	bin := strings.TrimSpace(opts.OpenCodeBin)
	if bin == "" {
		bin = defaultOpenCodeBin
	}
	runErr := run(ctx, OpenCodeSpec{Bin: bin, WorkDir: workspaceRoot, HomeDir: home, TempDir: temp, Gateway: request.ModelGateway, Prompt: prompt, MaxSteps: request.MaxSteps})
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	verifyErr := verifyInputs(verifyCtx, request, sourceRoot, artifactRoot)
	verifyCancel()
	if verifyErr != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("workspace changed during analysis: %v", verifyErr))
	}
	if runErr != nil {
		return fail(stateForContext(ctx), fmt.Sprintf("run OpenCode analyzer: %v", runErr))
	}
	raw, err := readSingleResult(resultRoot, request.OutputLimitBytes)
	if err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	analysis, err := agentanalysis.ParseWorkspaceAnalysis(raw, request.Manifest, artifactRoot, sourceRoot)
	if err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	result.TerminalState = engineruntime.TerminalSucceeded
	result.Analysis = &analysis
	result.DurationMs = max(now().Sub(started).Milliseconds(), 0)
	validated, err := agentanalysis.ValidateWorkspaceExecutionResult(result, request, artifactRoot, sourceRoot)
	if err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("validate analyzer result: %v", err))
	}
	return validated
}

func verifyInputs(ctx context.Context, request agentanalysis.WorkspaceExecutionRequest, sourceRoot, artifactRoot string) error {
	if err := agentanalysis.VerifySourceWorkspace(ctx, sourceRoot, request.Manifest.Source.Revision); err != nil {
		return err
	}
	return agentanalysis.VerifyArtifactWorkspace(artifactRoot, request.Manifest)
}

func prepareResultRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create result directory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read result directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("result directory must be empty before analysis")
	}
	return nil
}

func readSingleResult(root string, limit int64) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || entries[0].Name() != agentanalysis.WorkspaceResultFile || entries[0].Type()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("OpenCode must write exactly %s", filepath.Join(agentanalysis.WorkspaceResultDir, agentanalysis.WorkspaceResultFile))
	}
	path := filepath.Join(root, agentanalysis.WorkspaceResultFile)
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("analysis result exceeds %d bytes", limit)
	}
	return string(data), nil
}

func defaultRunOpenCode(ctx context.Context, spec OpenCodeSpec) error {
	if err := writeOpenCodeConfig(spec.HomeDir, spec.Gateway, spec.MaxSteps); err != nil {
		return err
	}
	bin, err := exec.LookPath(spec.Bin)
	if err != nil {
		return fmt.Errorf("OpenCode executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "run", "--dir", spec.WorkDir, "--format", "json", "--agent", "build", "--model", "engine/"+spec.Gateway.Model, spec.Prompt)
	cmd.Dir = spec.WorkDir
	cmd.Env = openCodeEnvironment(spec.HomeDir, spec.TempDir)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func writeOpenCodeConfig(home string, gateway engineruntime.ModelGatewayConfig, maxSteps int) error {
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "share": "disabled", "autoupdate": false, "snapshot": false,
		"provider": map[string]any{"engine": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "engine",
			"options": map[string]any{"baseURL": openAIBase(gateway.Endpoint)},
			"models":  map[string]any{gateway.Model: map[string]any{"limit": map[string]any{"context": 128000, "output": 8192}}},
		}},
		"agent": map[string]any{"build": map[string]any{"steps": maxSteps}},
		"permission": map[string]any{
			"edit": "allow", "bash": "allow", "webfetch": "deny", "task": "deny", "skill": "deny", "external_directory": "deny",
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "opencode.json"), data, 0o600)
}

func openCodeEnvironment(home, temp string) []string {
	env := []string{
		"HOME=" + home, "TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"OPENCODE_CONFIG=" + filepath.Join(home, ".config", "opencode", "opencode.json"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=true", "OPENCODE_DISABLE_AUTOUPDATE=true", "OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0",
	}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func openAIBase(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions")
	return strings.TrimRight(parsed.String(), "/")
}

func stateForContext(ctx context.Context) engineruntime.TerminalState {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return engineruntime.TerminalTimedOut
	case errors.Is(ctx.Err(), context.Canceled):
		return engineruntime.TerminalCancelled
	default:
		return engineruntime.TerminalFailed
	}
}

func boundedReason(value string) string {
	value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(value))
	if len(value) > 1024 {
		value = value[:1024]
	}
	if value == "" {
		return "workspace analysis failed"
	}
	return value
}
