package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fixedBrowserFactory struct {
	browser     artifacts.Browser
	prefix      string
	displayName string
}

func (f *fixedBrowserFactory) ForBuild(prefix, displayName string) artifacts.Browser {
	f.prefix = prefix
	f.displayName = displayName
	return f.browser
}

func (f *fixedBrowserFactory) ForBuilds(_ []analysischat.ArtifactBuild) artifacts.Browser {
	return f.browser
}

func newAnalysisChatAgentForTest(t *testing.T, serverURL string, browser artifacts.Browser, opts AnalysisChatOptions) *AnalysisChatAgent {
	t.Helper()
	registry, enabled := newTestRegistry(t)
	agent, err := NewAnalysisChatAgent(
		newAgenticTestClient(t, serverURL),
		ComposeAnalysisChatSystemPrompt("Project knowledge."),
		registry, enabled, &fixedBrowserFactory{browser: browser}, opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func chatRespToolCallWithContent(content, id, name string, args map[string]interface{}) string {
	encodedContent, _ := json.Marshal(content)
	encodedArgs, _ := json.Marshal(args)
	argumentString, _ := json.Marshal(string(encodedArgs))
	return fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":%s,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`,
		encodedContent, id, name, argumentString,
	)
}

func analysisChatTurn() analysischat.Turn {
	return analysischat.Turn{
		SessionID: "session-1", JobID: "periodic-demo", BuildPrefix: "logs/periodic-demo/123/",
		Build: models.BuildInfo{BuildID: "123", JobName: "periodic-demo", WebURL: "https://example.test/build/123"},
		TestCase: models.TestCase{
			Name: "TestCluster", JUnitFile: "junit.xml", Status: "failed", FailureMessage: "timed out",
			AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-23T12:00:00Z", RootCause: "The controller stopped.",
				Severity: "High", SuggestedFix: "Restart it.", RelevantFiles: []string{"build-log.txt"},
			},
		},
		Question: "Could the API server be the real cause?",
	}
}

func TestAnalysisChatAgentChallengesAfterReadingArtifact(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 200}))
	server.push(200, chatRespFinal(`{
		"answer":"The API server failed first, before the controller stopped.",
		"assessment":"challenges",
		"citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"API server exited"}],
		"proposed_revision":{"root_cause":"The API server exited before the controller.","suggested_fix":"Correct the API server configuration and rerun."}
	}`))
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("API server exited\ncontroller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Assessment != "challenges" || reply.ProposedRevision == nil || reply.ToolCalls != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	if len(reply.Citations) != 1 || reply.Citations[0].Path != "build-log.txt" || reply.GCSBytes == 0 {
		t.Fatalf("reply citations = %+v", reply)
	}
	if reply.Citations[0].LineStart != 0 || reply.Citations[0].LineEnd != 0 {
		t.Fatalf("tail citation retained unverifiable lines: %+v", reply.Citations[0])
	}
}

func TestAnalysisChatAgentReportsProgressPhases(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	server.push(200, chatRespFinal(`{
		"answer":"The controller exit supports the current analysis.",
		"assessment":"supports",
		"citations":[{"path":"build-log.txt","quote":"controller stopped"}],
		"proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})
	turn := analysisChatTurn()
	var phases []string
	turn.Progress = func(phase string) { phases = append(phases, phase) }
	if _, err := agent.Reply(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{analysischat.PhaseReadingEvidence, analysischat.PhaseEvaluating, analysischat.PhaseFinalizing} {
		if !slices.Contains(phases, want) {
			t.Fatalf("progress phases %v missing %q", phases, want)
		}
	}
}

func TestAnalysisChatAgentAllowsExplanationWithoutTools(t *testing.T) {
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{
		"answer":"The published analysis links the timeout to the controller exit.",
		"assessment":"explains",
		"citations":[],
		"proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Explain the current conclusion."

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)
	reply, err := agent.Reply(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.Assessment != "" || reply.ToolCalls != 0 || len(reply.Citations) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
	if statuses := analysisChatEvidenceTraceStatuses(store.Snapshot()); !slices.Contains(statuses, analysisChatEvidenceNotRequired) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
}

func TestAnalysisChatAgentRepairsUnreadCitation(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{
		"answer":"The log proves it.","assessment":"supports",
		"citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"controller stopped"}],
		"proposed_revision":null
	}`))
	server.push(200, chatRespFinal(`{
		"answer":"I cannot support that claim without reading the artifact.","assessment":"inconclusive",
		"citations":[],"proposed_revision":null
	}`))
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 4, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToolCalls != 0 || reply.Assessment != "inconclusive" {
		t.Fatalf("reply = %+v", reply)
	}
	server.mu.Lock()
	requests := append([][]byte(nil), server.requests...)
	server.mu.Unlock()
	if len(requests) < 2 || !strings.Contains(string(requests[1]), "artifact not read during this turn") ||
		!strings.Contains(string(requests[1]), `"response_format"`) {
		t.Fatalf("repair prompt missing from second request: %s", requests[1])
	}
}

func TestAnalysisChatAgentKeepsValidatedCitedDraftWhenFinalizeIsInvalid(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	valid := `{"answer":"The controller exit supports the published conclusion.","assessment":"supports","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(valid, "call-2", "list_artifacts", map[string]interface{}{"path": ""}))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "The controller exit supports the published conclusion." || reply.ToolCalls != 2 || len(reply.Citations) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatAgentRejectsStaleValidatedDraft(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	valid := `{"answer":"The first log supports the published conclusion.","assessment":"supports","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(valid, "call-2", "tail_artifact", map[string]interface{}{"path": "later.log", "lines": 20}))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
		"later.log":     []byte("new evidence\n"),
	}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
}

func TestAnalysisChatAgentNeverKeepsDraftWithInvalidCitations(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	invalid := `{"answer":"The log supports the conclusion.","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(invalid, "call-2", "list_artifacts", map[string]interface{}{"path": ""}))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
}

func TestAnalysisChatAgentKeepsValidDraftWhenFinalizeRequestFails(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	valid := `{"answer":"The existing conclusion remains plausible.","assessment":"explains","citations":[],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(valid, "call-1", "list_artifacts", map[string]interface{}{"path": ""}))
	server.push(500, `private provider body`)
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)

	reply, err := agent.Reply(ctx, analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.Answer != "The existing conclusion remains plausible." || reply.ToolCalls != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	var responseEvents []TraceEvent
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "analysis_chat_response" {
			responseEvents = append(responseEvents, event)
		}
	}
	if len(responseEvents) != 1 || responseEvents[0].Outcome != "fallback" {
		t.Fatalf("response events = %+v", responseEvents)
	}
}

func TestAnalysisChatAgentReturnsSafeValidationCategory(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	server.push(200, chatRespFinal(`still not JSON`))
	server.push(200, chatRespFinal(`still not JSON`))
	server.push(200, chatRespFinal(`still not JSON`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})

	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
	if strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("validation error leaked model content: %v", err)
	}
}

func TestAnalysisChatAgentReturnsSafeProviderCategory(t *testing.T) {
	server := newScriptedChatServer(t)
	server.push(500, `private provider body`)
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})

	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrProviderRequestFailed) {
		t.Fatalf("Reply error = %v", err)
	}
	if strings.Contains(err.Error(), "private provider body") {
		t.Fatalf("provider error leaked response content: %v", err)
	}
}

