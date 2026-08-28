package models

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const lifecycleRevision = "0123456789abcdef0123456789abcdef01234567"

func lifecyclePattern() PatternAnalysis {
	return PatternAnalysis{
		Systemic: true, SharedBuilds: []string{"failure-1", "failure-2"}, SourceRef: "example/repo@" + lifecycleRevision,
		RemediationVerification: &PatternRemediationVerification{
			State: PatternRemediationAlreadyPresent, Reason: "already present", Repository: "example/repo", Revision: lifecycleRevision,
			FailureState: PatternRemediationUnresolved, FailureBuilds: []string{"failure-1", "failure-2"},
		},
	}
}

func TestApplyPatternLifecycle(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*PatternAnalysis)
		want   PatternLifecycleState
		passes int
	}{
		"unresolved remains active": {
			mutate: func(pattern *PatternAnalysis) {
				pattern.RemediationVerification = &PatternRemediationVerification{State: PatternRemediationUnresolved, Reason: "missing"}
			},
			want: PatternLifecycleActive,
		},
		"present without historical proof remains active": {
			mutate: func(pattern *PatternAnalysis) { pattern.RemediationVerification.FailureBuilds = nil },
			want:   PatternLifecycleActive,
		},
		"present waits for runs": {want: PatternLifecycleObserving},
		"one passing run observes": {
			mutate: func(pattern *PatternAnalysis) { pattern.RemediationVerification.PassingBuilds = []string{"pass-1"} },
			want:   PatternLifecycleObserving, passes: 1,
		},
		"two passing runs verify fixed": {
			mutate: func(pattern *PatternAnalysis) {
				pattern.RemediationVerification.PassingBuilds = []string{"pass-1", "pass-2"}
			},
			want: PatternLifecycleVerifiedFixed, passes: 2,
		},
		"remediation present during failure remains active": {
			mutate: func(pattern *PatternAnalysis) {
				pattern.RemediationVerification.FailureState = PatternRemediationAlreadyPresent
			},
			want: PatternLifecycleActive,
		},
		"non-immutable source remains active": {
			mutate: func(pattern *PatternAnalysis) { pattern.RemediationVerification.Revision = "main" },
			want:   PatternLifecycleActive,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pattern := lifecyclePattern()
			if test.mutate != nil {
				test.mutate(&pattern)
			}
			ApplyPatternLifecycle(JobDetail{}, &pattern)
			if pattern.Lifecycle == nil || pattern.Lifecycle.State != test.want || len(pattern.Lifecycle.PassingBuilds) != test.passes {
				t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
			}
		})
	}
}

func TestApplyPatternLifecycleSkipsNonSystemic(t *testing.T) {
	pattern := lifecyclePattern()
	pattern.Systemic = false
	ApplyPatternLifecycle(JobDetail{}, &pattern)
	if pattern.Lifecycle != nil {
		t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
	}
}

func TestPatternIsActive(t *testing.T) {
	if !PatternIsActive(PatternAnalysis{}) || !PatternIsActive(PatternAnalysis{Lifecycle: &PatternLifecycle{State: PatternLifecycleActive}}) {
		t.Fatal("active or legacy pattern was rejected")
	}
	for _, state := range []PatternLifecycleState{PatternLifecycleRecovered, PatternLifecycleObserving, PatternLifecycleVerifiedFixed} {
		if PatternIsActive(PatternAnalysis{Lifecycle: &PatternLifecycle{State: state}}) {
			t.Fatalf("state %q remained active", state)
		}
	}
}

func TestApplyPatternLifecycleObservationRecovery(t *testing.T) {
	for _, test := range []struct {
		name         string
		passingRuns  int
		newerFailure bool
		sparse       bool
		wantState    PatternLifecycleState
		wantStreak   int
	}{
		{name: "three passes recover", passingRuns: 3, wantState: PatternLifecycleRecovered, wantStreak: 3},
		{name: "five passes recover", passingRuns: 5, wantState: PatternLifecycleRecovered, wantStreak: 5},
		{name: "two passes remain active", passingRuns: 2, wantState: PatternLifecycleActive, wantStreak: 2},
		{name: "new failure reactivates", passingRuns: 5, newerFailure: true, wantState: PatternLifecycleActive},
		{name: "sparse schedules count observations", passingRuns: 3, sparse: true, wantState: PatternLifecycleRecovered, wantStreak: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			pattern := PatternAnalysis{
				Systemic: true, Recurrence: PatternRecurrenceSharedCause,
				CausalGroups: []PatternCausalGroup{{
					Builds:    []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
					RootCause: "shared cause", Confidence: "high",
				}},
				SharedBuilds: []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"},
			}
			detail := observedRecoveryDetail(test.passingRuns, test.newerFailure, test.sparse)
			ApplyPatternLifecycle(detail, &pattern)
			if pattern.Lifecycle == nil || pattern.Lifecycle.State != test.wantState || pattern.Lifecycle.RecoveryStreak != test.wantStreak || len(pattern.Lifecycle.RecoveryBuilds) != test.wantStreak {
				t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
			}
			if test.wantState == PatternLifecycleRecovered && strings.Contains(pattern.Lifecycle.Reason, "verified") && !strings.Contains(pattern.Lifecycle.Reason, "not been source-verified as a fix") {
				t.Fatalf("observation recovery reason implied source verification: %q", pattern.Lifecycle.Reason)
			}
		})
	}
}

