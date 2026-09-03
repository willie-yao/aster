package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/models"
)

type fixedBrowserFactory struct {
	browser     artifacts.Browser
	prefix      string
	displayName string
}

type countingSourceReader struct {
	lists int
	reads int
}

func (r *countingSourceReader) ListTree(context.Context) ([]string, error) {
	r.lists++
	return nil, nil
}

func (r *countingSourceReader) ReadFile(context.Context, string) (string, bool, error) {
	r.reads++
	return "", false, nil
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

func newAnalysisChatAgentWithRepoToolsForTest(t *testing.T, serverURL string, browser artifacts.Browser, opts AnalysisChatOptions) *AnalysisChatAgent {
	t.Helper()
	registry, _ := newTestRegistry(t)
	repotree.Register(registry)
	enabled, err := registry.Enable([]string{"filesystem", "repotree"})
	if err != nil {
		t.Fatal(err)
	}
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

func causeAnalysisChatTurn() analysischat.Turn {
	turn := analysisChatTurn()
	turn.Scope = analysischat.ScopeCause
	turn.Pattern = &models.PatternAnalysis{ID: "pattern-1", Subject: "controller stopped", Systemic: true}
	turn.Build.RepoRefs = map[string]string{
		"kubernetes-sigs/cluster-api-provider-azure": "0123456789abcdef0123456789abcdef01234567",
	}
	turn.EvidenceBuilds = []analysischat.ArtifactBuild{{BuildPrefix: turn.BuildPrefix, Build: turn.Build}}
	return turn
}

// analysisChatReplyVerified reports whether a parse produced an answer whose
// citations passed verification.
func analysisChatReplyVerified(reply analysischat.Reply, err error) bool {
	return err == nil && !reply.Unverified
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

func TestAnalysisChatAgentAcceptsMixedReplyWithoutCorrectiveRound(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{
		"path": "builds/123/build-log.txt", "offset": 0, "length": 1024,
	}))
	server.push(200, chatRespFinal(`{
		"answer":"The artifact proves the controller stopped, but the source attribution is not verified.",
		"assessment":"challenges",
		"citations":[
			{"repository":null,"revision":null,"path":"builds/123/build-log.txt","line_start":1,"line_end":1,"quote":"controller stopped"},
			{"repository":"kubernetes-sigs/cluster-api-provider-azure","revision":"0123456789abcdef0123456789abcdef01234567","path":"controllers/cluster_controller.go","line_start":10,"line_end":10,"quote":"return err"}
		],
		"proposed_revision":{"root_cause":"The controller stopped before completing reconciliation.","suggested_fix":"Correct the controller failure and rerun."}
	}`))
	agent := newAnalysisChatAgentWithRepoToolsForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"builds/123/build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{
		MaxIters: 3, Timeout: time.Second,
		SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
	})

	reply, err := agent.Reply(t.Context(), causeAnalysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.ValidationRetries != 0 || reply.Assessment != "challenges" || reply.ProposedRevision != nil {
		t.Fatalf("reply = %+v", reply)
	}
	if len(reply.Citations) != 1 || reply.Citations[0].Path != "builds/123/build-log.txt" || len(reply.EvidenceWarnings) != 2 ||
		!strings.Contains(strings.Join(reply.EvidenceWarnings, " "), "current tested-branch source revision was unavailable") {
		t.Fatalf("reply evidence = %+v", reply)
	}
	if len(server.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(server.requests))
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
	if len(requests) < 2 || !strings.Contains(string(requests[1]), "artifact not read during this conversation") ||
		!strings.Contains(string(requests[1]), `"tools"`) {
		t.Fatalf("repair prompt missing from second request: %s", requests[1])
	}
}

