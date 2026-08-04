package actions

import (
	"context"
	"errors"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	EligibilityActionable            = "actionable"
	EligibilityInvestigationRequired = "investigation_required"
	EligibilityAlreadyPresent        = "already_present"
	EligibilityMoreEvidenceRequired  = "more_evidence_required"
)

// Eligibility describes whether a new issue or fix draft can start.
type Eligibility struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// ActionEligibility verifies the current published subject without generating a draft.
func (s *Service) ActionEligibility(ctx context.Context, failureID string) (Eligibility, error) {
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return Eligibility{}, err
	}
	if subject.Kind == actionSubjectPattern && subject.Pattern != nil {
		targets := subject.Pattern.RemediationTargets
		if len(targets) == 0 {
			return moreEvidenceEligibility(), nil
		}
		for _, target := range targets {
			if actionverify.InvalidTargetReason(target) != "" {
				return moreEvidenceEligibility(), nil
			}
		}
		for _, target := range targets {
			if target.Intent == models.RemediationIntentInvestigate {
				return Eligibility{
					State:  EligibilityInvestigationRequired,
					Reason: "The published remediation requires source investigation before an issue or fix can be drafted.",
				}, nil
			}
		}
	}
	if s.cfg == nil || s.sourceVerifier == nil {
		return moreEvidenceEligibility(), nil
	}
	repo := s.cfg.EffectiveAnalysisSourceRepo()
	if repo.Owner == "" || repo.Name == "" {
		return moreEvidenceEligibility(), nil
	}
	if err := s.verifyRemediation(ctx, subject); err != nil {
		switch {
		case errors.Is(err, ErrRemediationAlreadyPresent):
			return Eligibility{
				State:  EligibilityAlreadyPresent,
				Reason: "The grounded source already contains the proposed remediation.",
			}, nil
		case errors.Is(err, ErrRemediationInconclusive):
			return moreEvidenceEligibility(), nil
		default:
			return Eligibility{}, err
		}
	}
	return Eligibility{
		State:  EligibilityActionable,
		Reason: "A verified implementation target remains at the pinned source commit.",
	}, nil
}

func moreEvidenceEligibility() Eligibility {
	return Eligibility{
		State:  EligibilityMoreEvidenceRequired,
		Reason: "The published analysis does not contain enough verified source evidence for an implementation-ready action.",
	}
}
