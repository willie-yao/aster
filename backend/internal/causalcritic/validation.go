package causalcritic

import (
	"errors"
	"fmt"
)

// ValidationCode is a stable deterministic contract rejection category.
type ValidationCode string

const (
	ValidationInputSchema       ValidationCode = "input_schema"
	ValidationInputEvidence     ValidationCode = "input_evidence"
	ValidationInputDraft        ValidationCode = "input_draft"
	ValidationInputCitation     ValidationCode = "input_citation"
	ValidationInputIdentity     ValidationCode = "input_identity"
	ValidationInputSize         ValidationCode = "input_size"
	ValidationReviewIdentity    ValidationCode = "review_identity"
	ValidationReviewVerdict     ValidationCode = "review_verdict"
	ValidationReviewFindings    ValidationCode = "review_findings"
	ValidationReviewFinding     ValidationCode = "review_finding"
	ValidationReviewReference   ValidationCode = "review_reference"
	ValidationReviewDuplicate   ValidationCode = "review_duplicate"
	ValidationReviewGuidance    ValidationCode = "review_guidance"
	ValidationReviewConfidence  ValidationCode = "review_confidence"
	ValidationExecutionContract ValidationCode = "execution_contract"
	ValidationExecutionGateway  ValidationCode = "execution_gateway"
	ValidationExecutionTimeout  ValidationCode = "execution_timeout"
	ValidationExecutionOutput   ValidationCode = "execution_output"
	ValidationExecutionSize     ValidationCode = "execution_size"
	ValidationResultIdentity    ValidationCode = "result_identity"
	ValidationResultDuration    ValidationCode = "result_duration"
	ValidationResultUsage       ValidationCode = "result_usage"
	ValidationResultTerminal    ValidationCode = "result_terminal"
)

// ValidationError preserves errors.Is behavior while exposing a stable code.
type ValidationError struct {
	Code  ValidationCode
	Cause error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ValidationCodeOf returns the stable code carried by a deterministic rejection.
func ValidationCodeOf(err error) ValidationCode {
	var coded *ValidationError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func validationError(code ValidationCode, sentinel error, format string, args ...any) error {
	return &ValidationError{Code: code, Cause: fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))}
}

func withValidationCode(code ValidationCode, err error) error {
	if err == nil || ValidationCodeOf(err) != "" {
		return err
	}
	return &ValidationError{Code: code, Cause: err}
}
