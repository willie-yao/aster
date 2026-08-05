package ai

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestSemanticCritique_ParsesObjections(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["root cause is a teardown symptom, not the trigger","did not verify the PR is at fault"]}`))
	client := newAgenticTestClient(t, srv.URL)

	objs, err := client.semanticCritique(context.Background(),
		analysisResponse{RootCause: "credential expiry", SuggestedFix: "re-run"},
		[]string{"build-log.txt"}, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}))
	if err != nil {
		t.Fatalf("semanticCritique: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("objections = %v, want 2", objs)
	}
	if !strings.Contains(objs[0], "teardown") {
		t.Errorf("first objection = %q", objs[0])
	}
}

func TestSemanticCritique_EmptyMeansSound(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	objs, err := client.semanticCritique(context.Background(), analysisResponse{RootCause: "x"}, nil, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}))
	if err != nil {
		t.Fatalf("semanticCritique: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("expected no objections, got %v", objs)
	}
}

// TestAgentic_SemanticJudge_ObjectsThenReprompts verifies the judge, when
// enabled, reviews an accepted draft, and its objections drive one re-prompt
// that the model answers with a corrected final.
func TestAgentic_SemanticJudge_ObjectsThenReprompts(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: a draft that passes the deterministic gate (concrete fix, no
	// unread citations) but is semantically shallow.
	shallow := `{"summary":"flake","is_transient":false,"root_cause":"the PR broke it","severity":"High","suggested_fix":"revert kustomize/cluster-template.yaml line 5","relevant_files":[]}`
	srv.push(200, chatRespFinal(shallow))
	srv.push(200, chatRespFinal(`{"objections":["root_cause blames the PR without evidence; check the cluster network config"]}`))
	// Round 2: after the objection, a corrected final.
	corrected := `{"summary":"deep","is_transient":false,"root_cause":"control-plane subnet route table missing","severity":"High","suggested_fix":"set the control-plane subnet route table in kustomize/cluster-template.yaml line 5","relevant_files":[]}`
	srv.push(200, chatRespFinal(corrected))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
		SemanticJudge:      true,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:semantic", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected corrected final after judge objection, got summary=%q", summary.Summary)
	}
	if !strings.Contains(analysis.RootCause, "route table") {
		t.Errorf("expected corrected root cause, got %q", analysis.RootCause)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (draft + judge + corrected)", got)
	}
}

func TestAgentic_SemanticJudgeErrorKeepsPassingRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(providerIDPuntFinalJSON))
	passing := `{"summary":"providerID blocked","is_transient":false,"root_cause":"The worker Node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API.","severity":"High","suggested_fix":"Restart cloud-node-manager with the correct Kubernetes API endpoint.","relevant_files":[]}`
	srv.push(200, chatRespFinal(passing))
	srv.push(200, chatRespFinal("not json"))

	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:semantic-error-keeps-passing-repair"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 1, SemanticJudge: true,
		}), key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.CritiquePassed || !strings.Contains(analysis.SuggestedFix, "Restart cloud-node-manager") {
		t.Fatalf("semantic judge error discarded passing repair: %+v", analysis)
	}
	if _, ok := client.Cache().Get(key); !ok {
		t.Fatal("selected passing repair was not cached")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3 (draft + repair + semantic judge)", got)
	}
}

func TestAgentic_UnparseableSemanticRepairKeepsSelectedDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	initial := `{"summary":"sound fallback","is_transient":false,"root_cause":"control-plane subnet route table missing","severity":"High","suggested_fix":"Set the control-plane subnet route table.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespFinal(`{"objections":["verify the diagnosis"]}`))
	srv.push(200, chatRespFinal("not json"))

	client := newAgenticTestClient(t, srv.URL)
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 1, SemanticJudge: true,
		}), "agentic:test:semantic-unparseable-fallback", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RootCause != "control-plane subnet route table missing" || analysis.SuggestedFix == "Unable to parse structured response" {
		t.Fatalf("semantic parse failure discarded selected draft: %+v", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
}

func TestAgentic_ForcedFinalizeSemanticRepairCanBeSelected(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	initial := `{"summary":"initial","is_transient":false,"root_cause":"the PR broke it","severity":"High","suggested_fix":"Revert the PR.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespFinal(`{"objections":["check the cluster network config"]}`))
	revised := `{"summary":"revised","is_transient":false,"root_cause":"control-plane subnet route table missing","severity":"High","suggested_fix":"Set the control-plane subnet route table.","relevant_files":[]}`
	srv.push(200, chatRespFinal(revised))

	client := newAgenticTestClient(t, srv.URL)
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 1, SemanticJudge: true,
		}), "agentic:test:semantic-forced-finalize", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RootCause != "control-plane subnet route table missing" || !analysis.JudgeRevised {
		t.Fatalf("forced-finalize semantic repair not selected: %+v", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
}

// TestApplySemanticJudgePostLoop_RefinalizesOnObjection verifies the post-loop
// judge: on objections it refinalizes, and accepts the revised draft only when
// it still clears the deterministic critique.
func TestApplySemanticJudgePostLoop_RefinalizesOnObjection(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["blames the PR without checking the network config"]}`))
	srv.push(200, chatRespFinal(`{"summary":"deep","is_transient":false,"root_cause":"control-plane subnet route table missing","severity":"High","suggested_fix":"set the route table in kustomize/cluster-template.yaml line 5","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	state := &agentState{readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{}}
	orig := analysisResponse{Summary: "shallow", RootCause: "the PR broke it", SuggestedFix: "revert it"}
	got := client.applySemanticJudgePostLoop(context.Background(), state, []modelMessage{{Role: "user", Content: strPtr("u")}}, "shallow-final", nil, orig, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}), CritiqueCachePolicyStrict)

	if !strings.Contains(got.RootCause, "route table") {
		t.Errorf("expected the refinalized draft, got root_cause=%q", got.RootCause)
	}
	if !state.judgeRan || !state.judgeObjected || !state.judgeRevised {
		t.Errorf("telemetry = ran:%v objected:%v revised:%v, want all true", state.judgeRan, state.judgeObjected, state.judgeRevised)
	}
}

func TestApplySemanticJudgePostLoopRejectsNonSanitizableRevision(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["verify whether the failure is transient"]}`))
	srv.push(200, chatRespFinal(`{"summary":"revised","is_transient":true,"root_cause":"temporary provider failure","severity":"High","suggested_fix":"Set the route table.","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	state := &agentState{
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
		analysisEvidence: map[string]*analysisChatEvidence{}, consecutiveFailures: 3,
	}
	orig := analysisResponse{Summary: "sound", RootCause: "verified root cause", SuggestedFix: "Set the route table."}
	state.bestDraft = &critiqueDraftCandidate{
		parsed: orig, content: "sound-final", attempt: 1,
		rawQuality: critiqueQuality{Passed: true}, quality: critiqueQuality{Passed: true},
	}
	got := client.applySemanticJudgePostLoop(context.Background(), state, []modelMessage{{Role: "user", Content: strPtr("u")}}, "sound-final", nil, orig, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}), CritiqueCachePolicyStrict)

	if got.RootCause != orig.RootCause || state.judgeRevised || !state.judgeRevisionRejected {
		t.Fatalf("non-sanitizable semantic revision replaced the valid draft: got=%+v state=%+v", got, state)
	}
}

func TestApplySemanticJudgePostLoopMarksStrictPolicyRevisionRejected(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["make the remediation concrete"]}`))
	srv.push(200, chatRespFinal(`{"summary":"revised","is_transient":false,"root_cause":"same cause","severity":"High","suggested_fix":"Check the controller logs.","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	state := &agentState{
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
		readSourceFull: map[string]bool{}, analysisEvidence: map[string]*analysisChatEvidence{},
	}
	orig := analysisResponse{Summary: "sound", RootCause: "same cause", SuggestedFix: "Apply the verified fix."}
	state.bestDraft = &critiqueDraftCandidate{
		parsed: orig, content: "sound-final", attempt: 1,
		rawQuality: critiqueQuality{Passed: true}, quality: critiqueQuality{Passed: true},
	}
	got := client.applySemanticJudgePostLoop(context.Background(), state, nil, "sound-final", nil, orig, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}), CritiqueCachePolicyStrict)
	if got.RootCause != orig.RootCause || state.judgeRevised || !state.judgeRevisionRejected {
		t.Fatalf("strict-policy semantic revision was not retained as rejected: got=%+v state=%+v", got, state)
	}
}

func TestApplySemanticJudgePostLoopAllowsPreservedRawHardFinding(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["clarify the cause"]}`))
	revisedJSON := `{"summary":"revised","is_transient":false,"root_cause":"revised cause","severity":"High","suggested_fix":"Apply the fix.","evidence_citations":[{"path":"missing.log","line_start":1,"line_end":1,"quote":"missing"}]}`
	srv.push(200, chatRespFinal(revisedJSON))
	client := newAgenticTestClient(t, srv.URL)
	state := &agentState{
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{}, readSourceFull: map[string]bool{},
		evidenceArtifactsFull: map[string]bool{}, evidenceContentByPath: map[string][]string{}, analysisEvidence: map[string]*analysisChatEvidence{},
	}
	orig := analysisResponse{
		Summary: "original", RootCause: "original cause", Severity: "High", SuggestedFix: "Apply the fix.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "missing.log", LineStart: 1, LineEnd: 1, Quote: "missing"}},
	}
	origOut := critiqueDraftWithContent(orig, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, nil, 0, analysisCitationContext{Evidence: state.analysisEvidence})
	origJSON, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	state.considerDraft(state.newDraftCandidate("initial", string(origJSON), nil, orig, origOut), false)
	got := client.applySemanticJudgePostLoop(context.Background(), state, nil, string(origJSON), nil, orig, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}), CritiqueCachePolicyStrict)
	if got.RootCause != "revised cause" || !state.judgeRevised || state.judgeRevisionRejected {
		t.Fatalf("semantic revision was not selected: got=%+v state=%+v", got, state)
	}
}

