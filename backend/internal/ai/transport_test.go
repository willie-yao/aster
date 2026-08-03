package ai

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
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
		Attempts: 2, HTTPStatus: 200, Usage: aiusage.TokenUsage{Reported: true, InputTokens: 11, CachedInputTokens: 3, OutputTokens: 7, ReasoningTokens: 2},
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
	if event.Kind != "model_request" || event.ResponseID != "resp-1" || event.Attempts != 2 || !event.UsageReported || event.InputTokens != 11 || event.CachedInputTokens != 3 || event.OutputTokens != 7 || event.ReasoningTokens != 2 || event.ToolCallCount != 1 {
		t.Fatalf("event = %+v", event)
	}
}

func TestClientCallModelRecordsUsageOperation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{result: &modelResponse{Usage: aiusage.TokenUsage{Reported: true, InputTokens: 9, OutputTokens: 4}}}
	client := &Client{model: "model-a", transport: transport}
	ctx, operation := aiusage.Begin(t.Context(), recorder, aiusage.Metadata{LogicalID: "request", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureFailureAnalysis, StartedAt: now})
	if _, err := client.callModel(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	operation.Finish(aiusage.OutcomeSuccess)
	got := recorder.Snapshot().Days[0].Totals
	if got.ModelRequests != 1 || got.ReportedRequests != 1 || got.InputTokens != 9 || got.OutputTokens != 4 {
		t.Fatalf("usage totals = %+v", got)
	}
}

func TestClientCallModelRecordsUnreportedUsage(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{err: errors.New("provider failed")}
	client := &Client{model: "model-a", transport: transport}
	ctx, operation := aiusage.Begin(t.Context(), recorder, aiusage.Metadata{LogicalID: "request", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureFailureAnalysis, StartedAt: now})
	if _, err := client.callModel(ctx, nil, nil, nil); err == nil {
		t.Fatal("expected provider error")
	}
	operation.Finish(aiusage.OutcomeError)
	got := recorder.Snapshot().Days[0].Totals
	if got.ModelRequests != 1 || got.ReportedRequests != 0 || got.UnreportedRequests != 1 || got.Failures != 1 {
		t.Fatalf("usage totals = %+v", got)
	}
}

func TestChatTokenUsageDistinguishesAbsentAndZero(t *testing.T) {
	if got := chatTokenUsage(nil); got.Reported {
		t.Fatalf("absent usage = %+v", got)
	}
	if got := chatTokenUsage(&chatCompletionsUsage{}); !got.Reported || got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("present zero usage = %+v", got)
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
