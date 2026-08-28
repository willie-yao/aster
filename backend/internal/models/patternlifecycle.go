package models

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	minObservedRecoveryPasses = 3
	minVerifiedFixedPasses    = 2
)

var immutableSourceRevision = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// PatternIsActive reports whether a pattern belongs in active recurring
// surfaces and may start actions. Legacy patterns without lifecycle metadata
// remain active until refreshed under the current engine contract.
func PatternIsActive(pattern PatternAnalysis) bool {
	return pattern.Lifecycle == nil || pattern.Lifecycle.State == PatternLifecycleActive
}

// PatternIsRecovered reports observation-only recovery without source proof.
func PatternIsRecovered(pattern PatternAnalysis) bool {
	return pattern.Lifecycle != nil && pattern.Lifecycle.State == PatternLifecycleRecovered
}

// RefreshRetainedPatternLifecycle updates observation state while preserving a
// prior model verdict whose correlated failure may have left the current window.
func RefreshRetainedPatternLifecycle(detail JobDetail, pattern *PatternAnalysis) {
	if pattern == nil || !pattern.Systemic {
		return
	}
	previous := clonePatternLifecycle(pattern.Lifecycle)
	ApplyPatternLifecycle(detail, pattern)
	if pattern.Lifecycle == nil {
		return
	}

	newerRuns := runsAfterCorrelatedFailure(detail, *pattern)
	builds, encounteredFailure := leadingPassingBuilds(JobDetail{Runs: newerRuns})
	if encounteredFailure {
		pattern.Lifecycle.State = PatternLifecycleActive
		pattern.Lifecycle.Reason = "A newer completed failure occurred after the prior lifecycle evidence."
		pattern.Lifecycle.PassingBuilds = nil
		pattern.Lifecycle.RecoveryBuilds = builds
		pattern.Lifecycle.RecoveryStreak = len(builds)
		applyObservedRecovery(pattern.Lifecycle)
		return
	}
	if pattern.Lifecycle.State == PatternLifecycleObserving || pattern.Lifecycle.State == PatternLifecycleVerifiedFixed || hasCorrelatedFailure(detail, *pattern) {
		return
	}

	pattern.Lifecycle.RecoveryBuilds = builds
	pattern.Lifecycle.RecoveryStreak = len(builds)
	if len(builds) >= minObservedRecoveryPasses {
		applyObservedRecovery(pattern.Lifecycle)
		return
	}
	if previous != nil && previous.State == PatternLifecycleRecovered {
		pattern.Lifecycle.RecoveryBuilds = mergeBuildIDs(builds, previous.RecoveryBuilds)
		pattern.Lifecycle.RecoveryStreak = max(previous.RecoveryStreak, len(pattern.Lifecycle.RecoveryBuilds))
		applyObservedRecovery(pattern.Lifecycle)
	}
}

