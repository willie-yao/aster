package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/aitest"
)

// stubTool is a trivial tool for exercising the loop plumbing. It records how
// many times it was dispatched and echoes its "msg" arg back.
type stubTool struct{ calls int }

func (*stubTool) Name() string  { return "echo" }
func (*stubTool) Group() string { return "stub" }
func (*stubTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "echo",
			Description: "echo the msg arg",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"msg": map[string]interface{}{"type": "string"}},
				"required":   []string{"msg"},
			},
		},
	}
}

type requiredStubTool struct {
	name      string
	calls     int
	arguments []string
	result    func(json.RawMessage) tools.Result
}

func (s *requiredStubTool) Name() string { return s.name }
func (*requiredStubTool) Group() string  { return "required" }
func (s *requiredStubTool) Schema() tools.Schema {
	return tools.Schema{Type: "function", Function: tools.FunctionDecl{
		Name: s.name, Description: "required test tool",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
			},
		},
	}}
}
func (s *requiredStubTool) Dispatch(_ context.Context, _ *tools.Env, raw json.RawMessage) tools.Result {
	s.calls++
	s.arguments = append(s.arguments, string(raw))
	if s.result != nil {
		return s.result(raw)
	}
	return tools.Result{Payload: map[string]interface{}{"ok": true}}
}

type toolLoopTransportResult struct {
	response *modelResponse
	err      error
}

type toolLoopTransport struct {
	requests []modelRequest
	results  []toolLoopTransportResult
}

func (t *toolLoopTransport) Complete(ctx context.Context, request modelRequest) (*modelResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.requests = append(t.requests, cloneToolLoopRequest(request))
	result := t.results[len(t.requests)-1]
	return result.response, result.err
}

func cloneToolLoopRequest(request modelRequest) modelRequest {
	request.Messages = append([]modelMessage(nil), request.Messages...)
	for index := range request.Messages {
		request.Messages[index].ToolCalls = append([]modelToolCall(nil), request.Messages[index].ToolCalls...)
		request.Messages[index].ProviderItems = append([]json.RawMessage(nil), request.Messages[index].ProviderItems...)
	}
	request.Tools = append([]tools.Schema(nil), request.Tools...)
	return request
}

func toolLoopFinal(content string) *modelResponse {
	return &modelResponse{HasMessage: true, Message: modelMessage{Role: "assistant", Content: strPtr(content)}}
}

func toolLoopCall(id, name, arguments string) *modelResponse {
	return &modelResponse{HasMessage: true, Message: modelMessage{Role: "assistant", ToolCalls: []modelToolCall{{
		ID: id, Type: "function", Function: modelFunction{Name: name, Arguments: arguments},
	}}}}
}

func newRecordedToolLoopClient(apiMode string, results ...*modelResponse) (*Client, *toolLoopTransport) {
	transport := &toolLoopTransport{}
	for _, response := range results {
		transport.results = append(transport.results, toolLoopTransportResult{response: response})
	}
	return &Client{transport: transport, model: "m", apiMode: apiMode}, transport
}
func (s *stubTool) Dispatch(_ context.Context, _ *tools.Env, raw json.RawMessage) tools.Result {
	s.calls++
	var a struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &a)
	return tools.Result{Payload: map[string]interface{}{"echo": a.Msg}}
}

func newLoopClient(url string) *Client {
	return NewClientWithOptions(Options{Token: "t", Endpoint: url, Model: "m"})
}

func TestToolLoop_ToolThenFinal(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "echo", map[string]any{"msg": "hi"})
	script.PushFinal(`{"files":["pkg/a.go"]}`)

	stub := &stubTool{}
	reg := tools.NewRegistry()
	reg.Register(stub)

	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MaxIters: 5},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["pkg/a.go"]}` {
		t.Errorf("final = %q, want the files JSON", out)
	}
	if stub.calls != 1 {
		t.Errorf("tool dispatched %d times, want 1", stub.calls)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (tool turn + final)", script.ChatCalls())
	}
}

func TestToolLoop_ExhaustsItersThenForceFinalize(t *testing.T) {
	script := aitest.NewScriptServer(t)
	// One tool call fills the only iteration; the loop then forces a finalize
	// round (tools omitted) which the second response answers.
	script.PushToolCall("c1", "echo", map[string]any{"msg": "again"})
	script.PushFinal(`{"files":["x.yaml"]}`)

	stub := &stubTool{}
	reg := tools.NewRegistry()
	reg.Register(stub)

	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MaxIters: 1},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["x.yaml"]}` {
		t.Errorf("finalize result = %q, want the files JSON", out)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (tool turn + forced finalize)", script.ChatCalls())
	}
}

