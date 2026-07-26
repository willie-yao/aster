package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

	reply, err := agent.Reply(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Assessment != "explains" || reply.ToolCalls != 0 {
		t.Fatalf("reply = %+v", reply)
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
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	server.push(200, chatRespFinal(`{
		"answer":"After reading the log, the controller exit supports the analysis.","assessment":"supports",
		"citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"controller stopped"}],
		"proposed_revision":null
	}`))
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 4, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToolCalls != 1 || reply.Assessment != "supports" {
		t.Fatalf("reply = %+v", reply)
	}
	server.mu.Lock()
	requests := append([][]byte(nil), server.requests...)
	server.mu.Unlock()
	if len(requests) < 2 || !strings.Contains(string(requests[1]), "artifact not read during this turn") {
		t.Fatalf("repair prompt missing from second request: %s", requests[1])
	}
}

func TestAnalysisChatAgentKeepsValidDraftWhenFinalizeIsInvalid(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	valid := `{"answer":"The existing conclusion remains plausible.","assessment":"explains","citations":[],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(valid, "call-1", "list_artifacts", map[string]interface{}{"path": ""}))
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "The existing conclusion remains plausible." || reply.ToolCalls != 1 {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatAgentKeepsValidDraftWhenFinalizeRequestFails(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	valid := `{"answer":"The existing conclusion remains plausible.","assessment":"explains","citations":[],"proposed_revision":null}`
	server.push(200, chatRespToolCallWithContent(valid, "call-1", "list_artifacts", map[string]interface{}{"path": ""}))
	server.push(500, `private provider body`)
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 1, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "The existing conclusion remains plausible." || reply.ToolCalls != 1 {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAnalysisChatAgentReturnsSafeValidationCategory(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{"answer":"unfinished"`))
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
	trace.Finish("error", analysischat.ErrCitationValidationFailed)

	snapshot := store.Snapshot()
	if len(snapshot.Traces) != 1 || len(snapshot.Traces[0].Events) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	event := snapshot.Traces[0].Events[0]
	if event.Kind != "analysis_chat_response" || event.Status != "finalize_validation" || event.ModelCallCount != 9 ||
		event.Attempts != 11 || event.HTTPStatus != 200 || event.CandidateCount != 4 || event.ErrorCode != analysisChatValidationCitation {
		t.Fatalf("event = %+v", event)
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
		want string
	}{
		{name: "fenced", raw: "```json\n" + valid + "\n```", want: "valid answer"},
		{name: "metadata wrapper", raw: `{"metadata":{"finish_reason":"stop"},"result":` + valid + `}`, want: "valid answer"},
		{name: "final valid draft", raw: valid + `\n{"answer":"final answer","assessment":"explains","citations":[],"proposed_revision":null}`, want: "final answer"},
		{name: "malformed final draft", raw: valid + `\n{"answer":"unfinished"`, want: "valid answer"},
		{name: "nested object in malformed final draft", raw: valid + `\n{"answer":"unfinished","citations":[{"path":"x"}]`, want: "valid answer"},
		{name: "quoted braces", raw: `{"answer":"value with {nested text}","assessment":"explains","citations":[],"proposed_revision":null}`, want: "value with {nested text}"},
		{name: "quoted prose brace", raw: `The token "{" was emitted. ` + valid, want: "valid answer"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reply, stats, err := parseAnalysisChatReplyCandidates(testCase.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Answer != testCase.want || stats.CandidateCount == 0 {
				t.Fatalf("reply=%+v stats=%+v", reply, stats)
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

func TestParseAnalysisChatReplyKeepsEarlierValidCitation(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	valid := `{"answer":"supported","assessment":"supports","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":null}`
	invalid := `{"answer":"bad update","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	reply, _, err := parseAnalysisChatReplyCandidates(valid+"\n"+invalid, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "supported" {
		t.Fatalf("reply = %+v", reply)
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

func TestComposeAnalysisChatSystemPromptKeepsSeparateSchema(t *testing.T) {
	prompt := ComposeAnalysisChatSystemPrompt("Consumer fact.")
	for _, want := range []string{"Consumer fact.", "published AI analysis is a hypothesis", `"assessment": "explains"`} {
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
	invalid := `{"answer":"The controller stopped.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":41,"line_end":41,"quote":"controller stopped"}],"proposed_revision":null}`
	if _, err := parseAnalysisChatReply(invalid, evidence); err == nil {
		t.Fatal("fabricated line range was accepted")
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
	if analysisChatQuoteInRange(evidence.Lines, 10, 12, "first snippet\nsecond snippet") {
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

func TestParseAnalysisChatReplySelectsFinalDraft(t *testing.T) {
	raw := `{"answer":"first","assessment":"explains","citations":[],"proposed_revision":null}` +
		`{"answer":"second","assessment":"explains","citations":[],"proposed_revision":null}`
	reply, err := parseAnalysisChatReply(raw, nil)
	if err != nil || reply.Answer != "second" {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
}

func TestParseAnalysisChatReplyRejectsTrailingUnrelatedJSON(t *testing.T) {
	raw := `{"answer":"first","assessment":"explains","citations":[],"proposed_revision":null}` +
		`{"unrelated":true}`
	if _, err := parseAnalysisChatReply(raw, nil); err == nil || !strings.Contains(err.Error(), "trailing unrelated JSON") {
		t.Fatalf("trailing response error = %v", err)
	}
}

func TestAnalysisChatJSONCandidatesAreBounded(t *testing.T) {
	raw := strings.Repeat("{not-json}", analysisChatMaxCandidates+20) +
		`{"answer":"final","assessment":"explains","citations":[],"proposed_revision":null}`
	candidates := analysisChatJSONCandidates(raw)
	if len(candidates) != analysisChatMaxCandidates {
		t.Fatalf("candidate count = %d", len(candidates))
	}
	reply, err := parseAnalysisChatReply(raw, nil)
	if err != nil || reply.Answer != "final" {
		t.Fatalf("reply=%+v err=%v", reply, err)
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
	for _, want := range []string{"recurring-pattern analysis", `"pattern_id": "pattern-1"`, `"artifact_builds"`, "builds/<build-id>/<path>"} {
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