// ApplyPatternLifecycle derives active, recovered, observing, or verified-fixed
// state from current run observations and pinned-source verification.
func ApplyPatternLifecycle(detail JobDetail, pattern *PatternAnalysis) {
	if pattern == nil || !pattern.Systemic {
		return
	}
	lifecycle := observedPatternLifecycle(detail, pattern.SharedBuilds)
	pattern.Lifecycle = lifecycle
	verification := pattern.RemediationVerification
	if verification == nil || verification.State != PatternRemediationAlreadyPresent {
		if verification != nil && verification.Reason != "" {
			lifecycle.Reason = verification.Reason
		}
		applyObservedRecovery(lifecycle)
		return
	}

	_, revision, ok := immutableVerificationSource(*verification)
	if !ok {
		lifecycle.Reason = "The remediation is present, but the pattern source revision is not immutable."
		applyObservedRecovery(lifecycle)
		return
	}
	lifecycle.SourceRevision = revision
	if verification.FailureState != PatternRemediationUnresolved || len(verification.FailureBuilds) != len(pattern.SharedBuilds) {
		lifecycle.Reason = "The remediation is present, but it was not proven absent from every correlated failure revision."
		applyObservedRecovery(lifecycle)
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

// CausalGroupLifecycle derives observation-only recovery for one causal group.
func CausalGroupLifecycle(detail JobDetail, builds []string) *PatternLifecycle {
	return observedPatternLifecycle(detail, builds)
}

func observedPatternLifecycle(detail JobDetail, builds []string) *PatternLifecycle {
	recoveryBuilds := observedRecoveryBuilds(detail, PatternAnalysis{SharedBuilds: builds})
	lifecycle := &PatternLifecycle{
		State:          PatternLifecycleActive,
		Reason:         "The recurring remediation remains unresolved.",
		RecoveryStreak: len(recoveryBuilds),
		RecoveryBuilds: recoveryBuilds,
	}
	applyObservedRecovery(lifecycle)
	return lifecycle
}

func applyObservedRecovery(lifecycle *PatternLifecycle) {
	if lifecycle == nil || lifecycle.RecoveryStreak < minObservedRecoveryPasses {
		return
	}
	lifecycle.State = PatternLifecycleRecovered
	lifecycle.Reason = fmt.Sprintf(
		"The job has passed %d consecutive observed runs since the last correlated failure. The recovery has not been source-verified as a fix.",
		lifecycle.RecoveryStreak,
	)
}

// observedRecoveryBuilds returns the newest consecutive completed passes after
// the most recent correlated failure. Pending runs do not count or break the streak.
func observedRecoveryBuilds(detail JobDetail, pattern PatternAnalysis) []string {
	lastCorrelatedFailure := correlatedFailureIndex(detail, pattern)
	if lastCorrelatedFailure < 0 {
		return nil
	}
	builds, _ := leadingPassingBuilds(JobDetail{Runs: detail.Runs[:lastCorrelatedFailure]})
	return builds
}

func hasCorrelatedFailure(detail JobDetail, pattern PatternAnalysis) bool {
	return correlatedFailureIndex(detail, pattern) >= 0
}

func runsAfterCorrelatedFailure(detail JobDetail, pattern PatternAnalysis) []BuildResult {
	index := correlatedFailureIndex(detail, pattern)
	if index < 0 {
		return detail.Runs
	}
	return detail.Runs[:index]
}

func correlatedFailureIndex(detail JobDetail, pattern PatternAnalysis) int {
	if len(detail.Runs) == 0 || len(pattern.SharedBuilds) == 0 {
		return -1
	}
	shared := make(map[string]bool, len(pattern.SharedBuilds))
	for _, buildID := range pattern.SharedBuilds {
		shared[buildID] = true
	}
	for index := range detail.Runs {
		run := detail.Runs[index]
		if shared[run.BuildID] && run.Result != "PENDING" && !run.Passed {
			return index
		}
	}
	return -1
}

func leadingPassingBuilds(detail JobDetail) ([]string, bool) {
	builds := make([]string, 0, len(detail.Runs))
	for index := range detail.Runs {
		run := detail.Runs[index]
		if run.Result == "PENDING" {
			continue
		}
		if !run.Passed {
			return builds, true
		}
		builds = append(builds, run.BuildID)
	}
	return builds, false
}

func clonePatternLifecycle(lifecycle *PatternLifecycle) *PatternLifecycle {
	if lifecycle == nil {
		return nil
	}
	clone := *lifecycle
	clone.PassingBuilds = append([]string(nil), lifecycle.PassingBuilds...)
	clone.RecoveryBuilds = append([]string(nil), lifecycle.RecoveryBuilds...)
	return &clone
}

func mergeBuildIDs(current, previous []string) []string {
	merged := make([]string, 0, len(current)+len(previous))
	seen := make(map[string]bool, len(current)+len(previous))
	for _, builds := range [][]string{current, previous} {
		for _, buildID := range builds {
			if buildID == "" || seen[buildID] {
				continue
			}
			seen[buildID] = true
			merged = append(merged, buildID)
		}
	}
	return merged
}

func immutableVerificationSource(verification PatternRemediationVerification) (repository, revision string, ok bool) {
	repository = strings.TrimSpace(verification.Repository)
	revision = strings.ToLower(strings.TrimSpace(verification.Revision))
	return repository, revision, repository != "" && immutableSourceRevision.MatchString(revision)
}
