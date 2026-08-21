package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
)

// twoGroupEvidenceSkill requires two independent evidence groups so a model
// that reads one and finalizes leaves available evidence unread.
const twoGroupEvidenceSkill = `
id: twogroup
triggers: ["two group failure"]
required_evidence:
  - id: group-a
    description: First group
    any_of: ["(?:^|/)alpha\\.log$"]
  - id: group-b
    description: Second group
    any_of: ["(?:^|/)beta\\.log$"]
procedure: Read both groups before concluding.
`

func newTwoGroupEvidenceInputs(t *testing.T, browser *fakeBrowser) AgenticInputs {
	t.Helper()
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 6, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	})
	in.Skills = loadAgenticSkillsForTest(t, map[string]string{"twogroup": twoGroupEvidenceSkill})
	in.FailureSignal = "two group failure"
	return in
}

func evidencePlanEvents(t *testing.T, store *TraceStore) []TraceEvent {
	t.Helper()
	var out []TraceEvent
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "evidence_plan" {
			out = append(out, event)
		}
	}
	return out
}

// TestAgentic_EvidenceGateReopensFinalizeWithUnreadPlannedGroup reproduces the
// in-process stop-early behavior: the model reads one available evidence group
// and finalizes while the other remains unread and available.
func TestAgentic_EvidenceGateReopensFinalizeWithUnreadPlannedGroup(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, final)
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/beta.log"}))
	srv.push(200, final)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-gate", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("evidence_plan events = %d, want 2: %+v", len(events), events)
	}
	nudge := events[0]
	if nudge.Outcome != string(evidenceGateNudge) {
		t.Fatalf("first evidence_plan outcome = %q, want %q", nudge.Outcome, evidenceGateNudge)
	}
	if nudge.EvidencePlan == nil || nudge.EvidencePlan.Applicable != 2 || nudge.EvidencePlan.Satisfied != 1 || nudge.EvidencePlan.Unmet != 1 {
		t.Fatalf("nudge coverage = %+v, want applicable=2 satisfied=1 unmet=1", nudge.EvidencePlan)
	}
	if got := nudge.EvidencePlan.UnreadGroups; len(got) != 1 || got[0].SkillID != "twogroup" || got[0].GroupID != "group-b" {
		t.Fatalf("unmet groups = %+v, want twogroup/group-b", got)
	}
	if events[1].Outcome != string(evidenceGateCovered) {
		t.Fatalf("final evidence_plan outcome = %q, want %q", events[1].Outcome, evidenceGateCovered)
	}

	srv.mu.Lock()
	nudgeRequest := string(srv.requests[2])
	srv.mu.Unlock()
	for _, want := range []string{"required evidence for this failure class is still unread", "group-b", "logs/beta.log"} {
		if !strings.Contains(nudgeRequest, want) {
			t.Errorf("nudge request missing %q", want)
		}
	}
}

func TestAgentic_EvidenceGateReservesFinalIterationBeforeForcedFinalize(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion and beta.log shows the skewed client version","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/beta.log"}))
	srv.push(200, final)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	in := newTwoGroupEvidenceInputs(t, browser)
	in.Opts.MaxIters = 2
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, in, "agentic:test:evidence-final-iteration", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("evidence_plan events = %d, want 2: %+v", len(events), events)
	}
	if events[0].Status != "iteration_headroom" || events[0].Outcome != string(evidenceGateNudge) {
		t.Fatalf("headroom event = %+v, want iteration_headroom/nudge", events[0])
	}
	if events[1].Status != "forced_finalize" || events[1].Outcome != string(evidenceGateCovered) {
		t.Fatalf("forced finalize event = %+v, want forced_finalize/covered", events[1])
	}
	srv.mu.Lock()
	requests := append([][]byte(nil), srv.requests...)
	srv.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("model requests = %d, want two configured iterations plus one forced finalize", len(requests))
	}
	for _, want := range []string{"final tools-enabled iteration", "group-b", "logs/beta.log"} {
		if !bytes.Contains(requests[1], []byte(want)) {
			t.Errorf("reserved request missing %q", want)
		}
	}
}

func TestAgentic_EvidenceGateRecordsIterationExhaustion(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/gamma.log"}))
	srv.push(200, final)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
		"logs/gamma.log": []byte("unrelated detail\n"),
	}}
	in := newTwoGroupEvidenceInputs(t, browser)
	in.Opts.MaxIters = 2
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, in, "agentic:test:evidence-iteration-exhausted", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("evidence_plan events = %d, want 2: %+v", len(events), events)
	}
	if events[0].Status != "iteration_headroom" || events[0].Outcome != string(evidenceGateNudge) {
		t.Fatalf("headroom event = %+v, want iteration_headroom/nudge", events[0])
	}
	if events[1].Status != "forced_finalize" || events[1].Outcome != string(evidenceGateIterationExhausted) {
		t.Fatalf("forced finalize event = %+v, want forced_finalize/iteration_exhausted", events[1])
	}
}