func TestAnalysisChatAgentKeepsValidatedCitedDraftWhenFinalizeIsInvalid(t *testing.T) {
	shrinkCallDelay(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
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
	// The finalize stage carries the rule its own candidates tripped.
	if !strings.Contains(logs.String(), `validation_detail="no JSON response object found"`) {
		t.Fatalf("finalize log missing the specific rule: %s", logs.String())
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

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatalf("Reply error = %v", err)
	}
	// The stale draft must not be the answer, and what survives is unverified.
	if strings.Contains(reply.Answer, "The first log supports") || !reply.Unverified {
		t.Fatalf("stale draft was returned: %+v", reply)
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

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatalf("Reply error = %v", err)
	}
	// A draft whose citations never verified must not become the answer.
	if strings.Contains(reply.Answer, "The log supports the conclusion.") || !reply.Unverified {
		t.Fatalf("unverifiable draft was returned: %+v", reply)
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

// Only a closed engine-owned gate reaches the owner, never the text the model
// or the provider produced.
func TestAnalysisChatSafeValidationErrorCarriesNoModelText(t *testing.T) {
	err := analysisChatSafeValidationError(newAnalysisChatValidationError(
		analysisChatValidationJSON, errors.New("sentinel-model-text"),
	))
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("error = %v", err)
	}
	gate, ok := analysischat.ValidationGateOf(err)
	if !ok || gate != analysischat.GateJSON {
		t.Fatalf("gate = %q ok = %t", gate, ok)
	}
	if strings.Contains(err.Error(), "sentinel-model-text") {
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

func TestAnalysisChatDecodeErrorNamesTheAllowedNestedKeys(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{}
	// The live failure: the model guessed a key for the artifact path.
	_, _, err := parseAnalysisChatReplyCandidates(
		`{"answer":"x","citations":[{"artifact_path":"build-log.txt","quote":"boom"}],"assessment":null,"proposed_revision":null}`,
		evidence,
	)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if got := analysisChatValidationCategory(err); got != analysisChatValidationContract {
		t.Fatalf("category = %q", got)
	}
	if !strings.Contains(err.Error(), "path, line_start, line_end, and quote") {
		t.Fatalf("error does not name the allowed citation keys: %v", err)
	}
	if strings.Contains(err.Error(), "artifact_path") {
		t.Fatalf("error echoed the model-chosen key: %v", err)
	}
	// The same decoder error is raised by proposed_revision, so the message
	// must not blame a citation for it.
	_, _, revisionErr := parseAnalysisChatReplyCandidates(
		`{"answer":"x","citations":[],"assessment":"challenges","proposed_revision":{"root_cause":"r","suggested_fix":"f","extra":"e"}}`,
		evidence,
	)
	if revisionErr == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(revisionErr.Error(), "root_cause and suggested_fix") {
		t.Fatalf("error does not name the allowed revision keys: %v", revisionErr)
	}
}

func TestAnalysisChatDecodeErrorReportsWrongFieldType(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{}
	for name, raw := range map[string]string{
		"citations":         `{"answer":"x","citations":"build-log.txt","assessment":null,"proposed_revision":null}`,
		"assessment":        `{"answer":"x","citations":[],"assessment":5,"proposed_revision":null}`,
		"proposed_revision": `{"answer":"x","citations":[],"assessment":"challenges","proposed_revision":"nope"}`,
	} {
		_, _, err := parseAnalysisChatReplyCandidates(raw, evidence)
		if err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
		// The guidance must state the whole contract, since the single
		// corrective round relays it verbatim.
		for _, want := range []string{
			"answer is a string",
			"citations is an array",
			"line_start and line_end are integers or null",
			"assessment is a string or null",
			"proposed_revision is null or an object",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: guidance missing %q: %v", name, want, err)
			}
		}
	}
	// A null line range is valid, so the guidance must not promise otherwise.
	_, _, err := parseAnalysisChatReplyCandidates(
		`{"answer":"x","citations":[{"path":"p","quote":"q","line_start":null,"line_end":null}],"assessment":null,"proposed_revision":null}`,
		evidence,
	)
	if err != nil {
		t.Fatalf("null line range rejected: %v", err)
	}
}

func TestAnalysisChatPromptShowsTheCitationShape(t *testing.T) {
	for _, key := range []string{`"repository"`, `"revision"`, `"path"`, `"line_start"`, `"line_end"`, `"quote"`} {
		if !strings.Contains(analysisChatResponseFormat, key) {
			t.Fatalf("prompt does not name the citation key %s", key)
		}
	}
	if !strings.Contains(analysisChatResponseFormat, "only the keys repository, revision, path, line_start") {
		t.Fatal("prompt does not close the citation key set")
	}
	if !strings.Contains(analysisChatResponseFormat, "empty citations array") {
		t.Fatal("prompt does not say an uncited answer uses an empty array")
	}
	for _, want := range []string{
		"Artifact citations use null repository and",
		"canonical owner/repo and full revision",
		"grep_repo locates code",
		"Call read_repo_file",
		"Only read_repo_file provides\nauthoritative source coordinates",
		"Choose a narrow sub-range inside its returned",
		"set quote to exactly the text from that cited sub-range",
	} {
		if !strings.Contains(analysisChatResponseFormat, want) {
			t.Fatalf("prompt missing source citation rule %q", want)
		}
	}
	schema, err := json.Marshal(analysisChatStructuredFormat().Schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"additionalProperties":false`, `"repository"`, `"revision"`, `"required":["repository","revision","path","line_start","line_end","quote"]`} {
		if !bytes.Contains(schema, []byte(want)) {
			t.Fatalf("structured citation schema missing %s: %s", want, schema)
		}
	}
}

func TestAnalysisChatResponseLogsValidationDetail(t *testing.T) {
	shrinkCallDelay(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	// A descriptive answer that omits citations entirely trips the contract.
	uncited := `{"answer":"sentinel-answer-text","assessment":"inconclusive"}`
	for range 4 {
		server.push(200, chatRespFinal(uncited))
	}
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("controller stopped\n")}}
	agent := newAnalysisChatAgentForTest(t, server.URL, browser, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatalf("Reply error = %v", err)
	}
	// The answer survives as unverified, and the log still names the rule it
	// tripped without echoing the model's text.
	if reply.Answer != "sentinel-answer-text" || reply.UnverifiedReason != analysischat.UnverifiedFormat {
		t.Fatalf("reply = %+v", reply)
	}
	if !strings.Contains(logs.String(), `validation=response_contract`) {
		t.Fatalf("log missing the validation category: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `validation_detail="response requires answer and citations"`) {
		t.Fatalf("log missing the specific contract rule: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "outcome=degraded") {
		t.Fatalf("log missing the degraded outcome: %s", logs.String())
	}
	if strings.Contains(logs.String(), "sentinel-answer-text") {
		t.Fatalf("validation detail leaked model content: %s", logs.String())
	}
}

func TestAnalysisChatResponseOmitsEmptyValidationDetail(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	recordAnalysisChatResponseFailure(
		context.Background(), "tool_loop_request", 1, 1, &modelResponse{HTTPStatus: 500},
		analysisChatParseStats{}, "provider_request",
	)
	if strings.Contains(logs.String(), "validation_detail") {
		t.Fatalf("empty detail should be omitted: %s", logs.String())
	}
}

func TestAnalysisChatResponseOmitsStaleValidationDetail(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// A provider failure after a rejected candidate reports its own category
	// while the earlier candidate's detail is still in stats.
	recordAnalysisChatResponseFailure(
		context.Background(), "finalize_request", 3, 3, &modelResponse{HTTPStatus: 200},
		analysisChatParseStats{
			Category:         analysisChatValidationContract,
			ValidationDetail: "response requires answer and citations",
		},
		"provider_request",
	)
	if strings.Contains(logs.String(), "validation_detail") {
		t.Fatalf("stale detail attributed to another category: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "validation=provider_request") {
		t.Fatalf("log missing the reported category: %s", logs.String())
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
	trace.Finish("error", analysischat.ErrResponseValidationFailed)

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
	if got := modelResponseAttempts(nil); got != 0 {
		t.Fatalf("nil response attempts = %d", got)
	}
	if got := modelResponseAttempts(&modelResponse{}); got != 1 {
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
		if analysisChatReplyVerified(parseAnalysisChatReply(raw, map[string]*analysisChatEvidence{"build-log.txt": {Segments: []string{"controller stopped"}}})) {
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
	invalid := `{"answer":"bad update","assessment":"maybe","citations":[],"proposed_revision":null}`
	_, stats, err := parseAnalysisChatReplyCandidates(valid+"\n"+invalid, evidence)
	if err == nil || stats.Category != analysisChatValidationContract {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestParseAnalysisChatReplyCategorizesCitationMismatch(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	raw := `{"answer":"bad update","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Category != analysisChatValidationCitation || stats.EvidenceGate != analysischat.UnverifiedCitation || stats.EvidenceDetail == "" {
		t.Fatalf("stats=%+v", stats)
	}
	if !reply.Unverified || reply.UnverifiedReason != analysischat.UnverifiedCitation ||
		reply.Assessment != "inconclusive" || len(reply.Citations) != 0 {
		t.Fatalf("reply=%+v", reply)
	}
}

func TestAnalysisChatRepoReadPublishesRecordedSourceLineRange(t *testing.T) {
	const (
		path     = "pkg/controller.go"
		revision = "0123456789abcdef0123456789abcdef01234567"
	)
	content := "partial leading line\ncomplete line two\ncomplete line three\npartial trailing line"
	offset := strings.Index(content, "leading")
	end := strings.Index(content, " trailing")
	length := end - offset
	requested := content[offset:end]

	registry := tools.NewRegistry()
	repotree.Register(registry)
	enabled, err := registry.Enable([]string{"repotree"})
	if err != nil {
		t.Fatal(err)
	}
	primary := tools.RepoSource{
		ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: revision,
		Reader: &fakeSourceRepo{files: map[string]string{path: content}},
	}
	state := &agentState{
		sources:  testSourceCatalog(t, tools.PrimarySourceID, primary),
		registry: registry, enabledTools: enabled, cache: tools.NewCache(),
		opts: AgenticOptions{ModelByteBudget: 100_000, GCSByteBudget: 100_000}, startTime: time.Now(),
		sourceEvidenceByPath: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{},
	}
	arguments, err := json.Marshal(map[string]interface{}{
		"source_id": tools.PrimarySourceID, "path": path, "offset": offset, "length": length,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := dispatchAgenticTool(t.Context(), state, modelToolCall{
		ID: "repo-read", Type: "function",
		Function: modelFunction{Name: "read_repo_file", Arguments: string(arguments)},
	})
	visible := modelVisibleToolPayload(envelope)
	if visible == nil {
		t.Fatalf("model-visible payload is not JSON: %q", envelope)
	}
	if visible["offset"] != float64(offset) || visible["length"] != float64(length) || visible["content"] != requested {
		t.Fatalf("visible raw range = offset %v length %v content %q, want %d %d %q", visible["offset"], visible["length"], visible["content"], offset, length, requested)
	}
	lineStart, startOK := visible["line_start"].(float64)
	lineEnd, endOK := visible["line_end"].(float64)
	if !startOK || !endOK || lineStart != 2 || lineEnd != 3 {
		t.Fatalf("visible line range = %v-%v, want 2-3", visible["line_start"], visible["line_end"])
	}

	evidence := state.sourceEvidenceByPath[analysisChatSourceEvidenceKey{SourceID: tools.PrimarySourceID, Path: path}]
	if evidence == nil {
		t.Fatal("source evidence was not recorded")
	}
	if len(evidence.Lines) != 2 || evidence.Lines[2] != "complete line two" || evidence.Lines[3] != "complete line three" {
		t.Fatalf("recorded source lines = %+v", evidence.Lines)
	}
	if _, ok := evidence.Lines[1]; ok {
		t.Fatalf("partial leading line became line-addressable: %+v", evidence.Lines)
	}
	if _, ok := evidence.Lines[4]; ok {
		t.Fatalf("partial trailing line became line-addressable: %+v", evidence.Lines)
	}

	citation := analysischat.Citation{
		Repository: "example/project", Revision: revision, Path: path,
		LineStart: int(lineStart), LineEnd: int(lineStart), Quote: evidence.Lines[int(lineStart)],
	}
	source := &analysisChatSourceCitationContext{Catalog: state.sources, Evidence: state.sourceEvidenceByPath}
	if failure := validateAnalysisChatCitation(&citation, nil, source, 1); failure != nil {
		t.Fatalf("published line citation did not validate: %+v", failure)
	}
	if citation.Quote != "complete line two" || citation.LineStart != 2 || citation.LineEnd != 2 {
		t.Fatalf("validated citation = %+v", citation)
	}
}

func TestAnalysisChatSourceCitationValidatesRecordedCurrentTurnBytes(t *testing.T) {
	reader := &countingSourceReader{}
	revision := "0123456789abcdef0123456789abcdef01234567"
	catalog := testSourceCatalog(t, tools.PrimarySourceID, tools.RepoSource{
		ID: tools.PrimarySourceID, Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure",
		Revision: revision, Reader: reader,
	})
	source := &analysisChatSourceCitationContext{
		Catalog: catalog,
		Evidence: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{
			{SourceID: tools.PrimarySourceID, Path: "pkg/controller.go"}: {
				Segments: []string{"if err != nil {\nreturn err\n}"},
				Lines:    map[int]string{10: "if err != nil {", 11: "return err"},
			},
		},
	}
	raw := `{"answer":"pkg/controller.go:10-11 returns the reconciliation error.","citations":[{"repository":"KUBERNETES-SIGS/CLUSTER-API-PROVIDER-AZURE","revision":"` + revision + `","path":"./pkg/controller.go","line_start":10,"line_end":11,"quote":"return err"}],"assessment":"supports","proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidatesWithSource(raw, nil, source)
	if err != nil || stats.EvidenceGate != "" || reply.Unverified || len(reply.Citations) != 1 {
		t.Fatalf("reply=%+v stats=%+v err=%v", reply, stats, err)
	}
	citation := reply.Citations[0]
	if citation.Repository != "kubernetes-sigs/cluster-api-provider-azure" || citation.Revision != revision ||
		citation.Path != "pkg/controller.go" || citation.LineStart != 10 || citation.LineEnd != 11 {
		t.Fatalf("citation = %+v", citation)
	}
	if reader.lists != 0 || reader.reads != 0 {
		t.Fatalf("citation validation accessed source reader: lists=%d reads=%d", reader.lists, reader.reads)
	}

	base := analysischat.Citation{
		Repository: "kubernetes-sigs/cluster-api-provider-azure", Revision: revision,
		Path: "pkg/controller.go", LineStart: 10, LineEnd: 11, Quote: "return err",
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*analysischat.Citation)
	}{
		{name: "missing revision", mutate: func(c *analysischat.Citation) { c.Revision = "" }},
		{name: "repository mismatch", mutate: func(c *analysischat.Citation) { c.Repository = "other/repo" }},
		{name: "revision mismatch", mutate: func(c *analysischat.Citation) { c.Revision = strings.Repeat("f", 40) }},
		{name: "path mismatch", mutate: func(c *analysischat.Citation) { c.Path = "pkg/other.go" }},
		{name: "quote mismatch", mutate: func(c *analysischat.Citation) { c.Quote = "different text" }},
		{name: "line mismatch", mutate: func(c *analysischat.Citation) { c.LineStart, c.LineEnd = 12, 12 }},
		{name: "partial line range", mutate: func(c *analysischat.Citation) { c.LineStart = 0 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			citation := base
			testCase.mutate(&citation)
			if failure := validateAnalysisChatCitation(&citation, nil, source, 1); failure == nil {
				t.Fatalf("citation unexpectedly verified: %+v", citation)
			}
		})
	}
	if reader.lists != 0 || reader.reads != 0 {
		t.Fatalf("mismatch validation accessed source reader: lists=%d reads=%d", reader.lists, reader.reads)
	}
}

func TestAnalysisChatGrepSourceEvidenceIsQuoteOnly(t *testing.T) {
	revision := strings.Repeat("1", 40)
	catalog := testSourceCatalog(t, tools.PrimarySourceID, tools.RepoSource{
		ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: revision, Reader: &countingSourceReader{},
	})
	source := &analysisChatSourceCitationContext{
		Catalog: catalog,
		Evidence: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{
			{SourceID: tools.PrimarySourceID, Path: "pkg/controller.go"}: {Segments: []string{"func reconcile() error"}, Lines: map[int]string{}},
		},
	}
	quoteOnly := `{"answer":"The source declares reconcile.","citations":[{"repository":"example/project","revision":"` + revision + `","path":"pkg/controller.go","line_start":null,"line_end":null,"quote":"func reconcile"}],"assessment":"supports","proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidatesWithSource(quoteOnly, nil, source)
	if err != nil || stats.EvidenceGate != "" || len(reply.Citations) != 1 || reply.Citations[0].LineStart != 0 {
		t.Fatalf("quote-only reply=%+v stats=%+v err=%v", reply, stats, err)
	}
	withLines := `{"answer":"The source declares reconcile.","citations":[{"repository":"example/project","revision":"` + revision + `","path":"pkg/controller.go","line_start":7,"line_end":7,"quote":"func reconcile"}],"assessment":"supports","proposed_revision":null}`
	reply, stats, err = parseAnalysisChatReplyCandidatesWithSource(withLines, nil, source)
	if err != nil || !reply.Unverified || stats.EvidenceGate != analysischat.UnverifiedCitation {
		t.Fatalf("line-ranged grep reply=%+v stats=%+v err=%v", reply, stats, err)
	}
}

func TestAnalysisChatSourceLineClaimsRequireMatchingVerifiedRange(t *testing.T) {
	revision := strings.Repeat("2", 40)
	catalog := testSourceCatalog(t, tools.PrimarySourceID, tools.RepoSource{
		ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: revision, Reader: &countingSourceReader{},
	})
	source := &analysisChatSourceCitationContext{
		Catalog: catalog,
		Evidence: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{
			{SourceID: tools.PrimarySourceID, Path: "pkg/controller.go"}: {Segments: []string{"return err"}, Lines: map[int]string{10: "return err"}},
		},
	}
	citation := `{"repository":"example/project","revision":"` + revision + `","path":"pkg/controller.go","line_start":10,"line_end":10,"quote":"return err"}`
	for _, testCase := range []struct {
		name, answer string
		wantClaim    string
	}{
		{name: "matching", answer: "pkg/controller.go:10 returns the error.", wantClaim: "pkg/controller.go:10"},
		{name: "mismatched", answer: "pkg/controller.go:11 returns the error.", wantClaim: "pkg/controller.go"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := `{"answer":` + strconv.Quote(testCase.answer) + `,"citations":[` + citation + `],"assessment":"supports","proposed_revision":null}`
			reply, stats, err := parseAnalysisChatReplyCandidatesWithSource(raw, nil, source)
			if err != nil || stats.EvidenceGate != "" || !strings.Contains(reply.Answer, testCase.wantClaim) {
				t.Fatalf("reply=%+v stats=%+v err=%v", reply, stats, err)
			}
			if testCase.name == "mismatched" && strings.Contains(reply.Answer, ":11") {
				t.Fatalf("unsupported source line survived: %q", reply.Answer)
			}
		})
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
		name string
		raw  string
		gate string
	}{
		{name: "stripped build prefix", raw: `{"answer":"x","assessment":"supports","citations":[{"path":"build-log.txt","quote":"build 103 failed first"}],"proposed_revision":null}`, gate: analysischat.UnverifiedReference},
		{name: "quote from another build", raw: `{"answer":"x","assessment":"supports","citations":[{"path":"builds/103/build-log.txt","quote":"build 104 timed out later"}],"proposed_revision":null}`, gate: analysischat.UnverifiedCitation},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reply, stats, err := parseAnalysisChatReplyCandidates(testCase.raw, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if stats.EvidenceGate != testCase.gate || !reply.Unverified || reply.UnverifiedReason != testCase.gate {
				t.Fatalf("stats=%+v reply=%+v", stats, reply)
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
	for _, want := range []string{"Consumer fact.", "published AI analysis is a hypothesis", `"citations": [`, `{"repository": null, "revision": null, "path": "build-log.txt"`, "preserve the full builds/<build-id>/"} {
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
			EvidenceWarnings: []string{"partial-evidence-marker"},
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
	for _, want := range []string{"revised-root-marker", "revised-fix-marker", "proposed_revision", "challenges", "partial-evidence-marker", "Engine evidence note"} {
		if !strings.Contains(request, want) {
			t.Errorf("request omitted structured history %q", want)
		}
	}
	if strings.Contains(request, `"evidence_warnings"`) {
		t.Fatalf("history taught the model an unsupported response field: %s", request)
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
	}}}, analysisChatEvidenceFallbackMaxBytes)
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
	reply, err = parseAnalysisChatReply(invalid, evidence)
	if err != nil || reply.Unverified || len(reply.EvidenceWarnings) != 0 || len(reply.Citations) != 1 {
		t.Fatalf("recoverable line range reply = %+v err=%v", reply, err)
	}
	if reply.Citations[0].LineStart != 0 || reply.Citations[0].LineEnd != 0 || reply.Citations[0].Quote != "controller stopped" {
		t.Fatalf("recovered citation = %+v", reply.Citations[0])
	}
}

func TestAnalysisChatCitationValidationRetainsVerifiedSubset(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"good.log":  {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
		"other.log": {Segments: []string{"different evidence"}, Lines: map[int]string{}},
	}
	raw := `{"answer":"The controller stopped at lines 80-81.","assessment":"supports","citations":[{"path":"good.log","quote":"controller stopped"},{"path":"other.log","line_start":80,"line_end":81,"quote":"missing evidence"}],"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Unverified || reply.Assessment != "supports" || len(reply.Citations) != 1 || reply.Citations[0].Path != "good.log" {
		t.Fatalf("partial reply = %+v", reply)
	}
	if strings.Contains(reply.Answer, "80-81") {
		t.Fatalf("unsupported line claim was retained: %q", reply.Answer)
	}
	if len(reply.EvidenceWarnings) != 1 || !strings.Contains(reply.EvidenceWarnings[0], "citation 2 quote does not appear") {
		t.Fatalf("evidence warnings = %v", reply.EvidenceWarnings)
	}
	if stats.EvidenceGate != analysischat.UnverifiedCitation || !strings.Contains(stats.EvidenceDetail, "citation 2") {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAnalysisChatCitationValidationFindsValidEntriesAfterInputLimit(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"good.log":  {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
		"other.log": {Segments: []string{"different evidence"}, Lines: map[int]string{}},
	}
	citations := make([]string, 0, 21)
	for range 20 {
		citations = append(citations, `{"path":"other.log","quote":"missing evidence"}`)
	}
	citations = append(citations, `{"path":"good.log","quote":"controller stopped"}`)
	raw := `{"answer":"The controller stopped.","assessment":"supports","citations":[` + strings.Join(citations, ",") + `],"proposed_revision":null}`
	reply, _, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil || reply.Unverified || len(reply.Citations) != 1 || reply.Citations[0].Path != "good.log" {
		t.Fatalf("reply = %+v err=%v", reply, err)
	}
	if len(reply.EvidenceWarnings) != 20 {
		t.Fatalf("evidence warnings = %d, want 20", len(reply.EvidenceWarnings))
	}
}

func TestAnalysisChatCitationValidationCapsVerifiedEntries(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{}
	citations := make([]string, 0, 21)
	for i := range 21 {
		path := fmt.Sprintf("log-%d.txt", i)
		quote := fmt.Sprintf("unique evidence %d", i)
		evidence[path] = &analysisChatEvidence{Segments: []string{quote}, Lines: map[int]string{}}
		citations = append(citations, fmt.Sprintf(`{"path":%q,"quote":%q}`, path, quote))
	}
	raw := `{"answer":"The evidence supports it.","assessment":"supports","citations":[` + strings.Join(citations, ",") + `],"proposed_revision":null}`
	reply, _, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil || reply.Unverified || len(reply.Citations) != 20 {
		t.Fatalf("reply = %+v err=%v", reply, err)
	}
	if !slices.Contains(reply.EvidenceWarnings, "additional citations were discarded after 20 verified entries") {
		t.Fatalf("evidence warnings = %v", reply.EvidenceWarnings)
	}
}

func TestAnalysisChatCitationRecoveryDiscardsMalformedCoordinates(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"boot.log": {
		Segments: []string{"GET result: Not Found"}, Lines: map[int]string{1065: "GET result: Not Found"},
	}}
	for _, testCase := range []struct {
		name  string
		start int
		end   int
	}{
		{name: "reversed", start: 1068, end: 1064},
		{name: "missing end", start: 1065, end: 0},
		{name: "missing start", start: 0, end: 1065},
		{name: "negative", start: -1, end: -1},
		{name: "excessive", start: 1000, end: 1100},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"answer":"Ignition failed.","assessment":"supports","citations":[{"path":"boot.log","line_start":%d,"line_end":%d,"quote":"GET result: Not Found"}],"proposed_revision":null}`, testCase.start, testCase.end)
			reply, err := parseAnalysisChatReply(raw, evidence)
			if err != nil || reply.Unverified || len(reply.Citations) != 1 {
				t.Fatalf("reply = %+v err=%v", reply, err)
			}
			if reply.Citations[0].LineStart != 0 || reply.Citations[0].LineEnd != 0 {
				t.Fatalf("recovered coordinates = %+v", reply.Citations[0])
			}
		})
	}
}

func TestAnalysisChatCitationRecoveryRejectsAmbiguousMalformedCoordinates(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"boot.log": {
		Segments: []string{"first GET result: Not Found", "second GET result: Not Found context"}, Lines: map[int]string{},
	}}
	raw := `{"answer":"Ignition failed.","assessment":"supports","citations":[{"path":"boot.log","line_start":9,"line_end":2,"quote":"GET result: Not Found"}],"proposed_revision":null}`
	reply, err := parseAnalysisChatReply(raw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Unverified || len(reply.Citations) != 0 || len(reply.EvidenceWarnings) != 1 ||
		!strings.Contains(reply.EvidenceWarnings[0], "matches more than one passage") {
		t.Fatalf("reply = %+v", reply)
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
			if analysisChatReplyVerified(parseAnalysisChatReply(testCase.raw, evidence)) {
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
	if analysisChatReplyVerified(parseAnalysisChatReply(raw, evidence)) {
		t.Fatal("quote naming no recorded passage was accepted")
	}
}

// The model supplies a locator; the engine answers with its own recorded text,
// so what the maintainer reads is what a tool returned rather than what the
// model retyped.
func TestAnalysisChatCitationQuoteIsAttributedFromEvidence(t *testing.T) {
	recorded := "  \x1b[31mE0412 controller stopped\x1b[0m\n    reason: timeout"
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {
		Segments: []string{"unrelated preamble\n" + recorded + "\ntrailing noise"},
	}}
	raw := `{"answer":"x","citations":[{"path":"build-log.txt","quote":"E0412 controller stopped reason: timeout"}]}`
	reply, err := parseAnalysisChatReply(raw, evidence)
	if err != nil || reply.Unverified {
		t.Fatalf("re-wrapped locator was not verified: err=%v reply=%+v", err, reply)
	}
	if reply.Citations[0].Quote != recorded {
		t.Fatalf("attributed quote = %q, want %q", reply.Citations[0].Quote, recorded)
	}
}

// A locator that names two different passages cannot be resolved to one of
// them, so the citation is unverified rather than silently attributed to the
// first match. The same locator naming one repeated line is unambiguous,
// because the attributed text is the same wherever it matched.
func TestAnalysisChatCitationRejectsAmbiguousLocator(t *testing.T) {
	ambiguous := map[string]*analysisChatEvidence{"build-log.txt": {
		Segments: []string{"reconcile failed for node-a", "reconcile failed for node-b"},
	}}
	raw := `{"answer":"x","citations":[{"path":"build-log.txt","quote":"reconcile failed"}],"assessment":null,"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, ambiguous)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !reply.Unverified || !strings.Contains(stats.EvidenceDetail, "matches more than one passage") {
		t.Fatalf("reply=%+v detail=%q", reply, stats.EvidenceDetail)
	}
	repeated := map[string]*analysisChatEvidence{"build-log.txt": {
		Segments: []string{"reconcile failed\nnode ready\nreconcile failed"},
	}}
	resolved, err := parseAnalysisChatReply(raw, repeated)
	if err != nil || resolved.Unverified || resolved.Citations[0].Quote != "reconcile failed" {
		t.Fatalf("repeated line was not resolved: err=%v reply=%+v", err, resolved)
	}
}

func TestAnalysisChatCitationUsesExactSafePath(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"foo.log": {Segments: []string{"controller stopped"}}}
	caseMismatch := `{"answer":"x","assessment":"supports","citations":[{"path":"FOO.log","quote":"controller stopped"}],"proposed_revision":null}`
	if analysisChatReplyVerified(parseAnalysisChatReply(caseMismatch, evidence)) {
		t.Fatal("case-mismatched artifact citation was accepted")
	}
	suffixMismatch := `{"answer":"x","assessment":"supports","citations":[{"path":"foo","quote":"controller stopped"}],"proposed_revision":null}`
	if analysisChatReplyVerified(parseAnalysisChatReply(suffixMismatch, evidence)) {
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

func TestAnalysisChatEvidenceNormalizationKeepsTheGuarantee(t *testing.T) {
	evidence := &analysisChatEvidence{Segments: []string{
		"line one alpha\nline two middle\nline three omega",
		"echo safe \\ \nrm file",
		"if authorized:\n    grant()\ndeny()",
	}}
	// Wrapping, indentation, and spacing are presentation the model routinely
	// drops. The engine answers with its own recorded text, so a sloppy locator
	// never changes what the maintainer reads.
	for name, locator := range map[string]string{
		"contiguous span":           "line one alpha\nline two middle",
		"whole segment":             "line one alpha\nline two middle\nline three omega",
		"single line":               "line two middle",
		"drops indentation":         "if authorized:\ngrant()\ndeny()",
		"joins lines onto one":      "line one alpha line two middle",
		"drops trailing whitespace": "echo safe \\\nrm file",
	} {
		quote, matches := attributeAnalysisChatQuote(evidence, locator)
		if matches != 1 {
			t.Fatalf("%s: matches = %d", name, matches)
		}
		if !slices.Contains(evidence.Segments, quote) && !strings.Contains(evidence.Segments[0], quote) &&
			!strings.Contains(evidence.Segments[2], quote) {
			t.Fatalf("%s: attributed quote %q is not recorded text", name, quote)
		}
	}
	for name, locator := range map[string]string{
		"skips content":          "line one alpha\nline three omega",
		"reorders content":       "line three omega\nline one alpha",
		"invents content":        "line one alpha and then nothing happened",
		"spans separate reads":   "line three omega\necho safe",
		"is entirely whitespace": "   \n  ",
	} {
		if _, matches := attributeAnalysisChatQuote(evidence, locator); matches != 0 {
			t.Fatalf("%s: quote was attributed", name)
		}
	}
}

func TestAnalysisChatEvidenceRejectsQuotesSpanningSeparateReads(t *testing.T) {
	evidence := &analysisChatEvidence{Segments: []string{"first snippet", "second snippet"}}
	one := map[string]*analysisChatEvidence{"build-log.txt": evidence}
	_, stats, err := parseAnalysisChatReplyCandidates(
		`{"answer":"x","citations":[{"path":"build-log.txt","quote":"first snippet\nsecond snippet"}],"assessment":null,"proposed_revision":null}`,
		one,
	)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !strings.Contains(stats.EvidenceDetail, "quote text the tools returned") {
		t.Fatalf("joined quote detail = %q", stats.EvidenceDetail)
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

// A read that overruns the budget is rejected whole, because the model-visible
// envelope is replaced when it does not fit: retaining part of it would index
// text the model never received and could not cite. The remaining room is
// reported so the model can retry a narrower range instead of giving up.
func TestAnalysisChatEvidenceOverflowIsAtomicAndReportsRoom(t *testing.T) {
	const budget = 200
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{strings.Repeat("a", 150)}, Bytes: 150, Lines: map[int]string{}},
	}
	beforeBytes := evidence["build-log.txt"].Bytes
	beforeSegments := len(evidence["build-log.txt"].Segments)

	roomLeft, recorded := recordAnalysisChatEvidence(evidence, modelToolCall{Function: modelFunction{
		Name: "read_artifact", Arguments: `{"path":"build-log.txt"}`,
	}}, map[string]interface{}{"content": strings.Repeat("b", 100)}, budget)

	if recorded {
		t.Fatal("an overflowing read was accepted")
	}
	if roomLeft != budget-beforeBytes {
		t.Fatalf("roomLeft = %d, want %d", roomLeft, budget-beforeBytes)
	}
	entry := evidence["build-log.txt"]
	if entry.Bytes != beforeBytes || len(entry.Segments) != beforeSegments {
		t.Fatalf("overflow mutated evidence: bytes=%d segments=%d", entry.Bytes, len(entry.Segments))
	}
	// A read that fits is still recorded, and reports the room it left.
	roomLeft, recorded = recordAnalysisChatEvidence(evidence, modelToolCall{Function: modelFunction{
		Name: "read_artifact", Arguments: `{"path":"build-log.txt"}`,
	}}, map[string]interface{}{"content": strings.Repeat("c", 40)}, budget)
	if !recorded || roomLeft != budget-190 {
		t.Fatalf("fitting read: recorded=%v roomLeft=%d", recorded, roomLeft)
	}
}

func TestAnalysisChatEvidenceBudgetTracksContextBudget(t *testing.T) {
	// No configured budget keeps the conservative fallback.
	if got := analysisChatEvidenceBudget(0); got != analysisChatEvidenceFallbackMaxBytes {
		t.Fatalf("fallback budget = %d", got)
	}
	// A larger configured window buys proportionally larger reads.
	small := analysisChatEvidenceBudget(244880)
	large := analysisChatEvidenceBudget(994880)
	if large <= small {
		t.Fatalf("budget does not track the context budget: small=%d large=%d", small, large)
	}
	// A million-token window must clear the artifact sizes this exists for,
	// well past the fallback that could not hold a controller log.
	if large < 3*analysisChatEvidenceFallbackMaxBytes {
		t.Fatalf("large-window budget = %d", large)
	}
	// A tiny configured budget never drops below the fallback.
	if got := analysisChatEvidenceBudget(1000); got != analysisChatEvidenceFallbackMaxBytes {
		t.Fatalf("small context budget = %d", got)
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

func TestAnalysisChatRepositoryToolsAreCauseScopedAndResolved(t *testing.T) {
	tests := []struct {
		name       string
		turn       func() analysischat.Turn
		wantSource bool
	}{
		{name: "resolved cause", turn: causeAnalysisChatTurn, wantSource: true},
		{name: "unresolved cause", turn: func() analysischat.Turn {
			turn := causeAnalysisChatTurn()
			turn.Build.RepoRefs = nil
			turn.EvidenceBuilds[0].Build = turn.Build
			return turn
		}},
		{name: "whole pattern", turn: func() analysischat.Turn {
			turn := causeAnalysisChatTurn()
			turn.Scope = analysischat.ScopePattern
			return turn
		}},
		{name: "test", turn: analysisChatTurn},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := newScriptedChatServer(t)
			server.push(200, chatRespFinal(`{"answer":"The published context is sufficient.","citations":[],"assessment":"explains","proposed_revision":null}`))
			agent := newAnalysisChatAgentWithRepoToolsForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{
				MaxIters: 2, Timeout: time.Second,
				SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
			})
			if _, err := agent.Reply(t.Context(), testCase.turn()); err != nil {
				t.Fatal(err)
			}
			request := string(server.requests[0])
			for _, name := range []string{"list_repo_tree", "read_repo_file", "grep_repo"} {
				if got := strings.Contains(request, `"name":"`+name+`"`); got != testCase.wantSource {
					t.Fatalf("request source tool %s present=%t want=%t: %s", name, got, testCase.wantSource, request)
				}
			}
			if testCase.wantSource {
				for _, want := range []string{
					"kubernetes-sigs/cluster-api-provider-azure",
					"0123456789abcdef0123456789abcdef01234567",
					"selected failed build 123",
				} {
					if !strings.Contains(request, want) {
						t.Fatalf("resolved cause request missing %q: %s", want, request)
					}
				}
			}
		})
	}
}

func TestAnalysisChatFinalizationUsesForcedFunctionAndRecordsUsage(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCallWithUsage("call-1", "list_artifacts", map[string]interface{}{"path": ""}, 10, 2, 0))
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
	if reply.Answer != "The published context is sufficient." || reply.ValidationRetries != 0 {
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
	if len(structuredAttempts) != 2 || structuredAttempts[0].StructuredPhase != "analysis_chat_finalize" ||
		structuredAttempts[0].StructuredAttempt != "response_format" || structuredAttempts[0].StructuredOutcome != "no_candidate" ||
		structuredAttempts[1].StructuredAttempt != "forced_function" || structuredAttempts[1].StructuredOutcome != "accepted" {
		t.Fatalf("structured attempt events = %+v", structuredAttempts)
	}
	if responseEvent == nil || responseEvent.Outcome != "success" || responseEvent.Status != "finalize" ||
		responseEvent.StructuredAttempt != "forced_function" || responseEvent.ModelCallCount != 3 || responseEvent.Attempts != 3 || responseEvent.HTTPStatus != 200 {
		t.Fatalf("response event = %+v", responseEvent)
	}
}

func chatRespToolCallWithUsage(id, name string, args map[string]interface{}, input, output, reasoning int) string {
	encodedArgs, _ := json.Marshal(args)
	encodedArgsString, _ := json.Marshal(string(encodedArgs))
	return fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
		id, name, encodedArgsString, input, output, reasoning,
	)
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
			RecoveryStreak: 3, RecoveryBuilds: []string{"107", "106", "105"},
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
	if !slices.Equal(context.CausalGroups[0].Builds, []string{"104", "103"}) || !slices.Equal(context.CausalGroups[0].ArtifactBuilds, []string{"104"}) {
		t.Fatalf("group a = %+v", context.CausalGroups[0])
	}
	if len(context.CausalGroups[1].ArtifactBuilds) != 0 {
		t.Fatalf("singleton = %+v", context.CausalGroups[1])
	}
	if !slices.Equal(context.CausalGroups[2].ArtifactBuilds, []string{"101"}) {
		t.Fatalf("group b = %+v", context.CausalGroups[2])
	}
	if context.Lifecycle == nil || context.Lifecycle.State != models.PatternLifecycleRecovered || context.Lifecycle.Reason != "three later builds passed" ||
		context.Lifecycle.RecoveryStreak != 3 || !slices.Equal(context.Lifecycle.RecoveryBuilds, []string{"107", "106", "105"}) {
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

func TestAnalysisChatCauseContextNamesTheSelectedCause(t *testing.T) {
	pattern := &models.PatternAnalysis{
		ID: "pattern", Subject: "job", BuildsAnalyzed: 2, Confidence: "high",
		CausalGroups: []models.PatternCausalGroup{{
			ID: "cause", ContentHash: "cause-hash", Builds: []string{"2", "1"},
			RootCause: "same cause", Confidence: "high",
		}},
		SharedRootCause: "same cause", SharedBuilds: []string{"2", "1"}, Summary: "same cause",
		Lifecycle: &models.PatternLifecycle{
			State: models.PatternLifecycleActive, Reason: "one later run passed", RecoveryStreak: 1, RecoveryBuilds: []string{"3"},
		},
	}
	context, err := analysisChatContext(analysischat.Turn{
		Scope: analysischat.ScopeCause, JobID: "periodic-demo", Pattern: pattern,
		EvidenceBuilds: []analysischat.ArtifactBuild{{Build: models.BuildInfo{BuildID: "2"}}, {Build: models.BuildInfo{BuildID: "1"}}},
		Comparison: &analysischat.CauseComparison{
			ArtifactBuild: analysischat.ArtifactBuild{Build: models.BuildInfo{
				BuildID: "3", Result: "SUCCESS", Passed: true,
				Started: time.Date(2026, time.August, 27, 4, 22, 14, 0, time.UTC), Commit: "5eafa966",
			}},
			TestNames: []string{"Flatcar sysext cluster"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context, "Selected published causal-group analysis") || strings.Contains(context, "Selected published recurring-pattern analysis") {
		t.Fatalf("context = %s", context)
	}
	for _, want := range []string{`"comparison_build"`, `"build_id": "3"`, `"representative_tests"`, "newest completed run available when this conversation was created", "A passing comparison proves only that the cause was not reproduced"} {
		if !strings.Contains(context, want) {
			t.Fatalf("context missing %q: %s", want, context)
		}
	}
	if strings.Contains(context, "Answer only about this cause and its listed builds") {
		t.Fatalf("context = %s", context)
	}
}

func TestAnalysisChatCauseComparisonBuildIsSeparateFromMemberBuilds(t *testing.T) {
	turn := analysischat.Turn{
		Scope: analysischat.ScopeCause,
		EvidenceBuilds: []analysischat.ArtifactBuild{
			{Build: models.BuildInfo{BuildID: "2"}},
			{Build: models.BuildInfo{BuildID: "1"}},
		},
		Comparison: &analysischat.CauseComparison{
			ArtifactBuild: analysischat.ArtifactBuild{Build: models.BuildInfo{BuildID: "3"}},
		},
	}
	builds := analysisChatArtifactBuilds(turn)
	got := make([]string, 0, len(builds))
	for _, build := range builds {
		got = append(got, build.Build.BuildID)
	}
	if !slices.Equal(got, []string{"2", "1", "3"}) {
		t.Fatalf("artifact builds = %v", got)
	}
	if !slices.Equal([]string{turn.EvidenceBuilds[0].Build.BuildID, turn.EvidenceBuilds[1].Build.BuildID}, []string{"2", "1"}) {
		t.Fatalf("member builds mutated: %+v", turn.EvidenceBuilds)
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
		{name: "total bytes", mutate: func(pattern *models.PatternAnalysis) {
			pattern.SharedRootCause = strings.Repeat("r", 32<<10)
			pattern.SuggestedFix = strings.Repeat("f", 16<<10)
			for index := 0; index < analysisChatMaxPatternCausalGroups; index++ {
				group := models.PatternCausalGroup{ID: fmt.Sprintf("g-%d", index), ContentHash: fmt.Sprintf("h-%d", index), Builds: []string{fmt.Sprintf("%d", index)}, RootCause: strings.Repeat("c", analysisChatMaxPatternRootCauseBytes), Confidence: "high"}
				pattern.CausalGroups = append(pattern.CausalGroups, group)
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
	encoded, err := json.Marshal(store.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "controller stopped") {
		t.Fatalf("private telemetry retained evidence content: %s", encoded)
	}
}

func TestAnalysisChatEmptyOrFailedArtifactRecordsNoContent(t *testing.T) {
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
			server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
			server.push(200, chatRespFinal(`{
				"answer":"The artifact could not be read, so this stays unresolved.",
				"citations":[],"assessment":"inconclusive","proposed_revision":null
			}`))
			agent := newAnalysisChatAgentForTest(t, server.URL, testCase.browser, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
			turn := analysisChatTurn()
			turn.Question = "Re-check the artifacts before answering."
			store := NewTraceStore()
			trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
			ctx := withAnalysisTrace(t.Context(), trace)

			reply, err := agent.Reply(ctx, turn)
			if err != nil {
				t.Fatal(err)
			}
			trace.Finish("success", nil)
			if reply.Unverified || len(reply.Citations) != 0 {
				t.Fatalf("reply = %+v", reply)
			}
			if statuses := analysisChatEvidenceTraceStatuses(store.Snapshot()); !slices.Contains(statuses, analysisChatEvidenceNoContent) {
				t.Fatalf("evidence statuses = %v", statuses)
			}
		})
	}
}
func TestAnalysisChatEvidenceClaimWithoutCitationsDegradesToUnverified(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "tail_artifact", map[string]interface{}{"path": "build-log.txt", "lines": 20}))
	uncited := `{"answer":"The artifact supports it.","citations":[],"assessment":"supports","proposed_revision":null}`
	server.push(200, chatRespFinal(uncited))
	server.push(200, chatRespFinal(uncited))
	server.push(200, chatRespFinal(uncited))
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
	if !reply.Unverified || reply.UnverifiedReason != analysischat.UnverifiedMissing ||
		reply.Assessment != "inconclusive" || reply.ValidationRetries != 2 {
		t.Fatalf("reply = %+v", reply)
	}
	if statuses := analysisChatEvidenceTraceStatuses(store.Snapshot()); !slices.Contains(statuses, analysisChatEvidenceUnverified) {
		t.Fatalf("evidence statuses = %v", statuses)
	}
}
func TestAnalysisChatUnprovenCitationDegradesAfterCorrectiveRounds(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	invalid := `{"answer":"The artifact supports it.","citations":[{"path":"build-log.txt","quote":"different evidence"}],"assessment":"supports","proposed_revision":null}`
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	server.push(200, chatRespFinal(invalid))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Read the artifact evidence for this root cause."

	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Unverified || reply.UnverifiedReason != analysischat.UnverifiedCitation ||
		reply.Assessment != "inconclusive" || len(reply.Citations) != 0 || reply.ProposedRevision != nil {
		t.Fatalf("reply = %+v", reply)
	}
	if got, want := int(server.calls), 4; got != want {
		t.Fatalf("provider calls = %d, want %d", got, want)
	}
}
func TestAnalysisChatPartialEvidenceTraceAfterCorrectiveRounds(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "good.log", "offset": 0, "length": 1024}))
	server.push(200, chatRespToolCall("call-2", "read_artifact", map[string]interface{}{"path": "other.log", "offset": 0, "length": 1024}))
	partial := `{"answer":"The controller stopped.","citations":[{"path":"good.log","quote":"controller stopped"},{"path":"other.log","quote":"missing evidence"}],"assessment":"supports","proposed_revision":null}`
	server.push(200, chatRespFinal(partial))
	server.push(200, chatRespFinal(partial))
	server.push(200, chatRespFinal(partial))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"good.log": []byte("controller stopped\n"), "other.log": []byte("different evidence\n"),
	}}, AnalysisChatOptions{MaxIters: 5, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Read the artifact evidence for this root cause."
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(t.Context(), trace)
	reply, err := agent.Reply(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if reply.Unverified || len(reply.Citations) != 1 || len(reply.EvidenceWarnings) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	if statuses := analysisChatEvidenceTraceStatuses(store.Snapshot()); !slices.Contains(statuses, analysisChatEvidencePartial) {
		t.Fatalf("evidence statuses = %v", statuses)
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

func TestSeedAnalysisChatEvidenceRestoresProvenCitations(t *testing.T) {
	history := []analysischat.Message{
		{Role: "user", Content: "What does the log show?"},
		{Role: "assistant", Citations: []analysischat.Citation{
			{Path: "build-log.txt", LineStart: 41, LineEnd: 42, Quote: "controller stopped\nreconcile aborted"},
			{Path: "../secret", Quote: "escaped path"},
			{Path: "junit.xml", LineStart: 7, LineEnd: 9, Quote: "one line only"},
		}},
	}
	evidence := seedAnalysisChatEvidence(history, 0)
	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v", evidence)
	}
	entry := evidence["build-log.txt"]
	if entry.Lines[41] != "controller stopped" || entry.Lines[42] != "reconcile aborted" {
		t.Fatalf("seeded lines = %+v", entry.Lines)
	}
	if !analysisChatEvidenceContains(entry, "controller stopped") {
		t.Fatalf("seeded segments = %+v", entry.Segments)
	}
	// A quote whose line count does not match its recorded range cannot be
	// mapped back to lines, so only its text is restored.
	if len(evidence["junit.xml"].Lines) != 0 {
		t.Fatalf("mismatched range seeded lines: %+v", evidence["junit.xml"].Lines)
	}
}

func TestSeedAnalysisChatEvidenceSkipsSourceCitationsBeforePathNormalization(t *testing.T) {
	history := []analysischat.Message{{
		Role: "assistant",
		Citations: []analysischat.Citation{{
			Repository: "example/project", Revision: strings.Repeat("1", 40),
			Path: "pkg/controller.go", LineStart: 10, LineEnd: 10, Quote: "return err",
		}},
	}}
	evidence := seedAnalysisChatEvidence(history, 0)
	if len(evidence) != 0 {
		t.Fatalf("source citation entered artifact evidence: %+v", evidence)
	}
	raw := `{"answer":"The artifact repeats the source text.","citations":[{"path":"pkg/controller.go","quote":"return err"}],"assessment":"supports","proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil || !reply.Unverified || stats.EvidenceGate != analysischat.UnverifiedReference {
		t.Fatalf("reply=%+v stats=%+v err=%v", reply, stats, err)
	}
}

func TestSeedAnalysisChatEvidenceBoundsCarriedBytes(t *testing.T) {
	quote := strings.Repeat("a", 32<<10)
	history := make([]analysischat.Message, 0, 16)
	for i := 0; i < 16; i++ {
		history = append(history, analysischat.Message{Role: "assistant", Citations: []analysischat.Citation{
			{Path: fmt.Sprintf("build-%d.log", i), Quote: quote},
		}})
	}
	if bytes := analysisChatEvidenceBytes(seedAnalysisChatEvidence(history, 0)); bytes > analysisChatSeedBudget(0) {
		t.Fatalf("seeded bytes = %d", bytes)
	}
}

func TestAnalysisChatFollowUpCitesEarlierTurnEvidence(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespFinal(`{
		"answer":"The same line still supports the conclusion.",
		"citations":[{"path":"build-log.txt","quote":"controller stopped"}],
		"assessment":"supports","proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})
	turn := analysisChatTurn()
	turn.Question = "Why did you conclude that?"
	turn.History = []analysischat.Message{
		{Role: "user", Content: "What does the build log show?"},
		{Role: "assistant", Content: "The controller stopped.", Assessment: "supports", Citations: []analysischat.Citation{
			{Path: "build-log.txt", Quote: "controller stopped"},
		}},
	}

	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Unverified || reply.ToolCalls != 0 || len(reply.Citations) != 1 ||
		reply.Citations[0].Path != "build-log.txt" {
		t.Fatalf("reply = %+v", reply)
	}
}

// A prose answer is a formatting failure, not a reasoning one. It reaches the
// maintainer marked unverified instead of being discarded.
func TestAnalysisChatProseAnswerDegradesToUnverified(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	for i := 0; i < 6; i++ {
		server.push(200, chatRespFinal("The controller log stops at 12:04, so look there."))
	}
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	reply, err := agent.Reply(t.Context(), analysisChatTurn())
	if err != nil {
		t.Fatalf("Reply error = %v", err)
	}
	if !reply.Unverified || reply.UnverifiedReason != analysischat.UnverifiedFormat {
		t.Fatalf("reply = %+v", reply)
	}
	if reply.Answer != "The controller log stops at 12:04, so look there." || len(reply.Citations) != 0 {
		t.Fatalf("salvaged answer = %+v", reply)
	}
}

func TestAnalysisChatEmptyAnswerReportsGate(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	for i := 0; i < 6; i++ {
		server.push(200, chatRespFinal("   "))
	}
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 2, Timeout: time.Second})

	_, err := agent.Reply(t.Context(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
	gate, ok := analysischat.ValidationGateOf(err)
	if !ok || gate != analysischat.GateCandidate {
		t.Fatalf("gate = %q ok = %t", gate, ok)
	}
}

func TestSalvageAnalysisChatReply(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "prose", raw: "the node never joined", want: "the node never joined"},
		{
			name: "prose quoting a JSON path",
			raw:  "run kubectl -o jsonpath='{.status.phase}': the pod never went Ready",
			want: "run kubectl -o jsonpath='{.status.phase}': the pod never went Ready",
		},
		{
			name: "prose quoting a contract key",
			raw:  `The provider returned {"citations":null}; that is why validation failed.`,
			want: `The provider returned {"citations":null}; that is why validation failed.`,
		},
		{
			name: "prose carrying a patch",
			raw:  "Apply this to stop the retries:\n{\"spec\":{\"template\":{\"spec\":{\"restartPolicy\":\"Never\"}}}}",
			want: "Apply this to stop the retries:\n{\"spec\":{\"template\":{\"spec\":{\"restartPolicy\":\"Never\"}}}}",
		},
		{
			name: "contract failure keeps the answer field",
			raw:  `{"answer":"the node never joined","citations":[],"confidence":0.9}`,
			want: "the node never joined",
		},
		{
			name: "nested answer beside another field",
			raw:  `{"draft":{"answer":"maybe timeout"},"final":"container was OOMKilled"}`,
			want: `{"draft":{"answer":"maybe timeout"},"final":"container was OOMKilled"}`,
		},
		{
			name: "metadata wrapper keeps the whole response",
			raw:  `{"result":{"answer":"the node never joined","citations":[]}}`,
			want: `{"result":{"answer":"the node never joined","citations":[]}}`,
		},
		{
			name: "draft object followed by a prose conclusion",
			raw:  "{\"draft\":{\"answer\":\"maybe timeout\"}}\nFinal conclusion: the container was OOMKilled.",
			want: "{\"draft\":{\"answer\":\"maybe timeout\"}}\nFinal conclusion: the container was OOMKilled.",
		},
		{
			name: "two different answers keep the whole response",
			raw:  `{"draft":{"answer":"maybe timeout"},"final":{"answer":"actually OOM"}}`,
			want: `{"draft":{"answer":"maybe timeout"},"final":{"answer":"actually OOM"}}`,
		},
		{
			name: "duplicate answer keys keep the whole response",
			raw:  `{"answer":"maybe timeout","answer":"actually OOM","citations":[]}`,
			want: `{"answer":"maybe timeout","answer":"actually OOM","citations":[]}`,
		},
		{name: "empty", raw: "  "},
		{name: "preamble", raw: "Let me now look at what happens during the deletion window:"},
		{name: "contract-shaped preamble", raw: `{"answer":"Let me read the controller log:","citations":[],"confidence":0.9}`},
		{
			name: "answer ending in a quoted key that carries a colon",
			raw:  "The manifest is missing **`securityGroup:`**",
			want: "The manifest is missing **`securityGroup:`**",
		},
		{
			name: "answer ending in an ellipsis",
			raw:  "The evidence remains inconclusive…",
			want: "The evidence remains inconclusive…",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			reply, ok := salvageAnalysisChatReply(testCase.raw)
			if ok != (testCase.want != "") {
				t.Fatalf("salvaged = %t", ok)
			}
			if !ok {
				return
			}
			if reply.Answer != testCase.want || !reply.Unverified ||
				reply.UnverifiedReason != analysischat.UnverifiedFormat || reply.Assessment != "inconclusive" {
				t.Fatalf("reply = %+v", reply)
			}
		})
	}
}

// Narration alongside a tool call is not an answer, and neither is narration on
// its own: a model that announces a step and calls no tool produces a tools-free
// turn the loop would otherwise treat as its final answer.
func TestAnalysisChatNarrationIsNotSalvaged(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCallWithContent(
		"Let me check the controller log.", "call-1", "read_artifact",
		map[string]interface{}{"path": "build-log.txt"},
	))
	for i := 0; i < 6; i++ {
		server.push(200, chatRespFinal("   "))
	}
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})

	if _, err := agent.Reply(t.Context(), analysisChatTurn()); !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
}

const analysisChatAnnouncementTurn = "The controller logs are very revealing. Let me now look for the critical" +
	" pattern, what happens with test-security-group during the deletion window:"

// A turn that only announces its next step fails rather than reaching the
// maintainer as an answer, however many times the model repeats it.
func TestAnalysisChatAnnouncementFailsInsteadOfBeingSalvaged(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	for i := 0; i < analysisChatMaxCorrectiveRounds+1; i++ {
		server.push(200, chatRespFinal(analysisChatAnnouncementTurn))
	}
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})

	_, err := agent.Reply(t.Context(), analysisChatTurn())
	if !errors.Is(err, analysischat.ErrResponseValidationFailed) {
		t.Fatalf("Reply error = %v", err)
	}
	if gate, ok := analysischat.ValidationGateOf(err); !ok || gate != analysischat.GateCandidate {
		t.Fatalf("gate = %q ok = %t", gate, ok)
	}
}

// The corrective rounds name the mistake and leave room to recover from it.
func TestAnalysisChatAnnouncementRecoversOnTheLastCorrectiveRound(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("call-1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	server.push(200, chatRespFinal(analysisChatAnnouncementTurn))
	server.push(200, chatRespFinal(analysisChatAnnouncementTurn))
	server.push(200, chatRespFinal(`{
		"answer":"The controller exit supports the published conclusion.",
		"assessment":"supports",
		"citations":[{"path":"build-log.txt","quote":"controller stopped"}],
		"proposed_revision":null
	}`))
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("controller stopped\n"),
	}}, AnalysisChatOptions{MaxIters: 3, Timeout: time.Second})

	reply, err := agent.Reply(t.Context(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Unverified || len(reply.Citations) != 1 || reply.ValidationRetries != 2 {
		t.Fatalf("reply = %+v", reply)
	}
	corrections := 0
	for _, request := range server.requests {
		if bytes.Contains(request, []byte("announced a next step instead of taking it")) {
			corrections++
		}
	}
	if corrections != 2 {
		t.Fatalf("announcement corrections = %d, want 2", corrections)
	}
}

func TestAnalysisChatRepairPromptNamesTheFailure(t *testing.T) {
	generic := analysisChatCorrectivePrompt("no JSON response object found")
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "announcement", content: analysisChatAnnouncementTurn, want: analysisChatAnnouncementCorrectivePrompt},
		{name: "prose answer without the JSON envelope", content: "The node never joined.", want: generic},
		{name: "response cut off inside its own JSON", content: `{"answer":"the deletion window:`, want: generic},
		{
			name:    "evidence gate on an answer ending in a colon",
			content: `{"answer":"The failing pods were:","citations":[],"assessment":"supports","proposed_revision":null}`,
			want:    generic,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, stats, err := parseAnalysisChatReplyCandidates(testCase.content, map[string]*analysisChatEvidence{})
			got := analysisChatRepairPrompt(testCase.content, stats, err, "no JSON response object found")
			if got != testCase.want {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

func TestAnalysisChatAnnouncementPromptOffersBothWaysOut(t *testing.T) {
	for _, want := range []string{
		"announced a next step instead of taking it",
		"make that tool call now",
		"return the analysis-conversation JSON object",
		"Output JSON only.",
	} {
		if !strings.Contains(analysisChatAnnouncementCorrectivePrompt, want) {
			t.Fatalf("announcement prompt missing %q", want)
		}
	}
}

func TestParseAnalysisChatReplyPrefersVerifiedCandidateOverEarlierDegradedDraft(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	draft := `{"answer":"draft","assessment":"supports","citations":[{"path":"build-log.txt","quote":"different evidence"}],"proposed_revision":null}`
	final := `{"answer":"The controller exit supports it.","assessment":"supports","citations":[{"path":"build-log.txt","quote":"controller stopped"}],"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(draft+"\n"+final, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Unverified || reply.Answer != "The controller exit supports it." || stats.EvidenceGate != "" {
		t.Fatalf("reply=%+v stats=%+v", reply, stats)
	}
	// The reverse order is still ambiguous: an unverified answer that supersedes
	// a verified one must not be reported as verified.
	if _, _, err := parseAnalysisChatReplyCandidates(final+"\n"+draft, evidence); err == nil {
		t.Fatal("trailing degraded candidate was ignored")
	}
}

// A verified answer offers a fix, so the quote cap must leave a worst-case
// conversation still generatable. Raising the cap without checking this would
// show the fix button and then fail generation.
func TestAnalysisChatQuoteCapFitsDownstreamBudgets(t *testing.T) {
	citations := make([]fixpr.Evidence, 0, 16)
	for range 16 {
		citations = append(citations, fixpr.Evidence{
			Path:  strings.Repeat("a", 120),
			Quote: strings.Repeat("q", analysisChatMaxQuoteBytes),
		})
	}
	context := fixpr.GenerationContext{
		AssistantAnswer:   strings.Repeat("s", 8<<10),
		ProposedRevision:  &fixpr.RevisionContext{RootCause: "cause", SuggestedFix: "fix"},
		ArtifactCitations: citations,
	}
	if err := context.Validate(); err != nil {
		t.Fatalf("a conversation at the quote cap cannot generate a fix: %v", err)
	}
}

// A citation whose passage does not fit the recording budget is unverified.
// Storing only its head would show the maintainer text that no longer covers
// what the citation claimed.
func TestAnalysisChatQuoteTooLongToRecordIsUnverified(t *testing.T) {
	line := strings.Repeat("x", 900)
	evidence := map[string]*analysisChatEvidence{"log.txt": {
		Segments: []string{line + "\n" + line + "\n" + line},
	}}
	raw := `{"answer":"a","citations":[{"path":"log.txt","quote":"` + line + ` ` + line + ` ` + line + `"}],"assessment":null,"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !reply.Unverified || !strings.Contains(stats.EvidenceDetail, "too long to record") {
		t.Fatalf("reply=%+v detail=%q", reply, stats.EvidenceDetail)
	}
}

// A cited line range that does not fit narrows to the lines that do, so the
// stored text and the cited lines still describe each other. Narrowing past the
// passage the model pointed at leaves nothing to verify.
func TestAnalysisChatQuoteCapNarrowsTheCitedRange(t *testing.T) {
	line := strings.Repeat("x", 900)
	evidence := map[string]*analysisChatEvidence{"log.txt": {
		Segments: []string{line + "\n" + line + "\n" + line},
		Lines:    map[int]string{10: line, 11: line, 12: line},
	}}
	raw := `{"answer":"a","citations":[{"path":"log.txt","line_start":10,"line_end":12,"quote":"` + line[:20] + `"}],"assessment":null,"proposed_revision":null}`
	reply, stats, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil || stats.EvidenceGate != "" {
		t.Fatalf("an over-cap range was rejected: gate=%q err=%v", stats.EvidenceGate, err)
	}
	citation := reply.Citations[0]
	if citation.Quote != line+"\n"+line || citation.LineStart != 10 || citation.LineEnd != 11 {
		t.Fatalf("clamped citation = %+v", citation)
	}
	long := strings.Repeat("x", 1000)
	dropped := map[string]*analysisChatEvidence{"log.txt": {
		Segments: []string{long + "\n" + long + "\nFATAL: OOM"},
		Lines:    map[int]string{10: long, 11: long, 12: "FATAL: OOM"},
	}}
	raw = `{"answer":"a","citations":[{"path":"log.txt","line_start":10,"line_end":12,"quote":"FATAL: OOM"}],"assessment":null,"proposed_revision":null}`
	reply, stats, err = parseAnalysisChatReplyCandidates(raw, dropped)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !reply.Unverified || !strings.Contains(stats.EvidenceDetail, "too long to record") {
		t.Fatalf("narrowing past the cited passage stayed verified: reply=%+v detail=%q", reply, stats.EvidenceDetail)
	}
}

// A quote of nothing but colour codes normalizes away, and an empty locator
// would otherwise match any passage.
func TestAnalysisChatCitationRejectsEmptyNormalizedQuote(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"log.txt": {Segments: []string{"controller stopped"}}}
	raw := `{"answer":"a","citations":[{"path":"log.txt","quote":"\u001b[31m"}],"assessment":null,"proposed_revision":null}`
	reply, _, err := parseAnalysisChatReplyCandidates(raw, evidence)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !reply.Unverified {
		t.Fatalf("colour-code-only quote was verified: %+v", reply)
	}
	if normalizedQuoteInRange(map[int]string{1: "controller stopped"}, 1, 1, "\x1b[31m") {
		t.Fatal("colour-code-only analyzer quote matched a range")
	}
}

// A single line longer than the cap has no line boundary to trim on, so it is
// cut on a rune boundary instead of rejecting the citation.
func TestAnalysisChatQuoteCapClampsOneLongLine(t *testing.T) {
	line := strings.Repeat("é", analysisChatMaxQuoteBytes)
	quote, kept := clampAnalysisChatQuote(line)
	if len(quote) > analysisChatMaxQuoteBytes || !strings.HasPrefix(line, quote) || kept != 1 {
		t.Fatalf("clamped quote length = %d kept = %d", len(quote), kept)
	}
	if !utf8.ValidString(quote) {
		t.Fatal("clamped quote is not valid UTF-8")
	}
	// Artifact bytes are not guaranteed to be valid UTF-8, and the quote is now
	// the engine's copy of them rather than decoded model text.
	if quote, _ := clampAnalysisChatQuote("controller \xff stopped"); !utf8.ValidString(quote) {
		t.Fatalf("under-cap quote is not valid UTF-8: %q", quote)
	}
}

// Colour codes are presentation, but cursor movement changes where text
// renders, so only the first may be dropped from a locator.
func TestAnalysisChatEvidenceSeparatesPresentationFromPositioning(t *testing.T) {
	evidence := &analysisChatEvidence{Segments: []string{
		"  \x1b[38;5;9mstatus: failed\n    reason: timeout",
		"cursor=\x1b[2Amoved",
	}}
	if _, matches := attributeAnalysisChatQuote(evidence, "status: failed\nreason: timeout"); matches != 1 {
		t.Fatal("locator dropping colour codes and indentation was rejected")
	}
	if _, matches := attributeAnalysisChatQuote(evidence, "cursor=moved"); matches != 0 {
		t.Fatal("locator dropping a cursor-movement sequence was accepted")
	}
}

func TestAnalysisChatPromptStatesTheQuoteRules(t *testing.T) {
	for _, want := range []string{
		"copy enough of the tool output to identify one passage and no other",
		"the engine replaces it with the exact text the tool returned",
		"matches several passages cannot be resolved",
	} {
		if !strings.Contains(analysisChatResponseFormat, want) {
			t.Fatalf("prompt does not state %q", want)
		}
	}
}

func TestAnalysisChatCauseSourcesResolveHistoricalAndCurrent(t *testing.T) {
	const historical = "0123456789abcdef0123456789abcdef01234567"
	const current = "89abcdef0123456789abcdef0123456789abcdef"
	for _, testCase := range []struct {
		name            string
		resolved        string
		status          int
		wantSources     int
		wantCurrentID   bool
		wantUnavailable bool
	}{
		{name: "distinct revisions", resolved: current, status: http.StatusOK, wantSources: 2, wantCurrentID: true},
		{name: "equal revisions", resolved: historical, status: http.StatusOK, wantSources: 1},
		{name: "current unavailable", status: http.StatusNotFound, wantSources: 1, wantUnavailable: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/repos/kubernetes-sigs/cluster-api-provider-azure/commits/release-1.2" {
					http.NotFound(w, r)
					return
				}
				if testCase.status != http.StatusOK {
					http.Error(w, "unavailable", testCase.status)
					return
				}
				_, _ = w.Write([]byte(`{"sha":"` + testCase.resolved + `"}`))
			}))
			defer github.Close()
			oldAPI := githubAPIBase
			githubAPIBase = github.URL
			defer func() { githubAPIBase = oldAPI }()

			server := newScriptedChatServer(t)
			server.push(200, chatRespFinal(`{"answer":"The artifact context is sufficient.","citations":[],"assessment":"explains","proposed_revision":null}`))
			agent := newAnalysisChatAgentWithRepoToolsForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{
				MaxIters: 2, Timeout: time.Second,
				SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
			})
			turn := causeAnalysisChatTurn()
			turn.Build.RepoRefs = map[string]string{
				"kubernetes-sigs/cluster-api-provider-azure": "release-1.2:" + historical,
			}
			turn.EvidenceBuilds[0].Build = turn.Build
			if _, err := agent.Reply(t.Context(), turn); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("branch-tip lookups = %d, want 1", calls)
			}
			request := string(server.requests[0])
			for _, want := range []string{"selected failed build 123", "release-1.2", historical, "comparison-build artifacts", "current source investigation"} {
				if !strings.Contains(request, want) {
					t.Fatalf("request missing %q: %s", want, request)
				}
			}
			if got := strings.Count(request, "source_id `current`: "); (got == 1) != testCase.wantCurrentID {
				t.Fatalf("current catalog entries = %d, wantCurrent=%t", got, testCase.wantCurrentID)
			}
			if testCase.wantCurrentID && !strings.Contains(request, current) {
				t.Fatalf("request missing current revision: %s", request)
			}
			if got := strings.Contains(request, "current immutable revision could not be resolved"); got != testCase.wantUnavailable {
				t.Fatalf("unavailable guidance present=%t want=%t: %s", got, testCase.wantUnavailable, request)
			}
		})
	}
}

func TestAnalysisChatPreparedCauseUsesHistoricalSourceWithoutCurrentLookup(t *testing.T) {
	shrinkCallDelay(t)
	const historicalRevision = "0123456789abcdef0123456789abcdef01234567"
	apiCalls := 0
	github := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { apiCalls++ }))
	defer github.Close()
	sourceReads := 0
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceReads++
		want := "/kubernetes-sigs/cluster-api-provider-azure/" + historicalRevision + "/controllers/fix.go"
		if r.URL.Path != want {
			t.Fatalf("historical source path = %q, want %q", r.URL.Path, want)
		}
		_, _ = w.Write([]byte("historical fix target\n"))
	}))
	defer raw.Close()
	oldAPI, oldRaw := githubAPIBase, rawContentBase
	githubAPIBase, rawContentBase = github.URL, raw.URL
	defer func() { githubAPIBase, rawContentBase = oldAPI, oldRaw }()

	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("source-1", "read_repo_file", map[string]interface{}{
		"source_id": tools.PrimarySourceID, "path": "controllers/fix.go", "offset": 0, "length": 1024,
	}))
	server.push(200, chatRespFinal(`{
		"answer":"The historical source supports revising the diagnosis.",
		"citations":[{"repository":"kubernetes-sigs/cluster-api-provider-azure","revision":"`+historicalRevision+`","path":"controllers/fix.go","line_start":1,"line_end":1,"quote":"historical fix target"}],
		"assessment":"challenges",
		"proposed_revision":{"root_cause":"The historical implementation used the wrong target.","suggested_fix":"Update the target after current investigation."}
	}`))
	agent := newAnalysisChatAgentWithRepoToolsForTest(t, server.URL, &fakeBrowser{}, AnalysisChatOptions{
		MaxIters: 3, Timeout: time.Second,
		SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
	})
	turn := causeAnalysisChatTurn()
	turn.Build.RepoRefs = map[string]string{
		"kubernetes-sigs/cluster-api-provider-azure": "main:" + historicalRevision,
	}
	turn.EvidenceBuilds[0].Build = turn.Build
	turn.HistoricalSourceOnly = true
	turn.Question = analysischat.PreparedCauseQuestion
	reply, err := agent.Reply(t.Context(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if apiCalls != 0 {
		t.Fatalf("prepared cause performed %d branch-tip lookups", apiCalls)
	}
	if sourceReads != 1 {
		t.Fatalf("historical source reads = %d, want 1", sourceReads)
	}
	if reply.ProposedRevision == nil || len(reply.EvidenceWarnings) != 0 || reply.Unverified || len(reply.Citations) != 1 {
		t.Fatalf("prepared historical reply = %+v", reply)
	}
	citation := reply.Citations[0]
	if citation.Repository != "kubernetes-sigs/cluster-api-provider-azure" || citation.Revision != historicalRevision ||
		citation.Path != "controllers/fix.go" || citation.LineStart != 1 || citation.LineEnd != 1 || citation.Quote != "historical fix target" {
		t.Fatalf("historical citation = %+v", citation)
	}
	request := string(server.requests[0])
	for _, name := range []string{"list_repo_tree", "read_repo_file", "grep_repo"} {
		if !strings.Contains(request, `"name":"`+name+`"`) {
			t.Fatalf("prepared cause omitted %s: %s", name, request)
		}
	}
	for _, want := range []string{
		"source_id `primary`:", historicalRevision,
		"newest later completed run", "evidence of recovery", "challenge it and propose a revision",
		"Historical source evidence does not establish current remediation state.",
	} {
		if !strings.Contains(request, want) {
			t.Fatalf("prepared request missing %q: %s", want, request)
		}
	}
	if strings.Contains(request, "source_id `current`:") {
		t.Fatalf("prepared cause received a current source entry: %s", request)
	}
}

func TestAnalysisChatSourceCitationsAreIsolatedByCatalogSource(t *testing.T) {
	primaryRevision := strings.Repeat("1", 40)
	currentRevision := strings.Repeat("2", 40)
	reader := &countingSourceReader{}
	catalog := testSourceCatalog(t, tools.PrimarySourceID,
		tools.RepoSource{ID: tools.PrimarySourceID, Owner: "Example", Name: "Project", Revision: primaryRevision, Reader: reader},
		tools.RepoSource{ID: analysisChatCurrentSourceID, Owner: "Example", Name: "Project", Revision: currentRevision, Reader: reader},
	)
	context := &analysisChatSourceCitationContext{
		Catalog: catalog, CurrentSourceID: analysisChatCurrentSourceID,
		Evidence: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{
			{SourceID: tools.PrimarySourceID, Path: "pkg/same.go"}: {
				Segments: []string{"historical implementation"}, Lines: map[int]string{10: "historical implementation"},
			},
			{SourceID: analysisChatCurrentSourceID, Path: "pkg/same.go"}: {
				Segments: []string{"current implementation"}, Lines: map[int]string{20: "current implementation"},
			},
		},
	}
	for _, testCase := range []struct {
		name     string
		revision string
		line     int
		quote    string
	}{
		{name: "primary", revision: strings.ToUpper(primaryRevision), line: 10, quote: "historical implementation"},
		{name: "current", revision: strings.ToUpper(currentRevision), line: 20, quote: "current implementation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			citation := analysischat.Citation{
				Repository: " EXAMPLE/PROJECT ", Revision: testCase.revision, Path: "./pkg/same.go",
				LineStart: testCase.line, LineEnd: testCase.line, Quote: testCase.quote,
			}
			if failure := validateAnalysisChatCitation(&citation, nil, context, 1); failure != nil {
				t.Fatalf("citation failure = %+v", failure)
			}
			if citation.Repository != "Example/Project" || citation.Revision != strings.ToLower(testCase.revision) || citation.Path != "pkg/same.go" {
				t.Fatalf("canonical citation = %+v", citation)
			}
		})
	}

	third := analysischat.Citation{Repository: "example/project", Revision: strings.Repeat("3", 40), Path: "pkg/same.go", Quote: "historical implementation"}
	if failure := validateAnalysisChatCitation(&third, nil, context, 1); failure == nil || failure.Gate != analysischat.UnverifiedReference {
		t.Fatalf("third-revision failure = %+v", failure)
	}
	crossSource := analysischat.Citation{
		Repository: "example/project", Revision: currentRevision, Path: "pkg/same.go",
		LineStart: 10, LineEnd: 10, Quote: "historical implementation",
	}
	if failure := validateAnalysisChatCitation(&crossSource, nil, context, 1); failure == nil {
		t.Fatalf("cross-source citation verified: %+v", crossSource)
	}
	if reader.lists != 0 || reader.reads != 0 {
		t.Fatalf("citation validation accessed source readers: lists=%d reads=%d", reader.lists, reader.reads)
	}
}

