package causalcritic

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/models"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestValidationCodesRemainStable(t *testing.T) {
	input := criticInput(t)
	reference := input.HighSpecificityErrors[0].Reference
	objectReview := Review{
		SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash,
		Verdict: "object", Confidence: "high",
		Findings: []Finding{{
			Class: FindingSpecificErrorIgnored, Detail: "The draft ignores the earlier specific error.",
			References: []EvidenceReference{reference},
		}},
		AlternativeExplanation: "The specific error is the earlier supported event.",
		RevisionGuidance:       "Explain the specific error before the timeout.",
	}
	passReview := Review{
		SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash,
		Verdict: "pass", Findings: []Finding{}, Confidence: "medium",
	}
	request := ExecutionRequest{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, Input: input,
		ModelGateway: engineruntime.ModelGatewayConfig{
			Endpoint: "https://gateway.models.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1",
		},
		TimeoutSeconds: 60, OutputLimit: DefaultOutputLimit,
	}
	successResult := ExecutionResult{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash,
		TerminalState: engineruntime.TerminalSucceeded, Review: &passReview,
		Usage:      GatewayUsage{Status: "reported", Source: "gateway_response", Model: "critic-model", InputTokens: 10, OutputTokens: 2},
		DurationMs: 100,
	}

	tests := []struct {
		name     string
		code     ValidationCode
		sentinel error
		run      func() error
	}{
		{name: "input schema", code: ValidationInputSchema, sentinel: ErrInvalidInput, run: func() error {
			candidate := input
			candidate.SchemaVersion++
			return ValidateInput(candidate)
		}},
		{name: "input evidence", code: ValidationInputEvidence, sentinel: ErrInvalidInput, run: func() error {
			candidate := input
			candidate.Bundle.Hash = strings.Repeat("0", 64)
			return ValidateInput(candidate)
		}},
		{name: "input draft", code: ValidationInputDraft, sentinel: ErrInvalidInput, run: func() error {
			candidate := input
			candidate.Draft.Severity = "urgent"
			return ValidateInput(candidate)
		}},
		{name: "input citation", code: ValidationInputCitation, sentinel: ErrInvalidInput, run: func() error {
			candidate := input
			candidate.Draft.EvidenceCitations = append([]models.EvidenceCitation(nil), input.Draft.EvidenceCitations...)
			candidate.Draft.EvidenceCitations[0].Quote = "absent quote"
			return ValidateInput(candidate)
		}},
		{name: "input identity", code: ValidationInputIdentity, sentinel: ErrInvalidInput, run: func() error {
			candidate := input
			candidate.PairHash = strings.Repeat("0", 64)
			return ValidateInput(candidate)
		}},
		{name: "input size", code: ValidationInputSize, sentinel: ErrInvalidInput, run: func() error {
			return oversizedCriticInputError(t, input)
		}},
		{name: "review identity", code: ValidationReviewIdentity, sentinel: ErrInvalidReview, run: func() error {
			candidate := objectReview
			candidate.PairHash = strings.Repeat("0", 64)
			return ValidateReview(candidate, input)
		}},
		{name: "review verdict", code: ValidationReviewVerdict, sentinel: ErrInvalidReview, run: func() error {
			candidate := objectReview
			candidate.Verdict = "maybe"
			return ValidateReview(candidate, input)
		}},
		{name: "review findings", code: ValidationReviewFindings, sentinel: ErrInvalidReview, run: func() error {
			candidate := passReview
			candidate.Findings = nil
			return ValidateReview(candidate, input)
		}},
		{name: "review finding", code: ValidationReviewFinding, sentinel: ErrInvalidReview, run: func() error {
			candidate := cloneReview(objectReview)
			candidate.Findings[0].Class = "case_specific"
			return ValidateReview(candidate, input)
		}},
		{name: "review reference", code: ValidationReviewReference, sentinel: ErrInvalidReview, run: func() error {
			candidate := cloneReview(objectReview)
			candidate.Findings[0].References[0].LineStart = 99
			candidate.Findings[0].References[0].LineEnd = 99
			return ValidateReview(candidate, input)
		}},
		{name: "review duplicate", code: ValidationReviewDuplicate, sentinel: ErrInvalidReview, run: func() error {
			candidate := cloneReview(objectReview)
			candidate.Findings = append(candidate.Findings, candidate.Findings[0])
			return ValidateReview(candidate, input)
		}},
		{name: "review guidance", code: ValidationReviewGuidance, sentinel: ErrInvalidReview, run: func() error {
			candidate := passReview
			candidate.RevisionGuidance = "Revise the diagnosis."
			return ValidateReview(candidate, input)
		}},
		{name: "review confidence", code: ValidationReviewConfidence, sentinel: ErrInvalidReview, run: func() error {
			candidate := objectReview
			candidate.Confidence = "certain"
			return ValidateReview(candidate, input)
		}},
		{name: "execution contract", code: ValidationExecutionContract, sentinel: ErrInvalidInput, run: func() error {
			candidate := request
			candidate.SchemaVersion++
			return ValidateExecutionRequest(candidate)
		}},
		{name: "execution gateway", code: ValidationExecutionGateway, run: func() error {
			candidate := request
			candidate.ModelGateway.Endpoint = "https://api.openai.com/v1"
			return ValidateExecutionRequest(candidate)
		}},
		{name: "execution timeout", code: ValidationExecutionTimeout, sentinel: ErrInvalidInput, run: func() error {
			candidate := request
			candidate.TimeoutSeconds = 0
			return ValidateExecutionRequest(candidate)
		}},
		{name: "execution output", code: ValidationExecutionOutput, sentinel: ErrInvalidInput, run: func() error {
			candidate := request
			candidate.OutputLimit = 1
			return ValidateExecutionRequest(candidate)
		}},
		{name: "execution size", code: ValidationExecutionSize, sentinel: ErrInvalidInput, run: func() error {
			candidate := request
			candidate.ModelGateway.Endpoint = "https://gateway.models.svc.cluster.local/" + strings.Repeat("x", maxExecutionRequest)
			return ValidateExecutionRequest(candidate)
		}},
		{name: "result identity", code: ValidationResultIdentity, sentinel: ErrInvalidReview, run: func() error {
			candidate := successResult
			candidate.PairHash = strings.Repeat("0", 64)
			return ValidateExecutionResult(candidate, request)
		}},
		{name: "result duration", code: ValidationResultDuration, sentinel: ErrInvalidReview, run: func() error {
			candidate := successResult
			candidate.DurationMs = request.TimeoutSeconds*1000 + 5001
			return ValidateExecutionResult(candidate, request)
		}},
		{name: "result usage", code: ValidationResultUsage, run: func() error {
			candidate := successResult
			candidate.Usage.Source = "controller"
			return ValidateExecutionResult(candidate, request)
		}},
		{name: "result terminal", code: ValidationResultTerminal, sentinel: ErrInvalidReview, run: func() error {
			candidate := successResult
			candidate.TerminalState = engineruntime.TerminalFailed
			candidate.Review = nil
			candidate.FailureCode = "INVALID-CODE"
			candidate.FailureReason = "gateway failed"
			return ValidateExecutionResult(candidate, request)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if got := ValidationCodeOf(err); got != test.code {
				t.Fatalf("code=%q, want %q, err=%v", got, test.code, err)
			}
			if test.sentinel != nil && !errors.Is(err, test.sentinel) {
				t.Fatalf("err=%v does not preserve %v", err, test.sentinel)
			}
		})
	}
}

