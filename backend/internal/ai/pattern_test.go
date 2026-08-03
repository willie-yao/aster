package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
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
			IsTransient:    true,
			Severity:       "Transient-Ignore",
		})
	}
	return out
}

func TestAnalyzePattern_TooFewBuilds_NoCall(t *testing.T) {
	srv := newScriptedChatServer(t)
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(1))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa != nil {
		t.Errorf("expected nil verdict for <2 failures, got %+v", pa)
	}
	if atomic.LoadInt32(&srv.calls) != 0 {
		t.Errorf("expected no model call, got %d", srv.calls)
	}
}

func TestAnalyzePattern_SystemicVerdict(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	verdict := `{"systemic":true,"confidence":"high","shared_root_cause":"undersized burstable control-plane VM starves etcd","shared_builds":["abuild","bbuild","cbuild"],"suggested_fix":"use a non-burstable VM size","remediation_targets":[{"intent":"investigate"}],"summary":"8/10 builds share an etcd-join deadlock"}`
	srv.push(200, chatRespFinal(verdict))
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "the-job", patternFailures(3))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa == nil {
		t.Fatal("expected a verdict")
	}
	if !pa.Systemic {
		t.Error("expected systemic=true")
	}
	if pa.Confidence != "high" {
		t.Errorf("confidence = %q, want high", pa.Confidence)
	}
	if pa.Subject != "the-job" {
		t.Errorf("subject = %q, want the-job", pa.Subject)
	}
	if pa.BuildsAnalyzed != 3 {
		t.Errorf("builds_analyzed = %d, want 3", pa.BuildsAnalyzed)
	}
	if !strings.Contains(pa.SharedRootCause, "etcd") {
		t.Errorf("shared_root_cause = %q", pa.SharedRootCause)
	}
	if len(pa.SharedBuilds) != 3 {
		t.Errorf("shared_builds = %v", pa.SharedBuilds)
	}
}

func TestAnalyzePattern_CacheHit_NoSecondCall(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent flakes"}`))
	s := newPatternTestService(t, srv.URL)
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10})
	if err != nil {
		t.Fatal(err)
	}
	s.SetUsageRecorder(usage, aiusage.OriginFetcher)

	in := patternFailures(3)
	if _, err := s.AnalyzePattern(context.Background(), "job", "job", in); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call with the same failure set must be served from cache.
	cacheHits := 0
	pa, err := s.AnalyzePatternWithOptions(context.Background(), "job", "job", in, PatternAnalyzeOptions{
		AllowAmbiguityRepair: true,
		OnCacheHit:           func() { cacheHits++ },
	})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if pa == nil || pa.Systemic {
		t.Errorf("unexpected verdict: %+v", pa)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("expected 1 model call (second cached), got %d", got)
	}
	if cacheHits != 1 {
		t.Errorf("cache hits = %d, want 1", cacheHits)
	}
	snapshot := usage.Snapshot()
	if len(snapshot.RecentOperations) != 2 || snapshot.Days[0].Totals.CacheHits != 1 {
		t.Fatalf("usage = %+v", snapshot)
	}
}

func TestAnalyzePatternRejectsPartialCacheEntry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"fresh complete verdict"}`))
	s := newPatternTestService(t, srv.URL)
	failures := patternFailures(3)
	input := BuildPatternInput("job", failures)
	key := patternCacheKey(s.module.Name(), s.cacheGeneration, "job", "job", input.UserPrompt, "toolfree", s.client.modelFingerprint())
	if err := s.client.cache.Set(key, map[string]any{
		"systemic": false, "confidence": "low", "summary": "old partial verdict",
	}); err != nil {
		t.Fatal(err)
	}

	pa, err := s.AnalyzePattern(t.Context(), "job", "job", failures)
	if err != nil {
		t.Fatal(err)
	}
	if pa == nil || pa.Summary != "fresh complete verdict" {
		t.Fatalf("pattern = %+v", pa)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
}

func TestAnalyzePattern_InvalidConfidenceRejected(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"VERY-SURE","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent flakes"}`))
	s := newPatternTestService(t, srv.URL)

	if _, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(2)); patternValidationCategoryOf(err) != patternValidationSchema {
		t.Fatalf("AnalyzePattern error = %v", err)
	}
}

