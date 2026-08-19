package agentanalysis

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestWorkspaceProvenanceRetainsBoundedLifecycleTelemetry(t *testing.T) {
	request := WorkspaceExecutionRequest{
		Manifest:   WorkspaceManifest{Hash: strings.Repeat("a", 64), SkillSetHash: strings.Repeat("b", 64), EffectivePromptSHA256: strings.Repeat("c", 64), Source: repositoryForTelemetryTest()},
		PromptHash: strings.Repeat("d", 64), InputMode: WorkspaceInputStaged, TimeoutSeconds: 600,
		MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192,
	}
	stage := WorkspaceStageRequest{Hash: strings.Repeat("e", 64)}
	result := WorkspaceSandboxResult{
		Resources: engineruntime.ResourceMetadata{Namespace: "analysis", Name: "sandbox"},
		Execution: WorkspaceExecutionResult{
			TerminalState: engineruntime.TerminalFailed, DurationMs: 9000,
			Usage:             WorkspaceUsage{Available: true, Status: WorkspaceTelemetryAvailable, ModelRequests: 3, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 40, ReasoningTokens: 10, CostAvailable: true, CostUSD: "0.001"},
			ResultValidation:  WorkspaceResultValidation{Status: WorkspaceResultRejected, Codes: []string{WorkspaceInvalidArtifactPath}},
			OpenCodeTelemetry: WorkspaceOpenCodeTelemetry{ProviderRequests: 4, ProviderRequestsKnown: true, FailureCode: "provider_error", Error: WorkspaceOpenCodeErrorTelemetry{Classification: "http_error"}, RequestShape: WorkspaceOpenCodeRequestShape{Available: true, ModelID: "model"}},
		},
		Telemetry: engineruntime.GenerateTelemetry{
			ProviderCredentialMode: "direct", ProviderAPI: "chat_completions", ProviderReasoningEffort: "high",
			TaskFinalized: true, TaskFinalizedMs: 8000, ResultAvailable: true, ResultAvailableMs: 8500,
			SchedulingAvailable: true, SchedulingMs: 100, StagingAvailable: true, StagingMs: 200,
			ExecutionAvailable: true, ExecutionMs: 7000, PublicationAvailable: true, PublicationMs: 300,
			PhaseTimingStatus: "available", FailurePhase: "execution", FailureCode: "executor_failed", ExecutorStarted: true,
			FinalizationChecked: true, CleanupCompleted: true, CleanupDurationMs: 400,
		},
	}
	got := ProvenanceFromWorkspaceResult(result, request, stage, strings.Repeat("f", 64))
	if got.InputTokens != 100 || got.CachedInputTokens != 20 || got.OutputTokens != 40 || got.ReasoningTokens != 10 || got.CostUSD != "0.001" ||
		got.SchedulingMs != 100 || got.StagingMs != 200 || got.ExecutionMs != 7000 || got.ResultPublicationMs != 300 || got.PhaseTimingStatus != "available" ||
		got.LifecycleFailurePhase != "execution" || got.LifecycleFailureCode != "executor_failed" || !got.ExecutorStarted ||
		got.TerminalState != string(engineruntime.TerminalFailed) || got.OpenCodeFailureCode != "provider_error" || got.OpenCodeErrorClassification != "http_error" ||
		got.ResultValidationStatus != WorkspaceResultRejected || len(got.ResultValidationCodes) != 1 || got.ResultValidationCodes[0] != WorkspaceInvalidArtifactPath {
		t.Fatalf("provenance=%+v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"prompt text", "provider body", "raw response"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("provenance contains %q", forbidden)
		}
	}
}

func repositoryForTelemetryTest() sourceinvestigation.Repository {
	return sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("1", 40)}
}

func TestResolveWorkspaceShadowStatusClassifiesStagingFailure(t *testing.T) {
	result := WorkspaceSandboxResult{Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, FailurePhase: "staging", FailureCode: "stager_exit_nonzero"}}
	if got := ResolveWorkspaceShadowStatus(result, errors.Join(engineruntime.ErrStaging, errors.New("diagnostic unavailable"))); got != ShadowStatusRuntimeFailed {
		t.Fatalf("status=%q", got)
	}
	if result.Execution.Analysis != nil {
		t.Fatal("staging failure populated analysis")
	}
}