// TestApplySemanticJudgePostLoop_NoObjectionsKeepsDraft verifies a sound draft
// is returned unchanged and no refinalize round is spent.
func TestApplySemanticJudgePostLoop_NoObjectionsKeepsDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	state := &agentState{readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{}}
	orig := analysisResponse{Summary: "sound", RootCause: "real cause", SuggestedFix: "real fix"}
	got := client.applySemanticJudgePostLoop(context.Background(), state, nil, "final", nil, orig, contextHeadroomFor(AgenticOptions{ContextByteBudget: 100_000}), CritiqueCachePolicyStrict)

	if got.RootCause != "real cause" {
		t.Errorf("sound draft should be unchanged, got %q", got.RootCause)
	}
	if calls := atomic.LoadInt32(&srv.calls); calls != 1 {
		t.Errorf("expected 1 call (judge only, no refinalize), got %d", calls)
	}
	if !state.judgeRan || state.judgeObjected || state.judgeRevised {
		t.Errorf("telemetry = ran:%v objected:%v revised:%v, want ran-only", state.judgeRan, state.judgeObjected, state.judgeRevised)
	}
}

// TestAgentic_SemanticJudge_DisabledByDefault verifies the judge does not fire
// when SemanticJudge is unset, so a single passing draft is accepted with no
// extra call.
func TestAgentic_SemanticJudge_DisabledByDefault(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(cleanFinalJSON))
	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2, // retries available, but judge is off
	}
	if _, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:nojudge", "sys", "user"); err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("call count = %d, want 1 (judge disabled, no extra call)", got)
	}
}

