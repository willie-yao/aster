package actions

import (
	"errors"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actionverify"
)

// ReasonCode is a stable machine-readable explanation for action availability.
type ReasonCode string

const (
	ReasonActionable                     ReasonCode = "actionable"
	ReasonRecovered                      ReasonCode = "recovered"
	ReasonObserving                      ReasonCode = "observing"
	ReasonVerifiedFixed                  ReasonCode = "verified_fixed"
	ReasonNonSystemic                    ReasonCode = "non_systemic"
	ReasonEvidenceUnavailable            ReasonCode = "evidence_unavailable"
	ReasonInvestigationRequired          ReasonCode = "investigation_required"
	ReasonNoReviewablePatch              ReasonCode = "no_reviewable_patch"
	ReasonContractGenerationFailed       ReasonCode = "contract_generation_failed"
	ReasonUnsafeRemediation              ReasonCode = "unsafe_remediation"
	ReasonAlreadyPresent                 ReasonCode = "already_present"
	ReasonSourceVerificationInconclusive ReasonCode = "source_verification_inconclusive"
	ReasonSourceBranchUnknown            ReasonCode = "source_branch_unknown"
	ReasonSourceRevisionDiverged         ReasonCode = "source_revision_diverged"
	ReasonSourceChanged                  ReasonCode = "source_changed"
	ReasonProviderCredentialRejected     ReasonCode = "provider_credential_rejected"
	ReasonGenerationFailed               ReasonCode = "generation_failed"
)

var reasonCodeOrder = []ReasonCode{
	ReasonActionable,
	ReasonRecovered,
	ReasonObserving,
	ReasonVerifiedFixed,
	ReasonNonSystemic,
	ReasonEvidenceUnavailable,
	ReasonInvestigationRequired,
	ReasonNoReviewablePatch,
	ReasonContractGenerationFailed,
	ReasonUnsafeRemediation,
	ReasonAlreadyPresent,
	ReasonSourceVerificationInconclusive,
	ReasonSourceBranchUnknown,
	ReasonSourceRevisionDiverged,
	ReasonSourceChanged,
	ReasonProviderCredentialRejected,
	ReasonGenerationFailed,
}

func validReasonCode(code ReasonCode) bool {
	for _, candidate := range reasonCodeOrder {
		if code == candidate {
			return true
		}
	}
	return false
}

// ReasonCodes returns the supported eligibility reason contract.
func ReasonCodes() []string {
	out := make([]string, 0, len(reasonCodeOrder))
	for _, code := range reasonCodeOrder {
		out = append(out, string(code))
	}
	return out
}

// ReasonMessage returns the operator-facing summary for a stable reason code.
func ReasonMessage(code ReasonCode) string {
	switch code {
	case ReasonActionable:
		return "A verified implementation target remains at the pinned source commit."
	case ReasonRecovered:
		return "Observed passing runs have recovered, but source verification has not proven a fix."
	case ReasonObserving:
		return "The remediation is present and the dashboard is observing later comparable runs."
	case ReasonVerifiedFixed:
		return "The remediation and multiple later passing runs have been verified at pinned source revisions."
	case ReasonNonSystemic:
		return "This result was classified as non-systemic and does not qualify for a recurring-pattern action."
	case ReasonEvidenceUnavailable:
		return "Current published evidence is unavailable or no longer matches the selected action subject."
	case ReasonInvestigationRequired:
		return "The published remediation requires maintainer investigation before an issue or fix can be drafted."
	case ReasonNoReviewablePatch:
		return "No reviewable patch was generated. Add a maintainer instruction and regenerate."
	case ReasonContractGenerationFailed:
		return "The action preview could not be generated from the current verified inputs."
	case ReasonUnsafeRemediation:
		return "The proposed remediation violates the deterministic safety policy and requires further investigation."
	case ReasonAlreadyPresent:
		return "The grounded source already contains the proposed remediation."
	case ReasonSourceVerificationInconclusive:
		return "Pinned-source verification was inconclusive; investigate the grounded source before starting an action."
	case ReasonSourceBranchUnknown:
		return "The build does not report a resolvable source branch, so a generation base cannot be established."
	case ReasonSourceRevisionDiverged:
		return "The failure commit is not an ancestor of its branch head, so a patch cannot be safely generated."
	case ReasonSourceChanged:
		return "A verified source path is unavailable or changed between the failure revision and its branch head."
	case ReasonProviderCredentialRejected:
		return "The model provider rejected the sandbox credential. Generation cannot succeed until the credential is fixed."
	case ReasonGenerationFailed:
		return "Draft generation did not complete successfully."
	default:
		return "This action is unavailable."
	}
}

