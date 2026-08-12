// Package analysisexecutor runs one non-secret OpenCode failure analysis.
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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	defaultWorkspaceRoot      = "/workspace"
	defaultTempRoot           = "/tmp"
	defaultOpenCodeBin        = "opencode"
	openCodeEvidenceAgent     = "analysis-evidence"
	openCodeFinalizationAgent = "analysis-finalize"
	openCodeFinalizationSteps = 2
)

// OpenCodeSpec is the non-secret analyzer invocation.
type OpenCodeSpec struct {
	Bin                string
	WorkDir            string
	HomeDir            string
	TempDir            string
	Provider           modelprovider.Config
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
	var credential modelprovider.CredentialGuard
	result := agentanalysis.WorkspaceExecutionResult{
		Version: agentanalysis.WorkspaceResultVersion, ContractVersion: agentanalysis.WorkspaceContractVersion,
		RequestHash: request.Hash, Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
		OpenCodeTelemetry: agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true},
	}
	fail := func(state engineruntime.TerminalState, reason string) agentanalysis.WorkspaceExecutionResult {
		result.TerminalState = state
		result.FailureReason = boundedReason(credential.SanitizeReason(reason))
		result.Analysis = nil
		result.DurationMs = max(now().Sub(started).Milliseconds(), 0)
		return result
	}
	if err := agentanalysis.ValidateWorkspaceExecutionRequest(request); err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	var err error
	credential, err = modelprovider.NewCredentialGuard(request.ModelProvider, os.LookupEnv)
	if err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	result.OpenCodeTelemetry.ProviderCredentialMode = request.ModelProvider.CredentialMode
	result.OpenCodeTelemetry.ProviderAPI = request.ModelProvider.API
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
	if err := verifyReadSafeWorkspace(ctx, sourceRoot, request.Manifest.Artifacts); err != nil {
		return fail(engineruntime.TerminalFailed, err.Error())
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
	runResult, runErr := run(ctx, OpenCodeSpec{Bin: bin, WorkDir: workspaceRoot, HomeDir: home, TempDir: temp, Provider: request.ModelProvider, Prompt: prompt, MaxSteps: request.MaxSteps, ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens})
	if err := validateCredentialFreeOpenCodeRun(credential, runResult, runErr); err != nil {
		result.OpenCodeTelemetry.FailureCode = "credential_exposure"
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	if runResult.Usage.Status == "" {
		runResult.Usage.Status = agentanalysis.WorkspaceTelemetryUnavailable
	}
	if runResult.Telemetry.Status == "" {
		runResult.Telemetry.Status = agentanalysis.WorkspaceTelemetryUnavailable
	}
	runResult.Telemetry.ProviderCredentialMode = request.ModelProvider.CredentialMode
	runResult.Telemetry.ProviderAPI = request.ModelProvider.API
	if runResult.Telemetry.ProviderRequests == 0 && runResult.Telemetry.StepsUsed > 0 {
		runResult.Telemetry.ProviderRequests = runResult.Telemetry.StepsUsed
		if !runResult.Telemetry.Error.Available {
			runResult.Telemetry.ProviderRequestsKnown = true
		}
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
	analysis, validation, err := agentanalysis.ParseWorkspaceAnalysis(string(runResult.Structured), request.Manifest, artifactRoot, sourceRoot)
	result.ResultValidation = validation
	if err != nil {
		result.OpenCodeTelemetry.FailureCode = "analysis_result_invalid"
		return fail(engineruntime.TerminalFailed, agentanalysis.WorkspaceResultRejectedReason)
	}
	if (len(analysis.SourceCitations) > 0 || len(analysis.RelevantFiles) > 0) && result.OpenCodeTelemetry.SourceEvidenceToolCalls < 1 {
		result.OpenCodeTelemetry.FailureCode = "source_evidence_unavailable"
		return fail(engineruntime.TerminalFailed, "workspace analysis contains source claims without successful source evidence")
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
	if err := credential.CheckBytes(canonical); err != nil {
		result.OpenCodeTelemetry.FailureCode = "credential_exposure"
		return fail(engineruntime.TerminalFailed, err.Error())
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
	if err := validateCredentialFreeWorkspaceResult(credential, result); err != nil {
		result.OpenCodeTelemetry.FailureCode = "credential_exposure"
		return fail(engineruntime.TerminalFailed, err.Error())
	}
	validated, err := agentanalysis.ValidateWorkspaceExecutionResult(result, request, artifactRoot, sourceRoot)
	if err != nil {
		return fail(engineruntime.TerminalFailed, fmt.Sprintf("validate analyzer result: %v", err))
	}
	return validated
}

func verifyInputs(ctx context.Context, request agentanalysis.WorkspaceExecutionRequest, sourceRoot, artifactRoot string) error {
	if err := agentanalysis.VerifyPreparedSourceWorkspace(ctx, sourceRoot, request.Manifest.Source.Revision, request.SourceModePolicy); err != nil {
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

func validateCredentialFreeOpenCodeRun(credential modelprovider.CredentialGuard, result OpenCodeRunResult, runErr error) error {
	if err := credential.CheckBytes(result.Structured); err != nil {
		return err
	}
	if runErr != nil {
		if err := credential.CheckStrings(runErr.Error()); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(struct {
		Usage     agentanalysis.WorkspaceUsage             `json:"usage"`
		Telemetry agentanalysis.WorkspaceOpenCodeTelemetry `json:"telemetry"`
	}{result.Usage, result.Telemetry})
	if err != nil {
		return fmt.Errorf("encode sanitized OpenCode result")
	}
	return credential.CheckBytes(encoded)
}

func validateCredentialFreeWorkspaceResult(credential modelprovider.CredentialGuard, result agentanalysis.WorkspaceExecutionResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode workspace analysis result")
	}
	return credential.CheckBytes(encoded)
}

func defaultRunOpenCode(ctx context.Context, spec OpenCodeSpec) (result OpenCodeRunResult, retErr error) {
	result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
	result.Telemetry = agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true}
	if err := writeOpenCodeConfig(spec.HomeDir, spec.Provider, spec.MaxSteps, spec.ModelContextTokens, spec.ModelOutputTokens); err != nil {
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
	env, err := openCodeEnvironment(spec.HomeDir, spec.TempDir, spec.Provider)
	if err != nil {
		return result, err
	}
	cmd.Env = env
	credential, err := modelprovider.NewCredentialGuard(spec.Provider, os.LookupEnv)
	if err != nil {
		return result, err
	}
	stdout := newBoundedCapture(maxOpenCodeStreamBytes)
	stderr := newBoundedCapture(maxOpenCodeStreamBytes)
	stdoutCredential := credential.NewDetector()
	stderrCredential := credential.NewDetector()
	cmd.Stdout = io.MultiWriter(stdout, stdoutCredential)
	cmd.Stderr = io.MultiWriter(stderr, stderrCredential)
	if err := cmd.Start(); err != nil {
		return result, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		stopOpenCodeProcess(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}, done)
		result.Telemetry.StdoutTruncated = stdout.Truncated()
		result.Telemetry.StderrTruncated = stderr.Truncated()
		if stdoutCredential.Detected() || stderrCredential.Detected() {
			result = OpenCodeRunResult{
				Usage:     agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
				Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true, FailureCode: "credential_exposure"},
			}
			retErr = modelprovider.ErrCredentialExposure
		}
	}()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{}
	version, err := waitForOpenCode(ctx, client, baseURL, done)
	if err != nil {
		return result, err
	}
	evidenceShape := newOpenCodeEvidenceRequestShape(spec, version)
	if toolCount, digest, schemaErr := fetchOpenCodeNativeToolSchemaDigest(ctx, client, baseURL, spec); schemaErr == nil {
		evidenceShape.ToolSchemaAvailable = true
		evidenceShape.ToolCount = toolCount
		evidenceShape.ToolSchemaSHA256 = digest
	}
	result.Telemetry.RequestShape = evidenceShape
	sessionID, err := createOpenCodeSession(ctx, client, baseURL, spec.WorkDir)
	if err != nil {
		return result, err
	}
	return runOpenCodePhases(ctx, client, baseURL, sessionID, spec, version, evidenceShape)
}

func stopOpenCodeProcess(terminate func(), done <-chan error) {
	if terminate != nil {
		terminate()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func runOpenCodePhases(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec, version string, evidenceShape agentanalysis.WorkspaceOpenCodeRequestShape) (result OpenCodeRunResult, retErr error) {
	credential, err := modelprovider.NewCredentialGuard(spec.Provider, os.LookupEnv)
	if err != nil {
		return result, err
	}
	result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
	result.Telemetry = agentanalysis.WorkspaceOpenCodeTelemetry{Status: agentanalysis.WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true, RequestShape: evidenceShape}
	evidenceErr := promptOpenCodeEvidence(ctx, client, baseURL, sessionID, spec)
	evidenceRaw, evidenceRawErr := fetchOpenCodeTelemetryRaw(ctx, client, baseURL, sessionID, spec.WorkDir)
	if evidenceRawErr == nil {
		if err := credential.CheckBytes(evidenceRaw); err != nil {
			result.Telemetry.FailureCode = "credential_exposure"
			return result, err
		}
	}
	evidenceUsage, evidenceTelemetry, evidenceFacts, evidenceTelemetryErr := parseOpenCodeTelemetryForWorkspace(evidenceRaw, spec.WorkDir)
	if evidenceRawErr != nil {
		evidenceTelemetryErr = evidenceRawErr
	}
	if evidenceTelemetryErr == nil {
		result.Usage, result.Telemetry = evidenceUsage, evidenceTelemetry
		result.Telemetry.RequestShape = evidenceShape
	} else {
		result.Usage.Status = telemetryStatusForError(evidenceTelemetryErr)
		result.Telemetry.Status = result.Usage.Status
		result.Telemetry.RequestShape = evidenceShape
	}
	applyOpenCodePromptError(&result, evidenceErr, 0, true, false)
	if evidenceErr != nil {
		return result, evidenceErr
	}
	if err := validateOpenCodeEvidencePhase(evidenceFacts, evidenceTelemetryErr); err != nil {
		result.Telemetry.FailureCode = "evidence_unavailable"
		return result, err
	}
	result.Telemetry.EvidencePhaseCompleted = true
	result.Telemetry.EvidencePhaseSteps = evidenceTelemetry.StepsUsed
	result.Telemetry.EvidencePhaseRequests = evidenceTelemetry.ProviderRequests
	result.Telemetry.ArtifactEvidenceToolCalls = evidenceFacts.ArtifactToolCalls
	result.Telemetry.SourceEvidenceToolCalls = evidenceFacts.SourceToolCalls

	finalShape := newOpenCodeRequestShape(spec, version)
	structured, finalMessage, finalErr := promptOpenCodeFinalizationWithMessage(ctx, client, baseURL, sessionID, spec)
	if err := credential.CheckBytes(structured, finalMessage); err != nil {
		result.Telemetry.FailureCode = "credential_exposure"
		return result, err
	}
	result.Structured = structured
	finalRaw, finalRawErr := fetchOpenCodeTelemetryRaw(ctx, client, baseURL, sessionID, spec.WorkDir)
	if finalRawErr == nil {
		if err := credential.CheckBytes(finalRaw); err != nil {
			result.Telemetry.FailureCode = "credential_exposure"
			return result, err
		}
	} else if combined, combineErr := appendOpenCodeTelemetryMessage(evidenceRaw, finalMessage); combineErr == nil {
		finalRaw = combined
		finalRawErr = nil
	}
	if finalRawErr != nil {
		result.Telemetry.RequestShape = finalShape
		applyOpenCodePromptError(&result, finalErr, evidenceTelemetry.ProviderRequests, evidenceTelemetry.ProviderRequestsKnown, true)
		result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
		if finalErr != nil {
			return result, finalErr
		}
		result.Telemetry.Error = agentanalysis.WorkspaceOpenCodeErrorTelemetry{}
		result.Telemetry.ProviderRequests = max(result.Telemetry.ProviderRequests, evidenceTelemetry.ProviderRequests+1)
		result.Telemetry.ProviderRequestsKnown = evidenceTelemetry.ProviderRequestsKnown
		result.Telemetry.FailureCode = "telemetry_unavailable"
		return result, fmt.Errorf("OpenCode finalization telemetry unavailable: %w", finalRawErr)
	}
	usage, telemetry, facts, telemetryErr := parseOpenCodeTelemetryForWorkspace(finalRaw, spec.WorkDir)
	if telemetryErr != nil {
		result.Telemetry.RequestShape = finalShape
		applyOpenCodePromptError(&result, finalErr, evidenceTelemetry.ProviderRequests, evidenceTelemetry.ProviderRequestsKnown, true)
		result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
		if finalErr != nil {
			return result, finalErr
		}
		result.Telemetry.Error = agentanalysis.WorkspaceOpenCodeErrorTelemetry{}
		result.Telemetry.ProviderRequests = max(result.Telemetry.ProviderRequests, evidenceTelemetry.ProviderRequests+1)
		result.Telemetry.ProviderRequestsKnown = evidenceTelemetry.ProviderRequestsKnown
		result.Telemetry.FailureCode = "telemetry_unavailable"
		return result, fmt.Errorf("OpenCode finalization telemetry unavailable: %w", telemetryErr)
	}
	result.Usage, result.Telemetry = usage, telemetry
	result.Telemetry.RequestShape = finalShape
	result.Telemetry.EvidencePhaseCompleted = true
	result.Telemetry.EvidencePhaseSteps = evidenceTelemetry.StepsUsed
	result.Telemetry.EvidencePhaseRequests = evidenceTelemetry.ProviderRequests
	result.Telemetry.ArtifactEvidenceToolCalls = evidenceFacts.ArtifactToolCalls
	result.Telemetry.SourceEvidenceToolCalls = evidenceFacts.SourceToolCalls
	result.Telemetry.FinalizationPhaseSteps = max(telemetry.StepsUsed-evidenceTelemetry.StepsUsed, 0)
	result.Telemetry.FinalizationPhaseRequests = max(telemetry.ProviderRequests-evidenceTelemetry.ProviderRequests, 0)
	result.Telemetry.StructuredOutputToolCalls = max(facts.StructuredOutputCalls-evidenceFacts.StructuredOutputCalls, 0)
	result.Telemetry.FinalizationPhaseCompleted = result.Telemetry.FinalizationPhaseSteps > 0 && result.Telemetry.FinalizationPhaseRequests > 0
	finalizationNativeTools := max(facts.NonStructuredToolCalls-evidenceFacts.NonStructuredToolCalls, 0)
	replaceEvidenceError := finalErr != nil && sameOpenCodeErrorIdentity(telemetry.Error, evidenceTelemetry.Error)
	applyOpenCodePromptError(&result, finalErr, evidenceTelemetry.ProviderRequests, evidenceTelemetry.ProviderRequestsKnown, replaceEvidenceError)
	if replaceEvidenceError {
		result.Usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
	}
	if finalErr != nil {
		return result, finalErr
	}
	if err := validateOpenCodeFinalizationPhase(result.Telemetry.StructuredOutputToolCalls, finalizationNativeTools); err != nil {
		result.Telemetry.FailureCode = "structured_output_unavailable"
		return result, err
	}
	if result.Telemetry.StepsUsed > spec.MaxSteps {
		result.Telemetry.FailureCode = "step_limit"
		return result, fmt.Errorf("OpenCode analysis exceeded the step limit")
	}
	return result, nil
}

func validateOpenCodeEvidencePhase(facts openCodeEvidenceFacts, telemetryErr error) error {
	if telemetryErr != nil {
		return fmt.Errorf("OpenCode evidence telemetry unavailable: %w", telemetryErr)
	}
	if facts.ArtifactToolCalls < 1 || facts.StructuredOutputCalls != 0 {
		return fmt.Errorf("OpenCode evidence unavailable")
	}
	return nil
}

func validateOpenCodeFinalizationPhase(structuredOutputCalls, nativeToolCalls int) error {
	if structuredOutputCalls != 1 || nativeToolCalls != 0 {
		return fmt.Errorf("OpenCode finalization tool sequence is invalid")
	}
	return nil
}

func sameOpenCodeErrorIdentity(left, right agentanalysis.WorkspaceOpenCodeErrorTelemetry) bool {
	left.BeforeProviderRequest = nil
	left.BeforeFirstTool = nil
	left.DuringStreamProcessing = nil
	left.DuringToolExecution = nil
	left.DuringSessionPersistence = nil
	right.BeforeProviderRequest = nil
	right.BeforeFirstTool = nil
	right.DuringStreamProcessing = nil
	right.DuringToolExecution = nil
	right.DuringSessionPersistence = nil
	return left == right
}

func applyOpenCodePromptError(result *OpenCodeRunResult, err error, priorProviderRequests int, priorProviderRequestsKnown, replaceExistingError bool) {
	if err == nil {
		return
	}
	var sessionErr *openCodePromptError
	if replaceExistingError && !errors.As(err, &sessionErr) {
		result.Telemetry.Error = agentanalysis.WorkspaceOpenCodeErrorTelemetry{}
		result.Telemetry.ProviderRequests = max(result.Telemetry.ProviderRequests, priorProviderRequests)
		result.Telemetry.ProviderRequestsKnown = false
		result.Telemetry.FailureCode = openCodeFailureCode(err)
		return
	}
	if errors.As(err, &sessionErr) {
		if replaceExistingError || !result.Telemetry.Error.Available {
			result.Telemetry.Error = sessionErr.telemetry
			providerProven := sessionErr.telemetry.Name != "UnknownError" || unknownErrorProvesResponseStream(sessionErr.telemetry.Classification)
			if providerProven {
				result.Telemetry.ProviderRequests = max(result.Telemetry.ProviderRequests, priorProviderRequests+1)
				result.Telemetry.ProviderRequestsKnown = priorProviderRequestsKnown
			} else {
				result.Telemetry.ProviderRequests = max(result.Telemetry.ProviderRequests, priorProviderRequests)
				result.Telemetry.ProviderRequestsKnown = false
			}
			if result.Telemetry.ProviderRequests > 0 {
				result.Telemetry.Error.BeforeProviderRequest = openCodeTelemetryBool(false)
				if len(result.Telemetry.Tools) > 0 {
					result.Telemetry.Error.BeforeFirstTool = openCodeTelemetryBool(false)
				} else {
					result.Telemetry.Error.BeforeFirstTool = openCodeTelemetryBool(true)
				}
			}
		}
		result.Telemetry.ContextLimit = result.Telemetry.ContextLimit || result.Telemetry.Error.ContextOverflow
	}
	if result.Telemetry.Error.Available {
		result.Telemetry.FailureCode = result.Telemetry.Error.Classification
	} else {
		result.Telemetry.FailureCode = openCodeFailureCode(err)
	}
}

const maxOpenCodeAPIResponseBytes = 1 << 20

func waitForOpenCode(ctx context.Context, client *http.Client, baseURL string, done <-chan error) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		var health struct {
			Healthy bool   `json:"healthy"`
			Version string `json:"version"`
		}
		err := openCodeJSON(requestCtx, client, http.MethodGet, baseURL+"/global/health", nil, &health)
		cancel()
		if err == nil && health.Healthy && strings.TrimSpace(health.Version) != "" && len(health.Version) <= 64 {
			return health.Version, nil
		}
		select {
		case err := <-done:
			return "", fmt.Errorf("OpenCode server exited before readiness: %v", err)
		case <-ctx.Done():
			return "", ctx.Err()
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

func promptOpenCodeEvidence(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) error {
	response, err := promptOpenCodeMessage(ctx, client, baseURL, sessionID, spec, openCodeEvidenceAgent, spec.Prompt, false)
	if err != nil {
		return err
	}
	if response.Info.Role != "assistant" {
		return fmt.Errorf("OpenCode evidence phase did not return an assistant message")
	}
	return nil
}

func promptOpenCode(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) ([]byte, error) {
	return promptOpenCodeFinalization(ctx, client, baseURL, sessionID, spec)
}

func promptOpenCodeFinalization(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) ([]byte, error) {
	structured, _, err := promptOpenCodeFinalizationWithMessage(ctx, client, baseURL, sessionID, spec)
	return structured, err
}

func promptOpenCodeFinalizationWithMessage(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) ([]byte, []byte, error) {
	response, err := promptOpenCodeMessage(ctx, client, baseURL, sessionID, spec, openCodeFinalizationAgent, agentanalysis.WorkspaceFinalizationInstruction(), true)
	message, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return nil, nil, fmt.Errorf("encode OpenCode finalization response")
	}
	if err != nil {
		return nil, message, err
	}
	if response.Info.Role != "assistant" || len(response.Info.Structured) == 0 || bytes.Equal(bytes.TrimSpace(response.Info.Structured), []byte("null")) {
		return nil, message, fmt.Errorf("OpenCode did not return structured output")
	}
	return slices.Clone(response.Info.Structured), message, nil
}

type openCodePromptResponse struct {
	Info struct {
		Role       string                 `json:"role"`
		Structured json.RawMessage        `json:"structured"`
		Error      *openCodeErrorEnvelope `json:"error"`
	} `json:"info"`
	Parts json.RawMessage `json:"parts"`
}

func promptOpenCodeMessage(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec, agent, prompt string, structured bool) (openCodePromptResponse, error) {
	payload := map[string]any{
		"agent": agent,
		"model": map[string]any{"providerID": "engine", "modelID": spec.Provider.Model},
		"parts": []any{map[string]any{"type": "text", "text": prompt}},
	}
	if structured {
		payload["format"] = map[string]any{"type": "json_schema", "schema": agentanalysis.WorkspaceResultSchema()}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return openCodePromptResponse{}, err
	}
	var response openCodePromptResponse
	endpoint := baseURL + "/session/" + url.PathEscape(sessionID) + "/message?directory=" + url.QueryEscape(spec.WorkDir)
	if err := openCodeJSON(ctx, client, http.MethodPost, endpoint, body, &response); err != nil {
		return response, fmt.Errorf("prompt OpenCode session: %w", err)
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return response, fmt.Errorf("encode OpenCode response")
	}
	credential, err := modelprovider.NewCredentialGuard(spec.Provider, os.LookupEnv)
	if err != nil {
		return response, err
	}
	if err := credential.CheckBytes(responseJSON); err != nil {
		return response, err
	}
	if response.Info.Error != nil {
		telemetry, sanitizeErr := sanitizeOpenCodeError(response.Info.Error)
		if sanitizeErr != nil {
			if recognizedOpenCodeError(response.Info.Error.Name) {
				telemetry = agentanalysis.WorkspaceOpenCodeErrorTelemetry{Available: true, Name: response.Info.Error.Name, Classification: "malformed_error"}
				return response, &openCodePromptError{name: response.Info.Error.Name, telemetry: telemetry}
			}
			return response, fmt.Errorf("OpenCode structured output failed: malformed error data")
		}
		return response, &openCodePromptError{name: response.Info.Error.Name, telemetry: telemetry}
	}
	return response, nil
}

func fetchOpenCodeTelemetryRaw(ctx context.Context, client *http.Client, baseURL, sessionID, workDir string) ([]byte, error) {
	endpoint := baseURL + "/session/" + url.PathEscape(sessionID) + "/message?directory=" + url.QueryEscape(workDir)
	return openCodeResponse(ctx, client, http.MethodGet, endpoint, nil, maxOpenCodeTelemetryBytes)
}

func appendOpenCodeTelemetryMessage(messagesRaw, messageRaw []byte) ([]byte, error) {
	if len(messagesRaw) == 0 || len(messageRaw) == 0 {
		return nil, fmt.Errorf("OpenCode phase telemetry is missing")
	}
	var messages []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(messagesRaw))
	if err := decoder.Decode(&messages); err != nil {
		return nil, fmt.Errorf("decode OpenCode evidence telemetry")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("OpenCode evidence telemetry has trailing data")
	}
	var message json.RawMessage
	decoder = json.NewDecoder(bytes.NewReader(messageRaw))
	if err := decoder.Decode(&message); err != nil {
		return nil, fmt.Errorf("decode OpenCode finalization telemetry")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("OpenCode finalization telemetry has trailing data")
	}
	messages = append(messages, slices.Clone(message))
	data, err := json.Marshal(messages)
	if err != nil || len(data) > maxOpenCodeTelemetryBytes {
		return nil, fmt.Errorf("combined OpenCode telemetry exceeds the bound")
	}
	return data, nil
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
	var sessionErr *openCodePromptError
	if errors.As(err, &sessionErr) {
		if sessionErr.telemetry.ContextOverflow {
			return "context_limit"
		}
		return sessionErr.telemetry.Classification
	}
	value := strings.ToLower(err.Error())
	switch {
	case strings.Contains(value, "context"):
		return "context_limit"
	case strings.Contains(value, "malformed error data"):
		return "malformed_error"
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

func verifyReadSafeWorkspace(ctx context.Context, sourceRoot string, files []agentanalysis.WorkspaceFile) error {
	for _, file := range files {
		if openCodeInstructionPath(file.Path) {
			return fmt.Errorf("artifact workspace contains an OpenCode instruction file")
		}
	}
	command := exec.CommandContext(ctx, "git", "-C", sourceRoot, "ls-files", "-z")
	command.Env = append(nonCredentialSubprocessEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("inspect source instruction files: %w", err)
	}
	stderr := newBoundedCapture(4096)
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("inspect source instruction files: %w", err)
	}
	const sourceInstructionPathLimit = 8 << 20
	output, readErr := io.ReadAll(io.LimitReader(stdout, sourceInstructionPathLimit+1))
	if len(output) > sourceInstructionPathLimit {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("source instruction path list exceeds the bound")
	}
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil {
		return fmt.Errorf("inspect source instruction files: %v", errors.Join(readErr, waitErr))
	}
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) > 0 && openCodeInstructionPath(filepath.ToSlash(string(record))) {
			return fmt.Errorf("source workspace contains an OpenCode instruction file")
		}
	}
	return nil
}

func openCodeInstructionPath(value string) bool {
	clean := strings.TrimPrefix(strings.ToLower(filepath.ToSlash(filepath.Clean(value))), "./")
	base := path.Base(clean)
	switch base {
	case "agents.md", "claude.md", "context.md":
		return true
	default:
		return false
	}
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

func writeOpenCodeConfig(home string, provider modelprovider.Config, maxSteps, contextTokens, outputTokens int) error {
	if maxSteps <= openCodeFinalizationSteps {
		return fmt.Errorf("OpenCode analysis requires at least three steps")
	}
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
	evidencePermissions := map[string]any{
		"*": "deny",
		"read": map[string]any{
			"*": "deny", "artifacts/*": "allow", "source/*": "allow", "*/artifacts/*": "allow", "*/source/*": "allow",
		},
		"glob": "allow", "grep": "allow",
		"bash": map[string]any{
			"*":                             "deny",
			"git diff --no-ext-diff --stat": "allow",
			"git log -1 --oneline":          "allow",
			"git status --short":            "allow",
		},
		"StructuredOutput": "deny",
		"edit":             "deny", "write": "deny", "apply_patch": "deny",
		"webfetch": "deny", "websearch": "deny", "task": "deny", "skill": "deny", "external_directory": "deny",
	}
	finalizationPermissions := map[string]any{
		"*": "deny", "StructuredOutput": "allow",
		"read": "deny", "glob": "deny", "grep": "deny", "bash": "deny",
		"edit": "deny", "write": "deny", "apply_patch": "deny",
		"webfetch": "deny", "websearch": "deny", "task": "deny", "skill": "deny", "external_directory": "deny",
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "share": "disabled", "autoupdate": false, "snapshot": false,
		"default_agent": openCodeEvidenceAgent,
		"provider": map[string]any{"engine": map[string]any{
			"npm": adapter.NPM, "name": "engine",
			"options": providerOptions,
			"models":  map[string]any{provider.Model: map[string]any{"limit": map[string]any{"context": contextTokens, "output": outputTokens}}},
		}},
		"agent": map[string]any{
			openCodeEvidenceAgent: map[string]any{
				"mode": "primary", "steps": maxSteps - openCodeFinalizationSteps, "prompt": agentanalysis.WorkspaceAgentPrompt(), "permission": evidencePermissions,
			},
			openCodeFinalizationAgent: map[string]any{
				"mode": "primary", "steps": openCodeFinalizationSteps, "prompt": agentanalysis.WorkspaceFinalizerPrompt(), "permission": finalizationPermissions,
			},
		},
		"permission": map[string]any{"*": "deny"},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "opencode.json"), data, 0o600)
}

func nonCredentialSubprocessEnvironment() []string {
	env := []string{}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func openCodeEnvironment(home, temp string, provider modelprovider.Config) ([]string, error) {
	credential, err := modelprovider.NewCredentialGuard(provider, os.LookupEnv)
	if err != nil {
		return nil, err
	}
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
	return append(env, credential.Environment()...), nil
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
