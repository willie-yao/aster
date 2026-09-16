package transport

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestChatTokenUsageDistinguishesAbsentAndZero(t *testing.T) {
	if got := chatTokenUsage(nil); got.Reported {
		t.Fatalf("absent usage = %+v", got)
	}
	if got := chatTokenUsage(&chatCompletionsUsage{}); !got.Reported || got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("present zero usage = %+v", got)
	}
}

func TestChatTokenUsageRecognizesCacheCreationFields(t *testing.T) {
	var wire chatCompletionsResponse
	if err := json.Unmarshal([]byte(`{"usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}`), &wire); err != nil {
		t.Fatal(err)
	}
	got := chatTokenUsage(wire.Usage)
	if !got.Reported || got.InputTokens != 23 || got.CachedInputTokens != 5 ||
		!got.CacheWriteInputTokensReported || got.CacheWriteInputTokens != 7 || got.OutputTokens != 3 {
		t.Fatalf("usage = %+v", got)
	}
	zero := 0
	got = chatTokenUsage(&chatCompletionsUsage{CacheCreationInputTokens: &zero})
	if !got.CacheWriteInputTokensReported || got.CacheWriteInputTokens != 0 {
		t.Fatalf("present zero cache write usage = %+v", got)
	}
}

func TestChatTokenUsageMarksOverflowInvalid(t *testing.T) {
	cacheWrite := 1
	got := chatTokenUsage(&chatCompletionsUsage{InputTokens: math.MaxInt, CacheCreationInputTokens: &cacheWrite})
	if got.InputTokens != -1 || !got.CacheWriteInputTokensReported {
		t.Fatalf("overflow usage = %+v", got)
	}
}

func TestChatCompletionsMessageRoundTrip(t *testing.T) {
	messages := []Message{
		{
			Role:    "assistant",
			Content: new("reasoning"),
			ToolCalls: []ToolCall{{
				ID: "call-1", Type: "function",
				Function: FunctionCall{Name: "read_artifact", Arguments: `{"path":"log.txt"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Name: "read_artifact", Content: new(`{"ok":true}`)},
	}

	wire := chatCompletionsResponse{ID: "chat-1", Usage: &chatCompletionsUsage{PromptTokens: 12, CompletionTokens: 4, PromptTokensDetails: chatPromptTokenDetails{CachedTokens: 5}, CompletionTokensDetails: chatOutputTokenDetails{ReasoningTokens: 2}}}
	wire.Choices = append(wire.Choices, chatCompletionsChoice{
		FinishReason: "tool_calls", Message: encodeChatMessages(messages)[0],
	})
	decoded := decodeChatResponse(wire)
	if !decoded.HasMessage || decoded.FinishReason != "tool_calls" || !reflect.DeepEqual(decoded.Message, messages[0]) {
		t.Fatalf("decoded response = %+v", decoded)
	}
	if decoded.ResponseID != "chat-1" || !decoded.Usage.Reported || decoded.Usage.InputTokens != 12 || decoded.Usage.CachedInputTokens != 5 || decoded.Usage.OutputTokens != 4 || decoded.Usage.ReasoningTokens != 2 {
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
