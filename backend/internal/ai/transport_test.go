package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/transport"
	"github.com/willie-yao/aster/backend/internal/aiusage"
)

type recordingTransport struct {
	request transport.Request
	result  *transport.Response
	err     error
	calls   int
}

func TestClientThrottleRemainsInsideTimedCall(t *testing.T) {
	old := callDelay
	callDelay = 20 * time.Millisecond
	t.Cleanup(func() { callDelay = old })
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				writeReasoningTestFinal(w, apiMode, "ok")
			}))
			defer server.Close()
			client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m"})
			for _, cancelled := range []bool{false, true} {
				store := NewTraceStore()
				trace := store.Start(TraceMetadata{JobID: "job", APIMode: apiMode})
				ctx, cancel := context.WithCancel(withAnalysisTrace(t.Context(), trace))
				if cancelled {
					cancel()
				}
				_, err := client.callModel(ctx, nil, nil, nil)
				cancel()
				if cancelled != errors.Is(err, context.Canceled) || !cancelled && err != nil {
					t.Fatalf("cancelled=%v error=%v", cancelled, err)
				}
				trace.Finish("complete", err)
				event := store.Snapshot().Traces[0].Events[0]
				if event.DurationMs < int(callDelay/time.Millisecond) {
					t.Fatalf("throttle omitted from timed call: %+v", event)
				}
			}
			if calls != 1 {
				t.Fatalf("provider calls = %d, want 1", calls)
			}
		})
	}
}

func (t *recordingTransport) Complete(_ context.Context, req transport.Request) (*transport.Response, error) {
	t.calls++
	t.request = req
	return t.result, t.err
}

func TestClientCompleteUsesModelTransport(t *testing.T) {
	provider := &recordingTransport{result: &transport.Response{
		HasMessage: true, Message: transport.Message{Role: "assistant", Content: strPtr("done")},
	}}
	client := &Client{model: "model-a", transport: provider}

	got, err := client.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "done" {
		t.Fatalf("Complete() = %q, want done", got)
	}
	wantMessages := []transport.Message{
		{Role: "system", Content: strPtr("system")},
		{Role: "user", Content: strPtr("user")},
	}
	if provider.request.Model != "model-a" || !reflect.DeepEqual(provider.request.Messages, wantMessages) {
		t.Fatalf("transport request = %+v", provider.request)
	}
}

func TestClientCallModelRecordsTrace(t *testing.T) {
	provider := &recordingTransport{result: &transport.Response{
		HasMessage: true, Message: transport.Message{Role: "assistant", ToolCalls: []transport.ToolCall{{ID: "call"}}},
		ResponseID: "resp-1", Status: "completed", FinishReason: "tool_calls",
		Attempts: 2, HTTPStatus: 200, WireRequestBytes: 321, Usage: aiusage.TokenUsage{
			Reported: true, InputTokens: 11, CachedInputTokens: 3,
			CacheWriteInputTokens: 2, CacheWriteInputTokensReported: true,
			OutputTokens: 7, ReasoningTokens: 2,
		},
	}}
	client := &Client{model: "model-a", reasoningEffort: ReasoningEffortHigh, transport: provider}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIResponses})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, err := client.callModel(ctx, []transport.Message{{Role: "user", Content: strPtr("user")}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	event := store.Snapshot().Traces[0].Events[0]
	wantBytes := requestSizeEstimate([]transport.Message{{Role: "user", Content: strPtr("user")}}, 0)
	if event.Kind != "model_request" || event.ResponseID != "resp-1" || event.Attempts != 2 || !event.UsageReported || event.InputTokens != 11 || event.CachedInputTokens != 3 || !event.CacheWriteInputTokensReported || event.CacheWriteInputTokens != 2 || event.OutputTokens != 7 || event.ReasoningTokens != 2 || event.ReasoningEffort != "high" || event.ServiceTier != "" || event.ToolCallCount != 1 || event.Bytes != wantBytes || event.WireRequestBytes != 321 {
		t.Fatalf("event = %+v", event)
	}
}

func TestClientCallModelRecordsRequestBytesOnProviderError(t *testing.T) {
	provider := &recordingTransport{err: errors.New("provider failed")}
	client := &Client{model: "model-a", transport: provider}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test"})
	messages := []transport.Message{{Role: "user", Content: strPtr("user")}}
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, err := client.callModel(ctx, messages, nil, nil); err == nil {
		t.Fatal("expected provider error")
	}
	trace.Finish("error", errors.New("provider failed"))
	event := store.Snapshot().Traces[0].Events[0]
	if event.Outcome != "error" || event.UsageReported || event.Bytes != requestSizeEstimate(messages, 0) {
		t.Fatalf("event = %+v", event)
	}
}

func TestClientCallModelRecordsUsageOperation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingTransport{result: &transport.Response{Usage: aiusage.TokenUsage{Reported: true, InputTokens: 9, OutputTokens: 4}}}
	client := &Client{model: "model-a", reasoningEffort: ReasoningEffortXHigh, transport: provider}
	ctx, operation := aiusage.Begin(t.Context(), recorder, aiusage.Metadata{LogicalID: "request", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureFailureAnalysis, StartedAt: now})
	if _, err := client.callModel(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	operation.Finish(aiusage.OutcomeSuccess)
	snapshot := recorder.Snapshot()
	got := snapshot.Days[0].Totals
	if len(snapshot.RecentOperations) != 1 || snapshot.RecentOperations[0].ReasoningEffort != "xhigh" {
		t.Fatalf("recent operations = %+v", snapshot.RecentOperations)
	}
	if got.ModelRequests != 1 || got.ReportedRequests != 1 || got.CacheWriteUnreportedRequests != 1 || got.InputTokens != 9 || got.OutputTokens != 4 {
		t.Fatalf("usage totals = %+v", got)
	}
}

func TestClientCallModelRecordsUnreportedUsage(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingTransport{err: errors.New("provider failed")}
	client := &Client{model: "model-a", transport: provider}
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

func TestContinuationCallsPairsSkippedResponsesCalls(t *testing.T) {
	msg := transport.Message{ToolCalls: []transport.ToolCall{{ID: "a"}, {ID: "b"}}}
	echo, skipped := continuationCalls(APIResponses, msg, msg.ToolCalls[:1])
	if len(echo) != 2 || len(skipped) != 1 || skipped[0].ToolCallID != "b" {
		t.Fatalf("echo=%+v skipped=%+v", echo, skipped)
	}
	chatEcho, chatSkipped := continuationCalls(APIChatCompletions, msg, msg.ToolCalls[:1])
	if len(chatEcho) != 1 || len(chatSkipped) != 0 {
		t.Fatalf("chat echo=%+v skipped=%+v", chatEcho, chatSkipped)
	}
}
