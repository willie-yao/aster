package ai

import (
	"context"
	"encoding/json"
	"errors"
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
	}}, `{"matches":[{"line":42,"context":["  41: before","> 42: controller stopped","  43: after"]}]}`)
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