func TestAnalyzePattern_IncompleteVerdictRejected(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Empty object has no summary, so it is rejected and not cached.
	srv.push(200, chatRespFinal(`{}`))
	// Systemic verdict with no root cause is rejected.
	srv.push(200, chatRespFinal(`{"systemic":true,"confidence":"high","summary":"x"}`))
	s := newPatternTestService(t, srv.URL)

	if _, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(2)); err == nil {
		t.Error("expected error for empty verdict")
	}
	if _, err := s.AnalyzePattern(context.Background(), "job", "job2", patternFailures(2)); err == nil {
		t.Error("expected error for systemic verdict without a root cause")
	}
}

func TestPatternCacheKey_TracksModelInput(t *testing.T) {
	base := patternFailures(3)
	p1 := buildPatternUserPrompt("job", base)
	k1 := patternCacheKey("kubernetes", "", "job", "job", p1, "toolfree", "model-fingerprint")

	// A changed root cause changes the rendered prompt, so the key changes.
	changed := patternFailures(3)
	changed[0].RootCause = "different cause"
	k2 := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", changed), "toolfree", "model-fingerprint")
	if k1 == k2 {
		t.Error("expected cache key to change when the evidence changes")
	}

	// A changed failure message also changes the key because evidence differs.
	msgChanged := patternFailures(3)
	msgChanged[0].FailureMessage = "a totally different symptom"
	k3 := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", msgChanged), "toolfree", "model-fingerprint")
	if k1 == k3 {
		t.Error("expected cache key to change when a failure message changes")
	}

	// Same inputs produce a stable key.
	if patternCacheKey("kubernetes", "", "job", "job", p1, "toolfree", "model-fingerprint") != k1 {
		t.Error("expected stable cache key for identical inputs")
	}
}

func TestPatternCacheKey_ChangesWhenAdditionalAnalysesBecomeAvailable(t *testing.T) {
	degraded := patternFailures(2)
	complete := patternFailures(3)
	degradedKey := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", degraded), "toolfree", "model-fingerprint")
	completeKey := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", complete), "toolfree", "model-fingerprint")
	if degradedKey == completeKey {
		t.Fatal("additional per-failure analysis did not change the pattern cache key")
	}
}