func TestAnalysisChatAgentReturnsSafeCitationCategory(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	invalid := `{"answer":"The log proves it.","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrCitationValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
}

func TestAnalysisChatResponseTelemetryIsContentFree(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	recordAnalysisChatResponseFailure(ctx, "finalize_validation", 9, 11, &modelResponse{HTTPStatus: 200}, analysisChatParseStats{
		CandidateCount: 4,
	}, analysisChatValidationCitation)
	recordAnalysisChatResponseFailure(ctx, "tool_loop_validation", 2, 2, &modelResponse{HTTPStatus: 200}, analysisChatParseStats{
		CandidateCount: 2,
	}, analysisChatValidationReference)
	trace.Finish("error", analysischat.ErrCitationValidationFailed)

	snapshot := store.Snapshot()
	if len(snapshot.Traces) != 1 || len(snapshot.Traces[0].Events) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	event := snapshot.Traces[0].Events[0]
	if event.Kind != "analysis_chat_response" || event.Status != "finalize_validation" || event.ModelCallCount != 9 ||
		event.Attempts != 11 || event.HTTPStatus != 200 || event.CandidateCount != 4 || event.ErrorCode != analysisChatValidationCitation {
		t.Fatalf("event = %+v", event)
	}
	referenceEvent := snapshot.Traces[0].Events[1]
	if referenceEvent.Status != "tool_loop_validation" || referenceEvent.ErrorCode != analysisChatValidationReference || referenceEvent.CandidateCount != 2 {
		t.Fatalf("reference event = %+v", referenceEvent)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"build-log.txt", "different evidence", "controller stopped"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("telemetry contains provider content %q", forbidden)
		}
	}
	if got := analysisChatResponseAttempts(nil); got != 0 {
		t.Fatalf("nil response attempts = %d", got)
	}
	if got := analysisChatResponseAttempts(&modelResponse{}); got != 1 {
		t.Fatalf("response without metadata attempts = %d", got)
	}
}

func TestAnalysisChatAgentRecordsCancelledRequestTelemetry(t *testing.T) {
	server := newScriptedChatServer(t)
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx, cancel := context.WithCancel(withAnalysisTrace(context.Background(), trace))
	cancel()

	_, err := agent.Reply(ctx, analysisChatTurn())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reply error = %v", err)
	}
	trace.Finish("error", err)
	var event *TraceEvent
	snapshot := store.Snapshot()
	for index := range snapshot.Traces[0].Events {
		candidate := &snapshot.Traces[0].Events[index]
		if candidate.Kind == "analysis_chat_response" {
			event = candidate
			break
		}
	}
	if event == nil || event.Status != "tool_loop_request" || event.ErrorCode != "request_cancelled" || event.ModelCallCount != 1 {
		t.Fatalf("event = %+v", event)
	}
	if got := analysisChatRequestErrorCategory(context.DeadlineExceeded); got != "request_timeout" {
		t.Fatalf("deadline category = %q", got)
	}
}

func TestParseAnalysisChatReplyRejectsUnsafeAndUnverifiedClaims(t *testing.T) {
	cases := []string{
		`{"answer":"x","assessment":"challenges","citations":[],"proposed_revision":{"root_cause":"r","suggested_fix":"f"}}`,
		`{"answer":"x","assessment":"supports","citations":[{"path":"../secret"}],"proposed_revision":null}`,
		`{"answer":"x","assessment":"supports","citations":[{"path":"other.log","quote":"missing text"}],"proposed_revision":null}`,
		`{"answer":"x","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`,
		`{"answer":"x","assessment":"supports","citations":[],"proposed_revision":{"root_cause":"r","suggested_fix":"f"}}`,
	}
	for _, raw := range cases {
		if _, err := parseAnalysisChatReply(raw, map[string]*analysisChatEvidence{"build-log.txt": {Segments: []string{"controller stopped"}}}); err == nil {
			t.Errorf("invalid reply accepted: %s", raw)
		}
	}
}

func TestParseAnalysisChatReplyScansKimiCandidates(t *testing.T) {
	valid := `{"answer":"valid answer","assessment":"explains","citations":[],"proposed_revision":null}`
	cases := []struct {
		name string
		raw  string
	}{
		{name: "fenced", raw: "```json\n" + valid + "\n```"},
		{name: "metadata wrapper", raw: `{"metadata":{"finish_reason":"stop"},"result":` + valid + `}`},
		{name: "quoted braces", raw: `{"answer":"value with {nested text}","assessment":"explains","citations":[],"proposed_revision":null}`},
		{name: "quoted prose brace", raw: `The token "{" was emitted. ` + valid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reply, stats, err := parseAnalysisChatReplyCandidates(testCase.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Answer == "" || stats.CandidateCount == 0 {
				t.Fatalf("reply=%+v stats=%+v", reply, stats)
			}
		})
	}
}

func TestParseAnalysisChatReplyRejectsMalformedAmbiguousAndNestedCandidates(t *testing.T) {
	valid := `{"answer":"valid answer","assessment":"explains","citations":[],"proposed_revision":null}`
	cases := []struct {
		name     string
		raw      string
		category string
	}{
		{name: "malformed candidate", raw: `{"answer":}`, category: analysisChatValidationJSON},
		{name: "ambiguous valid candidates", raw: valid + `\n` + valid, category: analysisChatValidationCandidate},
		{name: "nested contract", raw: `{"answer":{"answer":"nested","assessment":"explains","citations":[],"proposed_revision":null},"assessment":"explains","citations":[],"proposed_revision":null}`, category: analysisChatValidationContract},
		{name: "valid followed by malformed contract", raw: valid + `\n{"answer":"unfinished"`, category: analysisChatValidationJSON},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, stats, err := parseAnalysisChatReplyCandidates(testCase.raw, nil)
			if err == nil || stats.Category != testCase.category {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
		})
	}
}

func TestParseAnalysisChatReplyQuotedBracesDoNotEvictOuterCandidate(t *testing.T) {
	answer := strings.Repeat("{", analysisChatMaxCandidates+20) + "still text"
	raw, err := json.Marshal(map[string]any{
		"answer": answer, "assessment": "explains", "citations": []any{}, "proposed_revision": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, stats, err := parseAnalysisChatReplyCandidates(string(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != answer || stats.CandidateCount != 1 {
		t.Fatalf("reply answer bytes=%d candidates=%d", len(reply.Answer), stats.CandidateCount)
	}
}

func TestParseAnalysisChatReplyRejectsLaterInvalidContractCandidate(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	valid := `{"answer":"supported","assessment":"supports","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":null}`
	invalid := `{"answer":"bad update","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	_, stats, err := parseAnalysisChatReplyCandidates(valid+"\n"+invalid, evidence)
	if err == nil || stats.Category != analysisChatValidationCitation {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestParseAnalysisChatReplyCategorizesCitationMismatch(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	raw := `{"answer":"bad update","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	_, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err == nil || stats.Category != analysisChatValidationCitation {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if !errors.Is(analysisChatSafeValidationError(err), analysischat.ErrCitationValidationFailed) {
		t.Fatalf("safe error = %v", analysisChatSafeValidationError(err))
	}
}

func TestParseAnalysisChatReplyValidatesCrossBuildReferences(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"builds/103/build-log.txt": {Segments: []string{"build 103 failed first"}, Lines: map[int]string{}},
		"builds/104/build-log.txt": {Segments: []string{"build 104 timed out later"}, Lines: map[int]string{}},
	}
	valid := `{"answer":"The builds fail at different stages.","assessment":"supports","citations":[{"path":"builds/103/build-log.txt","quote":"build 103 failed first"},{"path":"builds/104/build-log.txt","quote":"build 104 timed out later"}],"proposed_revision":null}`
	if _, err := parseAnalysisChatReply(valid, evidence); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		raw      string
		category string
	}{
		{name: "stripped build prefix", raw: `{"answer":"x","assessment":"supports","citations":[{"path":"build-log.txt","quote":"build 103 failed first"}],"proposed_revision":null}`, category: analysisChatValidationReference},
		{name: "quote from another build", raw: `{"answer":"x","assessment":"supports","citations":[{"path":"builds/103/build-log.txt","quote":"build 104 timed out later"}],"proposed_revision":null}`, category: analysisChatValidationCitation},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, stats, err := parseAnalysisChatReplyCandidates(testCase.raw, evidence)
			if err == nil || stats.Category != testCase.category {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
			if !errors.Is(analysisChatSafeValidationError(err), analysischat.ErrCitationValidationFailed) {
				t.Fatalf("safe error = %v", analysisChatSafeValidationError(err))
			}
		})
	}
}

func TestParseAnalysisChatReplyAllowsMinimalAndOptionalContract(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{}
	minimal, err := parseAnalysisChatReply(`{"answer":"Direct answer.","citations":[]}`, evidence)
	if err != nil || minimal.Answer != "Direct answer." || minimal.Assessment != "" || minimal.ProposedRevision != nil {
		t.Fatalf("minimal reply = %+v, err=%v", minimal, err)
	}
	assessment, err := parseAnalysisChatReply(`{"answer":"The evidence remains incomplete.","citations":[],"assessment":"inconclusive"}`, evidence)
	if err != nil || assessment.Assessment != "inconclusive" || assessment.ProposedRevision != nil {
		t.Fatalf("optional assessment reply = %+v, err=%v", assessment, err)
	}
}

func TestParseAnalysisChatReplyRejectsDuplicateFields(t *testing.T) {
	cases := []string{
		`{"answer":"first","answer":"second","assessment":"explains","citations":[],"proposed_revision":null}`,
		`{"answer":"x","assessment":"supports","citations":[{"path":"build-log.txt","path":"other.log","quote":"controller stopped"}],"proposed_revision":null}`,
		`{"answer":"x","assessment":"challenges","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":{"root_cause":"one","root_cause":"two","suggested_fix":"fix"}}`,
	}
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}}}
	for _, raw := range cases {
		_, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
		if err == nil || stats.Category != analysisChatValidationContract {
			t.Fatalf("stats=%+v err=%v raw=%s", stats, err, raw)
		}
	}
}

