package ai

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemanticCritique_ParsesObjections(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"objections":["root cause is a teardown symptom, not the trigger","did not verify the PR is at fault"]}`))
	client := newAgenticTestClient(t, srv.URL)

	objs, err := client.semanticCritique(context.Background(),
		analysisResponse{RootCause: "credential expiry", SuggestedFix: "re-run"},
		[]string{"build-log.txt"})
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

	objs, err := client.semanticCritique(context.Background(), analysisResponse{RootCause: "x"}, nil)
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
	got := client.applySemanticJudgePostLoop(context.Background(), state, []modelMessage{{Role: "user", Content: strPtr("u")}}, "shallow-final", orig, 0)

	if !strings.Contains(got.RootCause, "route table") {
		t.Errorf("expected the refinalized draft, got root_cause=%q", got.RootCause)
	}
	if !state.judgeRan || !state.judgeObjected || !state.judgeRevised {
		t.Errorf("telemetry = ran:%v objected:%v revised:%v, want all true", state.judgeRan, state.judgeObjected, state.judgeRevised)
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
	got := client.applySemanticJudgePostLoop(context.Background(), state, nil, "final", orig, 0)

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
