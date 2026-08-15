package modelprovider

import (
	"errors"
	"strings"
)

// ErrUnsupportedReasoningEffort identifies a value outside the shared wire contract.
var ErrUnsupportedReasoningEffort = errors.New("reasoning effort is unsupported; want none, low, medium, high, xhigh, max, or empty")

// ReasoningEffort is the provider-requested reasoning effort. Empty uses the
// provider default.
type ReasoningEffort string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

// CanonicalReasoningEffort normalizes whitespace and case without validating
// provider or model support.
func CanonicalReasoningEffort(value string) ReasoningEffort {
	return ReasoningEffort(strings.ToLower(strings.TrimSpace(value)))
}

// NormalizeReasoningEffort normalizes and validates one requested effort.
func NormalizeReasoningEffort(value string) (ReasoningEffort, error) {
	effort := CanonicalReasoningEffort(value)
	switch effort {
	case "", ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium,
		ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return effort, nil
	default:
		return effort, ErrUnsupportedReasoningEffort
	}
}