func TestAnalysisChatProposedRevisionRequiresCurrentSourceCitation(t *testing.T) {
	primaryRevision := strings.Repeat("1", 40)
	currentRevision := strings.Repeat("2", 40)
	reader := &countingSourceReader{}
	catalog := testSourceCatalog(t, tools.PrimarySourceID,
		tools.RepoSource{ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: primaryRevision, Reader: reader},
		tools.RepoSource{ID: analysisChatCurrentSourceID, Owner: "example", Name: "project", Revision: currentRevision, Reader: reader},
	)
	sourceEvidence := map[analysisChatSourceEvidenceKey]*analysisChatEvidence{
		{SourceID: tools.PrimarySourceID, Path: "pkg/fix.go"}:       {Segments: []string{"historical code"}, Lines: map[int]string{}},
		{SourceID: analysisChatCurrentSourceID, Path: "pkg/fix.go"}: {Segments: []string{"current code"}, Lines: map[int]string{}},
	}
	equalCatalog := testSourceCatalog(t, tools.PrimarySourceID,
		tools.RepoSource{ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: primaryRevision, Reader: reader},
	)
	artifactEvidence := map[string]*analysisChatEvidence{
		"builds/123/build-log.txt": {Segments: []string{"controller stopped"}, Lines: map[int]string{}},
	}
	proposal := `"proposed_revision":{"root_cause":"The controller stopped.","suggested_fix":"Update the controller."}`
	for _, testCase := range []struct {
		name              string
		citation          string
		context           *analysisChatSourceCitationContext
		wantProposal      bool
		wantWarning       string
		wantEvidenceCount int
	}{
		{
			name: "current citation", wantProposal: true, wantEvidenceCount: 1,
			citation: `{"repository":"example/project","revision":"` + currentRevision + `","path":"pkg/fix.go","quote":"current code"}`,
			context:  &analysisChatSourceCitationContext{Catalog: catalog, Evidence: sourceEvidence, CurrentSourceID: analysisChatCurrentSourceID},
		},
		{
			name: "equal revision primary citation", wantProposal: true, wantEvidenceCount: 1,
			citation: `{"repository":"example/project","revision":"` + primaryRevision + `","path":"pkg/fix.go","quote":"historical code"}`,
			context:  &analysisChatSourceCitationContext{Catalog: equalCatalog, Evidence: sourceEvidence, CurrentSourceID: tools.PrimarySourceID},
		},
		{
			name: "historical citation", wantWarning: "did not cite", wantEvidenceCount: 1,
			citation: `{"repository":"example/project","revision":"` + primaryRevision + `","path":"pkg/fix.go","quote":"historical code"}`,
			context:  &analysisChatSourceCitationContext{Catalog: catalog, Evidence: sourceEvidence, CurrentSourceID: analysisChatCurrentSourceID},
		},
		{
			name: "artifact only", wantWarning: "did not cite", wantEvidenceCount: 1,
			citation: `{"repository":null,"revision":null,"path":"builds/123/build-log.txt","quote":"controller stopped"}`,
			context:  &analysisChatSourceCitationContext{Catalog: catalog, Evidence: sourceEvidence, CurrentSourceID: analysisChatCurrentSourceID},
		},
		{
			name: "current unavailable", wantWarning: "was unavailable", wantEvidenceCount: 1,
			citation: `{"repository":null,"revision":null,"path":"builds/123/build-log.txt","quote":"controller stopped"}`,
			context:  &analysisChatSourceCitationContext{Catalog: catalog, Evidence: sourceEvidence},
		},
		{
			name: "prepared historical only", wantProposal: true, wantEvidenceCount: 1,
			citation: `{"repository":null,"revision":null,"path":"builds/123/build-log.txt","quote":"controller stopped"}`,
			context:  &analysisChatSourceCitationContext{HistoricalSourceOnly: true},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := `{"answer":"Keep this answer unchanged.","assessment":"challenges","citations":[` + testCase.citation + `],` + proposal + `}`
			reply, stats, err := parseAnalysisChatReplyCandidatesWithSource(raw, artifactEvidence, testCase.context)
			if err != nil || stats.EvidenceGate != "" {
				t.Fatalf("reply=%+v stats=%+v err=%v", reply, stats, err)
			}
			if (reply.ProposedRevision != nil) != testCase.wantProposal || reply.Answer != "Keep this answer unchanged." || reply.Assessment != "challenges" || reply.Unverified {
				t.Fatalf("reply = %+v", reply)
			}
			if len(reply.Citations) != testCase.wantEvidenceCount {
				t.Fatalf("citations = %+v", reply.Citations)
			}
			warnings := strings.Join(reply.EvidenceWarnings, " ")
			if (testCase.wantWarning == "") != (warnings == "") || testCase.wantWarning != "" && !strings.Contains(warnings, testCase.wantWarning) {
				t.Fatalf("warnings = %v, want %q", reply.EvidenceWarnings, testCase.wantWarning)
			}
		})
	}

	warnings := make([]string, analysisChatMaxEvidenceWarnings)
	for i := range warnings {
		warnings[i] = fmt.Sprintf("warning-%d", i)
	}
	reply := analysischat.Reply{
		Answer: "unchanged", Assessment: "challenges", EvidenceWarnings: warnings,
		ProposedRevision: &analysischat.Revision{RootCause: "cause", SuggestedFix: "fix"},
	}
	qualifyAnalysisChatRemediationFreshness(&reply, &analysisChatSourceCitationContext{Catalog: catalog, CurrentSourceID: analysisChatCurrentSourceID})
	if reply.ProposedRevision != nil || len(reply.EvidenceWarnings) != analysisChatMaxEvidenceWarnings || reply.EvidenceWarnings[len(reply.EvidenceWarnings)-1] != "warning-19" {
		t.Fatalf("bounded warnings reply = %+v", reply)
	}
}