func TestToolLoop_ImmediateFinal(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushFinal(`done`)

	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != "done" {
		t.Errorf("out = %q, want done", out)
	}
}

func TestToolLoop_GroupNameNotResolved(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&stubTool{}) // group "stub", tool name "echo"
	_, err := newLoopClient("http://unused").ToolLoop(
		context.Background(), "sys", "user", reg, []string{"stub"}, &tools.Env{},
		ToolLoopOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "no tools enabled") {
		t.Errorf("expected no-tools-enabled error for a group alias, got %v", err)
	}
}

// TestToolLoop_MinToolCallsNudgesOnce verifies a premature tools-free answer is
// nudged once, then a second insistent answer is accepted so the loop still
// terminates.
func TestToolLoop_MinToolCallsNudgesOnce(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushFinal(`{"files":["a.go"]}`) // premature: no tool calls yet
	script.PushFinal(`{"files":["a.go"]}`) // after the nudge, model insists

	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MinToolCalls: 1},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["a.go"]}` {
		t.Errorf("out = %q", out)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (premature answer + nudge retry)", script.ChatCalls())
	}
}

func TestToolLoopFinalizeUnexpectedToolCallIsCategorized(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "echo", map[string]any{"msg": "again"})
	script.PushToolCall("unexpected", "echo", map[string]any{"msg": "still calling"})

	stub := &stubTool{}
	reg := tools.NewRegistry()
	reg.Register(stub)
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	out, err := newLoopClient(script.URL).ToolLoop(
		ctx, "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MaxIters: 1, PropagateFinalizeError: true},
	)
	trace.Finish("success", err)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("finalize output = %q, want empty", out)
	}
	found := false
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "finalize" && event.Outcome == "empty" && event.ErrorCode == "unexpected_tool_call" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing finalize category: %+v", store.Snapshot())
	}
}

func TestToolLoopObserveReportsContentFreeDispatchTelemetry(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "echo", map[string]any{"msg": "hi"})
	script.PushFinal(`done`)

	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	var events []ToolLoopEvent
	_, err := newLoopClient(script.URL).ToolLoop(
		t.Context(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{Observe: func(event ToolLoopEvent) { events = append(events, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Name != "echo" || events[0].Error || events[0].Path != "" {
		t.Fatalf("events=%+v", events)
	}
}

func TestToolLoopPrivateObservationIsOptIn(t *testing.T) {
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": "data"}, ContentBytes: 4, Observation: "private-metadata"}
	}}
	reg := tools.NewRegistry()
	reg.Register(read)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopCall("c1", "read", `{"path":"chosen.go"}`),
		toolLoopFinal("done"),
	)
	var privateEvents []ToolLoopPrivateEvent
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{
		ObservePrivate: func(event ToolLoopPrivateEvent) { privateEvents = append(privateEvents, event) },
	})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(privateEvents) != 1 || privateEvents[0].Name != "read" || privateEvents[0].Observation != "private-metadata" || privateEvents[0].Error {
		t.Fatalf("private events=%+v", privateEvents)
	}
	for _, request := range transport.requests {
		encoded, _ := json.Marshal(request.Messages)
		if strings.Contains(string(encoded), "private-metadata") {
			t.Fatal("private tool observation entered the model transcript")
		}
	}
}

func TestToolLoopRequiredToolsCompleteVoluntarilyInOrder(t *testing.T) {
	list := &requiredStubTool{name: "list"}
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": "data"}, ContentBytes: 4}
	}}
	reg := tools.NewRegistry()
	reg.Register(list)
	reg.Register(read)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopCall("c1", "list", `{}`),
		toolLoopCall("c2", "read", `{"path":"chosen.go"}`),
		toolLoopFinal("done"),
	)
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"list", "read"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 5,
		RequiredTools: []RequiredTool{
			{Name: "list", CorrectivePrompt: "List the repository."},
			{Name: "read", CorrectivePrompt: "Read one file.", RequireContent: true},
		},
	})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if list.calls != 1 || read.calls != 1 {
		t.Fatalf("list calls=%d read calls=%d", list.calls, read.calls)
	}
	for index, request := range transport.requests {
		if request.ToolChoice != nil {
			t.Fatalf("request %d unexpectedly forced %+v", index+1, request.ToolChoice)
		}
	}
}

func TestToolLoopRequiredReadAndGrepCompleteVoluntarily(t *testing.T) {
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": "source"}, ContentBytes: 6}
	}}
	grep := &requiredStubTool{name: "grep", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"matches": []string{"match"}}, ContentBytes: 5}
	}}
	reg := tools.NewRegistry()
	reg.Register(read)
	reg.Register(grep)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopCall("c1", "read", `{"path":"chosen.go"}`),
		toolLoopCall("c2", "grep", `{"pattern":"ExactIdentifier"}`),
		toolLoopFinal("done"),
	)
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read", "grep"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 5, RequiredTools: []RequiredTool{
			{Name: "read", CorrectivePrompt: "Read source.", RequireContent: true},
			{Name: "grep", CorrectivePrompt: "Search exact identifiers.", RequireContent: true},
		},
	})
	if err != nil || out != "done" || read.calls != 1 || grep.calls != 1 {
		t.Fatalf("out=%q err=%v read=%d grep=%d", out, err, read.calls, grep.calls)
	}
	for index, request := range transport.requests {
		if request.ToolChoice != nil {
			t.Fatalf("request %d unexpectedly forced %+v", index+1, request.ToolChoice)
		}
	}
}

func TestToolLoopRequiredZeroMatchGrepExhausts(t *testing.T) {
	grep := &requiredStubTool{name: "grep", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"matches": []string{}}, ContentBytes: 0}
	}}
	reg := tools.NewRegistry()
	reg.Register(grep)
	client, _ := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopFinal("premature"),
		toolLoopCall("c1", "grep", `{"pattern":"missing"}`),
		toolLoopFinal("still premature"),
	)
	_, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"grep"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 4, RequiredTools: []RequiredTool{{
			Name: "grep", CorrectivePrompt: "Search.", MaxAttempts: 1, RequireContent: true,
		}},
	})
	var requiredErr *RequiredToolError
	if !errors.As(err, &requiredErr) || requiredErr.Tool != "grep" || requiredErr.Code != RequiredToolAttemptsExhausted || requiredErr.Attempts != 1 {
		t.Fatalf("error=%v", err)
	}
}

func TestToolLoopForcesMissingRequiredToolWithModelArguments(t *testing.T) {
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": "data"}, BytesFetched: 4, ContentBytes: 4}
	}}
	reg := tools.NewRegistry()
	reg.Register(read)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopFinal("premature"),
		toolLoopCall("c1", "read", `{"path":"model-selected.go"}`),
		toolLoopFinal("done"),
	)
	var events []ToolLoopEvent
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 5,
		RequiredTools: []RequiredTool{{
			Name: "read", CorrectivePrompt: "Call read now.", MaxAttempts: 1, RequireContent: true,
		}},
		Observe: func(event ToolLoopEvent) { events = append(events, event) },
	})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(read.arguments) != 1 || read.arguments[0] != `{"path":"model-selected.go"}` {
		t.Fatalf("arguments=%v", read.arguments)
	}
	if len(transport.requests) != 3 || transport.requests[1].ToolChoice == nil || transport.requests[1].ToolChoice.Name != "read" {
		t.Fatalf("requests=%+v", transport.requests)
	}
	if parallel := transport.requests[1].ParallelToolCalls; parallel == nil || *parallel {
		t.Fatalf("forced parallel_tool_calls=%v, want false", parallel)
	}
	if !requestContainsText(transport.requests[1], "premature") || !requestContainsText(transport.requests[1], "Call read now.") {
		t.Fatalf("forced request lost the existing conversation: %+v", transport.requests[1].Messages)
	}
	if len(events) != 1 || !events[0].Forced || events[0].Name != "read" || events[0].BytesFetched != 4 || events[0].ContentBytes != 4 {
		t.Fatalf("events=%+v", events)
	}
}

func TestToolLoopRejectsRequiredToolThatIsNotEnabled(t *testing.T) {
	for _, name := range []string{"read", "unknown"} {
		t.Run(name, func(t *testing.T) {
			reg := tools.NewRegistry()
			reg.Register(&stubTool{})
			reg.Register(&requiredStubTool{name: "read"})
			client, transport := newRecordedToolLoopClient(APIChatCompletions, toolLoopFinal("unused"))
			_, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"echo"}, &tools.Env{}, ToolLoopOptions{
				RequiredTools: []RequiredTool{{Name: name, CorrectivePrompt: "Read."}},
			})
			var requiredErr *RequiredToolError
			if !errors.As(err, &requiredErr) || requiredErr.Code != RequiredToolNotEnabled || requiredErr.Tool != name {
				t.Fatalf("error=%v", err)
			}
			if len(transport.requests) != 0 {
				t.Fatalf("provider called %d time(s)", len(transport.requests))
			}
		})
	}
}

func TestToolLoopRequiredToolRejectsDispatchErrorAndZeroContent(t *testing.T) {
	tests := []struct {
		name   string
		result tools.Result
	}{
		{name: "tool error", result: tools.ErrPayload("private tool detail")},
		{name: "zero byte read", result: tools.Result{Payload: map[string]interface{}{"content": ""}, BytesFetched: 100}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result { return tc.result }}
			reg := tools.NewRegistry()
			reg.Register(read)
			client, _ := newRecordedToolLoopClient(APIChatCompletions,
				toolLoopFinal("premature"),
				toolLoopCall("c1", "read", `{"path":"chosen.go"}`),
				toolLoopFinal("still premature"),
			)
			_, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{
				MaxIters: 4,
				RequiredTools: []RequiredTool{{
					Name: "read", CorrectivePrompt: "Read.", MaxAttempts: 1, RequireContent: true,
				}},
			})
			var requiredErr *RequiredToolError
			if !errors.As(err, &requiredErr) || requiredErr.Code != RequiredToolAttemptsExhausted || requiredErr.Attempts != 1 {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "private tool detail") || strings.Contains(err.Error(), "chosen.go") {
				t.Fatalf("required-tool error exposed private content: %v", err)
			}
		})
	}
}

func TestToolLoopRequiredToolAttemptsExhausted(t *testing.T) {
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": ""}}
	}}
	reg := tools.NewRegistry()
	reg.Register(read)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopFinal("premature one"),
		toolLoopCall("c1", "read", `{"path":"one.go"}`),
		toolLoopFinal("premature two"),
		toolLoopCall("c2", "read", `{"path":"two.go"}`),
		toolLoopFinal("premature three"),
	)
	_, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 6,
		RequiredTools: []RequiredTool{{
			Name: "read", CorrectivePrompt: "Read.", MaxAttempts: 2, RequireContent: true,
		}},
	})
	var requiredErr *RequiredToolError
	if !errors.As(err, &requiredErr) || requiredErr.Attempts != 2 {
		t.Fatalf("error=%v", err)
	}
	if read.calls != 2 || len(transport.requests) != 5 {
		t.Fatalf("read calls=%d requests=%d", read.calls, len(transport.requests))
	}
	for _, index := range []int{1, 3} {
		if choice := transport.requests[index].ToolChoice; choice == nil || choice.Name != "read" {
			t.Fatalf("request %d tool choice=%+v", index+1, choice)
		}
	}
}

func TestToolLoopRequiredToolSurvivesContextCompaction(t *testing.T) {
	echo := &requiredStubTool{name: "echo", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": strings.Repeat("x", 16<<10)}}
	}}
	read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
		return tools.Result{Payload: map[string]interface{}{"content": "data"}, BytesFetched: 4, ContentBytes: 4}
	}}
	reg := tools.NewRegistry()
	reg.Register(echo)
	reg.Register(read)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		toolLoopCall("c0", "echo", `{}`),
		toolLoopFinal("premature"),
		toolLoopCall("c1", "read", `{"path":"chosen.go"}`),
		toolLoopFinal("done"),
	)
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"echo", "read"}, &tools.Env{}, ToolLoopOptions{
		MaxIters: 5, ContextByteBudget: 2048,
		RequiredTools: []RequiredTool{{
			Name: "read", CorrectivePrompt: "Call read after compaction.", RequireContent: true,
		}},
	})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	forced := transport.requests[2]
	if forced.ToolChoice == nil || forced.ToolChoice.Name != "read" || !requestContainsText(forced, "Call read after compaction.") {
		t.Fatalf("forced request=%+v", forced)
	}
	if !requestContainsText(forced, elisionMarker) {
		t.Fatalf("forced request did not retain compacted conversation: %+v", forced.Messages)
	}
}

func TestToolLoopRequiredToolCancellationAndTimeout(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&requiredStubTool{name: "read"})
	requirements := []RequiredTool{{Name: "read", CorrectivePrompt: "Read.", RequireContent: true}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client, transport := newRecordedToolLoopClient(APIChatCompletions, toolLoopFinal("unused"))
	if _, err := client.ToolLoop(canceled, "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{RequiredTools: requirements}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	if len(transport.requests) != 0 {
		t.Fatalf("canceled loop made %d request(s)", len(transport.requests))
	}

	waiting := &waitingToolLoopTransport{}
	timeoutClient := &Client{transport: waiting, model: "m", apiMode: APIChatCompletions}
	ctx, timeoutCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer timeoutCancel()
	if _, err := timeoutClient.ToolLoop(ctx, "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{RequiredTools: requirements}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
	if waiting.calls != 1 {
		t.Fatalf("timeout transport calls=%d", waiting.calls)
	}
}

func TestToolLoopRequiredGrepChoiceTransportEncoding(t *testing.T) {
	shrinkCallDelay(t)
	tests := []struct {
		name      string
		api       string
		responses []string
		check     func(*testing.T, map[string]interface{})
	}{
		{
			name: "chat completions", api: APIChatCompletions,
			responses: []string{
				`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"chosen.go\"}"}}]}}]}`,
				`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"premature"}}]}`,
				`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"ExactIdentifier\"}"}}]}}]}`,
				`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`,
			},
			check: func(t *testing.T, request map[string]interface{}) {
				choice := request["tool_choice"].(map[string]interface{})
				function := choice["function"].(map[string]interface{})
				if choice["type"] != "function" || function["name"] != "grep" {
					t.Fatalf("tool_choice=%#v", choice)
				}
			},
		},
		{
			name: "responses", api: APIResponses,
			responses: []string{
				`{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"chosen.go\"}"}]}`,
				`{"id":"r2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"premature"}]}]}`,
				`{"id":"r3","status":"completed","output":[{"type":"function_call","call_id":"c2","name":"grep","arguments":"{\"pattern\":\"ExactIdentifier\"}"}]}`,
				`{"id":"r4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`,
			},
			check: func(t *testing.T, request map[string]interface{}) {
				choice := request["tool_choice"].(map[string]interface{})
				if choice["type"] != "function" || choice["name"] != "grep" {
					t.Fatalf("tool_choice=%#v", choice)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests []map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body map[string]interface{}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.responses[len(requests)-1])
			}))
			defer server.Close()
			read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
				return tools.Result{Payload: map[string]interface{}{"content": "source"}, ContentBytes: 6}
			}}
			grep := &requiredStubTool{name: "grep", result: func(json.RawMessage) tools.Result {
				return tools.Result{Payload: map[string]interface{}{"matches": []string{"match"}}, ContentBytes: 5}
			}}
			reg := tools.NewRegistry()
			reg.Register(read)
			reg.Register(grep)
			client := NewClientWithOptions(Options{API: tc.api, Endpoint: server.URL, Model: "m"})
			out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read", "grep"}, &tools.Env{}, ToolLoopOptions{
				MaxIters: 6, RequiredTools: []RequiredTool{
					{Name: "read", CorrectivePrompt: "Read.", RequireContent: true},
					{Name: "grep", CorrectivePrompt: "Search exact identifiers.", RequireContent: true},
				},
			})
			if err != nil || out != "done" {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if len(requests) != 4 || len(grep.arguments) != 1 || grep.arguments[0] != `{"pattern":"ExactIdentifier"}` {
				t.Fatalf("requests=%d grep args=%v", len(requests), grep.arguments)
			}
			tc.check(t, requests[2])
			if parallel, ok := requests[2]["parallel_tool_calls"].(bool); !ok || parallel {
				t.Fatalf("parallel_tool_calls=%#v", requests[2]["parallel_tool_calls"])
			}
		})
	}
}

