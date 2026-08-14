package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func newPatternTestService(t *testing.T, serverURL string) *Service {
	t.Helper()
	client := newAgenticTestClient(t, serverURL)
	return NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
}

func patternFailures(n int) []PatternFailure {
	out := make([]PatternFailure, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, PatternFailure{
			BuildID:        string(rune('a'+i)) + "build",
			FailingTest:    "spec",
			FailureMessage: "Timed out after 3600s",
			RootCause:      "etcd-join deadlock on burstable VM",
			SuggestedFix:   "use a non-burstable control-plane VM",
			RelevantFiles:  []string{"controllers/machine.go"},
			IsTransient:    true,
			Severity:       "Transient-Ignore",
		})
	}
	return out
}

func withPatternRuns(failures []PatternFailure, runs []PatternRun) []PatternFailure {
	out := append([]PatternFailure(nil), failures...)
	for i := range out {
		out[i].RecentRuns = append([]PatternRun(nil), runs...)
	}
	return out
}

func patternRunWindowForTest() []PatternRun {
	started := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	return []PatternRun{
		{BuildID: "10", Result: "SUCCESS", Passed: true, StartedAt: started, SourceRevision: strings.Repeat("a", 40)},
		{BuildID: "9", Result: "FAILURE", StartedAt: started.Add(-time.Hour), SourceRevision: strings.Repeat("b", 40)},
		{BuildID: "8", Result: "SUCCESS", Passed: true, StartedAt: started.Add(-2 * time.Hour), SourceRevision: strings.Repeat("c", 40)},
		{BuildID: "7", Result: "FAILURE", StartedAt: started.Add(-3 * time.Hour), SourceRevision: strings.Repeat("d", 40)},
	}
}

func patternToolResponse(response patternResponse) string {
	groups := make([]map[string]any, 0, len(response.Groups))
	for _, group := range response.Groups {
		groups = append(groups, map[string]any{
			"builds": group.Builds, "root_cause": group.RootCause, "confidence": group.Confidence,
		})
	}
	unclassified := response.UnclassifiedBuilds
	if unclassified == nil {
		unclassified = []string{}
	}
	return chatRespToolCall("pattern", "submit_causal_groups", map[string]any{
		"groups": groups, "unclassified_builds": unclassified, "summary": response.Summary,
	})
}

func patternToolRaw(raw string) string {
	arguments, _ := json.Marshal(raw)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"pattern","type":"function","function":{"name":"submit_causal_groups","arguments":%s}}]}}]}`, arguments)
}

func sharedPatternResponse() patternResponse {
	return patternResponse{
		Groups: []patternCausalGroup{
			{Builds: []string{"abuild", "bbuild"}, RootCause: "shared API throttling mechanism", Confidence: "high"},
			{Builds: []string{"cbuild"}, RootCause: "independent image pull failure", Confidence: "medium"},
		},
		UnclassifiedBuilds: []string{},
		Summary:            "Two builds share one cause and one build is independent.",
	}
}

func TestAnalyzePatternTooFewBuildsMakesNoRequest(t *testing.T) {
	srv := newScriptedChatServer(t)
	pattern, err := newPatternTestService(t, srv.URL).AnalyzePattern(t.Context(), "job", "job", patternFailures(1))
	if err != nil || pattern != nil || atomic.LoadInt32(&srv.calls) != 0 {
		t.Fatalf("pattern=%+v error=%v calls=%d", pattern, err, srv.calls)
	}
}

func TestAnalyzePatternForcesCausalGroupFunctionAndDerivesSharedCause(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	response := patternToolResponse(sharedPatternResponse())
	srv.push(200, response)
	pattern, err := newPatternTestService(t, srv.URL).AnalyzePattern(t.Context(), "job", "the-job", patternFailures(3))
	if err != nil {
		t.Fatalf("error=%v calls=%d response=%s requests=%q", err, srv.calls, response, srv.requests)
	}
	if pattern == nil || pattern.Recurrence != models.PatternRecurrenceSharedCause || !pattern.Systemic {
		t.Fatalf("pattern=%+v", pattern)
	}
	if pattern.SharedRootCause != "shared API throttling mechanism" || strings.Join(pattern.SharedBuilds, ",") != "abuild,bbuild" || pattern.Confidence != "high" {
		t.Fatalf("compatibility fields=%+v", pattern)
	}
	if len(pattern.CausalGroups) != 2 || len(pattern.UnclassifiedBuilds) != 0 || pattern.SuggestedFix != "" || len(pattern.RemediationTargets) != 0 || pattern.RemediationVerification != nil {
		t.Fatalf("published analysis-only contract=%+v", pattern)
	}
	if len(srv.requests) != 1 {
		t.Fatalf("requests=%d", len(srv.requests))
	}
	body := string(srv.requests[0])
	for _, want := range []string{`"tool_choice":{"type":"function","function":{"name":"submit_causal_groups"}}`, `"strict":true`, `"parallel_tool_calls":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("request missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"response_format"`) {
		t.Fatalf("forced function request unexpectedly used response_format: %s", body)
	}
}