func TestComposeAnalysisChatSystemPromptKeepsSeparateSchema(t *testing.T) {
	prompt := ComposeAnalysisChatSystemPrompt("Consumer fact.")
	for _, want := range []string{"Consumer fact.", "published AI analysis is a hypothesis", `"citations": []`, "preserve the full builds/<build-id>/"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, `"is_transient"`) {
		t.Fatal("analysis response schema leaked into chat prompt")
	}
}

var _ artifacts.Factory = (*fixedBrowserFactory)(nil)

func TestAnalysisChatAgentReportsUnsupportedTools(t *testing.T) {
	server := newScriptedChatServer(t)
	server.push(400, `{"error":{"message":"tools are not supported by this model"}}`)
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	_, err := agent.Reply(context.Background(), analysisChatTurn())
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("Reply error = %v", err)
	}
	if !errors.Is(err, analysischat.ErrProviderRequestFailed) {
		t.Fatalf("Reply error missing provider category: %v", err)
	}
}

func TestAnalysisChatContextBoundsPublishedFields(t *testing.T) {
	turn := analysisChatTurn()
	turn.TestCase.AIAnalysis.RootCause = strings.Repeat("r", 100<<10)
	turn.TestCase.AIAnalysis.SuggestedFix = strings.Repeat("f", 100<<10)
	turn.TestCase.AIAnalysis.RelevantFiles = make([]string, 100)
	for i := range turn.TestCase.AIAnalysis.RelevantFiles {
		turn.TestCase.AIAnalysis.RelevantFiles[i] = strings.Repeat("p", 2048)
	}
	context, err := analysisChatContext(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(context) > 120<<10 {
		t.Fatalf("bounded context is %d bytes", len(context))
	}
	if !strings.Contains(context, "content elided") {
		t.Fatal("bounded context has no elision marker")
	}
}

func TestAnalysisChatAgentCapsToolCalls(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespTwoToolCalls("call-1", "list_artifacts", "call-2", "list_artifacts"))
	server.push(200, chatRespFinal(`{
		"answer":"The available context does not require a revised conclusion.",
		"assessment":"explains",
		"citations":[],
		"proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{
		MaxIters: 3, MaxToolCalls: 1, Timeout: time.Second,
	})
	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", reply.ToolCalls)
	}
}

func TestAnalysisChatAgentReplaysStructuredAssistantHistory(t *testing.T) {
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{
		"answer":"The revision followed the later API server evidence.",
		"assessment":"explains",
		"citations":[],
		"proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.History = []analysischat.Message{
		{Role: "user", Content: "Could the original conclusion be wrong?"},
		{
			Role: "assistant", Content: "The evidence supports a revision.", Assessment: "challenges",
			ProposedRevision: &analysischat.Revision{
				RootCause: "revised-root-marker", SuggestedFix: "revised-fix-marker",
			},
		},
	}
	turn.Question = "Why that revision?"
	if _, err := agent.Reply(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	request := string(server.requests[0])
	server.mu.Unlock()
	for _, want := range []string{"revised-root-marker", "revised-fix-marker", "proposed_revision", "challenges"} {
		if !strings.Contains(request, want) {
			t.Errorf("request omitted structured history %q", want)
		}
	}
}

func TestBuildAnalysisChatMessagesDropsOldestHistoryWithinBudget(t *testing.T) {
	var history []analysischat.Message
	for i := 0; i < 8; i++ {
		marker := "middle-marker"
		if i == 0 {
			marker = "oldest-marker"
		}
		if i == 7 {
			marker = "latest-marker"
		}
		history = append(history,
			analysischat.Message{Role: "user", Content: marker + strings.Repeat("u", 3000)},
			analysischat.Message{Role: "assistant", Content: marker + strings.Repeat("a", 10_000), Assessment: "explains"},
		)
	}
	const budget = 48 << 10
	messages, err := buildAnalysisChatMessages("system", "context", history, "question", 1024, budget)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "oldest-marker") {
		t.Fatal("oldest history was not removed")
	}
	if !strings.Contains(text, "latest-marker") {
		t.Fatal("latest history was not retained")
	}
	if size := requestSizeEstimate(messages, 1024); size > budget {
		t.Fatalf("request size = %d, budget = %d", size, budget)
	}
}

func TestAnalysisChatOptionsUseFallbackContextBudget(t *testing.T) {
	opts := (AnalysisChatOptions{}).normalized()
	if opts.ContextByteBudget != analysisChatFallbackContextBytes {
		t.Fatalf("context budget = %d", opts.ContextByteBudget)
	}
	if _, err := buildAnalysisChatMessages(strings.Repeat("x", 1024), "context", nil, "question", 0, 100); err == nil {
		t.Fatal("oversized base context was accepted")
	}
}

func TestAnalysisChatCitationLineValidation(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{}
	recordAnalysisChatEvidence(evidence, modelToolCall{Function: modelFunction{
		Name: "grep_artifact", Arguments: `{"path":"build-log.txt"}`,
	}}, map[string]interface{}{"matches": []interface{}{map[string]interface{}{
		"line": float64(42), "context": []interface{}{"  41: before", "> 42: controller stopped", "  43: after"},
	}}})
	valid := `{"answer":"The controller stopped.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":42,"line_end":42,"quote":"controller stopped"}],"proposed_revision":null}`
	reply, err := parseAnalysisChatReply(valid, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Citations[0].LineStart != 42 || reply.Citations[0].LineEnd != 42 {
		t.Fatalf("verified line range was not retained: %+v", reply.Citations[0])
	}
	canonical := `{"answer":"The controller stopped.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":42,"line_end":42,"quote":"the controller exited"}],"proposed_revision":null}`
	reply, err = parseAnalysisChatReply(canonical, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Citations[0].Quote != "controller stopped" {
		t.Fatalf("canonical quote = %q", reply.Citations[0].Quote)
	}
	invalid := `{"answer":"The controller stopped.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":40,"line_end":40,"quote":"controller stopped"}],"proposed_revision":null}`
	if _, err := parseAnalysisChatReply(invalid, evidence); err == nil {
		t.Fatal("fabricated line range was accepted")
	}
}

func TestAnalysisChatCitationRangeReconstructionFailsClosed(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {
		Segments: []string{"controller stopped"},
		Lines:    map[int]string{42: "controller stopped"},
		Bytes:    len("controller stopped"),
	}}
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "missing line",
			raw:  `{"answer":"x","citations":[{"path":"build-log.txt","line_start":42,"line_end":43,"quote":"ignored"}]}`,
		},
		{
			name: "unread path",
			raw:  `{"answer":"x","citations":[{"path":"other.log","line_start":42,"line_end":42,"quote":"ignored"}]}`,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseAnalysisChatReply(testCase.raw, evidence); err == nil {
				t.Fatal("invalid citation was accepted")
			}
		})
	}
}

func TestAnalysisChatCitationWithoutLineRangeStillRequiresExactQuote(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {
		Segments: []string{"controller stopped"}, Bytes: len("controller stopped"),
	}}
	raw := `{"answer":"x","citations":[{"path":"build-log.txt","quote":"the controller exited"}]}`
	if _, err := parseAnalysisChatReply(raw, evidence); err == nil {
		t.Fatal("mismatched quote without a verified line range was accepted")
	}
}

func TestAnalysisChatCitationUsesExactSafePath(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"foo.log": {Segments: []string{"controller stopped"}}}
	caseMismatch := `{"answer":"x","assessment":"supports","citations":[{"path":"FOO.log","quote":"controller stopped"}],"proposed_revision":null}`
	if _, err := parseAnalysisChatReply(caseMismatch, evidence); err == nil {
		t.Fatal("case-mismatched artifact citation was accepted")
	}
	suffixMismatch := `{"answer":"x","assessment":"supports","citations":[{"path":"foo","quote":"controller stopped"}],"proposed_revision":null}`
	if _, err := parseAnalysisChatReply(suffixMismatch, evidence); err == nil {
		t.Fatal("suffix-stripped artifact citation was accepted")
	}
}

func TestNewAnalysisChatAgentRequiresContentReader(t *testing.T) {
	registry, _ := newTestRegistry(t)
	_, err := NewAnalysisChatAgent(
		newAgenticTestClient(t, "http://example.test"),
		ComposeAnalysisChatSystemPrompt("project"), registry,
		[]string{"discover_clusters"}, &fixedBrowserFactory{browser: &fakeBrowser{}}, AnalysisChatOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "read_artifact") {
		t.Fatalf("constructor error = %v", err)
	}
}

func TestAnalysisChatEvidenceRequiresContiguousSegmentsAndLines(t *testing.T) {
	evidence := &analysisChatEvidence{
		Segments: []string{"first snippet", "second snippet"},
		Lines:    map[int]string{10: "first snippet", 12: "second snippet"},
	}
	if analysisChatEvidenceContains(evidence, "first snippet\nsecond snippet") {
		t.Fatal("quote spanning separate reads was accepted")
	}
	if _, ok := analysisChatQuoteForRange(evidence.Lines, 10, 12); ok {
		t.Fatal("line range with an unobserved gap was accepted")
	}
}

func TestAnalysisChatEvidenceSurvivesCappedModelEnvelope(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "grep_artifact", map[string]interface{}{
		"path": "build-log.txt", "pattern": "match", "max_matches": 100,
	}))
	server.push(200, chatRespFinal(`{
		"answer":"The last matching line confirms the failure.",
		"assessment":"supports",
		"citations":[{"path":"build-log.txt","line_start":100,"line_end":100,"quote":"match-099-marker"}],
		"proposed_revision":null
	}`))
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("match-%03d-marker %s", i, strings.Repeat("x", 500)))
	}
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte(strings.Join(lines, "\n"))}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})
	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Citations) != 1 || reply.Citations[0].LineStart != 100 {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestPrepareAnalysisChatFinalizeMessagesCompactsCompleteRequest(t *testing.T) {
	toolContent := strings.Repeat("x", 12<<10)
	messages := []modelMessage{
		{Role: "system", Content: strPtr("system")},
		{Role: "user", Content: strPtr("question")},
		{Role: "assistant", ToolCalls: []modelToolCall{{ID: "call-1", Type: "function", Function: modelFunction{Name: "read_artifact", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "call-1", Content: &toolContent},
	}
	before := requestSizeEstimate(messages, 0)
	budget := before + len(analysisChatFinalizePrompt)/2
	complete := append(slices.Clone(messages), modelMessage{Role: "user", Content: strPtr(analysisChatFinalizePrompt)})
	if requestSizeEstimate(complete, 0) <= budget {
		t.Fatal("test setup did not cross the context budget")
	}
	prepared, err := prepareAnalysisChatFinalizeMessages(messages, budget)
	if err != nil {
		t.Fatal(err)
	}
	if size := requestSizeEstimate(prepared, 0); size > budget {
		t.Fatalf("finalize request size = %d, budget = %d", size, budget)
	}
	if prepared[len(prepared)-1].Content == nil || *prepared[len(prepared)-1].Content != analysisChatFinalizePrompt {
		t.Fatal("finalize prompt was not preserved")
	}
}

func TestParseAnalysisChatReplyRejectsAmbiguousValidDrafts(t *testing.T) {
	raw := `{"answer":"first","assessment":"explains","citations":[],"proposed_revision":null}` +
		`{"answer":"second","assessment":"explains","citations":[],"proposed_revision":null}`
	_, stats, err := parseAnalysisChatReplyCandidates(raw, nil)
	if err == nil || stats.Category != analysisChatValidationCandidate {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestParseAnalysisChatReplyRejectsTrailingUnrelatedJSON(t *testing.T) {
	raw := `{"answer":"first","assessment":"explains","citations":[],"proposed_revision":null}` +
		`{"unrelated":true}`
	if _, err := parseAnalysisChatReply(raw, nil); err == nil || !strings.Contains(err.Error(), "trailing unrelated JSON") {
		t.Fatalf("trailing response error = %v", err)
	}
}

func TestAnalysisChatJSONCandidatesRejectTruncatedScan(t *testing.T) {
	raw := strings.Repeat("{not-json}", analysisChatMaxCandidates+20) +
		`{"answer":"final","assessment":"explains","citations":[],"proposed_revision":null}`
	scan := scanAnalysisChatJSONCandidates(raw)
	if len(scan.candidates) != analysisChatMaxCandidates || !scan.truncated {
		t.Fatalf("candidate count = %d truncated=%t", len(scan.candidates), scan.truncated)
	}
	_, stats, err := parseAnalysisChatReplyCandidates(raw, nil)
	if err == nil || stats.Category != analysisChatValidationCandidate {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestParseAnalysisChatReplyBoundsAggregateCandidateSpanWork(t *testing.T) {
	valid := `{"answer":"valid answer","assessment":"explains","citations":[],"proposed_revision":null}`
	largeWrapper := `{"padding":"` + strings.Repeat("x", analysisChatMaxResponseBytes/2) + `","result":` + valid + `}`
	raw := strings.Repeat(`{"metadata":`, 10) + largeWrapper + strings.Repeat("}", 10)
	if len(raw) >= analysisChatMaxResponseBytes {
		t.Fatalf("test response is %d bytes", len(raw))
	}
	scan := scanAnalysisChatJSONCandidates(raw)
	if scan.truncated || len(scan.candidates) < 10 {
		t.Fatalf("candidates=%d truncated=%t", len(scan.candidates), scan.truncated)
	}
	_, stats, err := parseAnalysisChatReplyCandidates(raw, nil)
	if err == nil || stats.Category != analysisChatValidationCandidate || !strings.Contains(err.Error(), "work budget") {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestAnalysisChatJSONCandidateScanStaysBoundedForUnclosedObjects(t *testing.T) {
	raw := strings.Repeat("{", analysisChatMaxCandidates) +
		strings.Repeat("x", analysisChatMaxResponseBytes-analysisChatMaxCandidates)
	scan := scanAnalysisChatJSONCandidates(raw)
	if len(scan.candidates) != 0 || len(scan.incomplete) != analysisChatMaxCandidates {
		t.Fatalf("candidates=%d incomplete=%d", len(scan.candidates), len(scan.incomplete))
	}
}

func TestParseAnalysisChatReplyHandlesDeepMetadataWrapper(t *testing.T) {
	valid := `{"answer":"valid answer","assessment":"explains","citations":[],"proposed_revision":null}`
	raw := strings.Repeat(`{"metadata":`, analysisChatMaxCandidates-1) + valid + strings.Repeat("}", analysisChatMaxCandidates-1)
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "valid answer" || stats.CandidateCount != analysisChatMaxCandidates {
		t.Fatalf("reply=%+v candidates=%d", reply, stats.CandidateCount)
	}
}

func TestAnalysisChatEvidenceOverflowIsAtomic(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{strings.Repeat("a", analysisChatEvidenceMaxBytes-3)}, Bytes: analysisChatEvidenceMaxBytes - 3, Lines: map[int]string{}},
	}
	beforeSegments := len(evidence["build-log.txt"].Segments)
	beforeBytes := evidence["build-log.txt"].Bytes
	ok := recordAnalysisChatEvidence(evidence, modelToolCall{Function: modelFunction{
		Name: "read_artifact", Arguments: `{"path":"build-log.txt"}`,
	}}, map[string]interface{}{"content": "four"})
	if ok {
		t.Fatal("overflowing evidence read was accepted")
	}
	entry := evidence["build-log.txt"]
	if entry.Bytes != beforeBytes || len(entry.Segments) != beforeSegments {
		t.Fatalf("overflow mutated evidence: bytes=%d segments=%d", entry.Bytes, len(entry.Segments))
	}
}

func TestAnalysisChatContextDescribesRecurringPatternBuilds(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern-1", Subject: "retry failures", BuildsAnalyzed: 4, Confidence: "high",
		SharedRootCause: "terminal failures retry", SuggestedFix: "stop terminal retries",
		SharedBuilds: []string{"104", "103", "102", "101"}, RelevantFiles: []string{"pkg/retry.go"},
	}
	contextMessage, err := analysisChatContext(analysischat.Turn{
		JobID: "periodic-demo", Pattern: pattern,
		EvidenceBuilds: []analysischat.ArtifactBuild{
			{Build: models.BuildInfo{BuildID: "104"}}, {Build: models.BuildInfo{BuildID: "103"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"recurring-pattern analysis", `"pattern_id": "pattern-1"`, `"artifact_builds"`, "Use that exact full path in citations"} {
		if !strings.Contains(contextMessage, want) {
			t.Fatalf("context missing %q: %s", want, contextMessage)
		}
	}
}

func TestPatternAnalysisChatToolsExcludeSingleBuildHelpers(t *testing.T) {
	got := patternAnalysisChatTools([]string{
		"discover_clusters", "find_my_cluster", "list_artifacts", "read_artifact", "timeline_artifacts",
	})
	if !slices.Equal(got, []string{"list_artifacts", "read_artifact"}) {
		t.Fatalf("pattern tools = %v", got)
	}
}

func TestAnalysisChatFinalizationUsesForcedFunctionAndRecordsUsage(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinalWithUsage(`{"answer":"unfinished"}`, 10, 2, 0))
	server.push(200, chatRespFinalWithUsage(`still invalid`, 11, 3, 0))
	valid := `{"answer":"The published context is sufficient.","assessment":"inconclusive","citations":[],"proposed_revision":null}`
	server.push(200, chatRespForcedFunctionWithUsage("analysis_chat_reply", valid, 12, 4, 1))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)

	reply, err := agent.Reply(ctx, analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.Answer != "The published context is sufficient." || reply.ValidationRetries != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	server.mu.Lock()
	requests := append([][]byte(nil), server.requests...)
	server.mu.Unlock()
	if len(requests) != 3 || !strings.Contains(string(requests[1]), `"response_format"`) ||
		!strings.Contains(string(requests[2]), `"tool_choice"`) || !strings.Contains(string(requests[2]), `"parallel_tool_calls":false`) {
		t.Fatalf("structured requests = %q", requests)
	}

	var modelRequests []TraceEvent
	var structuredAttempts []TraceEvent
	var responseEvent *TraceEvent
	snapshot := store.Snapshot()
	for index := range snapshot.Traces[0].Events {
		event := snapshot.Traces[0].Events[index]
		if event.Kind == "model_request" {
			modelRequests = append(modelRequests, event)
		}
		if event.Kind == "structured_completion" {
			structuredAttempts = append(structuredAttempts, event)
		}
		if event.Kind == "analysis_chat_response" {
			copy := event
			responseEvent = &copy
		}
	}
	if len(modelRequests) != 3 {
		t.Fatalf("model request events = %+v", modelRequests)
	}
	inputTokens, outputTokens, reasoningTokens := 0, 0, 0
	for _, event := range modelRequests {
		if !event.UsageReported {
			t.Fatalf("usage missing from event: %+v", event)
		}
		inputTokens += event.InputTokens
		outputTokens += event.OutputTokens
		reasoningTokens += event.ReasoningTokens
	}
	if inputTokens != 33 || outputTokens != 9 || reasoningTokens != 1 {
		t.Fatalf("usage totals input=%d output=%d reasoning=%d", inputTokens, outputTokens, reasoningTokens)
	}
	if len(structuredAttempts) != 2 || structuredAttempts[0].StructuredPhase != "analysis_chat_validation_retry" ||
		structuredAttempts[0].StructuredAttempt != "response_format" || structuredAttempts[0].StructuredOutcome != "no_candidate" ||
		structuredAttempts[1].StructuredAttempt != "forced_function" || structuredAttempts[1].StructuredOutcome != "accepted" {
		t.Fatalf("structured attempt events = %+v", structuredAttempts)
	}
	if responseEvent == nil || responseEvent.Outcome != "success" || responseEvent.Status != "validation_retry" ||
		responseEvent.StructuredAttempt != "forced_function" || responseEvent.ModelCallCount != 3 || responseEvent.Attempts != 3 || responseEvent.HTTPStatus != 200 {
		t.Fatalf("response event = %+v", responseEvent)
	}
}

func chatRespFinalWithUsage(content string, input, output, reasoning int) string {
	encoded, _ := json.Marshal(content)
	return fmt.Sprintf(
		`{"id":"chat-final","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%s}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
		encoded, input, output, reasoning,
	)
}

func chatRespForcedFunctionWithUsage(name, arguments string, input, output, reasoning int) string {
	encodedName, _ := json.Marshal(name)
	encodedArguments, _ := json.Marshal(arguments)
	return fmt.Sprintf(
		`{"id":"chat-forced","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"final-call","type":"function","function":{"name":%s,"arguments":%s}}]}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
		encodedName, encodedArguments, input, output, reasoning,
	)
}

func TestAnalysisChatPatternContextCurrentCausalFields(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern-1", Subject: "retry failures", BuildsAnalyzed: 6, Confidence: "medium",
		Recurrence: models.PatternRecurrenceMixedCauses, Systemic: true,
		CausalGroups: []models.PatternCausalGroup{
			{ID: "group-a", ContentHash: "hash-a", Builds: []string{"104", "103"}, RootCause: "retry exhaustion", Confidence: "high"},
			{ID: "singleton", ContentHash: "hash-single", Builds: []string{"102"}, RootCause: "unrelated quota", Confidence: "low"},
			{ID: "group-b", ContentHash: "hash-b", Builds: []string{"101", "100"}, RootCause: "webhook timeout", Confidence: "medium"},
		},
		UnclassifiedBuilds: []string{"99"}, SharedBuilds: []string{"104", "103", "101", "100"},
		Summary: "two recurring causes and one outlier",
		Lifecycle: &models.PatternLifecycle{
			State: models.PatternLifecycleRecovered, Reason: "three later builds passed",
			SourceRevision: "private-source-revision", PassingBuilds: []string{"private-passing-build"},
		},
		RemediationInvestigations: []models.PatternRemediationInvestigationSummary{
			{
				CausalGroupID: "group-a", CausalGroupHash: "hash-a", State: models.PatternRemediationActionable,
				Reason: "verified source target", CompletedAt: "2026-08-12T12:00:00Z",
				Target: &models.PatternRemediationTargetSummary{Path: "private/target.go", Repository: "private/repo", Revision: "private-revision"},
			},
			{CausalGroupID: "group-b", CausalGroupHash: "hash-b", State: models.PatternRemediationInsufficientEvidence, Reason: "source evidence is incomplete"},
		},
	}
	encoded, err := encodeAnalysisChatPatternContext(analysischat.Turn{
		JobID: "periodic-demo", Pattern: pattern,
		EvidenceBuilds: []analysischat.ArtifactBuild{
			{Build: models.BuildInfo{BuildID: "104"}}, {Build: models.BuildInfo{BuildID: "101"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var context analysisChatPatternContext
	if err := json.Unmarshal(encoded, &context); err != nil {
		t.Fatal(err)
	}
	if context.Recurrence != models.PatternRecurrenceMixedCauses || len(context.CausalGroups) != 3 || !slices.Equal(context.Unclassified, []string{"99"}) {
		t.Fatalf("context = %+v", context)
	}
	if !slices.Equal(context.CausalGroups[0].Builds, []string{"104", "103"}) || !slices.Equal(context.CausalGroups[0].ArtifactBuilds, []string{"104"}) ||
		context.CausalGroups[0].Remediation == nil || context.CausalGroups[0].Remediation.State != models.PatternRemediationActionable {
		t.Fatalf("group a = %+v", context.CausalGroups[0])
	}
	if len(context.CausalGroups[1].ArtifactBuilds) != 0 || context.CausalGroups[1].Remediation != nil {
		t.Fatalf("singleton = %+v", context.CausalGroups[1])
	}
	if !slices.Equal(context.CausalGroups[2].ArtifactBuilds, []string{"101"}) || context.CausalGroups[2].Remediation == nil || context.CausalGroups[2].Remediation.State != models.PatternRemediationInsufficientEvidence {
		t.Fatalf("group b = %+v", context.CausalGroups[2])
	}
	if context.Lifecycle == nil || context.Lifecycle.State != models.PatternLifecycleRecovered || context.Lifecycle.Reason != "three later builds passed" {
		t.Fatalf("lifecycle = %+v", context.Lifecycle)
	}
	text := string(encoded)
	for _, forbidden := range []string{"private/target.go", "private/repo", "private-revision", "private-source-revision", "private-passing-build", `"target"`, `"usage"`, `"provider"`, `"evidence_catalog"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("pattern context exposed %q: %s", forbidden, text)
		}
	}
	if strings.Contains(text, "published_suggested_fix") {
		t.Fatalf("pattern context invented a suggested fix: %s", text)
	}
}