func TestToolLoopRequiredToolChoiceTransportEncoding(t *testing.T) {
	shrinkCallDelay(t)
	tests := []struct {
		name      string
		api       string
		responses []string
		check     func(*testing.T, map[string]interface{})
	}{
		{
			name: "chat completions", api: APIChatCompletions,
			responses: []string{
				`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"premature"}}]}`,
				`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"chosen.go\"}"}}]}}]}`,
				`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`,
			},
			check: func(t *testing.T, request map[string]interface{}) {
				choice := request["tool_choice"].(map[string]interface{})
				function := choice["function"].(map[string]interface{})
				if choice["type"] != "function" || function["name"] != "read" {
					t.Fatalf("tool_choice=%#v", choice)
				}
			},
		},
		{
			name: "responses", api: APIResponses,
			responses: []string{
				`{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"premature"}]}]}`,
				`{"id":"r2","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"chosen.go\"}"}]}`,
				`{"id":"r3","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`,
			},
			check: func(t *testing.T, request map[string]interface{}) {
				choice := request["tool_choice"].(map[string]interface{})
				if choice["type"] != "function" || choice["name"] != "read" {
					t.Fatalf("tool_choice=%#v", choice)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests []map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body map[string]interface{}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.responses[len(requests)-1])
			}))
			defer server.Close()
			read := &requiredStubTool{name: "read", result: func(json.RawMessage) tools.Result {
				return tools.Result{Payload: map[string]interface{}{"content": "data"}, BytesFetched: 4, ContentBytes: 4}
			}}
			reg := tools.NewRegistry()
			reg.Register(read)
			client := NewClientWithOptions(Options{API: tc.api, Endpoint: server.URL, Model: "m"})
			out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"read"}, &tools.Env{}, ToolLoopOptions{
				MaxIters: 5,
				RequiredTools: []RequiredTool{{
					Name: "read", CorrectivePrompt: "Read.", RequireContent: true,
				}},
			})
			if err != nil || out != "done" {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if len(requests) != 3 {
				t.Fatalf("requests=%d", len(requests))
			}
			tc.check(t, requests[1])
			if parallel, ok := requests[1]["parallel_tool_calls"].(bool); !ok || parallel {
				t.Fatalf("parallel_tool_calls=%#v", requests[1]["parallel_tool_calls"])
			}
		})
	}
}

func TestToolLoopLegacyOptionsRemainAutomatic(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	client, transport := newRecordedToolLoopClient(APIChatCompletions, toolLoopFinal("done"))
	out, err := client.ToolLoop(t.Context(), "sys", "user", reg, []string{"echo"}, &tools.Env{}, ToolLoopOptions{})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(transport.requests) != 1 || transport.requests[0].ToolChoice != nil || transport.requests[0].ParallelToolCalls != nil {
		t.Fatalf("legacy request=%+v", transport.requests)
	}
}

func requestContainsText(request modelRequest, text string) bool {
	for _, message := range request.Messages {
		if message.Content != nil && strings.Contains(*message.Content, text) {
			return true
		}
	}
	return false
}

type waitingToolLoopTransport struct{ calls int }

func (t *waitingToolLoopTransport) Complete(ctx context.Context, _ modelRequest) (*modelResponse, error) {
	t.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}
