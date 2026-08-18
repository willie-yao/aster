package agentanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// WorkspaceAttemptIdentity seals one scheduled comparison before input preparation.
func WorkspaceAttemptIdentity(subject Subject, requestHash, authoritativeHash, skillSetHash, effectivePromptHash string, source sourceinvestigation.Repository, runtimeIdentity, artifactBaseURL string, provider modelprovider.Config, timeout time.Duration, maxSteps, contextTokens, outputTokens int, outputLimit int64, requireSourceEvidence bool) string {
	providerData, _ := json.Marshal(provider)
	return hashString(strings.Join([]string{
		WorkspaceContractVersion, WorkspaceSkillHash(), requestHash, authoritativeHash, skillSetHash, effectivePromptHash,
		source.Owner, source.Name, source.Revision, subject.JobID, subject.BuildID, subject.TestName, subject.TestSource, subject.JUnitFile, subject.SuiteName, subject.ClassName,
		strings.TrimSpace(runtimeIdentity), strings.TrimSpace(artifactBaseURL), string(providerData), timeout.String(), fmt.Sprintf("%d", maxSteps), fmt.Sprintf("%d", contextTokens), fmt.Sprintf("%d", outputTokens), fmt.Sprintf("%d", outputLimit), fmt.Sprintf("%t", requireSourceEvidence),
	}, "\x00"))
}

// WorkspaceComparisonIdentity binds one attempted comparison to its exact workspace request.
func WorkspaceComparisonIdentity(attemptHash string, manifest WorkspaceManifest, request WorkspaceExecutionRequest, stage WorkspaceStageRequest, publisherHash string) string {
	return hashString(strings.Join([]string{attemptHash, manifest.Hash, request.Hash, stage.Hash, strings.TrimSpace(publisherHash), manifest.EffectivePromptSHA256, manifest.SkillSetHash, manifest.Source.Revision}, "\x00"))
}

// WorkspaceEvidenceManifest returns content-free artifact and skill-plan identities.
func WorkspaceEvidenceManifest(manifest WorkspaceManifest) (ArtifactScan, []EvidenceManifestEntry, []string) {
	data, _ := json.Marshal(manifest.Artifacts)
	scan := ArtifactScan{PathCount: len(manifest.Artifacts), Digest: hashString(string(data))}
	evidence := make([]EvidenceManifestEntry, 0, len(manifest.Artifacts))
	for _, file := range manifest.Artifacts {
		evidence = append(evidence, EvidenceManifestEntry{
			ID: "artifact-" + hashString(file.Path + "\x00" + file.SHA256)[:16], Path: file.Path, Kind: "file", ContentSHA256: file.SHA256,
		})
	}
	planIDs := make([]string, 0, len(manifest.SkillPlan))
	for _, planned := range manifest.SkillPlan {
		planIDs = append(planIDs, planned.ID)
	}
	return scan, evidence, planIDs
}

// ProvenanceFromWorkspaceResult converts one file-backed analysis into ledger telemetry.
func ProvenanceFromWorkspaceResult(result WorkspaceSandboxResult, request WorkspaceExecutionRequest, stage WorkspaceStageRequest, runtimeIdentity string) Provenance {
	usage := result.Execution.Usage
	telemetry := result.Telemetry
	openCode := result.Execution.OpenCodeTelemetry
	identityAvailable := openCode.RequestShape.Available && strings.TrimSpace(openCode.RequestShape.ModelID) != ""
	identityStatus := "model_identity_unavailable"
	if identityAvailable {
		identityStatus = "available"
	}
	return Provenance{
		Runtime: "agent-sandbox-opencode", AgentNamespace: result.Resources.Namespace, AgentRef: result.Resources.Name,
		ContractVersion: WorkspaceContractVersion, ToolPolicyVersion: WorkspacePromptVersion,
		EvidenceHash: request.Manifest.Hash, SkillHash: request.Manifest.SkillSetHash, SourceSHA: request.Manifest.Source.Revision,
		IdentityHash: runtimeIdentity, ExecutionID: result.Resources.Name, Timeout: (time.Duration(request.TimeoutSeconds) * time.Second).String(),
		Attempts: usage.ModelRequests, RuntimeDurationMs: result.Execution.DurationMs,
		TaskFinalized: telemetry.TaskFinalized, TaskFinalizedMs: telemetry.TaskFinalizedMs,
		ResultAvailable: telemetry.ResultAvailable, ResultAvailableMs: telemetry.ResultAvailableMs,
		FinalizationChecked: telemetry.FinalizationChecked, FinalizationValid: telemetry.FinalizationValid,
		CleanupCompleted: telemetry.CleanupCompleted, CleanupDurationMs: telemetry.CleanupDurationMs,
		TokenUsageAvailable: usage.Available, CostAvailable: usage.CostAvailable, UsageStatus: usage.Status,
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens, CostUSD: usage.CostUSD,
		ModelIdentityAvailable: identityAvailable, ProviderIdentityAvailable: true, IdentityStatus: identityStatus,
		ManifestHash: request.Manifest.Hash, StageHash: stage.Hash, EffectivePromptSHA256: request.Manifest.EffectivePromptSHA256,
		SkillSetHash: request.Manifest.SkillSetHash, WorkspacePromptHash: request.PromptHash, InputMode: request.InputMode,
		MaxSteps: request.MaxSteps, ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens,
		ProviderRequests: openCode.ProviderRequests, ProviderRequestsKnown: openCode.ProviderRequestsKnown,
		EvidenceStepBudget: openCode.EvidenceStepBudget, EvidenceExhausted: openCode.EvidenceExhausted,
		EvidenceExhaustedSteps: openCode.EvidenceExhaustedSteps, EvidenceExhaustedRequests: openCode.EvidenceExhaustedRequests,
		EvidenceExhaustionClass: openCode.EvidenceExhaustionClass,
		EvidenceReadCalls:       openCode.EvidenceReadCalls, DuplicateReadCalls: openCode.DuplicateReadCalls,
		SchedulingAvailable: telemetry.SchedulingAvailable, SchedulingMs: telemetry.SchedulingMs,
		StagingAvailable: telemetry.StagingAvailable, StagingMs: telemetry.StagingMs,
		ExecutionAvailable: telemetry.ExecutionAvailable, ExecutionMs: telemetry.ExecutionMs,
		ResultPublicationAvailable: telemetry.PublicationAvailable, ResultPublicationMs: telemetry.PublicationMs,
		PhaseTimingStatus: telemetry.PhaseTimingStatus, ProviderCredentialMode: telemetry.ProviderCredentialMode,
		ProviderAPI: telemetry.ProviderAPI, ProviderReasoningEffort: telemetry.ProviderReasoningEffort,
		TerminalState: string(result.Execution.TerminalState), OpenCodeFailureCode: openCode.FailureCode,
		OpenCodeErrorClassification: openCode.Error.Classification, ResultValidationStatus: result.Execution.ResultValidation.Status,
		ResultValidationCodes: slices.Clone(result.Execution.ResultValidation.Codes),
	}
}