// ReasonError is an action error that carries a stable reason code. Callers
// outside the package classify a rejection by matching it with errors.As.
type ReasonError struct {
	Code   ReasonCode
	Reason string
	Cause  error
}

func (e *ReasonError) Error() string {
	if strings.TrimSpace(e.Reason) != "" {
		return e.Reason
	}
	return ReasonMessage(e.Code)
}

func (e *ReasonError) Unwrap() error { return e.Cause }

func withReason(code ReasonCode, cause error, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = ReasonMessage(code)
	}
	return &ReasonError{Code: code, Reason: reason, Cause: cause}
}

// ReasonCodeOf classifies an action error without exposing private details.
func ReasonCodeOf(err error) ReasonCode {
	if err == nil {
		return ReasonActionable
	}
	if code, ok := ReasonCodeFrom(err); ok {
		return code
	}
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRequestNotFound), errors.Is(err, ErrPreviewNotFound):
		return ReasonEvidenceUnavailable
	case errors.Is(err, ErrRemediationAlreadyPresent):
		return ReasonAlreadyPresent
	case errors.Is(err, ErrRemediationInconclusive):
		return ReasonSourceVerificationInconclusive
	case errors.Is(err, ErrPreviewTargetChanged):
		return ReasonEvidenceUnavailable
	case errors.Is(err, ErrPreviewRejected), errors.Is(err, ErrDraftRefinementRejected):
		return ReasonContractGenerationFailed
	default:
		return ReasonGenerationFailed
	}
}

// ReasonCodeFrom returns a reason code only when one was explicitly attached.
// Callers that must distinguish a classified rejection from an unclassified
// error use this instead of ReasonCodeOf, which always returns a code.
func ReasonCodeFrom(err error) (ReasonCode, bool) {
	var reasonErr *ReasonError
	if errors.As(err, &reasonErr) && validReasonCode(reasonErr.Code) {
		return reasonErr.Code, true
	}
	return "", false
}

func reasonErrorForCode(code ReasonCode, reason string) error {
	cause := ErrRemediationInconclusive
	if verificationStateForCode(code) == actionverify.StateAlreadyPresent {
		cause = ErrRemediationAlreadyPresent
	}
	return withReason(code, cause, reason)
}

func previewValidationReasonCode(err error) ReasonCode {
	if ReasonCodeOf(err) == ReasonUnsafeRemediation {
		return ReasonUnsafeRemediation
	}
	return ReasonContractGenerationFailed
}

func eligibilityStateForCode(code ReasonCode) string {
	switch code {
	case ReasonActionable:
		return EligibilityActionable
	case ReasonInvestigationRequired:
		return EligibilityInvestigationRequired
	case ReasonAlreadyPresent, ReasonObserving, ReasonVerifiedFixed:
		return EligibilityAlreadyPresent
	case ReasonRecovered:
		return EligibilityRecovered
	default:
		return EligibilityMoreEvidenceRequired
	}
}

func verificationStateForCode(code ReasonCode) string {
	switch code {
	case ReasonActionable:
		return actionverify.StateUnresolved
	case ReasonAlreadyPresent, ReasonObserving, ReasonVerifiedFixed:
		return actionverify.StateAlreadyPresent
	default:
		return actionverify.StateInconclusive
	}
}

func eligibilityForCode(code ReasonCode, reason string) Eligibility {
	if strings.TrimSpace(reason) == "" {
		reason = ReasonMessage(code)
	}
	return Eligibility{State: eligibilityStateForCode(code), Code: code, Reason: reason}
}
