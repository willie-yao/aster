package ai

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNonSystemicPatternNormalizationClearsProhibitedFields(t *testing.T) {
	buildIDs := patternBuildIDs(patternFailures(3))
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "suggested fix",
			raw:  `{"systemic":false,"confidence":"medium","shared_root_cause":"","shared_builds":["abuild","bbuild"],"suggested_fix":"restart the individual job","remediation_targets":[],"summary":"the failures have separate causes"}`,
		},
		{
			name: "targets",
			raw:  `{"systemic":false,"confidence":"medium","shared_root_cause":"","shared_builds":["abuild","bbuild"],"suggested_fix":"","remediation_targets":[{"intent":"investigate"}],"summary":"the failures have separate causes"}`,
		},
		{
			name: "root cause prose",
			raw:  `{"systemic":false,"confidence":"medium","shared_root_cause":"one build timed out during cleanup","shared_builds":["abuild","bbuild"],"suggested_fix":"","remediation_targets":[],"summary":"the failures have separate causes"}`,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, stats, err := parsePatternResponseWithStats(testCase.raw, buildIDs)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Systemic || parsed.Confidence != "medium" || parsed.Summary != "the failures have separate causes" ||
				strings.Join(parsed.SharedBuilds, ",") != "abuild,bbuild" {
				t.Fatalf("preserved fields = %+v", parsed)
			}
			if parsed.SharedRootCause != "" || parsed.SuggestedFix != "" || len(parsed.RemediationTargets) != 0 {
				t.Fatalf("prohibited fields were not cleared: %+v", parsed)
			}
			if stats.NonSystemicNormalizedCount != 1 {
				t.Fatalf("parse stats = %+v", stats)
			}
		})
	}
}

func TestNonSystemicPatternNormalizationKeepsStrictValidation(t *testing.T) {
	buildIDs := patternBuildIDs(patternFailures(3))
	tests := []struct {
		name         string
		raw          string
		wantCategory patternValidationCategory
	}{
		{
			name:         "invalid confidence",
			raw:          `{"systemic":false,"confidence":"certain","shared_root_cause":"individual cause","shared_builds":["abuild","bbuild"],"suggested_fix":"individual fix","remediation_targets":[],"summary":"separate failures"}`,
			wantCategory: patternValidationSchema,
		},
		{
			name:         "invalid build",
			raw:          `{"systemic":false,"confidence":"low","shared_root_cause":"individual cause","shared_builds":["abuild","unknown"],"suggested_fix":"individual fix","remediation_targets":[],"summary":"separate failures"}`,
			wantCategory: patternValidationBuilds,
		},
		{
			name:         "missing summary",
			raw:          `{"systemic":false,"confidence":"low","shared_root_cause":"individual cause","shared_builds":["abuild","bbuild"],"suggested_fix":"individual fix","remediation_targets":[]}`,
			wantCategory: patternValidationSchema,
		},
		{
			name:         "malformed json",
			raw:          `{"systemic":false`,
			wantCategory: patternValidationJSON,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := parsePatternResponseWithStats(testCase.raw, buildIDs)
			if got := patternValidationCategoryOf(err); got != testCase.wantCategory {
				t.Fatalf("category=%q want=%q error=%v", got, testCase.wantCategory, err)
			}
		})
	}
}

func TestNonSystemicPatternNormalizationDoesNotHideConflictingContracts(t *testing.T) {
	first := `{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":["abuild","bbuild"],"suggested_fix":"restart job A","remediation_targets":[],"summary":"separate failures"}`
	second := `{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":["abuild","bbuild"],"suggested_fix":"restart job B","remediation_targets":[],"summary":"separate failures"}`
	_, _, err := parsePatternResponseWithStats(first+"\n"+second, patternBuildIDs(patternFailures(3)))
	if got := patternValidationCategoryOf(err); got != patternValidationAmbiguous || patternValidationIssueOf(err) != "conflicting_valid_contracts" {
		t.Fatalf("category=%q issue=%q error=%v", got, patternValidationIssueOf(err), err)
	}
}