func TestRecordSourceContentKeepsCurrentEvidenceSeparateFromPrimaryGrounding(t *testing.T) {
	primaryRevision := strings.Repeat("1", 40)
	currentRevision := strings.Repeat("2", 40)
	reader := &fakeSourceRepo{files: map[string]string{}}
	catalog := testSourceCatalog(t, tools.PrimarySourceID,
		tools.RepoSource{ID: tools.PrimarySourceID, Owner: "example", Name: "project", Revision: primaryRevision, Reader: reader},
		tools.RepoSource{ID: analysisChatCurrentSourceID, Owner: "example", Name: "project", Revision: currentRevision, Reader: reader},
	)
	state := &agentState{
		sources: catalog, sourceEvidenceByPath: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{},
		readSourceFull: map[string]bool{},
	}
	call := modelToolCall{Function: modelFunction{
		Name: "read_repo_file", Arguments: `{"source_id":"current","path":"pkg/same.go"}`,
	}}
	payload := map[string]interface{}{
		"source_id": analysisChatCurrentSourceID, "content": "current line\n", "length": len("current line\n"),
	}
	badObservation := repotree.ReadObservation{
		SourceID: tools.PrimarySourceID, Path: "pkg/same.go", LineStart: 1, LineEnd: 1, ByteStart: 0, ByteEnd: len("current line\n"),
	}
	state.recordSourceContent(call, payload, badObservation)
	key := analysisChatSourceEvidenceKey{SourceID: analysisChatCurrentSourceID, Path: "pkg/same.go"}
	if evidence := state.sourceEvidenceByPath[key]; evidence != nil {
		t.Fatalf("mismatched observation recorded evidence: %+v", evidence)
	}
	goodObservation := badObservation
	goodObservation.SourceID = analysisChatCurrentSourceID
	state.recordSourceContent(call, payload, goodObservation)
	if evidence := state.sourceEvidenceByPath[key]; evidence == nil || evidence.Lines[1] != "current line" {
		t.Fatalf("current source lines = %+v", evidence)
	}
	if len(state.sourceContentByPath) != 0 || len(state.readSourceFull) != 0 {
		t.Fatalf("current source grounded primary-only state: content=%v reads=%v", state.sourceContentByPath, state.readSourceFull)
	}
}