func TestCausalGroupLifecycleUsesGroupBuilds(t *testing.T) {
	for _, test := range []struct {
		name       string
		passes     int
		wantState  PatternLifecycleState
		wantStreak int
	}{
		{name: "one pass remains active", passes: 1, wantState: PatternLifecycleActive, wantStreak: 1},
		{name: "three passes recover", passes: 3, wantState: PatternLifecycleRecovered, wantStreak: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := observedRecoveryDetail(test.passes, false, false)
			lifecycle := CausalGroupLifecycle(detail, []string{"failure-5", "failure-4"})
			if lifecycle.State != test.wantState || lifecycle.RecoveryStreak != test.wantStreak || len(lifecycle.RecoveryBuilds) != test.wantStreak {
				t.Fatalf("lifecycle = %+v", lifecycle)
			}
			if lifecycle.SourceRevision != "" || len(lifecycle.PassingBuilds) != 0 {
				t.Fatalf("cause lifecycle gained source-verification state: %+v", lifecycle)
			}
		})
	}
}

func TestApplyPatternLifecycleSourceObservationTakesPrecedenceOverObservationRecovery(t *testing.T) {
	pattern := lifecyclePattern()
	pattern.RemediationVerification.PassingBuilds = []string{"pass-1"}
	detail := observedRecoveryDetail(5, false, false)
	pattern.SharedBuilds = []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"}
	pattern.RemediationVerification.FailureBuilds = append([]string(nil), pattern.SharedBuilds...)
	ApplyPatternLifecycle(detail, &pattern)
	if pattern.Lifecycle == nil || pattern.Lifecycle.State != PatternLifecycleObserving || pattern.Lifecycle.RecoveryStreak != 5 {
		t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
	}
}

func TestApplyPatternLifecycleSourceVerificationTakesPrecedenceOverObservationRecovery(t *testing.T) {
	pattern := lifecyclePattern()
	pattern.RemediationVerification.PassingBuilds = []string{"pass-1", "pass-2"}
	detail := observedRecoveryDetail(5, false, false)
	pattern.SharedBuilds = []string{"failure-5", "failure-4", "failure-3", "failure-2", "failure-1"}
	pattern.RemediationVerification.FailureBuilds = append([]string(nil), pattern.SharedBuilds...)
	ApplyPatternLifecycle(detail, &pattern)
	if pattern.Lifecycle == nil || pattern.Lifecycle.State != PatternLifecycleVerifiedFixed || pattern.Lifecycle.RecoveryStreak != 5 {
		t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
	}
}

func observedRecoveryDetail(passingRuns int, newerFailure, sparse bool) JobDetail {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	step := time.Hour
	if sparse {
		step = 7 * 24 * time.Hour
	}
	chronological := make([]BuildResult, 0, 5+passingRuns+1)
	for index := 1; index <= 5; index++ {
		chronological = append(chronological, BuildResult{BuildInfo: BuildInfo{
			BuildID: fmt.Sprintf("failure-%d", index), Result: "FAILURE", Started: base.Add(time.Duration(len(chronological)) * step),
		}})
	}
	for index := 1; index <= passingRuns; index++ {
		chronological = append(chronological, BuildResult{BuildInfo: BuildInfo{
			BuildID: fmt.Sprintf("pass-%d", index), Result: "SUCCESS", Passed: true, Started: base.Add(time.Duration(len(chronological)) * step),
		}})
	}
	if newerFailure {
		chronological = append(chronological, BuildResult{BuildInfo: BuildInfo{
			BuildID: "new-failure", Result: "FAILURE", Started: base.Add(time.Duration(len(chronological)) * step),
		}})
	}
	runs := make([]BuildResult, len(chronological))
	for index := range chronological {
		runs[len(chronological)-1-index] = chronological[index]
	}
	return JobDetail{Runs: runs}
}