// ResolveWorkspaceShadowStatus maps one workspace lifecycle to the private ledger state.
func ResolveWorkspaceShadowStatus(result WorkspaceSandboxResult, err error) ShadowStatus {
	if result.CleanupWork != nil || errors.Is(err, engineruntime.ErrCleanupPending) {
		if result.Execution.Analysis != nil {
			return ShadowStatusCleanupPending
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || result.Execution.TerminalState == engineruntime.TerminalTimedOut {
		return ShadowStatusTimeout
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, engineruntime.ErrCancelled) || result.Execution.TerminalState == engineruntime.TerminalCancelled {
		return ShadowStatusCancellation
	}
	if result.Execution.Analysis != nil && result.Telemetry.FinalizationValid {
		if result.Telemetry.CleanupCompleted {
			return ShadowStatusSucceeded
		}
		return ShadowStatusCleanupPending
	}
	if result.Execution.ResultValidation.Status == WorkspaceResultRejected || errors.Is(err, engineruntime.ErrResultContract) {
		return ShadowStatusContractViolation
	}
	if result.Telemetry.ResultAvailable && !result.Telemetry.FinalizationValid || errors.Is(err, engineruntime.ErrMalformedResult) {
		return ShadowStatusMalformedResult
	}
	if result.Telemetry.TaskFinalized && !result.Telemetry.ResultAvailable {
		return ShadowStatusNoResult
	}
	return ShadowStatusRuntimeFailed
}

// EvaluateWorkspaceQuality applies the existing deterministic critique to grounded workspace output.
func EvaluateWorkspaceQuality(analysis WorkspaceAnalysis, skillSet *skills.Set, consecutiveFailures int) ShadowQuality {
	evidence := make([]ai.ExternalDraftEvidence, 0, len(analysis.EvidenceCitations))
	for _, citation := range analysis.EvidenceCitations {
		evidence = append(evidence, ai.ExternalDraftEvidence{Path: citation.Path, Content: citation.Quote})
	}
	sourcePaths := make([]string, 0, len(analysis.SourceCitations))
	for _, citation := range analysis.SourceCitations {
		if citation.Verified {
			sourcePaths = append(sourcePaths, citation.Path)
		}
	}
	critique := ai.EvaluateExternalDraftCritique(ai.ExternalDraftCritiqueInput{
		Summary: &models.AISummary{Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		Analysis: &models.AIAnalysis{
			RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
			RelevantFiles: append([]string(nil), analysis.RelevantFiles...), EvidenceCitations: append([]models.EvidenceCitation(nil), analysis.EvidenceCitations...),
		},
		Evidence: evidence, SourcePaths: sourcePaths, Skills: skillSet, ConsecutiveFailures: consecutiveFailures,
	})
	return ShadowQuality{
		DeterministicStatus: critique.Status, DeterministicPassed: critique.Passed,
		RuleIDs: critique.RuleIDs, HardRules: critique.HardRules, SoftRules: critique.SoftRules,
		SemanticStatus: "unavailable", SemanticReason: "evidence_aware_semantic_judge_not_exposed",
	}
}