func TestCollectRelevantFiles_LeadsWithLocation(t *testing.T) {
	failures := []PatternFailure{
		{LocationFile: "test/e2e/foo_test.go", RelevantFiles: []string{"config/a.yaml", "test/e2e/foo_test.go"}},
		{LocationFile: "test/e2e/foo_test.go", RelevantFiles: []string{"config/b.yaml"}},
	}
	got := collectRelevantFiles(failures)
	want := []string{"test/e2e/foo_test.go", "config/a.yaml", "config/b.yaml"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
}

// TestCollectRelevantFiles_NotInCacheKey guards the invariant that the failing-
// test location seeds the fix harness without perturbing the pattern cache key:
// LocationFile must not be rendered into the correlation prompt.
func TestCollectRelevantFiles_NotInCacheKey(t *testing.T) {
	base := patternFailures(2)
	withLoc := patternFailures(2)
	withLoc[0].LocationFile = "test/e2e/foo_test.go"

	k1 := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", base), "toolfree", "model-fingerprint")
	k2 := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", withLoc), "toolfree", "model-fingerprint")
	if k1 != k2 {
		t.Error("LocationFile must not change the pattern cache key (kept out of the prompt)")
	}
	// But it must still surface in the pattern's relevant files.
	if got := collectRelevantFiles(withLoc); len(got) == 0 || got[0] != "test/e2e/foo_test.go" {
		t.Errorf("LocationFile should lead collectRelevantFiles, got %v", got)
	}
}

func TestBuildPatternUserPrompt_RendersFixAndFiles(t *testing.T) {
	failures := patternFailures(2)
	failures[0].SuggestedFix = "serialize nodepool scaling operations"
	failures[0].RelevantFiles = []string{"exp/controllers/azuremachinepool_controller.go", "test/e2e/aks_byo_node.go"}

	prompt := buildPatternUserPrompt("job", failures)
	for _, want := range []string{
		"suggested_fix: serialize nodepool scaling operations",
		"relevant_files: exp/controllers/azuremachinepool_controller.go, test/e2e/aks_byo_node.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}

	// A changed per-build suggested fix is new evidence, so the cache key moves.
	base := patternCacheKey("kubernetes", "", "job", "job", buildPatternUserPrompt("job", patternFailures(2)), "toolfree", "model-fingerprint")
	withFix := patternCacheKey("kubernetes", "", "job", "job", prompt, "toolfree", "model-fingerprint")
	if base == withFix {
		t.Error("expected cache key to change when a per-build suggested_fix is added")
	}
}

func TestAnalyzePattern_CapsBuilds(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":true,"confidence":"high","shared_root_cause":"x","shared_builds":["obuild","nbuild"],"suggested_fix":"fix x","remediation_targets":[{"intent":"investigate"}],"summary":"x"}`))
	s := newPatternTestService(t, srv.URL)

	pa, err := s.AnalyzePattern(context.Background(), "job", "job", patternFailures(maxPatternBuilds+5))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa.BuildsAnalyzed != maxPatternBuilds {
		t.Errorf("builds_analyzed = %d, want capped at %d", pa.BuildsAnalyzed, maxPatternBuilds)
	}
	// The prompt the model received must reflect the cap, not the full set.
	var reqs int
	srv.mu.Lock()
	reqs = len(srv.requests)
	srv.mu.Unlock()
	if reqs != 1 {
		t.Fatalf("expected one request captured, got %d", reqs)
	}
	var sent chatCompletionsRequest
	if err := json.Unmarshal(srv.requests[0], &sent); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	user := *sent.Messages[len(sent.Messages)-1].Content
	if strings.Count(user, "--- Build ") != maxPatternBuilds {
		t.Errorf("prompt build count = %d, want %d", strings.Count(user, "--- Build "), maxPatternBuilds)
	}
}

func TestParsePatternResultMarksPathsUnverified(t *testing.T) {
	result := `{
		"systemic": true,
		"confidence": "high",
		"shared_root_cause": "the controller writes stale state",
		"shared_builds": ["abuild", "bbuild"],
		"suggested_fix": "serialize updates in config/controller.yaml",
		"remediation_targets": [{"intent":"investigate"}],
		"summary": "the same controller path failed twice"
	}`
	pa, err := ParsePatternResult("job", patternFailures(2), result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pa.SuggestedFix, "config/controller.yaml (unverified path)") {
		t.Fatalf("suggested fix = %q, want unverified path annotation", pa.SuggestedFix)
	}
}

func TestParsePatternResponseCandidates(t *testing.T) {
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"the builds share one cause"}`
	valid2 := `{"systemic":true,"confidence":"medium","shared_root_cause":"second cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/second.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"a different valid verdict"}`
	missing := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"summary":"missing suggested fix"}`
	invalidBuild := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","unknown-build"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"bad build reference"}`
	nullFields := `{"systemic":null,"confidence":"low","shared_root_cause":null,"shared_builds":[],"suggested_fix":null,"summary":"null fields"}`
	partialFinal := `{"systemic":false,"confidence":"low"}`
	contractWrapper := `{"systemic":` + valid + `,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","summary":"outer partial verdict"}`
	duplicateField := `{"systemic":true,"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[{"intent":"investigate"}],"summary":"duplicate verdict"}`
	cases := []struct {
		name         string
		raw          string
		wantSummary  string
		wantCategory patternValidationCategory
	}{
		{name: "plain valid JSON", raw: valid, wantSummary: "the builds share one cause"},
		{name: "fenced valid JSON", raw: "```json\n" + valid + "\n```", wantSummary: "the builds share one cause"},
		{name: "metadata wrapper", raw: `{"metadata":{"finish_reason":"stop"},"result":` + valid + `}`, wantSummary: "the builds share one cause"},
		{name: "contract-like wrapper", raw: contractWrapper, wantCategory: patternValidationSchema},
		{name: "duplicate contract field", raw: duplicateField, wantCategory: patternValidationSchema},
		{name: "one contract candidate", raw: missing + "\n" + valid, wantSummary: "the builds share one cause"},
		{name: "ambiguous valid candidates", raw: valid + "\n" + valid2, wantCategory: patternValidationAmbiguous},
		{name: "malformed followed by valid", raw: `{"systemic": tru` + "\n" + valid, wantSummary: "the builds share one cause"},
		{name: "valid followed by unrelated prose", raw: valid + "\nThis paragraph is unrelated.", wantSummary: "the builds share one cause"},
		{name: "observed trailing W shape", raw: valid + "\nWhat this means is that the failures recur.", wantSummary: "the builds share one cause"},
		{name: "valid followed by metadata object", raw: valid + `\n{"metadata":{"finish_reason":"stop"}}`, wantSummary: "the builds share one cause"},
		{name: "valid followed by truncated candidate", raw: valid + `\n{"systemic":`, wantCategory: patternValidationJSON},
		{name: "valid followed by complete partial candidate", raw: valid + "\n" + partialFinal, wantCategory: patternValidationSchema},
		{name: "missing required field", raw: missing, wantCategory: patternValidationSchema},
		{name: "null required fields", raw: nullFields, wantCategory: patternValidationSchema},
		{name: "invalid affected build", raw: invalidBuild, wantCategory: patternValidationBuilds},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, err := parsePatternResponse(testCase.raw, patternBuildIDs(patternFailures(3)))
			if testCase.wantCategory != "" {
				if got := patternValidationCategoryOf(err); got != testCase.wantCategory {
					t.Fatalf("category = %q, want %q, error=%v", got, testCase.wantCategory, err)
				}
				if err != nil && (strings.Contains(err.Error(), testCase.raw) || strings.Contains(err.Error(), "unknown-build")) {
					t.Fatalf("error exposed provider content: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Summary != testCase.wantSummary {
				t.Fatalf("summary = %q, want %q", parsed.Summary, testCase.wantSummary)
			}
		})
	}
}

func TestParsePatternResponseValidatesRemediationTargets(t *testing.T) {
	base := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix the target","remediation_targets":TARGETS,"summary":"shared failure"}`
	tests := []struct {
		name    string
		targets string
		wantErr bool
	}{
		{name: "modify symbol", targets: `[{"intent":"modify_symbol","symbol":"MachinePoolModelHasChanged","path":"controllers/helpers.go"}]`},
		{name: "configuration", targets: `[{"intent":"set_configuration","path":"templates/dra.yaml","value":"GenericWorkload=true"}]`},
		{name: "unknown field", targets: `[{"intent":"investigate","verified":true}]`, wantErr: true},
		{name: "duplicate field", targets: `[{"intent":"investigate","intent":"add_symbol"}]`, wantErr: true},
		{name: "unsafe path", targets: `[{"intent":"modify_symbol","symbol":"Fix","path":"../helpers.go"}]`, wantErr: true},
		{name: "invalid configuration value", targets: `[{"intent":"set_configuration","path":"templates/dra.yaml","value":"GenericWorkload=true\nOther=true"}]`, wantErr: true},
		{name: "configuration missing assignment", targets: `[{"intent":"set_configuration","path":"templates/dra.yaml","value":"GenericWorkload"}]`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parsePatternResponse(strings.Replace(base, "TARGETS", test.targets, 1), patternBuildIDs(patternFailures(2)))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %t", err, test.wantErr)
			}
		})
	}
}

