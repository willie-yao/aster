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
	return NewService(ServiceConfig{Client: client, Module: &stubModule{name: "kubernetes"}, SystemPrompt: "sys", ConsecutiveFailures: nil})
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
			pattern := buildPatternAnalysis("job", 2, canonicalizePatternResponse(testCase.response), nil, "2026-01-01T00:00:00Z")
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

// TestAnalyzePatternRepublishesCachedGenerationTimestamp pins that a cache hit
// carries the timestamp the verdict was generated with. Re-stamping it every
// pass changes the published pattern while its content is identical, which
// rejects cause-scoped conversations bound to it.
func TestAnalyzePatternRepublishesCachedGenerationTimestamp(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	service.patternNow = func() time.Time { return now }
	first, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if err != nil {
		t.Fatal(err)
	}
	if first.GeneratedAt != now.Format(time.RFC3339) {
		t.Fatalf("generated_at = %q, want %q", first.GeneratedAt, now.Format(time.RFC3339))
	}
	now = now.Add(30 * time.Minute)
	second, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if err != nil {
		t.Fatal(err)
	}
	if second.GeneratedAt != first.GeneratedAt || models.PatternHash(*second) != models.PatternHash(*first) {
		t.Fatalf("republished generated_at = %q, want %q", second.GeneratedAt, first.GeneratedAt)
	}
}

// TestAnalyzePatternDatesUntimestampedCacheEntryFromItsAge covers entries written
// before the verdict timestamp was recorded: they must resolve to when they were
// cached, so republishing neither moves the timestamp nor renews the entry.
func TestAnalyzePatternDatesUntimestampedCacheEntryFromItsAge(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	service.patternNow = func() time.Time { return now }
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatal(err)
	}
	// Rewrite the single cached verdict in the pre-timestamp shape.
	cached := service.client.cache.EntriesWithPrefix("pattern:")
	if len(cached) != 1 {
		t.Fatalf("cached entries = %d, want 1", len(cached))
	}
	var key string
	var data patternCacheData
	for entryKey, entry := range cached {
		key = entryKey
		if err := json.Unmarshal(entry.Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	// Cache validity is wall-clock based, so the entry's age must stay relative.
	entryCreated := time.Now().UTC().Add(-13 * 24 * time.Hour).Truncate(time.Second)
	if err := service.client.cache.StoreEntry(CacheEntry{
		Key: key, CreatedAt: entryCreated,
		Data: mustJSON(patternCacheData{Version: patternCacheVersion, Response: data.Response}),
	}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		now = now.Add(30 * time.Minute)
		pattern, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
		if err != nil {
			t.Fatal(err)
		}
		if pattern.GeneratedAt != entryCreated.Format(time.RFC3339) {
			t.Fatalf("attempt %d generated_at = %q, want %q", attempt, pattern.GeneratedAt, entryCreated.Format(time.RFC3339))
		}
	}
	if entry, ok := service.client.cache.Lookup(key); !ok || !entry.CreatedAt.Equal(entryCreated) {
		t.Fatalf("cache entry age was renewed: %v", entry.CreatedAt)
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

// TestGroupRemediationCarriesMostSevereMember verifies a causal group reports
// the remediation its own member analyses produced, taken from the most severe
// member so the action matches the failure the cause is built from.
func TestGroupRemediationCarriesMostSevereMember(t *testing.T) {
	byBuild := map[string]PatternFailure{
		"1": {BuildID: "1", Severity: "Low", SuggestedFix: "  Widen the readiness probe.  "},
		"2": {BuildID: "2", Severity: "Critical", SuggestedFix: "Add retry-on-conflict for 409."},
		"3": {BuildID: "3", Severity: "High", SuggestedFix: "Raise the join budget."},
	}

	got := groupRemediation([]string{"1", "2", "3"}, byBuild)
	if got == nil {
		t.Fatal("a group whose members proposed fixes reported none")
	}
	if got.BuildID != "2" {
		t.Errorf("BuildID = %q, want the most severe member %q", got.BuildID, "2")
	}
	// The carried text is one member's verbatim fix, trimmed but never merged.
	if got.SuggestedFix != "Add retry-on-conflict for 409." {
		t.Errorf("SuggestedFix = %q, want the most severe member's fix", got.SuggestedFix)
	}
}

// TestGroupRemediationSkipsMembersWithoutAFix verifies a member that proposed
// nothing cannot win the selection, and that a group with no proposal anywhere
// stays unreported rather than inventing one.
func TestGroupRemediationSkipsMembersWithoutAFix(t *testing.T) {
	byBuild := map[string]PatternFailure{
		"1": {BuildID: "1", Severity: "Critical", SuggestedFix: "   "},
		"2": {BuildID: "2", Severity: "Low", SuggestedFix: "Widen the readiness probe."},
	}
	got := groupRemediation([]string{"1", "2"}, byBuild)
	if got == nil || got.BuildID != "2" {
		t.Fatalf("a blank fix on the most severe member suppressed the group remediation: %+v", got)
	}

	empty := map[string]PatternFailure{"1": {BuildID: "1", Severity: "High"}}
	if got := groupRemediation([]string{"1"}, empty); got != nil {
		t.Errorf("a group with no proposed fix reported %+v", got)
	}
	// A build the correlation named but that carries no analysis contributes
	// nothing, the same way ownership merging treats a missing member.
	if got := groupRemediation([]string{"absent"}, byBuild); got != nil {
		t.Errorf("an unanalyzed build produced a remediation: %+v", got)
	}
	if got := groupRemediation(nil, byBuild); got != nil {
		t.Errorf("an empty group produced a remediation: %+v", got)
	}
}

// TestGroupRemediationIsExcludedFromCausalGroupIdentity verifies refreshing the
// displayed suggestion never churns causal-group identity, which would
// invalidate a cause chat already running against the same published cause.
func TestGroupRemediationIsExcludedFromCausalGroupIdentity(t *testing.T) {
	base := models.PatternCausalGroup{
		Builds: []string{"1", "2"}, RootCause: "etcd learner never joined", Confidence: "high",
	}
	withFix := base
	withFix.Remediation = &models.PatternCausalGroupRemediation{SuggestedFix: "Raise the budget.", BuildID: "1"}

	if models.PatternCausalGroupHash(base) != models.PatternCausalGroupHash(withFix) {
		t.Error("the reported remediation changed the causal-group content hash")
	}
	if models.PatternCausalGroupID("p", base) != models.PatternCausalGroupID("p", withFix) {
		t.Error("the reported remediation changed the causal-group ID")
	}
}