func TestNonSystemicPatternNormalizationLeavesSystemicValidationUnchanged(t *testing.T) {
	invalid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"","remediation_targets":[{"intent":"investigate"}],"summary":"shared"}`
	if _, _, err := parsePatternResponseWithStats(invalid, patternBuildIDs(patternFailures(3))); patternValidationCategoryOf(err) != patternValidationSchema || patternValidationIssueOf(err) != "systemic_contract" {
		t.Fatalf("invalid systemic error=%v category=%q issue=%q", err, patternValidationCategoryOf(err), patternValidationIssueOf(err))
	}

	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"change the shared configuration","remediation_targets":[{"intent":"investigate"}],"summary":"shared"}`
	parsed, stats, err := parsePatternResponseWithStats(valid, patternBuildIDs(patternFailures(3)))
	if err != nil || !parsed.Systemic || parsed.SharedRootCause != "shared cause" || parsed.SuggestedFix != "change the shared configuration" || len(parsed.RemediationTargets) != 1 {
		t.Fatalf("systemic pattern=%+v stats=%+v error=%v", parsed, stats, err)
	}
	if stats.NonSystemicNormalizedCount != 0 {
		t.Fatalf("systemic result was normalized: %+v", stats)
	}
}

func TestAnalyzePatternNormalizesCachesAndTracesWithoutRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	const privateFix = "PRIVATE_INDIVIDUAL_FIX"
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"individual failure detail","shared_builds":["abuild","bbuild"],"suggested_fix":"`+privateFix+`","remediation_targets":[{"intent":"investigate"}],"summary":"separate failures"}`))
	s := newPatternTestService(t, srv.URL)
	store := NewTraceStore()
	s.SetTraceStore(store)
	repairs := 0
	pa, err := s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		AllowValidationRepair: true,
		OnRepair:              func(PatternRepairAttempt) { repairs++ },
	})
	if err != nil || pa == nil || pa.Systemic || pa.SharedRootCause != "" || pa.SuggestedFix != "" || len(pa.RemediationTargets) != 0 {
		t.Fatalf("pattern=%+v error=%v", pa, err)
	}
	if repairs != 0 || atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("repairs=%d model_calls=%d", repairs, srv.calls)
	}

	cacheHits := 0
	cached, err := s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		OnCacheHit: func() { cacheHits++ },
	})
	if err != nil || cached == nil || cached.Systemic || cached.SharedRootCause != "" || cached.SuggestedFix != "" || len(cached.RemediationTargets) != 0 {
		t.Fatalf("cached pattern=%+v error=%v", cached, err)
	}
	if cacheHits != 1 || atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("cache_hits=%d model_calls=%d", cacheHits, srv.calls)
	}

	traceData, err := json.Marshal(store.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(traceData), privateFix) || strings.Contains(string(traceData), "individual failure detail") {
		t.Fatalf("normalization trace exposed removed content: %s", traceData)
	}
	var sawNormalization bool
	for _, trace := range store.Snapshot().Traces {
		for _, event := range trace.Events {
			if event.Kind == "pattern_normalization" && event.Status == "tool_free" && event.Outcome == "applied" && event.NormalizedCount == 1 {
				sawNormalization = true
			}
		}
	}
	if !sawNormalization {
		t.Fatalf("normalization event missing: %+v", store.Snapshot())
	}
}

func TestGroundedPatternExtractionNormalizesWithoutRepairCascade(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("tree", "list_repo_tree", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal("The failures are independent and no systemic contract was emitted."))
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"medium","shared_root_cause":"one build has an individual cause","shared_builds":["abuild","bbuild"],"suggested_fix":"apply an individual fix","remediation_targets":[{"intent":"investigate"}],"summary":"the failures are independent"}`))
	s := newPatternTestService(t, srv.URL)
	s.SetSourceRepo("owner", "repo")
	s.SetPatternRepoReader(&fakeRepoReader{files: map[string]string{"config.yaml": "kind: Config"}})
	repairs := 0
	pa, err := s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		AllowValidationRepair: true,
		OnRepair:              func(PatternRepairAttempt) { repairs++ },
	})
	if err != nil || pa == nil || pa.Systemic || pa.SharedRootCause != "" || pa.SuggestedFix != "" || len(pa.RemediationTargets) != 0 {
		t.Fatalf("pattern=%+v error=%v", pa, err)
	}
	if repairs != 0 || atomic.LoadInt32(&srv.calls) != 3 {
		t.Fatalf("repairs=%d model_calls=%d, want extraction only", repairs, srv.calls)
	}
	if _, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatalf("cached result: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) != 3 {
		t.Fatalf("cached result made another provider call: %d", srv.calls)
	}
}