func TestAnalysisChatCauseSourceResolutionPropagatesCancellation(t *testing.T) {
	oldAPI := githubAPIBase
	githubAPIBase = "http://127.0.0.1:1"
	defer func() { githubAPIBase = oldAPI }()
	agent := &AnalysisChatAgent{opts: AnalysisChatOptions{
		SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
	}}
	turn := causeAnalysisChatTurn()
	turn.Build.RepoRefs = map[string]string{
		"kubernetes-sigs/cluster-api-provider-azure": "main:0123456789abcdef0123456789abcdef01234567",
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := agent.resolveCauseSources(ctx, turn); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveCauseSources error = %v", err)
	}
}

func TestAnalysisChatCauseSourceResolutionDoesNotCacheBranchTipsAcrossTurns(t *testing.T) {
	const current = "89abcdef0123456789abcdef0123456789abcdef"
	calls := 0
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"sha":"` + current + `"}`))
	}))
	defer github.Close()
	oldAPI := githubAPIBase
	githubAPIBase = github.URL
	defer func() { githubAPIBase = oldAPI }()

	agent := &AnalysisChatAgent{opts: AnalysisChatOptions{
		SourceRepoOwner: "kubernetes-sigs", SourceRepoName: "cluster-api-provider-azure",
	}}
	turn := causeAnalysisChatTurn()
	turn.Build.RepoRefs = map[string]string{
		"kubernetes-sigs/cluster-api-provider-azure": "main:0123456789abcdef0123456789abcdef01234567",
	}
	for range 2 {
		_, state, err := agent.resolveCauseSources(t.Context(), turn)
		if err != nil || state.CurrentRevision != current {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	}
	if calls != 2 {
		t.Fatalf("branch-tip lookups = %d, want one per turn", calls)
	}
}
