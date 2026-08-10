package agentanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	WorkspaceExecutionRequestEnv = "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64"
	WorkspaceStageRequestEnv     = "PROW_AI_ANALYSIS_STAGE_REQUEST_B64"
)

// WorkspaceSandboxSpec describes one private staged-workspace analysis.
type WorkspaceSandboxSpec struct {
	Request      WorkspaceExecutionRequest
	StageRequest []byte
	SourceRoot   string
	ArtifactRoot string
	ExecutionID  string
	WorkObserver engineruntime.WorkObserver
}

// WorkspaceSandboxResult combines one validated analysis with lifecycle facts.
type WorkspaceSandboxResult struct {
	Execution   WorkspaceExecutionResult
	Resources   engineruntime.ResourceMetadata
	Telemetry   engineruntime.GenerateTelemetry
	CleanupWork *engineruntime.WorkRef
}

// WorkspaceSandboxRuntime runs file-backed analyses through Agent Sandbox.
type WorkspaceSandboxRuntime struct {
	Sandbox          agentsandbox.Runner
	Gateway          engineruntime.ModelGatewayConfig
	Timeout          time.Duration
	OutputLimitBytes int64
}

// RuntimeIdentity seals the analysis contract to the Sandbox workload configuration.
func (r *WorkspaceSandboxRuntime) RuntimeIdentity() string {
	if r == nil || r.Sandbox == nil {
		return ""
	}
	data, _ := json.Marshal(struct {
		ContractVersion string                           `json:"contract_version"`
		PromptHash      string                           `json:"prompt_hash"`
		Gateway         engineruntime.ModelGatewayConfig `json:"gateway"`
		Timeout         string                           `json:"timeout"`
		OutputLimit     int64                            `json:"output_limit_bytes"`
		SandboxIdentity string                           `json:"sandbox_identity"`
	}{WorkspaceContractVersion, WorkspaceSkillHash(), r.Gateway, r.Timeout.String(), r.OutputLimitBytes, r.Sandbox.RuntimeIdentity()})
	return hashString(string(data))
}

// Analyze runs one exact staged-workspace request without publication authority.
func (r *WorkspaceSandboxRuntime) Analyze(ctx context.Context, spec WorkspaceSandboxSpec) (result WorkspaceSandboxResult, retErr error) {
	if r == nil || r.Sandbox == nil {
		return result, fmt.Errorf("%w: workspace analysis runtime is unavailable", engineruntime.ErrUnavailable)
	}
	if err := ValidateWorkspaceExecutionRequest(spec.Request); err != nil {
		return result, err
	}
	if spec.Request.ModelGateway != r.Gateway || time.Duration(spec.Request.TimeoutSeconds)*time.Second != r.Timeout || spec.Request.OutputLimitBytes != r.OutputLimitBytes {
		return result, fmt.Errorf("workspace analysis request does not match configured gateway, timeout, or output limit")
	}
	if err := VerifySourceWorkspace(ctx, spec.SourceRoot, spec.Request.Manifest.Source.Revision); err != nil {
		return result, err
	}
	if err := VerifyArtifactWorkspace(spec.ArtifactRoot, spec.Request.Manifest); err != nil {
		return result, err
	}
	requestJSON, err := json.Marshal(spec.Request)
	if err != nil {
		return result, fmt.Errorf("encode workspace analysis request: %w", err)
	}
	sandboxSpec := agentsandbox.Spec{
		Purpose: "analysis", ExecutionID: spec.ExecutionID,
		RequestEnv: WorkspaceExecutionRequestEnv, Request: requestJSON,
		Timeout: r.Timeout, OutputLimitBytes: r.OutputLimitBytes, WorkObserver: spec.WorkObserver,
		StagedWorkspace: &agentsandbox.StagedWorkspace{RequestEnv: WorkspaceStageRequestEnv, Request: spec.StageRequest},
	}
	if err := agentsandbox.ValidateSpec(sandboxSpec); err != nil {
		return result, err
	}
	raw, runErr := r.Sandbox.Run(ctx, sandboxSpec)
	result.Resources = raw.Resources
	result.Telemetry = raw.Telemetry
	if raw.Work != nil && !raw.Telemetry.CleanupCompleted {
		work := *raw.Work
		result.CleanupWork = &work
	}
	if strings.TrimSpace(raw.Output) == "" {
		if runErr == nil {
			runErr = fmt.Errorf("%w: workspace analysis result is empty", engineruntime.ErrMalformedResult)
		}
		return result, runErr
	}
	result.Telemetry.FinalizationChecked = true
	parsed, err := DecodeWorkspaceExecutionResult(raw.Output)
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: workspace analysis result: %v", engineruntime.ErrMalformedResult, err), runErr)
	}
	parsed, err = ValidateWorkspaceExecutionResult(parsed, spec.Request, spec.ArtifactRoot, spec.SourceRoot)
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: workspace analysis result: %v", engineruntime.ErrResultContract, err), runErr)
	}
	result.Execution = parsed
	if raw.FinishedReason == "PodSucceeded" && parsed.TerminalState != engineruntime.TerminalSucceeded {
		return result, errors.Join(fmt.Errorf("%w: succeeded Pod reported %q", engineruntime.ErrResultContract, parsed.TerminalState), runErr)
	}
	if raw.FinishedReason == "PodFailed" && parsed.TerminalState == engineruntime.TerminalSucceeded {
		return result, errors.Join(fmt.Errorf("%w: failed Pod reported success", engineruntime.ErrResultContract), runErr)
	}
	result.Telemetry.FinalizationValid = true
	if parsed.TerminalState != engineruntime.TerminalSucceeded {
		switch parsed.TerminalState {
		case engineruntime.TerminalCancelled:
			return result, errors.Join(fmt.Errorf("%w: %s", engineruntime.ErrCancelled, parsed.FailureReason), runErr)
		case engineruntime.TerminalTimedOut:
			return result, errors.Join(fmt.Errorf("workspace analysis timed out: %s", parsed.FailureReason), runErr)
		default:
			return result, errors.Join(fmt.Errorf("workspace analysis failed: %s", parsed.FailureReason), runErr)
		}
	}
	return result, runErr
}