func oversizedCriticInputError(t *testing.T, input Input) error {
	t.Helper()
	bundle, err := agentanalysis.NewEvidenceBundle(
		input.Bundle.Request, input.Bundle.Source, input.Bundle.Scan, input.Bundle.Plan,
		[]agentanalysis.EvidenceExcerpt{
			{Path: "large/a.log", Kind: "tail", Content: strings.Repeat("a", 46<<10)},
			{Path: "large/b.log", Kind: "tail", Content: strings.Repeat("b", 46<<10)},
		},
		input.Bundle.SkillSetHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildInput(bundle, Draft{Summary: "summary", RootCause: "cause", Severity: "Low", SuggestedFix: "fix"})
	return err
}

func cloneReview(review Review) Review {
	cloned := review
	cloned.Findings = append([]Finding(nil), review.Findings...)
	for index := range cloned.Findings {
		cloned.Findings[index].References = append([]EvidenceReference(nil), review.Findings[index].References...)
	}
	return cloned
}

func TestSuccessfulExecutionResultRejectsFailureCode(t *testing.T) {
	input := criticInput(t)
	request := ExecutionRequest{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, Input: input,
		ModelGateway:   engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.models.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		TimeoutSeconds: int64(time.Minute / time.Second), OutputLimit: DefaultOutputLimit,
	}
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	result := ExecutionResult{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash,
		TerminalState: engineruntime.TerminalSucceeded, Review: &review,
		Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, FailureCode: "stale_failure",
	}
	if err := ValidateExecutionResult(result, request); ValidationCodeOf(err) != ValidationResultTerminal {
		t.Fatalf("err=%v", err)
	}
}
