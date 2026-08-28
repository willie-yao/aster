package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

func continuationRegistry(t *testing.T, payload string) (*tools.Registry, []string) {
	t.Helper()
	registry := tools.NewRegistry()
	for _, name := range []string{"read", "grep"} {
		tool := &requiredStubTool{name: name, result: func(json.RawMessage) tools.Result {
			return tools.Result{Payload: map[string]any{"content": payload}, ContentBytes: len(payload), BytesFetched: len(payload)}
		}}
		registry.Register(tool)
	}
	enabled, err := registry.Enable([]string{"required"})
	if err != nil {
		t.Fatal(err)
	}
	return registry, enabled
}

func continuationToolResponse(providerItems ...json.RawMessage) *modelResponse {
	return &modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{
		Role: "assistant", ProviderItems: providerItems,
		ToolCalls: []modelToolCall{
			{ID: "read-1", Type: "function", Function: modelFunction{Name: "read", Arguments: `{"path":"config/jobs.yaml"}`}},
			{ID: "grep-1", Type: "function", Function: modelFunction{Name: "grep", Arguments: `{"path":"config/jobs.yaml"}`}},
		},
	}}
}

func continuationFinalResponse(memo string, providerItems ...json.RawMessage) *modelResponse {
	return &modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{Role: "assistant", Content: strPtr(memo), ProviderItems: providerItems}}
}

func TestToolLoopContinuationRetainsToolResultsAndProviderItems(t *testing.T) {
	const privateSource = "job=soak-tests-capz-windows-2019 container=test KUBERNETES_VERSION=v1.23.5"
	toolItem := json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque-tool-turn"}`)
	memoItem := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary only"}]}`)
	client, transport := newRecordedToolLoopClient(APIResponses,
		continuationToolResponse(toolItem),
		continuationFinalResponse("summary only", memoItem),
		structuredContent(`{"body":"safe"}`),
	)
	registry, enabled := continuationRegistry(t, privateSource)
	memo, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "investigate", registry, enabled, &tools.Env{}, ToolLoopOptions{MaxIters: 4})
	if err != nil {
		t.Fatal(err)
	}
	if memo != "summary only" || strings.Contains(memo, "KUBERNETES_VERSION") {
		t.Fatalf("memo=%q", memo)
	}
	metadata, err := client.ContinueStructuredWithMetadata(t.Context(), continuation, "submit the exact target", structuredBodyFormat(), bodyValidator("safe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attempts) != 1 || metadata.Attempts[0].Outcome != StructuredOutcomeAccepted {
		t.Fatalf("metadata=%+v", metadata)
	}
	request := transport.requests[2]
	encoded, _ := json.Marshal(request.Messages)
	text := string(encoded)
	for _, want := range []string{privateSource, "opaque-tool-turn", "summary only", "submit the exact target"} {
		if !strings.Contains(text, want) {
			t.Fatalf("continued request missing %q: %s", want, text)
		}
	}
	input, _ := json.Marshal(encodeResponsesInput(request.Messages))
	if !strings.Contains(string(input), "opaque-tool-turn") || !strings.Contains(string(input), privateSource) {
		t.Fatalf("responses continuation lost provider or tool items: %s", input)
	}
}

func TestContinueStructuredSupportsChatAndResponses(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			client, transport := newRecordedToolLoopClient(apiMode,
				continuationFinalResponse("memo"),
				structuredContent(`{"body":"safe"}`),
			)
			registry, enabled := continuationRegistry(t, "source")
			_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.ContinueStructured(t.Context(), continuation, "final instruction", structuredBodyFormat(), bodyValidator("safe")); err != nil {
				t.Fatal(err)
			}
			request := transport.requests[1]
			if request.ResponseFormat == nil || request.ToolChoice != nil || len(request.Messages) != 4 || request.Messages[3].Content == nil || *request.Messages[3].Content != "final instruction" {
				t.Fatalf("request=%+v", request)
			}
		})
	}
}