func TestAgentic_EvidenceGateLeavesEarlyFinalizeUnchanged(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/beta.log"}))
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion and beta.log shows the skewed client version","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`))

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	in := newTwoGroupEvidenceInputs(t, browser)
	in.Opts.MaxIters = 4
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, in, "agentic:test:evidence-early-finalize", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	srv.mu.Lock()
	requests := append([][]byte(nil), srv.requests...)
	srv.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("model requests = %d, want the unchanged tool, tool, final sequence", len(requests))
	}
	if bytes.Contains(requests[2], []byte("final tools-enabled iteration")) {
		t.Fatal("early finalize request received the reserved-iteration nudge")
	}
	for _, event := range evidencePlanEvents(t, store) {
		if event.Status == "iteration_headroom" || event.Status == "forced_finalize" {
			t.Fatalf("early finalize recorded exhaustion telemetry: %+v", event)
		}
	}
}

// TestAgentic_ToolEnvelopeReportsUnreadEvidenceGroups keeps the ranked plan
// visible after every tool turn instead of only in the initial prompt.
func TestAgentic_ToolEnvelopeReportsUnreadEvidenceGroups(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/beta.log"}))
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`))

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		context.Background(), newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-envelope", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	afterAlpha := lastToolMessage(t, string(srv.requests[1]))
	afterBeta := lastToolMessage(t, string(srv.requests[2]))
	srv.mu.Unlock()
	if !strings.Contains(afterAlpha, "unread_evidence_groups") || !strings.Contains(afterAlpha, "group-b") {
		t.Errorf("tool envelope after the first read did not report group-b as unread: %s", afterAlpha)
	}
	if strings.Contains(afterAlpha, "group-a") {
		t.Errorf("tool envelope reported a group the model had just read: %s", afterAlpha)
	}
	if strings.Contains(afterBeta, "unread_evidence_groups") {
		t.Errorf("tool envelope still reported unread groups after the plan was covered: %s", afterBeta)
	}
}

// lastToolMessage returns the content of the newest tool-result message in a
// captured chat request.
func lastToolMessage(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode chat request: %v", err)
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" && req.Messages[i].Content != nil {
			return *req.Messages[i].Content
		}
	}
	t.Fatal("captured request carried no tool-result message")
	return ""
}

// TestAgentic_EvidenceGateCoversDraftTriggeredGroups covers the case where the
// draft's own prose triggers a recipe the failure-signal plan never showed, so
// the required group would otherwise only surface after the loop ended.
func TestAgentic_EvidenceGateCoversDraftTriggeredGroups(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"a version skew between the client and the test","severity":"High","suggested_fix":"Pin the client version and rerun.","relevant_files":[],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, final)
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "logs/gamma.log"}))
	srv.push(200, final)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/gamma.log": []byte("client version 1.35\n"),
	}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 6, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	})
	in.Skills = loadAgenticSkillsForTest(t, map[string]string{
		"signal": `
id: signal
triggers: ["one group failure"]
required_evidence:
  - id: group-a
    description: First group
    any_of: ["(?:^|/)alpha\\.log$"]
`,
		"draft": `
id: draft
triggers: ["version skew"]
required_evidence:
  - id: group-c
    description: Skew evidence
    any_of: ["(?:^|/)gamma\\.log$"]
`,
	})
	in.FailureSignal = "one group failure"

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, in, "agentic:test:evidence-draft-triggered", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) == 0 {
		t.Fatal("no evidence_plan events recorded")
	}
	nudge := events[0]
	if nudge.Outcome != string(evidenceGateNudge) || nudge.EvidencePlan == nil {
		t.Fatalf("first evidence_plan event = %+v, want a nudge", nudge)
	}
	if nudge.EvidencePlan.Unmet != 0 || nudge.EvidencePlan.DraftTriggered != 1 {
		t.Fatalf("coverage = %+v, want the plan satisfied and one draft-triggered group", nudge.EvidencePlan)
	}
	if got := nudge.EvidencePlan.UnreadGroups; len(got) != 1 || got[0].SkillID != "draft" || got[0].GroupID != "group-c" {
		t.Fatalf("unread groups = %+v, want draft/group-c", got)
	}
	if events[len(events)-1].Outcome != string(evidenceGateCovered) {
		t.Fatalf("final evidence_plan outcome = %q, want %q", events[len(events)-1].Outcome, evidenceGateCovered)
	}
}

