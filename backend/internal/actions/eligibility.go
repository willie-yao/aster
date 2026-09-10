package actions

import (
	"context"
	"errors"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actionverify"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/remediationpolicy"
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
	if !models.PatternAllowsActions(*pattern) {
		return ReasonContractGenerationFailed, "This causal-group result is analysis-only and cannot start an action."
	}
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

func manualFixWarnings(subject *ActionSubject, minConfidence string) []string {
	if subject == nil {
		return nil
	}
	var warnings []string
	if subject.Kind == actionSubjectBuild && subject.Build != nil {
		analysis := subject.Build.Failure.AIAnalysis
		if analysis == nil {
			return nil
		}
		if !ai.MeetsCurrentCritiqueContract(analysis) {
			warnings = append(warnings, "The original analysis does not pass the current critique quality contract.")
		}
		if strings.TrimSpace(analysis.GeneratedAt) == "" {
			warnings = append(warnings, "The original analysis has no generation timestamp.")
		}
		if strings.TrimSpace(analysis.RootCause) == "" {
			warnings = append(warnings, "The original analysis has no root-cause hypothesis.")
		}
		if strings.TrimSpace(analysis.SuggestedFix) == "" {
			warnings = append(warnings, "The original analysis has no suggested fix.")
		}
		if strings.EqualFold(strings.TrimSpace(analysis.Severity), "Transient-Ignore") {
			warnings = append(warnings, "The original analysis classifies this failure as transient.")
		}
		if len(analysis.RelevantFiles) == 0 && len(analysis.FileLinks) == 0 {
			warnings = append(warnings, "The original analysis has no source hints; the coding agent must investigate the repository.")
		}
		if remediationpolicy.RelationshipTextWarning(strings.Join([]string{analysis.RootCause, analysis.SuggestedFix}, "\n")) != "" {
			warnings = append(warnings, "The original analysis triggered a text-only remediation-policy concern.")
		}
		return warnings
	}
	if subject.Kind != actionSubjectPattern || subject.Pattern == nil {
		return warnings
	}
	pattern := subject.Pattern
	if !fixpr.Eligible(*pattern, minConfidence) {
		warnings = append(warnings, "The published pattern does not meet automatic Fix eligibility; this manual attempt will investigate.")
	}
	if strings.TrimSpace(pattern.SharedRootCause) == "" {
		warnings = append(warnings, "The published pattern has no shared root-cause hypothesis.")
	}
	if strings.TrimSpace(pattern.SuggestedFix) == "" {
		warnings = append(warnings, "The published pattern has no suggested fix.")
	}
	if pattern.Lifecycle != nil && pattern.Lifecycle.State != models.PatternLifecycleActive {
		warnings = append(warnings, "The published pattern lifecycle is not active.")
	}
	completeTarget := false
	sourceHint := len(pattern.RelevantFiles) > 0 || len(pattern.FileLinks) > 0
	for _, target := range pattern.RemediationTargets {
		if strings.TrimSpace(target.Path) != "" {
			sourceHint = true
		}
		if target.Intent != models.RemediationIntentInvestigate && actionverify.PatternTargetReason(target) == "" {
			completeTarget = true
		}
	}
	if !completeTarget {
		warnings = append(warnings, "The published remediation contract is incomplete or investigative.")
	}
	if !sourceHint {
		warnings = append(warnings, "The published pattern has no source hints; the coding agent must investigate the repository.")
	}
	if remediationpolicy.RelationshipTextWarning(strings.Join([]string{pattern.SharedRootCause, pattern.SuggestedFix, pattern.Summary}, "\n")) != "" {
		warnings = append(warnings, "The published analysis triggered a text-only remediation-policy concern.")
	}
	return warnings
}