func TestBuildPatternAnalysisDerivesRecurrence(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		response   patternResponse
		want       models.PatternRecurrence
		systemic   bool
		rootCause  string
		shared     string
		confidence string
	}{
		{name: "mixed", response: patternResponse{Groups: []patternCausalGroup{
			{Builds: []string{"a", "b"}, RootCause: "cause one", Confidence: "high"},
			{Builds: []string{"c", "d"}, RootCause: "cause two", Confidence: "medium"},
		}, Summary: "mixed"}, want: models.PatternRecurrenceMixedCauses, systemic: true, rootCause: "Multiple recurring causes: cause one; cause two", shared: "a,b,c,d", confidence: "medium"},
		{name: "unrelated", response: patternResponse{Groups: []patternCausalGroup{
			{Builds: []string{"a"}, RootCause: "one", Confidence: "high"}, {Builds: []string{"b"}, RootCause: "two", Confidence: "low"},
		}, Summary: "unrelated"}, want: models.PatternRecurrenceUnrelated, confidence: "low"},
		{name: "partial evidence unrelated", response: patternResponse{Groups: []patternCausalGroup{{Builds: []string{"a"}, RootCause: "one", Confidence: "medium"}}, UnclassifiedBuilds: []string{"b"}, Summary: "partial"}, want: models.PatternRecurrenceUnrelated, confidence: "medium"},
		{name: "insufficient", response: patternResponse{UnclassifiedBuilds: []string{"a", "b"}, Summary: "insufficient"}, want: models.PatternRecurrenceInsufficientEvidence, confidence: "low"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pattern := buildPatternAnalysis("job", 2, canonicalizePatternResponse(testCase.response), nil)
			if pattern.Recurrence != testCase.want || pattern.Systemic != testCase.systemic || pattern.SharedRootCause != testCase.rootCause || strings.Join(pattern.SharedBuilds, ",") != testCase.shared || pattern.Confidence != testCase.confidence {
				t.Fatalf("pattern=%+v", pattern)
			}
		})
	}
}

