package models

import (
	"regexp"
	"strings"
)

const minVerifiedFixedPasses = 2

var immutableSourceRevision = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// ApplyPatternLifecycle derives active, observing, or verified-fixed state from
// pinned-source verification and revision-verified post-fix runs.
func ApplyPatternLifecycle(_ JobDetail, pattern *PatternAnalysis) {
	if pattern == nil || !pattern.Systemic {
		return
	}
	lifecycle := &PatternLifecycle{State: PatternLifecycleActive, Reason: "The recurring remediation remains unresolved."}
	pattern.Lifecycle = lifecycle
	verification := pattern.RemediationVerification
	if verification == nil || verification.State != PatternRemediationAlreadyPresent {
		if verification != nil && verification.Reason != "" {
			lifecycle.Reason = verification.Reason
		}
		return
	}

	_, revision, ok := immutableVerificationSource(*verification)
	if !ok {
		lifecycle.Reason = "The remediation is present, but the pattern source revision is not immutable."
		return
	}
	lifecycle.SourceRevision = revision
	if verification.FailureState != PatternRemediationUnresolved || len(verification.FailureBuilds) != len(pattern.SharedBuilds) {
		lifecycle.Reason = "The remediation is present, but it was not proven absent from every correlated failure revision."
		return
	}

	lifecycle.PassingBuilds = append([]string(nil), verification.PassingBuilds...)
	if len(lifecycle.PassingBuilds) >= minVerifiedFixedPasses {
		lifecycle.State = PatternLifecycleVerifiedFixed
		lifecycle.Reason = "The remediation is present and multiple later comparable runs passed at revisions that contain it."
		return
	}
	lifecycle.State = PatternLifecycleObserving
	if len(lifecycle.PassingBuilds) == 1 {
		lifecycle.Reason = "The remediation is present and one later comparable run passed at a revision that contains it."
	} else {
		lifecycle.Reason = "The remediation is present; comparable post-fix runs are still pending."
	}
}

func immutableVerificationSource(verification PatternRemediationVerification) (repository, revision string, ok bool) {
	repository = strings.TrimSpace(verification.Repository)
	revision = strings.ToLower(strings.TrimSpace(verification.Revision))
	return repository, revision, repository != "" && immutableSourceRevision.MatchString(revision)
}
