package causalcritic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func TestExecutionRequestBoundFitsSingleEnvironmentEntry(t *testing.T) {
	encoded := base64.StdEncoding.EncodedLen(maxExecutionRequest)
	if got := len(RequestEnv) + 1 + encoded + 1; got >= 128<<10 {
		t.Fatalf("environment entry bytes = %d, want below %d", got, 128<<10)
	}
}

func criticInput(t *testing.T) Input {
	t.Helper()
	bundle, err := agentanalysis.NewEvidenceBundle(
		ai.FailureAnalysisRequest{
			JobID: "periodic::job", BuildPrefix: "logs/job/1/",
			Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": strings.Repeat("a", 40)}},
			TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "deployment timed out"},
		},
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		agentanalysis.ArtifactScan{PathCount: 1}, nil,
		[]agentanalysis.EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "deployment timed out waiting for readiness\nGET widgets.example.io/v2 returned 404 unsupported while v1 was served\nlater the same controller reconciled and became Ready\n"}},
		strings.Repeat("b", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := agentanalysis.AuthoritativeSnapshot{
		Summary: "The deployment timed out.", RootCause: "The deployment readiness timeout caused the failure.",
		Severity: "High", SuggestedFix: "Investigate deployment readiness.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "deployment timed out waiting for readiness"}},
	}
	input, err := NewInput(bundle, authoritative)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestNewInputSealsDraftAndEvidence(t *testing.T) {
	input := criticInput(t)
	if err := ValidateInput(input); err != nil {
		t.Fatal(err)
	}
	if input.PairHash == input.DraftHash || input.EvidenceHash != input.Bundle.Hash || len(input.HighSpecificityErrors) == 0 || len(input.SuccessCounterevidence) == 0 || len(input.CitedEvidence) != 1 {
		t.Fatalf("input = %+v", input)
	}
	changed := input
	changed.Draft.RootCause = "different cause"
	if err := ValidateInput(changed); err == nil {
		t.Fatal("tampered draft was accepted")
	}
}

func TestValidateReview(t *testing.T) {
	input := criticInput(t)
	reference := input.HighSpecificityErrors[0].Reference
	review := Review{
		SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash,
		Verdict: "object", Confidence: "high",
		Findings:               []Finding{{Class: FindingSpecificErrorIgnored, Detail: "The draft ignores the more specific API error.", References: []EvidenceReference{reference}}},
		AlternativeExplanation: "The unsupported API version is an earlier supported event.", RevisionGuidance: "Explain the version mismatch before the timeout.",
	}
	if err := ValidateReview(review, input); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Review){
		"unknown class":    func(r *Review) { r.Findings[0].Class = "benchmark_specific" },
		"wrong pair":       func(r *Review) { r.PairHash = strings.Repeat("0", 64) },
		"bad reference":    func(r *Review) { r.Findings[0].References[0].LineStart = 99; r.Findings[0].References[0].LineEnd = 99 },
		"pass objects":     func(r *Review) { r.Verdict = "pass" },
		"oversized detail": func(r *Review) { r.Findings[0].Detail = strings.Repeat("x", maxFindingDetailBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := review
			candidate.Findings = append([]Finding(nil), review.Findings...)
			candidate.Findings[0].References = append([]EvidenceReference(nil), review.Findings[0].References...)
			mutate(&candidate)
			if err := ValidateReview(candidate, input); err == nil {
				t.Fatal("invalid review was accepted")
			}
		})
	}
}

type fakeSandboxRunner struct {
	run func(agentsandbox.Spec) (agentsandbox.Result, error)
}

func (f fakeSandboxRunner) Run(_ context.Context, spec agentsandbox.Spec) (agentsandbox.Result, error) {
	return f.run(spec)
}
func (fakeSandboxRunner) Cleanup(context.Context, engineruntime.WorkRef) error { return nil }
func (fakeSandboxRunner) RuntimeIdentity() string                              { return strings.Repeat("c", 64) }

