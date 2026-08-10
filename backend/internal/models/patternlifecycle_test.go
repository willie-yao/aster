package models

import "testing"

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
