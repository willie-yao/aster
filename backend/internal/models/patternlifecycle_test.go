package models

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func recurrenceTestFailure(name, message string) TestCase {
	return TestCase{
		Name: name, Status: "failed", SuiteName: "e2e", ClassName: "cluster",
		FailureLocation: "test/e2e/cluster.go:120", FailureMessage: message,
		AIAnalysis: &AIAnalysis{RootCause: "cause", Disposition: AnalysisDispositionPreliminary},
	}
}

func TestRefreshRetainedPatternLifecycleSameCause(t *testing.T) {
	failure := recurrenceTestFailure("Azure Linux 3", "Timed out after 1200.001s joining etcd")
	unrelated := recurrenceTestFailure("MSI", "identity unavailable")
	detail := JobDetail{JobID: "job"}
	signature := FailureRecurrenceSignature(detail, &failure)
	pass := func(id string) BuildResult {
		return BuildResult{BuildInfo: BuildInfo{BuildID: id, Result: "SUCCESS", Passed: true}}
	}
	newFailure := func(cases ...TestCase) BuildResult {
		return BuildResult{
			BuildInfo: BuildInfo{
				BuildID: "recurrence", Result: "FAILURE",
				Started: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
			},
			TestCases: cases,
		}
	}
	for _, test := range []struct {
		name       string
		runs       []BuildResult
		mutate     func(*PatternAnalysis, *JobDetail)
		wantState  PatternLifecycleState
		wantBuilds []string
		recurrence bool
	}{
		{
			name:      "unrelated failure does not reset or count",
			runs:      []BuildResult{pass("3"), newFailure(unrelated), pass("2")},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3", "2"},
		},
		{
			name:      "three passes separated by unrelated failures recover",
			runs:      []BuildResult{pass("3"), newFailure(unrelated), pass("2"), pass("1")},
			wantState: PatternLifecycleRecovered, wantBuilds: []string{"3", "2", "1"},
		},
		{
			name:      "non representative matching failure resets",
			runs:      []BuildResult{pass("3"), newFailure(unrelated, failure), pass("2"), pass("1")},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"}, recurrence: true,
		},
		{
			name: "varying timeout and failure line still match",
			runs: []BuildResult{pass("3"), newFailure(failure), pass("2"), pass("1")},
			mutate: func(_ *PatternAnalysis, detail *JobDetail) {
				detail.Runs[1].TestCases[0].FailureMessage = "Timed out after 1234.987s joining etcd"
				detail.Runs[1].TestCases[0].FailureLocation = "test/e2e/cluster.go:456:12"
			},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"}, recurrence: true,
		},
		{
			name: "legacy identity resets conservatively",
			runs: []BuildResult{pass("3"), newFailure(unrelated), pass("2")},
			mutate: func(pattern *PatternAnalysis, _ *JobDetail) {
				pattern.CausalGroups[0].RecurrenceSignature = ""
			},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"},
		},
		{
			name:      "unanalyzed failure resets conservatively",
			runs:      []BuildResult{pass("3"), newFailure(), pass("2")},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"},
		},
		{
			name:      "one unanalyzed failed test prevents ruling out recurrence",
			runs:      []BuildResult{pass("3"), newFailure(unrelated, TestCase{Name: "unknown", Status: "failed"}), pass("2")},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"},
		},
		{
			name: "incomplete junit prevents ruling out recurrence",
			runs: []BuildResult{pass("3"), newFailure(unrelated), pass("2")},
			mutate: func(_ *PatternAnalysis, detail *JobDetail) {
				detail.Runs[1].TestsFailed = 2
			},
			wantState: PatternLifecycleActive, wantBuilds: []string{"3"},
		},
		{
			name: "pending failure does not reset",
			runs: []BuildResult{
				pass("3"), {BuildInfo: BuildInfo{BuildID: "pending", Result: "PENDING"}},
				newFailure(unrelated), pass("2"), pass("1"),
			},
			wantState: PatternLifecycleRecovered, wantBuilds: []string{"3", "2", "1"},
		},
		{
			name: "different provenance is unrelated",
			runs: []BuildResult{pass("3"), newFailure(failure), pass("2"), pass("1")},
			mutate: func(_ *PatternAnalysis, detail *JobDetail) {
				detail.Runs[1].TestCases[0].SuiteName = "other suite"
			},
			wantState: PatternLifecycleRecovered, wantBuilds: []string{"3", "2", "1"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pattern := PatternAnalysis{
				Systemic: true, SharedBuilds: []string{"old-1", "old-2"},
				CausalGroups: []PatternCausalGroup{{Builds: []string{"old-1", "old-2"}, RecurrenceSignature: signature}},
			}
			current := JobDetail{JobID: "job", Runs: test.runs}
			if test.mutate != nil {
				test.mutate(&pattern, &current)
			}
			RefreshRetainedPatternLifecycle(current, &pattern)
			got := pattern.Lifecycle
			if got.State != test.wantState || got.RecoveryStreak != len(test.wantBuilds) ||
				!slices.Equal(got.RecoveryBuilds, test.wantBuilds) {
				t.Fatalf("lifecycle = %+v, want %s with passes %v", got, test.wantState, test.wantBuilds)
			}
			if test.recurrence {
				if got.LatestRecurrenceBuildID != "recurrence" || got.LatestRecurrenceStarted != "2026-10-19T12:00:00Z" {
					t.Fatalf("recurrence = %+v", got)
				}
			} else if got.LatestRecurrenceBuildID != "" || got.LatestRecurrenceStarted != "" {
				t.Fatalf("unconfirmed recurrence published: %+v", got)
			}
		})
	}
}

func TestRefreshRetainedPatternLifecyclePreservesLatestRecurrence(t *testing.T) {
	pattern := PatternAnalysis{
		Systemic: true, SharedBuilds: []string{"old-1", "old-2"},
		Lifecycle: &PatternLifecycle{
			State: PatternLifecycleRecovered, RecoveryStreak: 3, RecoveryBuilds: []string{"3", "2", "1"},
			LatestRecurrenceBuildID: "recurrence", LatestRecurrenceStarted: "2026-10-19T12:00:00Z",
		},
	}
	RefreshRetainedPatternLifecycle(JobDetail{Runs: []BuildResult{{
		BuildInfo: BuildInfo{BuildID: "4", Result: "SUCCESS", Passed: true},
	}}}, &pattern)
	if pattern.Lifecycle.State != PatternLifecycleRecovered ||
		pattern.Lifecycle.LatestRecurrenceBuildID != "recurrence" ||
		pattern.Lifecycle.LatestRecurrenceStarted != "2026-10-19T12:00:00Z" {
		t.Fatalf("lifecycle = %+v", pattern.Lifecycle)
	}
}

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
