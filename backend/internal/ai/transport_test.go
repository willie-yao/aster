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

	wire := chatCompletionsResponse{}
	wire.Choices = append(wire.Choices, chatCompletionsChoice{
		FinishReason: "tool_calls", Message: encodeChatMessages(messages)[0],
	})
	decoded := decodeChatResponse(wire)
	if !decoded.HasMessage || decoded.FinishReason != "tool_calls" || !reflect.DeepEqual(decoded.Message, messages[0]) {
		t.Fatalf("decoded response = %+v", decoded)
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