func TestAnalysisChatPatternContextOneSharedGroup(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern", Subject: "shared", BuildsAnalyzed: 2, Systemic: true, Confidence: "high",
		Recurrence:   models.PatternRecurrenceSharedCause,
		CausalGroups: []models.PatternCausalGroup{{ID: "group", ContentHash: "hash", Builds: []string{"2", "1"}, RootCause: "same cause", Confidence: "high"}},
		SharedBuilds: []string{"2", "1"}, Summary: "shared cause",
	}
	encoded, err := encodeAnalysisChatPatternContext(analysischat.Turn{Pattern: pattern, EvidenceBuilds: []analysischat.ArtifactBuild{{Build: models.BuildInfo{BuildID: "2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var context analysisChatPatternContext
	if err := json.Unmarshal(encoded, &context); err != nil {
		t.Fatal(err)
	}
	if len(context.CausalGroups) != 1 || !slices.Equal(context.CausalGroups[0].ArtifactBuilds, []string{"2"}) {
		t.Fatalf("context = %+v", context)
	}
}

func TestAnalysisChatPatternContextLifecycleStates(t *testing.T) {
	for _, state := range []models.PatternLifecycleState{models.PatternLifecycleRecovered, models.PatternLifecycleObserving} {
		t.Run(string(state), func(t *testing.T) {
			pattern := &models.PatternAnalysis{
				ID: "pattern", Subject: "subject", Systemic: true, Confidence: "high", Summary: "summary",
				Lifecycle: &models.PatternLifecycle{State: state, Reason: "safe reason", SourceRevision: "hidden"},
			}
			encoded, err := encodeAnalysisChatPatternContext(analysischat.Turn{Pattern: pattern})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"state": "`+string(state)+`"`) || strings.Contains(string(encoded), "hidden") {
				t.Fatalf("encoded = %s", encoded)
			}
		})
	}
}

func TestAnalysisChatPatternContextBounds(t *testing.T) {
	base := models.PatternAnalysis{ID: "pattern", Subject: "subject", Systemic: true, Confidence: "high", Summary: "summary"}
	tests := []struct {
		name   string
		mutate func(*models.PatternAnalysis)
	}{
		{name: "groups", mutate: func(pattern *models.PatternAnalysis) {
			pattern.CausalGroups = make([]models.PatternCausalGroup, analysisChatMaxPatternCausalGroups+1)
		}},
		{name: "group builds", mutate: func(pattern *models.PatternAnalysis) {
			pattern.CausalGroups = []models.PatternCausalGroup{{ID: "group", Builds: make([]string, analysisChatMaxPatternBuildsPerGroup+1)}}
		}},
		{name: "unclassified", mutate: func(pattern *models.PatternAnalysis) {
			pattern.UnclassifiedBuilds = make([]string, analysisChatMaxPatternUnclassifiedBuilds+1)
		}},
		{name: "remediation summaries", mutate: func(pattern *models.PatternAnalysis) {
			pattern.RemediationInvestigations = make([]models.PatternRemediationInvestigationSummary, analysisChatMaxPatternRemediationSummaries+1)
		}},
		{name: "total bytes", mutate: func(pattern *models.PatternAnalysis) {
			pattern.SharedRootCause = strings.Repeat("r", 32<<10)
			pattern.SuggestedFix = strings.Repeat("f", 16<<10)
			for index := 0; index < analysisChatMaxPatternCausalGroups; index++ {
				group := models.PatternCausalGroup{ID: fmt.Sprintf("g-%d", index), ContentHash: fmt.Sprintf("h-%d", index), Builds: []string{fmt.Sprintf("%d", index)}, RootCause: strings.Repeat("c", analysisChatMaxPatternRootCauseBytes), Confidence: "high"}
				pattern.CausalGroups = append(pattern.CausalGroups, group)
				pattern.RemediationInvestigations = append(pattern.RemediationInvestigations, models.PatternRemediationInvestigationSummary{CausalGroupID: group.ID, CausalGroupHash: group.ContentHash, State: models.PatternRemediationActionable, Reason: strings.Repeat("x", analysisChatMaxPatternRemediationReasonBytes)})
			}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			pattern := base
			testCase.mutate(&pattern)
			if _, err := encodeAnalysisChatPatternContext(analysischat.Turn{Pattern: &pattern}); err == nil {
				t.Fatal("oversized pattern context was accepted")
			}
		})
	}
}

func TestAnalysisChatPatternTextBoundsIncludeElisionMarker(t *testing.T) {
	value := clampAnalysisChatPatternText(strings.Repeat("x", 10<<10), analysisChatMaxPatternRootCauseBytes)
	if len(value) > analysisChatMaxPatternRootCauseBytes || !strings.Contains(value, "content elided") {
		t.Fatalf("bounded value length=%d", len(value))
	}
}

func TestAnalysisChatPatternContextKeepsGroupsWithoutArtifactSlots(t *testing.T) {
	pattern := &models.PatternAnalysis{ID: "pattern", Subject: "subject", Systemic: true, Confidence: "high", Summary: "summary"}
	for index := 0; index < 4; index++ {
		pattern.CausalGroups = append(pattern.CausalGroups, models.PatternCausalGroup{
			ID: fmt.Sprintf("group-%d", index), ContentHash: fmt.Sprintf("hash-%d", index),
			Builds:    []string{fmt.Sprintf("%d-a", index), fmt.Sprintf("%d-b", index)},
			RootCause: fmt.Sprintf("cause-%d", index), Confidence: "high",
		})
	}
	encoded, err := encodeAnalysisChatPatternContext(analysischat.Turn{
		Pattern: pattern,
		EvidenceBuilds: []analysischat.ArtifactBuild{
			{Build: models.BuildInfo{BuildID: "0-a"}},
			{Build: models.BuildInfo{BuildID: "1-a"}},
			{Build: models.BuildInfo{BuildID: "2-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var context analysisChatPatternContext
	if err := json.Unmarshal(encoded, &context); err != nil {
		t.Fatal(err)
	}
	if len(context.CausalGroups) != 4 || len(context.CausalGroups[3].ArtifactBuilds) != 0 || context.CausalGroups[3].ID != "group-3" {
		t.Fatalf("context = %+v", context.CausalGroups)
	}
}

func TestAnalysisChatArtifactEvidenceQuestionClassifier(t *testing.T) {
	tests := []struct {
		question string
		want     bool
	}{
		{question: "What artifact evidence supports this root cause?", want: true},
		{question: "Inspect the artifacts before answering.", want: true},
		{question: "Read the build log and explain the first failure.", want: true},
		{question: "Check the logs for the actual error.", want: true},
		{question: "What does the JUnit output show?", want: true},
		{question: "Re-check the artifacts for this claim.", want: true},
		{question: "Which test output supports the conclusion?", want: true},
		{question: "What build evidence supports this?", want: true},
		{question: "What does the published analysis say?", want: false},
		{question: "How do the published causal groups differ?", want: false},
		{question: "Which builds are listed in each published causal group?", want: false},
		{question: "Explain the current conclusion.", want: false},
		{question: "What evidence supports the published conclusion?", want: false},
		{question: "Which artifacts are listed in the published analysis?", want: false},
		{question: "According to the published analysis, which artifact is relevant?", want: false},
		{question: "According to the published analysis, which artifacts support the root cause?", want: false},
		{question: "Check the published analysis for which artifacts it lists.", want: false},
		{question: "What does the current analysis cite from the logs?", want: false},
		{question: "Which logs are listed in the published analysis?", want: false},
		{question: "What artifact evidence supports the published analysis?", want: true},
		{question: "According to the published analysis, re-check the artifacts.", want: true},
		{question: "Read the published analysis and then inspect the build logs.", want: true},
		{question: "Check a different hypothesis.", want: false},
	}
	for _, testCase := range tests {
		t.Run(testCase.question, func(t *testing.T) {
			if got := analysisChatQuestionRequiresArtifactEvidence(testCase.question); got != testCase.want {
				t.Fatalf("classifier = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestAnalysisChatExplicitArtifactQuestionReadsAndCitesEvidence(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	server.push(200, chatRespFinal(`{
		"answer":"The artifact records the controller stopping.",
		"citations":[{"path":"build-log.txt","quote":"controller stopped"}],
		"assessment":"supports","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "What artifact evidence supports this root cause?"
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)

	reply, err := agent.Reply(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.ToolCalls != 1 || len(reply.Citations) != 1 || reply.Citations[0].Path != "build-log.txt" {
		t.Fatalf("reply = %+v", reply)
	}
	statuses := analysisChatEvidenceTraceStatuses(store.Snapshot())
	if !slices.Contains(statuses, analysisChatEvidenceRequired) || !slices.Contains(statuses, analysisChatEvidenceSatisfied) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
}

func TestAnalysisChatArtifactPromiseGetsOneToolEnabledRepair(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(analysisChatPromiseReply()))
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	server.push(200, chatRespFinal(`{
		"answer":"The artifact contains the terminal controller error.",
		"citations":[{"path":"build-log.txt","quote":"terminal controller error"}],
		"assessment":"supports","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("terminal controller error\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "What artifact evidence supports this root cause?"
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)

	reply, err := agent.Reply(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.ToolCalls != 1 || len(reply.Citations) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	server.mu.Lock()
	requests := append([][]byte(nil), server.requests...)
	server.mu.Unlock()
	if len(requests) != 3 || !strings.Contains(string(requests[1]), analysisChatEvidenceRepairPrompt) ||
		!strings.Contains(string(requests[1]), `"tools"`) || strings.Contains(string(requests[1]), `"response_format"`) {
		t.Fatalf("repair request = %s", requests[1])
	}
	snapshot := store.Snapshot()
	statuses := analysisChatEvidenceTraceStatuses(snapshot)
	if !slices.Contains(statuses, analysisChatEvidenceRepairStarted) || !slices.Contains(statuses, analysisChatEvidenceRepairSatisfied) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{analysisChatEvidenceRepairPrompt, "terminal controller error", analysisChatPromiseReply()} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("private telemetry retained provider or evidence content %q", forbidden)
		}
	}
}

func TestAnalysisChatSecondArtifactFreeFinalizationFailsSafely(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(analysisChatPromiseReply()))
	server.push(200, chatRespFinal(analysisChatPromiseReply()))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "What artifact evidence supports this root cause?"
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)

	_, err := agent.Reply(ctx, turn)
	if !errors.Is(err, analysischat.ErrCitationValidationFailed) {
		t.Fatalf("error = %v", err)
	}
	trace.Finish("error", err)
	if got := atomic.LoadInt32(&server.calls); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	statuses := analysisChatEvidenceTraceStatuses(store.Snapshot())
	if !slices.Contains(statuses, analysisChatEvidenceRepairStarted) || !slices.Contains(statuses, analysisChatEvidenceRepairMissing) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
}

func TestAnalysisChatEmptyOrFailedArtifactDoesNotSatisfyRepair(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		browser artifacts.Browser
	}{
		{
			name: "failed",
			browser: &trackingBrowser{fakeBrowser: &fakeBrowser{}, tailErrors: map[string]error{
				"build-log.txt": errors.New("unavailable"),
			}},
		},
		{
			name: "empty",
			browser: &trackingBrowser{fakeBrowser: &fakeBrowser{}, emptyTails: map[string]bool{
				"build-log.txt": true,
			}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			shrinkCallDelay(t)
			server := newScriptedChatServer(t)
			server.push(200, chatRespFinal(analysisChatPromiseReply()))
			server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
			agent := newAnalysisChatAgentForTest(t, server.URL, testCase.browser, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})
			turn := analysisChatTurn()
			turn.Question = "Re-check the artifacts before answering."
			store := NewTraceStore()
			trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
			ctx := withAnalysisTrace(t.Context(), trace)

			_, err := agent.Reply(ctx, turn)
			if !errors.Is(err, analysischat.ErrCitationValidationFailed) {
				t.Fatalf("error = %v", err)
			}
			trace.Finish("error", err)
			statuses := analysisChatEvidenceTraceStatuses(store.Snapshot())
			if !slices.Contains(statuses, analysisChatEvidenceNoContent) || !slices.Contains(statuses, analysisChatEvidenceRepairMissing) {
				t.Fatalf("evidence statuses = %v", statuses)
			}
		})
	}
}

func TestAnalysisChatArtifactEvidenceWithEmptyCitationsFailsValidation(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	invalid := `{"answer":"The artifact supports it.","citations":[],"assessment":"inconclusive","proposed_revision":null}`
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "What artifact evidence supports this root cause?"

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)
	_, err := agent.Reply(ctx, turn)
	if !errors.Is(err, analysischat.ErrCitationValidationFailed) {
		t.Fatalf("error = %v", err)
	}
	trace.Finish("error", err)
	if statuses := analysisChatEvidenceTraceStatuses(store.Snapshot()); !slices.Contains(statuses, analysisChatEvidenceCitationFailed) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
}

func TestAnalysisChatArtifactCitationMustMatchCurrentTurnEvidence(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	invalid := `{"answer":"The artifact supports it.","citations":[{"path":"build-log.txt","quote":"different evidence"}],"assessment":"supports","proposed_revision":null}`
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Read the artifact evidence for this root cause."

	_, err := agent.Reply(t.Context(), turn)
	if !errors.Is(err, analysischat.ErrCitationValidationFailed) {
		t.Fatalf("error = %v", err)
	}
}

func TestAnalysisChatPublishedPatternMembershipNeedsNoArtifactRead(t *testing.T) {
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{
		"answer":"Group A contains builds 104 and 103; Group B contains build 102.",
		"citations":[],"assessment":"explains","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Pattern = &models.PatternAnalysis{
		ID: "pattern", Subject: "failures", Systemic: true,
		CausalGroups: []models.PatternCausalGroup{
			{ID: "a", Builds: []string{"104", "103"}, RootCause: "cause a", Confidence: "high"},
			{ID: "b", Builds: []string{"102"}, RootCause: "cause b", Confidence: "medium"},
		},
	}
	turn.EvidenceBuilds = []analysischat.ArtifactBuild{{Build: models.BuildInfo{BuildID: "104"}}}
	turn.Question = "Which builds are listed in each published causal group?"

	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToolCalls != 0 || len(reply.Citations) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatRecheckArtifactsRepairsThroughEvidence(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(analysisChatPromiseReply()))
	server.push(200, chatRespToolCall("call-1", "grep_artifact", map[string]interface{}{
		"path": "build-log.txt", "pattern": "controller stopped", "max_matches": 10,
	}))
	server.push(200, chatRespFinal(`{
		"answer":"The build log records the controller stopping.",
		"citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"the controller exited"}],
		"assessment":"supports","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Re-check the artifacts and cite what supports the root cause."

	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToolCalls != 1 || len(reply.Citations) != 1 || reply.Citations[0].LineStart != 1 ||
		reply.Citations[0].Quote != "controller stopped" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatPatternArtifactCitationKeepsBuildPrefix(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "grep_artifact", map[string]interface{}{
		"path": "builds/104/build-log.txt", "pattern": "group failure", "max_matches": 10,
	}))
	server.push(200, chatRespFinal(`{
		"answer":"Build 104 records the group failure.",
		"citations":[{"path":"builds/104/build-log.txt","line_start":1,"line_end":1,"quote":"the group failed"}],
		"assessment":"supports","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"builds/104/build-log.txt": []byte("group failure\n"),
	}}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Pattern = &models.PatternAnalysis{ID: "pattern", Subject: "failures", Systemic: true}
	turn.EvidenceBuilds = []analysischat.ArtifactBuild{{Build: models.BuildInfo{BuildID: "104"}}}
	turn.Question = "What artifact evidence supports this causal group?"

	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Citations) != 1 || reply.Citations[0].Path != "builds/104/build-log.txt" ||
		reply.Citations[0].Quote != "group failure" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatEvidenceRepairPreservesChatAndResponsesHistory(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			providerInitial := []json.RawMessage{json.RawMessage(`{"type":"message","id":"message-1","role":"assistant","content":[{"type":"output_text","text":"promise"}]}`)}
			providerTool := []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"read_artifact","arguments":"{\"path\":\"build-log.txt\",\"offset\":0,\"length\":1024}"}`)}
			transport := &scriptedTransport{results: []scriptedTransportResult{
				{response: &modelResponse{
					HasMessage: true, Attempts: 1,
					Message: modelMessage{Role: "assistant", Content: strPtr(analysisChatPromiseReply()), ProviderItems: providerInitial},
				}},
				{response: &modelResponse{
					HasMessage: true, Attempts: 1,
					Message: modelMessage{Role: "assistant", ProviderItems: providerTool, ToolCalls: []modelToolCall{{
						ID: "call-1", Type: "function",
						Function: modelFunction{Name: "read_artifact", Arguments: `{"path":"build-log.txt","offset":0,"length":1024}`},
					}}},
				}},
				{response: &modelResponse{
					HasMessage: true, Attempts: 1,
					Message: modelMessage{Role: "assistant", Content: strPtr(`{"answer":"The artifact records the failure.","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"assessment":"supports","proposed_revision":null}`)},
				}},
			}}
			client := &Client{model: "model", apiMode: apiMode, transport: transport}
			registry, enabled := newTestRegistry(t)
			agent, err := NewAnalysisChatAgent(
				client, ComposeAnalysisChatSystemPrompt("project"), registry, enabled,
				&fixedBrowserFactory{browser: &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}},
				AnalysisChatOptions{MaxIters: 2, Timeout: time.Second},
			)
			if err != nil {
				t.Fatal(err)
			}
			turn := analysisChatTurn()
			turn.Question = "What artifact evidence supports this root cause?"

			if _, err := agent.Reply(t.Context(), turn); err != nil {
				t.Fatal(err)
			}
			if len(transport.requests) != 3 {
				t.Fatalf("requests = %d", len(transport.requests))
			}
			if !analysisChatMessagesContainText(transport.requests[1].Messages, analysisChatEvidenceRepairPrompt) ||
				!analysisChatMessagesContainProviderItem(transport.requests[1].Messages, "message-1") ||
				!analysisChatMessagesContainProviderItem(transport.requests[2].Messages, "call-1") ||
				!analysisChatMessagesContainToolResult(transport.requests[2].Messages, "call-1") {
				t.Fatalf("history was not preserved: %+v", transport.requests)
			}
		})
	}
}

func TestPrepareAnalysisChatEvidenceRepairMessagesRespectsContextBudget(t *testing.T) {
	toolContent := strings.Repeat("x", 12<<10)
	messages := []modelMessage{
		{Role: "system", Content: strPtr("system")},
		{Role: "user", Content: strPtr("question")},
		{Role: "assistant", ToolCalls: []modelToolCall{{ID: "call-1", Type: "function", Function: modelFunction{Name: "list_artifacts", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "call-1", Content: &toolContent},
	}
	budget := requestSizeEstimate(messages, 0) + len(analysisChatEvidenceRepairPrompt)/2
	prepared, err := prepareAnalysisChatEvidenceRepairMessages(messages, nil, 0, budget)
	if err != nil {
		t.Fatal(err)
	}
	if requestSizeEstimate(prepared, 0) > budget || !analysisChatMessagesContainText(prepared, analysisChatEvidenceRepairPrompt) {
		t.Fatalf("prepared messages exceed budget: %+v", prepared)
	}
}

func analysisChatPromiseReply() string {
	return `{"answer":"Let me pull the key artifacts to show the evidence supporting the root cause.","citations":[],"assessment":null,"proposed_revision":null}`
}

func analysisChatEvidenceTraceStatuses(snapshot AnalysisTraceFile) []string {
	if len(snapshot.Traces) == 0 {
		return nil
	}
	var statuses []string
	for _, event := range snapshot.Traces[0].Events {
		if event.Kind == "analysis_chat_evidence" {
			statuses = append(statuses, event.Status)
		}
	}
	return statuses
}

func analysisChatMessagesContainText(messages []modelMessage, text string) bool {
	for _, message := range messages {
		if message.Content != nil && strings.Contains(*message.Content, text) {
			return true
		}
	}
	return false
}

func analysisChatMessagesContainProviderItem(messages []modelMessage, marker string) bool {
	for _, message := range messages {
		for _, item := range message.ProviderItems {
			if strings.Contains(string(item), marker) {
				return true
			}
		}
	}
	return false
}

func analysisChatMessagesContainToolResult(messages []modelMessage, callID string) bool {
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == callID && message.Content != nil {
			return true
		}
	}
	return false
}