func TestGroundedPatternVerdictDoesNotRecoverRejectedJSON(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "list_repo_tree", map[string]interface{}{"path": ""}))
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"the builds share one cause"}`
	srv.push(200, chatRespFinal(valid+`\n{"systemic":`))
	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
	s.SetSourceRepo("example", "repo")
	s.SetPatternRepoReader(&fakeRepoReader{files: map[string]string{"config/controller.yaml": "enabled: true"}})
	if _, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2)); patternValidationCategoryOf(err) != patternValidationJSON {
		t.Fatalf("AnalyzePattern error = %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want 2 without extraction recovery", got)
	}
}

func TestParsePatternResponseNormalizesBuildIDs(t *testing.T) {
	raw := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":[" abuild ","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"the builds share one cause"}`
	parsed, err := parsePatternResponse(raw, patternBuildIDs(patternFailures(2)))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.SharedBuilds) != 2 || parsed.SharedBuilds[0] != "abuild" || parsed.SharedBuilds[1] != "bbuild" {
		t.Fatalf("shared builds = %q", parsed.SharedBuilds)
	}
}

func TestParsePatternResponseDeduplicatesEquivalentCandidates(t *testing.T) {
	first := `{"systemic":true,"confidence":" HIGH ","shared_root_cause":" shared cause ","shared_builds":["bbuild","abuild","bbuild"],"suggested_fix":" fix it ","remediation_targets":[{"intent":"investigate"}],"summary":" same summary "}`
	second := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix it","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	parsed, stats, err := parsePatternResponseWithStats(first+"\n"+second, patternBuildIDs(patternFailures(2)))
	if err != nil {
		t.Fatal(err)
	}
	if stats.CandidateCount != 4 || stats.ValidCount != 2 || stats.UniqueValidCount != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if parsed.Confidence != "high" || strings.Join(parsed.SharedBuilds, ",") != "abuild,bbuild" || parsed.SharedRootCause != "shared cause" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParsePatternResponseRejectsInvalidContractBetweenEquivalentCandidates(t *testing.T) {
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix it","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	raw := valid + "\n" + `{"systemic":true}` + "\n" + valid
	_, _, err := parsePatternResponseWithStats(raw, patternBuildIDs(patternFailures(2)))
	if got := patternValidationCategoryOf(err); got != patternValidationSchema {
		t.Fatalf("category = %q, error = %v", got, err)
	}
}

func TestParsePatternResponseRejectsDistinctCanonicalCandidates(t *testing.T) {
	first := `{"systemic":true,"confidence":"high","shared_root_cause":"first cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix it","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	second := strings.Replace(first, "first cause", "second cause", 1)
	_, stats, err := parsePatternResponseWithStats(first+"\n"+second, patternBuildIDs(patternFailures(2)))
	if got := patternValidationCategoryOf(err); got != patternValidationAmbiguous {
		t.Fatalf("category = %q, error = %v", got, err)
	}
	if stats.ValidCount != 2 || stats.UniqueValidCount != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAnalyzePatternRepairsAmbiguityOnce(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"private-shared-cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	ambiguous := valid + "\n" + strings.Replace(valid, "private-shared-cause", "private-other-cause", 1)
	srv.push(200, chatRespFinal(ambiguous))
	srv.push(200, chatRespFinal(valid))
	s := newPatternTestService(t, srv.URL)
	traceStore := NewTraceStore()
	s.SetTraceStore(traceStore)
	var repairs []PatternRepairAttempt
	pa, err := s.AnalyzePatternWithOptions(t.Context(), "job", "job", patternFailures(2), PatternAnalyzeOptions{
		AllowAmbiguityRepair: true,
		OnRepair:             func(attempt PatternRepairAttempt) { repairs = append(repairs, attempt) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if pa == nil || pa.SharedRootCause != "private-shared-cause" || len(repairs) != 1 || !repairs[0].Succeeded {
		t.Fatalf("pattern=%+v repairs=%+v", pa, repairs)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
	snapshot := traceStore.Snapshot()
	rawTrace, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawTrace), "private-shared-cause") || strings.Contains(string(rawTrace), "private-other-cause") {
		t.Fatalf("trace exposed candidate content: %s", rawTrace)
	}
	var sawAmbiguous, sawRepair bool
	for _, trace := range snapshot.Traces {
		for _, event := range trace.Events {
			if event.Kind == "pattern_parse" && event.Status == "tool_free" && event.ValidCount == 2 && event.UniqueCandidateCount == 2 {
				sawAmbiguous = true
			}
			if event.Kind == "pattern_repair" && event.Outcome == "success" {
				sawRepair = true
			}
		}
	}
	if !sawAmbiguous || !sawRepair {
		t.Fatalf("trace missing structural telemetry: %+v", snapshot)
	}
}

func TestAnalyzePatternRepairRejectsAmbiguousAndTransportFailures(t *testing.T) {
	shrinkCallDelay(t)
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	ambiguous := valid + "\n" + strings.Replace(valid, "shared cause", "other cause", 1)
	tests := []struct {
		name         string
		repairStatus int
		repairBody   string
		want         PatternFailureCategory
	}{
		{name: "ambiguous repair", repairStatus: 200, repairBody: chatRespFinal(ambiguous), want: PatternFailureAmbiguous},
		{name: "transport repair", repairStatus: 408, repairBody: "private repair timeout", want: PatternFailureRequestTimeout},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			srv := newScriptedChatServer(t)
			srv.push(200, chatRespFinal(ambiguous))
			srv.push(testCase.repairStatus, testCase.repairBody)
			s := newPatternTestService(t, srv.URL)
			_, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
			if category := PatternFailureCategoryOf(err); category != testCase.want {
				t.Fatalf("category=%q want=%q error=%v", category, testCase.want, err)
			}
			if err == nil || strings.Contains(err.Error(), "private repair timeout") {
				t.Fatalf("unsafe error=%v", err)
			}
			if got := atomic.LoadInt32(&srv.calls); got != 2 {
				t.Fatalf("model calls = %d, want 2", got)
			}
		})
	}
}

func TestAnalyzePatternRepairFailureDoesNotStartAnotherRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	valid := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"same summary"}`
	ambiguous := valid + "\n" + strings.Replace(valid, "shared cause", "other cause", 1)
	srv.push(200, chatRespFinal(ambiguous))
	srv.push(200, chatRespFinal(`{"systemic":true}`))
	s := newPatternTestService(t, srv.URL)
	_, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
	if category := PatternFailureCategoryOf(err); category != PatternFailureSchema {
		t.Fatalf("category=%q error=%v", category, err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
}

func TestParsePatternResponseRejectsTruncatedCandidateWindow(t *testing.T) {
	first := `{"systemic":true,"confidence":"high","shared_root_cause":"first cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix first","remediation_targets":[{"intent":"investigate"}],"summary":"first verdict"}`
	second := `{"systemic":true,"confidence":"medium","shared_root_cause":"second cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix second","remediation_targets":[{"intent":"investigate"}],"summary":"second verdict"}`
	var raw strings.Builder
	raw.WriteString(first)
	for index := 0; index < analysisChatMaxCandidates; index++ {
		fmt.Fprintf(&raw, `\n{"metadata":%d}`, index)
	}
	raw.WriteString("\n" + second)
	_, err := parsePatternResponse(raw.String(), patternBuildIDs(patternFailures(2)))
	if got := patternValidationCategoryOf(err); got != patternValidationAmbiguous {
		t.Fatalf("category = %q, error = %v", got, err)
	}
}

func TestParsePatternOutputDoesNotRepairTruncatedCandidateWindow(t *testing.T) {
	shrinkCallDelay(t)
	first := `{"systemic":true,"confidence":"high","shared_root_cause":"first cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix first","remediation_targets":[{"intent":"investigate"}],"summary":"first verdict"}`
	second := `{"systemic":true,"confidence":"medium","shared_root_cause":"second cause","shared_builds":["abuild","bbuild"],"suggested_fix":"fix second","remediation_targets":[{"intent":"investigate"}],"summary":"second verdict"}`
	var raw strings.Builder
	raw.WriteString(first)
	for index := 0; index < analysisChatMaxCandidates; index++ {
		fmt.Fprintf(&raw, `\n{"metadata":%d}`, index)
	}
	raw.WriteString("\n" + second)

	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(first))
	s := newPatternTestService(t, srv.URL)
	repairAttempted := false
	_, err := s.parsePatternOutput(t.Context(), "grounded", raw.String(), patternBuildIDs(patternFailures(2)), PatternAnalyzeOptions{
		AllowAmbiguityRepair: true,
		OnRepair:             func(PatternRepairAttempt) { repairAttempted = true },
	})
	if got := patternValidationCategoryOf(err); got != patternValidationAmbiguous {
		t.Fatalf("category = %q, error = %v", got, err)
	}
	if repairAttempted || atomic.LoadInt32(&srv.calls) != 0 {
		t.Fatalf("truncated candidate window attempted repair: observed=%t calls=%d", repairAttempted, srv.calls)
	}
}

func TestAnalyzePatternAcceptsKimiTrailingProse(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	verdict := `{"systemic":true,"confidence":"high","shared_root_cause":"shared cause","shared_builds":["abuild","bbuild"],"suggested_fix":"update config/controller.yaml","remediation_targets":[{"intent":"investigate"}],"summary":"the builds share one cause"}`
	srv.push(200, chatRespFinal(verdict+"\nWhat this means is recurring configuration drift."))
	s := newPatternTestService(t, srv.URL)
	pa, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
	if err != nil {
		t.Fatal(err)
	}
	if pa == nil || pa.Summary != "the builds share one cause" {
		t.Fatalf("pattern = %+v", pa)
	}
}

func TestAnalyzePatternProviderErrorsAreBodySafe(t *testing.T) {
	shrinkCallDelay(t)
	tests := []struct {
		name         string
		status       int
		body         string
		wantCategory PatternFailureCategory
	}{
		{name: "request timeout", status: 408, body: "private timeout response", wantCategory: PatternFailureRequestTimeout},
		{name: "server failure", status: 503, body: "private server response", wantCategory: PatternFailureProvider5xx},
		{name: "nonretryable failure", status: 400, body: "private request response", wantCategory: PatternFailureProvider},
		{name: "malformed success", status: 200, body: "private malformed response", wantCategory: PatternFailureProvider},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			srv := newScriptedChatServer(t)
			srv.push(testCase.status, testCase.body)
			s := newPatternTestService(t, srv.URL)
			_, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
			if category := PatternFailureCategoryOf(err); category != testCase.wantCategory {
				t.Fatalf("category = %q, want %q, error=%v", category, testCase.wantCategory, err)
			}
			if err == nil || strings.Contains(err.Error(), testCase.body) {
				t.Fatalf("error exposed provider body: %v", err)
			}
		})
	}
}