// TestAgentic_EvidenceGatePreservesTheBetterPreNudgeDraft keeps the reopened
// draft in the selection race, so a weaker post-nudge answer cannot win by
// being the first candidate the critique gate sees.
func TestAgentic_EvidenceGatePreservesTheBetterPreNudgeDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	grounded := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"logs/alpha.log line 1 shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[{"path":"logs/alpha.log","line_start":1,"line_end":1,"quote":"failing assertion"}]}`)
	// The reopened answer changes the diagnosis without reading anything new and
	// cites an artifact it never opened.
	weaker := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"logs/never-read.log proves a networking fault","severity":"High","suggested_fix":"Restart the network plugin.","relevant_files":["logs/never-read.log"],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, grounded)
	srv.push(200, weaker)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		context.Background(), newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-selection", "sys", "user",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(analysis.RootCause, "failing assertion") {
		t.Fatalf("published root cause = %q, want the grounded pre-nudge draft", analysis.RootCause)
	}
}

// TestAgentic_ModelErrorAfterDraftPublishesRetainedDraft keeps a failed
// follow-up request from discarding an answer the engine already has.
func TestAgentic_ModelErrorAfterDraftPublishesRetainedDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"logs/alpha.log line 1 shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`))
	srv.push(503, `{"error":{"message":"upstream unavailable"}}`)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		context.Background(), newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-model-error", "sys", "user",
	)
	if err != nil {
		t.Fatalf("a failed follow-up request discarded the retained draft: %v", err)
	}
	if !strings.Contains(analysis.RootCause, "failing assertion") {
		t.Fatalf("published root cause = %q, want the retained draft", analysis.RootCause)
	}
}

func TestFormatEvidenceNudge_NamesOnlyUnreadGroupsAndBoundsCandidates(t *testing.T) {
	nudge := formatEvidenceNudge([]skills.PlanGroupRef{{
		SkillID:        "twogroup",
		GroupID:        "group-b",
		Description:    "Second group",
		CandidatePaths: []string{"logs/beta.log", "logs/beta2.log", "logs/beta3.log", "logs/beta4.log"},
	}})
	for _, want := range []string{"group-b", "Second group", "logs/beta.log", "logs/beta3.log"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("nudge missing %q:\n%s", want, nudge)
		}
	}
	if strings.Contains(nudge, "logs/beta4.log") {
		t.Errorf("nudge listed more than %d candidates:\n%s", evidenceNudgeMaxCandidates, nudge)
	}
	if strings.Contains(nudge, "group-a") {
		t.Errorf("nudge named a group it was not given:\n%s", nudge)
	}
}

// TestAgentic_EvidenceGateStopsWhenModelMakesNoProgress bounds the gate: a
// model that ignores the nudge is accepted rather than looped indefinitely.
func TestAgentic_EvidenceGateStopsWhenModelMakesNoProgress(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, final)
	srv.push(200, final)

	browser := &fakeBrowser{files: map[string][]byte{
		"logs/alpha.log": []byte("failing assertion\n"),
		"logs/beta.log":  []byte("skewed client version\n"),
	}}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-gate-thrash", "sys", "user",
	)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("evidence_plan events = %d, want 2: %+v", len(events), events)
	}
	if events[0].Outcome != string(evidenceGateNudge) || events[1].Outcome != string(evidenceGateNoProgress) {
		t.Fatalf("outcomes = %q, %q; want nudge then no_progress", events[0].Outcome, events[1].Outcome)
	}
	if analysis == nil {
		t.Fatal("expected the unread-evidence draft to still publish")
	}
}

// TestAgentic_EvidenceGateIgnoresUnavailableGroups keeps the gate off failures
// whose required evidence does not exist in the build at all.
func TestAgentic_EvidenceGateIgnoresUnavailableGroups(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "logs/alpha.log"}))
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"alpha.log shows the failing assertion","severity":"High","suggested_fix":"Correct the assertion and rerun.","relevant_files":[],"evidence_citations":[]}`))

	browser := &fakeBrowser{files: map[string][]byte{"logs/alpha.log": []byte("failing assertion\n")}}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		ctx, newTwoGroupEvidenceInputs(t, browser), "agentic:test:evidence-gate-absent", "sys", "user",
	); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	events := evidencePlanEvents(t, store)
	if len(events) != 1 || events[0].Outcome != string(evidenceGateCovered) {
		t.Fatalf("evidence_plan events = %+v, want one covered outcome", events)
	}
	if events[0].EvidencePlan == nil || events[0].EvidencePlan.Unavailable != 1 {
		t.Fatalf("coverage = %+v, want unavailable=1", events[0].EvidencePlan)
	}
}