func TestParsePatternResponseRequiresExactCoverageAndContract(t *testing.T) {
	ids := map[string]struct{}{"a": {}, "b": {}, "c": {}}
	valid := `{"groups":[{"builds":["a","b"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":["c"],"summary":"summary"}`
	if _, _, err := parsePatternResponseWithStats(valid, ids); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name, raw, issue string
	}{
		{name: "missing build", raw: `{"groups":[{"builds":["a","b"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":[],"summary":"summary"}`, issue: "missing_build"},
		{name: "duplicate build", raw: `{"groups":[{"builds":["a","b"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":["b","c"],"summary":"summary"}`, issue: "duplicate_build"},
		{name: "duplicate within group", raw: `{"groups":[{"builds":["a","a"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":["b","c"],"summary":"summary"}`, issue: "duplicate_build"},
		{name: "unknown build", raw: `{"groups":[{"builds":["a","z"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":["b","c"],"summary":"summary"}`, issue: "unknown_build"},
		{name: "empty group", raw: `{"groups":[{"builds":[],"root_cause":"cause","confidence":"high"}],"unclassified_builds":["a","b","c"],"summary":"summary"}`, issue: "groups"},
		{name: "empty cause", raw: `{"groups":[{"builds":["a"],"root_cause":"","confidence":"high"}],"unclassified_builds":["b","c"],"summary":"summary"}`, issue: "groups"},
		{name: "confidence", raw: `{"groups":[{"builds":["a"],"root_cause":"cause","confidence":"certain"}],"unclassified_builds":["b","c"],"summary":"summary"}`, issue: "confidence"},
		{name: "extra field", raw: `{"groups":[],"unclassified_builds":["a","b","c"],"summary":"summary","suggested_fix":"bad"}`, issue: "required_fields"},
		{name: "duplicate field", raw: `{"groups":[],"groups":[],"unclassified_builds":["a","b","c"],"summary":"summary"}`, issue: "duplicate_field"},
		{name: "empty summary", raw: `{"groups":[],"unclassified_builds":["a","b","c"],"summary":""}`, issue: "summary"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := parsePatternResponseWithStats(testCase.raw, ids)
			if err == nil || patternValidationIssueOf(err) != testCase.issue {
				t.Fatalf("error=%v issue=%q", err, patternValidationIssueOf(err))
			}
		})
	}
}

func TestParsePatternResponseCanonicalizesAndRejectsConflicts(t *testing.T) {
	ids := map[string]struct{}{"a": {}, "b": {}}
	raw := `prefix {"groups":[{"builds":[" b ","a"],"root_cause":" cause ","confidence":"HIGH"}],"unclassified_builds":[],"summary":" summary "} suffix`
	parsed, stats, err := parsePatternResponseWithStats(raw, ids)
	if err != nil || stats.ValidCount != 1 || strings.Join(parsed.Groups[0].Builds, ",") != "a,b" || parsed.Groups[0].Confidence != "high" || parsed.Summary != "summary" {
		t.Fatalf("parsed=%+v stats=%+v error=%v", parsed, stats, err)
	}
	conflict := raw + ` {"groups":[],"unclassified_builds":["a","b"],"summary":"different"}`
	if _, _, err := parsePatternResponseWithStats(conflict, ids); patternValidationCategoryOf(err) != patternValidationAmbiguous {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestAnalyzePatternCachesStrictResponse(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	for attempt := 0; attempt < 2; attempt++ {
		pattern, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
		if err != nil || pattern == nil {
			t.Fatalf("attempt %d pattern=%+v error=%v", attempt, pattern, err)
		}
	}
	if atomic.LoadInt32(&srv.calls) != 1 {
		t.Fatalf("calls=%d", srv.calls)
	}
}

func TestAnalyzePatternRepairsCoverageOnceWithForcedFunction(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolRaw(`{"groups":[{"builds":["abuild","bbuild"],"root_cause":"cause","confidence":"high"}],"unclassified_builds":[],"summary":"missing c"}`))
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	var repairs []PatternRepairAttempt
	pattern, err := service.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(3), PatternAnalyzeOptions{
		AllowValidationRepair: true,
		OnRepair:              func(attempt PatternRepairAttempt) { repairs = append(repairs, attempt) },
	})
	if err != nil || pattern == nil || len(repairs) != 1 || !repairs[0].Succeeded || atomic.LoadInt32(&srv.calls) != 2 {
		t.Fatalf("pattern=%+v repairs=%+v calls=%d error=%v", pattern, repairs, srv.calls, err)
	}
	for _, request := range srv.requests {
		if !strings.Contains(string(request), `"name":"submit_causal_groups"`) {
			t.Fatalf("repair was not forced: %s", request)
		}
	}
}

func TestAnalyzePatternRejectsWrongOrMissingForcedFunction(t *testing.T) {
	shrinkCallDelay(t)
	for _, response := range []string{
		chatRespFinal(`{"groups":[],"unclassified_builds":["abuild","bbuild"],"summary":"plain"}`),
		chatRespToolCall("wrong", "other_function", map[string]any{}),
	} {
		srv := newScriptedChatServer(t)
		srv.push(200, response)
		_, err := newPatternTestService(t, srv.URL).AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
		if PatternFailureCategoryOf(err) != PatternFailureMissing {
			t.Fatalf("error=%v category=%s", err, PatternFailureCategoryOf(err))
		}
	}
}

