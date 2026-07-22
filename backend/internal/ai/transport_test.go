package ai

import (
	"context"
	"reflect"
	"testing"
)

type recordingTransport struct {
	request modelRequest
	result  *modelResponse
	err     error
}

func (t *recordingTransport) Complete(_ context.Context, req modelRequest) (*modelResponse, error) {
	t.request = req
	return t.result, t.err
}

func TestClientCompleteUsesModelTransport(t *testing.T) {
	transport := &recordingTransport{result: &modelResponse{
		HasMessage: true, Message: modelMessage{Role: "assistant", Content: strPtr("done")},
	}}
	client := &Client{model: "model-a", transport: transport}

	got, err := client.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "done" {
		t.Fatalf("Complete() = %q, want done", got)
	}
	wantMessages := []modelMessage{
		{Role: "system", Content: strPtr("system")},
		{Role: "user", Content: strPtr("user")},
	}
	if transport.request.Model != "model-a" || !reflect.DeepEqual(transport.request.Messages, wantMessages) {
		t.Fatalf("transport request = %+v", transport.request)
	}
}

func TestClientCallModelRecordsTrace(t *testing.T) {
	transport := &recordingTransport{result: &modelResponse{
		HasMessage: true, Message: modelMessage{Role: "assistant", ToolCalls: []modelToolCall{{ID: "call"}}},
		ResponseID: "resp-1", Status: "completed", FinishReason: "tool_calls",
		Attempts: 2, HTTPStatus: 200, InputTokens: 11, OutputTokens: 7,
	}}
	client := &Client{model: "model-a", transport: transport}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIResponses})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, err := client.callModel(ctx, []modelMessage{{Role: "user", Content: strPtr("user")}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	event := store.Snapshot().Traces[0].Events[0]
	if event.Kind != "model_request" || event.ResponseID != "resp-1" || event.Attempts != 2 || event.InputTokens != 11 || event.OutputTokens != 7 || event.ToolCallCount != 1 {
		t.Fatalf("event = %+v", event)
	}
}

func TestChatCompletionsMessageRoundTrip(t *testing.T) {
	messages := []modelMessage{
		{
			Role:    "assistant",
			Content: strPtr("reasoning"),
			ToolCalls: []modelToolCall{{
				ID: "call-1", Type: "function",
				Function: modelFunction{Name: "read_artifact", Arguments: `{"path":"log.txt"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Name: "read_artifact", Content: strPtr(`{"ok":true}`)},
	}

	wire := chatCompletionsResponse{ID: "chat-1", Usage: chatCompletionsUsage{PromptTokens: 12, CompletionTokens: 4}}
	wire.Choices = append(wire.Choices, chatCompletionsChoice{
		FinishReason: "tool_calls", Message: encodeChatMessages(messages)[0],
	})
	decoded := decodeChatResponse(wire)
	if !decoded.HasMessage || decoded.FinishReason != "tool_calls" || !reflect.DeepEqual(decoded.Message, messages[0]) {
		t.Fatalf("decoded response = %+v", decoded)
	}
	if decoded.ResponseID != "chat-1" || decoded.InputTokens != 12 || decoded.OutputTokens != 4 {
		t.Fatalf("decoded metadata = %+v", decoded)
	}
	if got := encodeChatMessages(messages); len(got) != 2 || got[1].ToolCallID != "call-1" || got[1].Name != "read_artifact" {
		t.Fatalf("encoded messages = %+v", got)
	}
}

func TestChatEncodingPreservesNilMessages(t *testing.T) {
	if got := encodeChatMessages(nil); got != nil {
		t.Fatalf("encodeChatMessages(nil) = %#v, want nil", got)
	}
	if got := decodeChatToolCalls(nil); got != nil {
		t.Fatalf("decodeChatToolCalls(nil) = %#v, want nil", got)
	}
}

func TestContinuationCallsPairsSkippedResponsesCalls(t *testing.T) {
	msg := modelMessage{ToolCalls: []modelToolCall{{ID: "a"}, {ID: "b"}}}
	echo, skipped := continuationCalls(APIResponses, msg, msg.ToolCalls[:1])
	if len(echo) != 2 || len(skipped) != 1 || skipped[0].ToolCallID != "b" {
		t.Fatalf("echo=%+v skipped=%+v", echo, skipped)
	}
	chatEcho, chatSkipped := continuationCalls(APIChatCompletions, msg, msg.ToolCalls[:1])
	if len(chatEcho) != 1 || len(chatSkipped) != 0 {
		t.Fatalf("chat echo=%+v skipped=%+v", chatEcho, chatSkipped)
	}
}