func TestContinueStructuredAttemptFallbacks(t *testing.T) {
	t.Run("forced function", func(t *testing.T) {
		client, transport := newRecordedToolLoopClient(APIResponses,
			continuationFinalResponse("memo"),
			structuredContent(`{"body":"wrong"}`),
			structuredFunction("return_body", `{"body":"safe"}`),
		)
		registry, enabled := continuationRegistry(t, "source")
		_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := client.ContinueStructuredWithMetadata(t.Context(), continuation, "final", structuredBodyFormat(), codedBodyValidator("safe", "candidate_kind"))
		if err != nil {
			t.Fatal(err)
		}
		if len(metadata.Attempts) != 2 || metadata.Attempts[0].Outcome != StructuredOutcomeValidatorRejected || metadata.Attempts[1].Path != StructuredAttemptForcedFunction || metadata.Attempts[1].Outcome != StructuredOutcomeAccepted {
			t.Fatalf("metadata=%+v", metadata)
		}
		forced := transport.requests[2]
		if forced.ToolChoice == nil || len(forced.Tools) != 1 || forced.Tools[0].Function.Name != "return_body" {
			t.Fatalf("forced request=%+v", forced)
		}
	})

	t.Run("plain fallback", func(t *testing.T) {
		unsupported := newModelHTTPError("responses", http.StatusBadRequest, "unsupported", http.Header{})
		transport := &toolLoopTransport{results: []toolLoopTransportResult{
			{response: continuationFinalResponse("memo")},
			{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: unsupported},
			{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: unsupported},
			{response: structuredContent("text\n{\"body\":\"safe\"}\n")},
		}}
		client := &Client{transport: transport, model: "m", apiMode: APIResponses}
		registry, enabled := continuationRegistry(t, "source")
		_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := client.ContinueStructuredWithMetadata(t.Context(), continuation, "final", structuredBodyFormat(), bodyValidator("safe"))
		if err != nil {
			t.Fatal(err)
		}
		final, _ := metadata.FinalAttempt()
		if len(metadata.Attempts) != 3 || final.Path != StructuredAttemptPlainFallback || final.Outcome != StructuredOutcomeAccepted {
			t.Fatalf("metadata=%+v", metadata)
		}
	})
}