func TestAgentic_SemanticNonSanitizableRevisionRejectedKeepsPassingDraftCacheable(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	initial := `{"summary":"sound","is_transient":false,"root_cause":"verified root cause","severity":"High","suggested_fix":"Set the supported configuration.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespFinal(`{"objections":["verify whether the failure is transient"]}`))
	srv.push(200, chatRespFinal(`{"summary":"revised","is_transient":true,"root_cause":"temporary provider failure","severity":"High","suggested_fix":"Set the supported configuration.","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:semantic-rejected-cache"
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, CritiqueMaxRetries: 1, CritiqueCachePolicy: CritiqueCachePolicyAdvisory, SemanticJudge: true}
	in := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in.ConsecutiveFailures = transientPersistThreshold
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.CritiquePassed || !analysis.JudgeObjected || analysis.JudgeRevised || analysis.RootCause != "verified root cause" {
		t.Fatalf("analysis = %+v", analysis)
	}
	if _, ok := client.Cache().Get(key); !ok {
		t.Fatalf("passing original was not cached: analysis=%+v", analysis)
	}
	cachedIn := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	cachedIn.ConsecutiveFailures = transientPersistThreshold
	_, cached, err := client.doAnalyzeAgentic(context.Background(), cachedIn, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit || !cached.JudgeRan || !cached.JudgeObjected || cached.JudgeRevised || atomic.LoadInt32(&srv.calls) != 3 {
		t.Fatalf("cached=%+v calls=%d", cached, atomic.LoadInt32(&srv.calls))
	}
}
