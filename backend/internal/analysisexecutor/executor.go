// Package analysisexecutor runs one credential-free OpenCode failure analysis.
package analysisexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	Bin                string
	WorkDir            string
	HomeDir            string
	TempDir            string
	Gateway            engineruntime.ModelGatewayConfig
	Prompt             string
	MaxSteps           int
	ModelContextTokens int
	ModelOutputTokens  int
}

// OpenCodeRunResult contains the structured result and sanitized aggregates only.
type OpenCodeRunResult struct {
	Structured []byte
	Usage      agentanalysis.WorkspaceUsage
	Telemetry  agentanalysis.WorkspaceOpenCodeTelemetry
}

// OpenCodeRunner runs one native OpenCode session and returns its structured result.
type OpenCodeRunner func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error)

// Options configure one executor process.
type Options struct {
	WorkspaceRoot string
	TempRoot      string
	OpenCodeBin   string
	RunOpenCode   OpenCodeRunner
	Now           func() time.Time
	MountVerifier func(string, string) error
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
		RequestHash: request.Hash, Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
		OpenCodeTelemetry: agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true},
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
	mountVerifier := opts.MountVerifier
	if mountVerifier == nil {
		mountVerifier = verifyPreparedMounts
	}
	if err := mountVerifier(workspaceRoot, request.Manifest.Hash); err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("verify prepared mounts: %v", err))
	}
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
	runResult, runErr := run(ctx, OpenCodeSpec{Bin: bin, WorkDir: workspaceRoot, HomeDir: home, TempDir: temp, Gateway: request.ModelGateway, Prompt: prompt, MaxSteps: request.MaxSteps, ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens})
	if runResult.Usage.Status == "" {
		runResult.Usage.Status = agentanalysis.WorkspaceTelemetryUnavailable
	}
	if runResult.Telemetry.Status == "" {
		runResult.Telemetry.Status = agentanalysis.WorkspaceTelemetryUnavailable
	}
	runResult.Telemetry.StructuredOutputRetriesKnown = true
	result.Usage = runResult.Usage
	result.OpenCodeTelemetry = runResult.Telemetry
	if err := mountVerifier(workspaceRoot, request.Manifest.Hash); err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("prepared mounts changed during analysis: %v", err))
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.OpenCodeTelemetry.TimedOut = true
			result.OpenCodeTelemetry.FailureCode = "timeout"
		}
		return fail(stateForContext(ctx), fmt.Sprintf("run OpenCode analyzer: %v", ctx.Err()))
	}
	if err := verifyInputsBounded(request, sourceRoot, artifactRoot); err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("workspace changed during analysis: %v", err))
	}
	if runErr != nil {
		if result.OpenCodeTelemetry.FailureCode == "" {
			result.OpenCodeTelemetry.FailureCode = openCodeFailureCode(runErr)
		}
		return fail(stateForContext(ctx), fmt.Sprintf("run OpenCode analyzer: %v", runErr))
	}
	analysis, err := agentanalysis.ParseWorkspaceAnalysis(string(runResult.Structured), request.Manifest, artifactRoot, sourceRoot)
	if err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	if err := mountVerifier(workspaceRoot, request.Manifest.Hash); err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("prepared mounts changed during result canonicalization: %v", err))
	}
	if err := verifyInputsBounded(request, sourceRoot, artifactRoot); err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("workspace changed during result canonicalization: %v", err))
	}
	canonical, err := agentanalysis.MarshalWorkspaceAnalysis(analysis)
	if err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("encode canonical analysis: %v", err))
	}
	if err := writeCanonicalResult(resultRoot, canonical, request.OutputLimitBytes); err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	if _, err := readSingleResult(resultRoot, request.OutputLimitBytes); err != nil {
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

func verifyInputsBounded(request agentanalysis.WorkspaceExecutionRequest, sourceRoot, artifactRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return verifyInputs(ctx, request, sourceRoot, artifactRoot)
}

func writeCanonicalResult(root string, data []byte, limit int64) error {
	if int64(len(data)) > limit {
		return fmt.Errorf("canonical analysis exceeds the result bound")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read result directory after analysis: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("result directory was modified by OpenCode")
	}
	file, err := os.OpenFile(filepath.Join(root, agentanalysis.WorkspaceResultFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create canonical analysis: %w", err)
	}
	written, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || written != len(data) {
		return fmt.Errorf("write canonical analysis: %v", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close canonical analysis: %w", closeErr)
	}
	return nil
}

func prepareResultRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create result directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("result directory is unsafe")
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
	return readSingleResultFile(root, agentanalysis.WorkspaceResultFile, limit)
}

func defaultRunOpenCode(ctx context.Context, spec OpenCodeSpec) (result OpenCodeRunResult, retErr error) {
	result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
	result.Telemetry = agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true}
	if err := writeOpenCodeConfig(spec.HomeDir, spec.Gateway, spec.MaxSteps, spec.ModelContextTokens, spec.ModelOutputTokens); err != nil {
		return result, err
	}
	bin, err := exec.LookPath(spec.Bin)
	if err != nil {
		return result, fmt.Errorf("OpenCode executable: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return result, fmt.Errorf("reserve OpenCode port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return result, fmt.Errorf("release OpenCode port: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = spec.WorkDir
	cmd.Env = openCodeEnvironment(spec.HomeDir, spec.TempDir)
	stdout := newBoundedCapture(maxOpenCodeStreamBytes)
	stderr := newBoundedCapture(maxOpenCodeStreamBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return result, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		result.Telemetry.StdoutTruncated = stdout.Truncated()
		result.Telemetry.StderrTruncated = stderr.Truncated()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{}
	if err := waitForOpenCode(ctx, client, baseURL, done); err != nil {
		return result, err
	}
	sessionID, err := createOpenCodeSession(ctx, client, baseURL, spec.WorkDir)
	if err != nil {
		return result, err
	}
	structured, promptErr := promptOpenCode(ctx, client, baseURL, sessionID, spec)
	result.Structured = structured
	usage, telemetry, telemetryErr := fetchOpenCodeTelemetry(ctx, client, baseURL, sessionID, spec.WorkDir)
	if telemetryErr == nil {
		result.Usage, result.Telemetry = usage, telemetry
	} else {
		result.Usage.Status = telemetryStatusForError(telemetryErr)
		result.Telemetry.Status = result.Usage.Status
	}
	if promptErr != nil {
		result.Telemetry.FailureCode = openCodeFailureCode(promptErr)
	}
	return result, promptErr
}

const maxOpenCodeAPIResponseBytes = 1 << 20

func waitForOpenCode(ctx context.Context, client *http.Client, baseURL string, done <-chan error) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/global/health", nil)
		if err == nil {
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					cancel()
					return nil
				}
			}
		}
		cancel()
		select {
		case err := <-done:
			return fmt.Errorf("OpenCode server exited before readiness: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func createOpenCodeSession(ctx context.Context, client *http.Client, baseURL, workDir string) (string, error) {
	var response struct {
		ID string `json:"id"`
	}
	if err := openCodeJSON(ctx, client, http.MethodPost, baseURL+"/session?directory="+url.QueryEscape(workDir), []byte(`{"title":"Prow failure analysis"}`), &response); err != nil {
		return "", fmt.Errorf("create OpenCode session: %w", err)
	}
	if strings.TrimSpace(response.ID) == "" || len(response.ID) > 128 {
		return "", fmt.Errorf("create OpenCode session: invalid session id")
	}
	return response.ID, nil
}

func promptOpenCode(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) ([]byte, error) {
	payload := map[string]any{
		"agent":  "analysis",
		"model":  map[string]any{"providerID": "engine", "modelID": spec.Gateway.Model},
		"format": map[string]any{"type": "json_schema", "schema": agentanalysis.WorkspaceResultSchema()},
		"tools": map[string]bool{
			"read": false, "bash": false, "edit": false, "write": false, "apply_patch": false,
			"webfetch": false, "websearch": false, "task": false, "skill": false,
		},
		"parts": []any{map[string]any{"type": "text", "text": spec.Prompt}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var response struct {
		Info struct {
			Role       string          `json:"role"`
			Structured json.RawMessage `json:"structured"`
			Error      *struct {
				Name string `json:"name"`
			} `json:"error"`
		} `json:"info"`
	}
	endpoint := baseURL + "/session/" + url.PathEscape(sessionID) + "/message?directory=" + url.QueryEscape(spec.WorkDir)
	if err := openCodeJSON(ctx, client, http.MethodPost, endpoint, body, &response); err != nil {
		return nil, fmt.Errorf("prompt OpenCode session: %w", err)
	}
	if response.Info.Error != nil {
		return nil, fmt.Errorf("OpenCode structured output failed: %s", boundedReason(response.Info.Error.Name))
	}
	if response.Info.Role != "assistant" || len(response.Info.Structured) == 0 || bytes.Equal(bytes.TrimSpace(response.Info.Structured), []byte("null")) {
		return nil, fmt.Errorf("OpenCode did not return structured output")
	}
	return slices.Clone(response.Info.Structured), nil
}

func fetchOpenCodeTelemetry(ctx context.Context, client *http.Client, baseURL, sessionID, workDir string) (agentanalysis.WorkspaceUsage, agentanalysis.WorkspaceOpenCodeTelemetry, error) {
	endpoint := baseURL + "/session/" + url.PathEscape(sessionID) + "/message?directory=" + url.QueryEscape(workDir)
	raw, err := openCodeResponse(ctx, client, http.MethodGet, endpoint, nil, maxOpenCodeTelemetryBytes)
	if err != nil {
		return agentanalysis.WorkspaceUsage{}, agentanalysis.WorkspaceOpenCodeTelemetry{}, err
	}
	return parseOpenCodeTelemetry(raw)
}

func telemetryStatusForError(err error) string {
	if strings.Contains(err.Error(), "exceeded the bound") {
		return agentanalysis.WorkspaceTelemetryTruncated
	}
	if strings.Contains(err.Error(), "decode") || strings.Contains(err.Error(), "telemetry") {
		return agentanalysis.WorkspaceTelemetryMalformed
	}
	return agentanalysis.WorkspaceTelemetryUnavailable
}

func openCodeFailureCode(err error) string {
	value := strings.ToLower(err.Error())
	switch {
	case strings.Contains(value, "context"):
		return "context_limit"
	case strings.Contains(value, "structured output"):
		return "structured_output"
	case strings.Contains(value, "http"):
		return "http_error"
	default:
		return "opencode_error"
	}
}

func openCodeResponse(ctx context.Context, client *http.Client, method, endpoint string, body []byte, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("OpenCode API response exceeded the bound")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenCode API returned HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func openCodeJSON(ctx context.Context, client *http.Client, method, endpoint string, body []byte, target any) error {
	data, err := openCodeResponse(ctx, client, method, endpoint, body, maxOpenCodeAPIResponseBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode OpenCode API response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("decode OpenCode API response: trailing data")
	}
	return nil
}

func verifyPreparedMounts(workspaceRoot, manifestHash string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("mountinfo exceeds the bound")
	}
	return verifyPreparedMountInfo(string(data), workspaceRoot, manifestHash)
}

func verifyPreparedMountInfo(raw, workspaceRoot, manifestHash string) error {
	expected := map[string]string{
		filepath.Clean(filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir)):    "/" + manifestHash + "/source",
		filepath.Clean(filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)): "/" + manifestHash + "/artifacts",
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		separator := slices.Index(fields, "-")
		if separator < 0 || separator+3 >= len(fields) {
			continue
		}
		mountPoint := unescapeMountInfo(fields[4])
		identity, ok := expected[mountPoint]
		if !ok {
			continue
		}
		root := unescapeMountInfo(fields[3])
		filesystem := fields[separator+1]
		identityVisible := strings.HasSuffix(root, identity)
		kataVirtioFS := root == "/" && filesystem == "virtiofs"
		if (!identityVisible && !kataVirtioFS) || !mountOption(fields[5], "ro") {
			return fmt.Errorf("mount %s is not the expected read-only prepared input", mountPoint)
		}
		seen[mountPoint] = true
	}
	for mountPoint := range expected {
		if !seen[mountPoint] {
			return fmt.Errorf("mount %s is unavailable", mountPoint)
		}
	}
	return nil
}

func mountOption(value, want string) bool {
	for _, option := range strings.Split(value, ",") {
		if option == want {
			return true
		}
	}
	return false
}

func unescapeMountInfo(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func writeOpenCodeConfig(home string, gateway engineruntime.ModelGatewayConfig, maxSteps, contextTokens, outputTokens int) error {
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	analysisPermissions := map[string]any{
		"*":    "deny",
		"glob": "allow", "grep": "allow", "StructuredOutput": "allow",
		"bash": "deny", "edit": "deny", "write": "deny", "apply_patch": "deny",
		"webfetch": "deny", "websearch": "deny", "task": "deny", "skill": "deny", "external_directory": "deny",
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "share": "disabled", "autoupdate": false, "snapshot": false,
		"provider": map[string]any{"engine": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "engine",
			"options": map[string]any{"baseURL": openAIBase(gateway.Endpoint)},
			"models":  map[string]any{gateway.Model: map[string]any{"limit": map[string]any{"context": contextTokens, "output": outputTokens}}},
		}},
		"agent": map[string]any{"analysis": map[string]any{
			"mode": "primary", "steps": maxSteps, "prompt": agentanalysis.WorkspaceAgentPrompt(), "permission": analysisPermissions,
		}},
		"permission": map[string]any{"*": "deny"},
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
