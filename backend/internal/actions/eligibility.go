package actions

import (
	"context"
	"errors"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
)

const (
	EligibilityActionable            = "actionable"
	EligibilityInvestigationRequired = "investigation_required"
	EligibilityAlreadyPresent        = "already_present"
	EligibilityRecovered             = "recovered"
	EligibilityMoreEvidenceRequired  = "more_evidence_required"
)

// Eligibility describes whether a new issue or fix draft can start.
type Eligibility struct {
	State  string     `json:"state"`
	Code   ReasonCode `json:"code"`
	Reason string     `json:"reason"`
}

// ActionEligibility verifies the current published subject without generating a draft.
func (s *Service) ActionEligibility(ctx context.Context, failureID string) (Eligibility, error) {
	subject, err := s.resolveSubjectForEligibility(failureID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Eligibility{}, err
		}
		if code := ReasonCodeOf(err); code != ReasonGenerationFailed {
			return eligibilityForCode(code, ""), nil
		}
		return Eligibility{}, err
	}
	if code, reason := subjectEligibilityReason(subject); code != "" {
		return eligibilityForCode(code, reason), nil
	}
	if s.cfg == nil || s.sourceVerifier == nil {
		return eligibilityForCode(ReasonSourceVerificationInconclusive, ""), nil
	}
	repo := s.cfg.EffectiveAnalysisSourceRepo()
	if repo.Owner == "" || repo.Name == "" {
		return eligibilityForCode(ReasonSourceVerificationInconclusive, ""), nil
	}
	if err := s.verifyRemediation(ctx, subject); err != nil {
		switch {
		case errors.Is(err, ErrRemediationAlreadyPresent):
			return eligibilityForCode(ReasonCodeOf(err), ""), nil
		case errors.Is(err, ErrRemediationInconclusive):
			return eligibilityForCode(ReasonCodeOf(err), ""), nil
		default:
			return Eligibility{}, err
		}
	}
	return eligibilityForCode(ReasonActionable, ""), nil
}

func subjectEligibilityReason(subject *ActionSubject) (ReasonCode, string) {
	if subject == nil {
		return ReasonEvidenceUnavailable, ""
	}
	if subject.Kind != actionSubjectPattern || subject.Pattern == nil {
		return "", ""
	}
	pattern := subject.Pattern
	published := strings.TrimSpace(pattern.ID) != ""
	if code := patternRefreshReasonCode(subject.PatternRefresh); code != "" {
		return code, ""
	}
	if published && !pattern.Systemic {
		return ReasonNonSystemic, ""
	}
	if pattern.Lifecycle != nil && pattern.Lifecycle.State != models.PatternLifecycleActive {
		switch pattern.Lifecycle.State {
		case models.PatternLifecycleRecovered:
			return ReasonRecovered, pattern.Lifecycle.Reason
		case models.PatternLifecycleObserving:
			return ReasonObserving, pattern.Lifecycle.Reason
		case models.PatternLifecycleVerifiedFixed:
			return ReasonVerifiedFixed, pattern.Lifecycle.Reason
		default:
			return ReasonEvidenceUnavailable, pattern.Lifecycle.Reason
		}
	}
	if published && len(pattern.RemediationTargets) == 0 {
		return ReasonContractGenerationFailed, ""
	}
	for _, target := range pattern.RemediationTargets {
		if actionverify.PatternTargetReason(target) != "" {
			return ReasonContractGenerationFailed, ""
		}
		if target.Intent == models.RemediationIntentInvestigate {
			return ReasonInvestigationRequired, ""
		}
	}
	policyText := strings.Join([]string{pattern.SuggestedFix, pattern.SharedRootCause, pattern.Summary, subject.PolicyText}, "\n")
	if remediationpolicy.Reason(policyText, pattern.RemediationTargets) != "" {
		return ReasonUnsafeRemediation, ""
	}
	return "", ""
}