func TestRuntimePreservesValidatedReviewWhenCleanupIsPending(t *testing.T) {
	input := criticInput(t)
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash,
			TerminalState: engineruntime.TerminalSucceeded, Review: &review,
			Usage: GatewayUsage{Status: "reported", Source: "gateway_response", Model: "critic-model", InputTokens: 10, OutputTokens: 2}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodSucceeded", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: false}}, engineruntime.ErrCleanupPending
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	result, err := runtime.Review(t.Context(), input, "critic-run", nil)
	if !errors.Is(err, engineruntime.ErrCleanupPending) || result.Execution.Review == nil || result.Execution.Review.Verdict != "pass" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRuntimePreservesCleanupIdentityOnContractFailure(t *testing.T) {
	input := criticInput(t)
	work := &engineruntime.WorkRef{Backend: agentsandbox.Backend, Namespace: "critic", Name: "critic-run", UID: "uid-1"}
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: strings.Repeat("0", 64),
			TerminalState: engineruntime.TerminalFailed, FailureCode: "invalid_pair", FailureReason: "invalid pair",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Work: work, Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: false}}, engineruntime.ErrCleanupPending
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	result, err := runtime.Review(t.Context(), input, "critic-run", nil)
	if !errors.Is(err, engineruntime.ErrResultContract) || !errors.Is(err, engineruntime.ErrCleanupPending) || result.CleanupWork == nil || result.CleanupWork.UID != work.UID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRuntimeRejectsEmptyResult(t *testing.T) {
	runtime := &Runtime{
		Sandbox: fakeSandboxRunner{run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
			return agentsandbox.Result{Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
		}},
		Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	if _, err := runtime.Review(t.Context(), criticInput(t), "critic-run", nil); !errors.Is(err, engineruntime.ErrMalformedResult) {
		t.Fatalf("err=%v", err)
	}
}

func TestExecutionRequestRequiresInternalGateway(t *testing.T) {
	input := criticInput(t)
	request := ExecutionRequest{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, Input: input,
		ModelGateway:   engineruntime.ModelGatewayConfig{Endpoint: "https://api.openai.com/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		TimeoutSeconds: 60, OutputLimit: DefaultOutputLimit,
	}
	if err := ValidateExecutionRequest(request); err == nil {
		t.Fatal("direct provider gateway was accepted")
	}
	request.ModelGateway.Endpoint = "https://gateway.platform.svc.cluster.local/v1"
	if err := ValidateExecutionRequest(request); err != nil {
		t.Fatal(err)
	}
	request.ModelGateway.Endpoint = "https://127.0.0.1/v1"
	if err := ValidateExecutionRequest(request); err == nil || !strings.Contains(err.Error(), "internal service DNS") {
		t.Fatalf("loopback gateway err=%v", err)
	}
}

func TestRuntimeIdentityIncludesImageAndGateway(t *testing.T) {
	gateway := engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.models.svc.cluster.local/v1", Model: "critic-a", ProtocolVersion: "openai-chat-completions-v1"}
	base := RuntimeIdentity(gateway, strings.Repeat("a", 64), time.Minute, DefaultOutputLimit)
	for _, changed := range []string{
		RuntimeIdentity(gateway, strings.Repeat("b", 64), time.Minute, DefaultOutputLimit),
		RuntimeIdentity(engineruntime.ModelGatewayConfig{Endpoint: gateway.Endpoint, Model: "critic-b", ProtocolVersion: gateway.ProtocolVersion}, strings.Repeat("a", 64), time.Minute, DefaultOutputLimit),
		RuntimeIdentity(gateway, strings.Repeat("a", 64), 2*time.Minute, DefaultOutputLimit),
	} {
		if changed == base {
			t.Fatal("runtime identity did not change")
		}
	}
}

func TestDecodeExecutionResultRejectsUnknownAndTrailingData(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"contract_version":"causal-critic-v1","pair_hash":"x","terminal_state":"failed","usage":{"status":"unavailable","source":"gateway_response"},"failure_reason":"x","unknown":true}`,
		`{"schema_version":1} {"schema_version":1}`,
		"not-json",
	} {
		if _, err := DecodeExecutionResult(raw); err == nil {
			t.Fatalf("invalid result accepted: %s", raw)
		}
	}
}

func TestRuntimeReturnsExecutorFailure(t *testing.T) {
	input := criticInput(t)
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash,
			TerminalState: engineruntime.TerminalFailed, FailureCode: "gateway_request", FailureReason: "model gateway request failed",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	result, err := runtime.Review(t.Context(), input, "critic-run", nil)
	if err == nil || result.Execution.FailureCode != "gateway_request" || result.Execution.FailureReason != "model gateway request failed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRuntimeNormalizesLegacyFailedResultCode(t *testing.T) {
	input := criticInput(t)
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash,
			TerminalState: engineruntime.TerminalFailed, FailureReason: "model gateway request failed",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	result, err := runtime.Review(t.Context(), input, "critic-legacy-failure", nil)
	if err == nil || result.Execution.FailureCode != "legacy_executor_failure" || !result.Telemetry.FinalizationValid {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRuntimePreservesValidationCodeOnInvalidFailureCode(t *testing.T) {
	input := criticInput(t)
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash,
			TerminalState: engineruntime.TerminalFailed, FailureCode: "INVALID-CODE", FailureReason: "model gateway request failed",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	result, err := runtime.Review(t.Context(), input, "critic-invalid-code", nil)
	if !errors.Is(err, engineruntime.ErrResultContract) || ValidationCodeOf(err) != ValidationResultTerminal || result.Execution.FailureCode != "" || result.Telemetry.FinalizationValid {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