func TestPatternRetryClassification(t *testing.T) {
	for _, err := range []error{
		&PatternProviderError{StatusCode: 408},
		&PatternProviderError{StatusCode: 429},
		&PatternProviderError{StatusCode: 500},
	} {
		if !IsRetryablePatternError(err) {
			t.Fatalf("error was not retryable: %v", err)
		}
	}
	for _, err := range []error{
		&patternValidationError{category: patternValidationAmbiguous},
		&patternValidationError{category: patternValidationSchema},
		&patternValidationError{category: patternValidationBuilds},
		&PatternProviderError{StatusCode: 400},
		context.Canceled,
	} {
		if IsRetryablePatternError(err) {
			t.Fatalf("error was retryable: %v", err)
		}
	}
}

func TestPatternLocalFailureClassification(t *testing.T) {
	for err, want := range map[error]PatternFailureCategory{
		ErrContextHeadroom:  PatternFailureContextHeadroom,
		ErrToolsUnsupported: PatternFailureToolsUnsupported,
	} {
		if got := PatternFailureCategoryOf(safePatternProviderError(err)); got != want {
			t.Fatalf("category=%q want=%q", got, want)
		}
		if IsRetryablePatternError(err) {
			t.Fatalf("local error was retryable: %v", err)
		}
	}
}

