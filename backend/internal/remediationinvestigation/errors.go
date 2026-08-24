package remediationinvestigation

import (
	"context"
	"errors"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai"
)

// Phase identifies one structured remediation finalization cycle.
type Phase string

const (
	PhaseTargetExtractionInitial        Phase = "target_extraction_initial"
	PhaseTargetExtractionRepair         Phase = "target_extraction_repair"
	PhaseNonActionableAssessmentInitial Phase = "non_actionable_assessment_initial"
	PhaseNonActionableAssessmentRepair  Phase = "non_actionable_assessment_repair"
)

// FailureDetails contains only bounded private failure metadata.
type FailureDetails struct {
	Category           FailureCategory                `json:"category"`
	Phase              Phase                          `json:"phase,omitempty"`
	ValidationCode     string                         `json:"validation_code,omitempty"`
	StructuredAttempts []ai.StructuredAttemptMetadata `json:"structured_attempts,omitempty"`
}

// FinalStructuredAttempt returns the final structured attempt when available.
func (d FailureDetails) FinalStructuredAttempt() (ai.StructuredAttemptMetadata, bool) {
	if len(d.StructuredAttempts) == 0 {
		return ai.StructuredAttemptMetadata{}, false
	}
	return d.StructuredAttempts[len(d.StructuredAttempts)-1], true
}

// DiagnosticErrorCode returns one bounded phase-specific error code.
func (d FailureDetails) DiagnosticErrorCode() string {
	switch d.Category {
	case FailureTargetExtractionTransport:
		return "target_extraction_transport"
	case FailureTargetExtractionValidation:
		if d.ValidationCode != "" {
			return "target_extraction_" + d.ValidationCode
		}
		return "target_extraction_structured_validation"
	case FailureTargetExtractionAttempts:
		return "target_extraction_attempt_exhausted"
	case FailureNonActionableAssessment:
		if d.ValidationCode != "" {
			return "non_actionable_assessment_" + d.ValidationCode
		}
		return "non_actionable_assessment_failure"
	case FailureCancelled:
		return "cancelled"
	case FailureTimeout:
		return "timeout"
	default:
		return string(d.Category)
	}
}

type resultError struct {
	details FailureDetails
	err     error
}

func (e *resultError) Error() string {
	return "remediation investigation result rejected: " + e.details.DiagnosticErrorCode()
}
func (e *resultError) Unwrap() error { return e.err }

type codedValidationError struct {
	code string
	err  error
}

func (e *codedValidationError) Error() string                    { return "structured result rejected: " + e.code }
func (e *codedValidationError) Unwrap() error                    { return e.err }
func (e *codedValidationError) StructuredValidationCode() string { return e.code }

// ErrorCode returns the caller-owned bounded validation code.
func ErrorCode(err error) string {
	if details, ok := FailureDetailsOf(err); ok {
		return details.ValidationCode
	}
	return ""
}

// FailureDetailsOf extracts bounded private remediation failure metadata.
func FailureDetailsOf(err error) (FailureDetails, bool) {
	var resultErr *resultError
	if !errors.As(err, &resultErr) {
		return FailureDetails{}, false
	}
	details := resultErr.details
	details.StructuredAttempts = append([]ai.StructuredAttemptMetadata(nil), details.StructuredAttempts...)
	return details, true
}

func newResultError(phase Phase, validationCode string, metadata ai.StructuredCompletionMetadata, err error) *resultError {
	metadata = sanitizeStructuredCompletionMetadata(metadata)
	details := FailureDetails{
		Phase:              validPhase(phase),
		ValidationCode:     boundedValidationCode(validationCode),
		StructuredAttempts: append([]ai.StructuredAttemptMetadata(nil), metadata.Attempts...),
	}
	details.Category = structuredFailureCategory(details.Phase, metadata, err)
	return &resultError{details: details, err: safeResultErrorCause(err)}
}

func structuredFailureCategory(phase Phase, metadata ai.StructuredCompletionMetadata, err error) FailureCategory {
	switch {
	case errors.Is(err, context.Canceled):
		return FailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case phase == PhaseNonActionableAssessmentInitial || phase == PhaseNonActionableAssessmentRepair:
		return FailureNonActionableAssessment
	}
	final, ok := metadata.FinalAttempt()
	if !ok {
		return FailureTargetExtractionAttempts
	}
	switch final.Outcome {
	case ai.StructuredOutcomeProviderError:
		return FailureTargetExtractionTransport
	case ai.StructuredOutcomeInvalidJSON, ai.StructuredOutcomeValidatorRejected:
		return FailureTargetExtractionValidation
	default:
		return FailureTargetExtractionAttempts
	}
}

func validationErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, item := range []struct{ contains, code string }{
		{"duplicate field", "duplicate_field"},
		{"unknown field", "unknown_field"},
		{"decode remediation target extraction", "decode"},
		{"decode remediation non-actionable assessment", "decode"},
		{"decode remediation investigation result", "decode"},
		{"configuration field", "configuration_target"},
		{"candidate field", "candidate_missing_field"},
		{"target extraction field", "invalid_target_extraction"},
		{"non-actionable assessment field", "invalid_non_actionable_assessment"},
		{"version must be the integer", "invalid_version"},
		{"version", "invalid_version"},
		{"cause assessment", "cause_assessment"},
		{"typed non-actionable reason", "non_actionable_reason"},
		{"non_actionable_reason to null", "candidate_non_actionable_conflict"},
		{"must support or refine", "unsupported_cause"},
		{"candidate kind", "candidate_kind"},
		{"candidate target type", "candidate_kind"},
		{"required-call candidate", "required_call_target"},
		{"symbol-addition candidate", "symbol_target"},
		{"prow environment candidate", "prow_environment_target"},
		{"engine-issued source evidence ID", "missing_source_evidence"},
		{"was not issued by the investigation ledger", "unknown_evidence_id"},
		{"duplicate evidence ID", "duplicate_evidence_id"},
		{"evidence ID", "evidence_id"},
		{"evidence catalog", "evidence_catalog"},
		{"evidence", "evidence"},
	} {
		if strings.Contains(message, item.contains) {
			return item.code
		}
	}
	return "invalid_result"
}

var boundedValidationCodes = map[string]bool{
	"invalid_target_extraction":         true,
	"invalid_non_actionable_assessment": true,
	"decode":                            true,
	"unknown_field":                     true,
	"duplicate_field":                   true,
	"invalid_version":                   true,
	"cause_assessment":                  true,
	"non_actionable_reason":             true,
	"candidate_non_actionable_conflict": true,
	"unsupported_cause":                 true,
	"candidate_missing_field":           true,
	"candidate_kind":                    true,
	"required_call_target":              true,
	"symbol_target":                     true,
	"prow_environment_target":           true,
	"configuration_target":              true,
	"missing_source_evidence":           true,
	"unknown_evidence_id":               true,
	"duplicate_evidence_id":             true,
	"evidence_id":                       true,
	"evidence_catalog":                  true,
	"evidence":                          true,
	"invalid_result":                    true,
}

var boundedProviderCategories = map[string]bool{
	"context_canceled": true, "deadline_exceeded": true, "unsupported_api": true,
	"tools_unsupported": true, "request_marshal": true, "request_build": true,
	"request_transport": true, "response_decode": true, "provider_status": true,
	"http_error": true, "empty_response": true, "analysis_error": true,
}

func boundedValidationCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if boundedValidationCodes[code] {
		return code
	}
	return "invalid_result"
}

func validPhase(phase Phase) Phase {
	switch phase {
	case PhaseTargetExtractionInitial, PhaseTargetExtractionRepair, PhaseNonActionableAssessmentInitial, PhaseNonActionableAssessmentRepair:
		return phase
	default:
		return ""
	}
}

func sanitizeStructuredCompletionMetadata(metadata ai.StructuredCompletionMetadata) ai.StructuredCompletionMetadata {
	if len(metadata.Attempts) > 12 {
		metadata.Attempts = metadata.Attempts[len(metadata.Attempts)-12:]
	}
	out := ai.StructuredCompletionMetadata{Attempts: make([]ai.StructuredAttemptMetadata, 0, len(metadata.Attempts))}
	for _, attempt := range metadata.Attempts {
		phase := validPhase(Phase(attempt.Phase))
		if phase == "" {
			continue
		}
		switch attempt.Path {
		case ai.StructuredAttemptResponseFormat, ai.StructuredAttemptForcedFunction, ai.StructuredAttemptPlainFallback:
		default:
			continue
		}
		switch attempt.Outcome {
		case ai.StructuredOutcomeAccepted, ai.StructuredOutcomeProviderError, ai.StructuredOutcomeEmptyResponse,
			ai.StructuredOutcomeMissingForcedFunction, ai.StructuredOutcomeInvalidJSON, ai.StructuredOutcomeValidatorRejected, ai.StructuredOutcomeNoCandidate:
		default:
			continue
		}
		attempt.Phase = string(phase)
		if attempt.ValidationCode != "" {
			attempt.ValidationCode = boundedValidationCode(attempt.ValidationCode)
		}
		attempt.ProviderCategory = strings.ToLower(strings.TrimSpace(attempt.ProviderCategory))
		if !boundedProviderCategories[attempt.ProviderCategory] {
			attempt.ProviderCategory = ""
		}
		if attempt.ProviderStatus < 100 || attempt.ProviderStatus > 599 {
			attempt.ProviderStatus = 0
		}
		if attempt.ProviderAttempts < 0 || attempt.ProviderAttempts > 64 {
			attempt.ProviderAttempts = 0
			attempt.ProviderAttemptsKnown = false
		}
		out.Attempts = append(out.Attempts, attempt)
	}
	return out
}

func safeResultErrorCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	if _, ok := ai.StructuredCompletionFailureMetadata(err); ok {
		return err
	}
	if err != nil {
		return errors.New("remediation structured finalization failed")
	}
	return nil
}

// categorizedError carries the bounded failure category a run recorded. Recording
// the category and returning the error are one step so the published reason and
// the recorded telemetry can never describe different failures.
type categorizedError struct {
	category FailureCategory
	err      error
}

func (e *categorizedError) Error() string { return string(e.category) + ": " + e.err.Error() }
func (e *categorizedError) Unwrap() error { return e.err }

// FailureCategoryOf returns the bounded category a failure was recorded under.
func FailureCategoryOf(err error) (FailureCategory, bool) {
	var categorized *categorizedError
	if errors.As(err, &categorized) {
		return categorized.category, true
	}
	if details, ok := FailureDetailsOf(err); ok {
		return details.Category, true
	}
	return "", false
}