func TestContinueStructuredCompactsContext(t *testing.T) {
	large := strings.Repeat("private-source-content-", 400)
	client, transport := newRecordedToolLoopClient(APIChatCompletions,
		continuationToolResponse(),
		continuationFinalResponse("memo"),
		structuredContent(`{"body":"safe"}`),
	)
	registry, enabled := continuationRegistry(t, large)
	_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{MaxIters: 4, ContextByteBudget: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ContinueStructured(t.Context(), continuation, "final instruction", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	request := transport.requests[2]
	encoded, _ := json.Marshal(request.Messages)
	if strings.Contains(string(encoded), large) || !strings.Contains(string(encoded), "final instruction") {
		t.Fatalf("compacted request=%s", encoded)
	}
}

func TestToolLoopContinuationRejectsSerializationReuseAndStaleness(t *testing.T) {
	newContinuation := func(t *testing.T) (*Client, ToolLoopContinuation) {
		client, _ := newRecordedToolLoopClient(APIResponses, continuationFinalResponse("memo"), structuredContent(`{"body":"safe"}`))
		registry, enabled := continuationRegistry(t, "source")
		_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return client, continuation
	}

	client, continuation := newContinuation(t)
	if _, err := json.Marshal(struct {
		Continuation ToolLoopContinuation `json:"continuation"`
	}{Continuation: continuation}); !errors.Is(err, ErrToolLoopContinuationPrivate) {
		t.Fatalf("serialization err=%v", err)
	}
	if err := client.ContinueStructured(t.Context(), continuation, "final", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	if err := client.ContinueStructured(t.Context(), continuation, "final", structuredBodyFormat(), bodyValidator("safe")); !errors.Is(err, ErrToolLoopContinuationUsed) {
		t.Fatalf("reuse err=%v", err)
	}

	client, continuation = newContinuation(t)
	continuation.state.expiresAt = time.Now().Add(-time.Second)
	if err := client.ContinueStructured(t.Context(), continuation, "final", structuredBodyFormat(), bodyValidator("safe")); !errors.Is(err, ErrToolLoopContinuationStale) {
		t.Fatalf("stale err=%v", err)
	}
}

type blockingContinuationTransport struct{ calls int }

func (transport *blockingContinuationTransport) Complete(ctx context.Context, _ modelRequest) (*modelResponse, error) {
	transport.calls++
	if transport.calls == 1 {
		return continuationFinalResponse("memo"), nil
	}
	<-ctx.Done()
	return &modelResponse{Attempts: 1}, ctx.Err()
}

func TestContinueStructuredCancellationAndTimeout(t *testing.T) {
	registry, enabled := continuationRegistry(t, "source")
	client, _ := newRecordedToolLoopClient(APIResponses, continuationFinalResponse("memo"))
	_, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.ContinueStructured(cancelled, continuation, "final", structuredBodyFormat(), bodyValidator("safe")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	continuation.Discard()

	blocking := &blockingContinuationTransport{}
	client = &Client{transport: blocking, model: "m", apiMode: APIResponses}
	_, continuation, err = client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Millisecond)
	defer stop()
	metadata, err := client.ContinueStructuredWithMetadata(ctx, continuation, "final", structuredBodyFormat(), bodyValidator("safe"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err=%v", err)
	}
	final, ok := metadata.FinalAttempt()
	if !ok || final.Path != StructuredAttemptResponseFormat || final.Outcome != StructuredOutcomeProviderError {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestContinueStructuredRejectsOverBudgetWithoutProviderRequest(t *testing.T) {
	client, transport := newRecordedToolLoopClient(APIChatCompletions, continuationFinalResponse("memo"))
	registry, enabled := continuationRegistry(t, "source")
	_, continuation, err := client.ToolLoopWithContinuation(t.Context(), strings.Repeat("system", 100), "user", registry, enabled, &tools.Env{}, ToolLoopOptions{ContextByteBudget: 256})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ContinueStructured(t.Context(), continuation, strings.Repeat("final", 100), structuredBodyFormat(), bodyValidator("safe")); !errors.Is(err, ErrContextHeadroom) {
		t.Fatalf("err=%v", err)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("requests=%d", len(transport.requests))
	}
}

func TestResponsesContinuationSanitizesForcedFinalizationCall(t *testing.T) {
	ordinaryReasoning := json.RawMessage(`{"type":"reasoning","encrypted_content":"ordinary-reasoning"}`)
	ordinaryCall := json.RawMessage(`{"type":"function_call","call_id":"read-1","name":"read","arguments":"{\"path\":\"config/jobs.yaml\"}"}`)
	ordinaryGrepCall := json.RawMessage(`{"type":"function_call","call_id":"grep-1","name":"grep","arguments":"{\"path\":\"config/jobs.yaml\"}"}`)
	finalReasoning := json.RawMessage(`{"type":"reasoning","encrypted_content":"final-reasoning"}`)
	finalCall := json.RawMessage(`{"type":"function_call","call_id":"submit-1","name":"submit_analysis","arguments":` + string(mustJSON(cleanFinalJSON)) + `}`)
	client, transport := newRecordedToolLoopClient(APIResponses,
		continuationToolResponse(ordinaryReasoning, ordinaryCall, ordinaryGrepCall),
		&modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{
			Role: "assistant", ProviderItems: []json.RawMessage{finalReasoning, finalCall},
			ToolCalls: []modelToolCall{{ID: "submit-1", Type: "function", Function: modelFunction{Name: "submit_analysis", Arguments: cleanFinalJSON}}},
		}},
		structuredContent(`{"body":"safe"}`),
	)
	registry, enabled := continuationRegistry(t, "source")
	memo, continuation, err := client.ToolLoopWithContinuation(t.Context(), "system", "user", registry, enabled, &tools.Env{}, ToolLoopOptions{MaxIters: 1})
	if err != nil {
		t.Fatal(err)
	}
	if memo != cleanFinalJSON {
		t.Fatalf("memo=%q", memo)
	}
	if err := client.ContinueStructured(t.Context(), continuation, "continue", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	request := transport.requests[2]
	input, _ := json.Marshal(encodeResponsesInput(request.Messages))
	text := string(input)
	for _, want := range []string{"ordinary-reasoning", `"call_id":"read-1"`, `"call_id":"grep-1"`, "source", "third CP machine cloud-init empty", "continue"} {
		if !strings.Contains(text, want) {
			t.Fatalf("continued request missing %q: %s", want, text)
		}
	}
	for _, unwanted := range []string{"final-reasoning", `"call_id":"submit-1"`} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("continued request retained %q: %s", unwanted, text)
		}
	}
}