func TestGroundedPatternVerdictPropagatesFinalizeHTTPError(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	for index := 0; index < patternMaxIters; index++ {
		srv.push(200, chatRespToolCall(fmt.Sprintf("call_%d", index), "list_repo_tree", map[string]interface{}{"path": ""}))
	}
	srv.push(408, "private finalize response")
	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
	s.SetSourceRepo("example", "repo")
	s.SetPatternRepoReader(&fakeRepoReader{files: map[string]string{"config/controller.yaml": "enabled: true"}})

	_, err := s.AnalyzePattern(t.Context(), "job", "job", patternFailures(2))
	if category := PatternFailureCategoryOf(err); category != PatternFailureRequestTimeout {
		t.Fatalf("category = %q, error = %v", category, err)
	}
	if strings.Contains(err.Error(), "private finalize response") {
		t.Fatalf("error exposed provider body: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != int32(patternMaxIters+1) {
		t.Fatalf("model calls = %d, want %d", got, patternMaxIters+1)
	}
}

func TestPatternCacheKeyGeneration(t *testing.T) {
	base := patternCacheKey("universal", "", "job", "subject", "prompt", "toolfree", "model")
	generated := patternCacheKey("universal", "0123456789abcdef", "job", "subject", "prompt", "toolfree", "model")
	if generated == base || !strings.HasPrefix(generated, "pattern:universal:g:0123456789abcdef:") {
		t.Fatalf("generated pattern key = %q, base = %q", generated, base)
	}
	if got := patternCacheKey("universal", "", "job", "subject", "prompt", "toolfree", "model"); got != base {
		t.Fatalf("empty generation changed pattern key: %q vs %q", got, base)
	}
}