func TestPatternProviderErrorsAreBodySafe(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(503, "PRIVATE_PROVIDER_BODY")
	_, err := newPatternTestService(t, srv.URL).AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
	if PatternFailureCategoryOf(err) != PatternFailureProvider5xx || strings.Contains(err.Error(), "PRIVATE_PROVIDER_BODY") {
		t.Fatalf("error=%v category=%s", err, PatternFailureCategoryOf(err))
	}
}

func TestPatternPromptAndSchemaAreAnalysisOnly(t *testing.T) {
	for _, want := range []string{"causal groups", "unclassified_builds", "Call submit_causal_groups exactly once", "Return no remediation"} {
		if !strings.Contains(patternSystemPrompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	format := patternResponseFormat()
	if format.Name != "submit_causal_groups" {
		t.Fatalf("name=%q", format.Name)
	}
	properties := format.Schema["properties"].(map[string]any)
	if len(properties) != 3 || properties["groups"] == nil || properties["unclassified_builds"] == nil || properties["summary"] == nil {
		t.Fatalf("properties=%v", properties)
	}
	for _, forbidden := range []string{"systemic", "suggested_fix", "remediation_targets", "action"} {
		if strings.Contains(string(mustJSON(format.Schema)), forbidden) {
			t.Fatalf("schema contains %q: %s", forbidden, mustJSON(format.Schema))
		}
	}
}

func TestBuildPatternInputPreservesEvaluatedEvidence(t *testing.T) {
	failures := withPatternRuns(patternFailures(2), patternRunWindowForTest())
	failures[0].ProwJobName = "periodic-capz"
	failures[0].ProwConfigFile = "config/jobs/capz.yaml"
	failures[0].ProwConfigRevision = strings.Repeat("f", 40)
	input := BuildPatternInput("job", failures)
	for _, want := range []string{
		"Recent completed run window: 4 total, 2 failed, 2 passed.",
		"suggested_fix: use a non-burstable control-plane VM",
		"relevant_files: controllers/machine.go",
		"prow_job_name: periodic-capz",
		"test_infra_revision: " + strings.Repeat("f", 40),
	} {
		if !strings.Contains(input.UserPrompt, want) {
			t.Fatalf("prompt missing %q: %s", want, input.UserPrompt)
		}
	}
	if input.SystemPrompt != patternSystemPrompt || len(input.Failures) != 2 {
		t.Fatalf("input=%+v", input)
	}
}

func TestBuildPatternInputCapsBuildsAndCacheKeyTracksContract(t *testing.T) {
	input := BuildPatternInput("job", patternFailures(maxPatternBuilds+2))
	if len(input.Failures) != maxPatternBuilds {
		t.Fatalf("failures=%d", len(input.Failures))
	}
	key := patternCacheKeyForVersions(patternPromptVersion, patternRepairVersion, "module", "", "job", "subject", input.UserPrompt, "causal-groups", "model")
	old := patternCacheKeyForVersions(patternPromptVersion-1, patternRepairVersion, "module", "", "job", "subject", input.UserPrompt, "causal-groups", "model")
	if key == old {
		t.Fatal("prompt version did not change cache key")
	}
}

func TestParsePatternResultPublishesAnalysisOnlyFields(t *testing.T) {
	failures := patternFailures(3)
	pattern, err := ParsePatternResult("job", failures, string(mustJSON(sharedPatternResponse())))
	if err != nil {
		t.Fatal(err)
	}
	if pattern.Recurrence != models.PatternRecurrenceSharedCause || !pattern.Systemic || pattern.SuggestedFix != "" || len(pattern.RemediationTargets) != 0 {
		t.Fatalf("pattern=%+v", pattern)
	}
	if models.PatternAllowsActions(*pattern) {
		t.Fatalf("causal-group pattern allowed actions: %+v", pattern)
	}
	if len(pattern.RelevantFiles) == 0 || pattern.RelevantFiles[0] != "controllers/machine.go" {
		t.Fatalf("relevant files=%v", pattern.RelevantFiles)
	}
}
