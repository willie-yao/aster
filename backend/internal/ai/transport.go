package ai

import (
	"context"
	"errors"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/transport"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

const (
	APIChatCompletions = modelprovider.APIChatCompletions
	APIResponses       = modelprovider.APIResponses
)

// modelTransport executes one model turn without analysis policy.
type modelTransport interface {
	Complete(context.Context, transport.Request) (*transport.Response, error)
}

type throttledTransport struct {
	client *transport.Client
}

func (t throttledTransport) Complete(ctx context.Context, request transport.Request) (*transport.Response, error) {
	time.Sleep(callDelay)
	return t.client.Complete(ctx, request)
}

func recordServiceTierFallback(ctx context.Context, tier string) {
	recordTrace(ctx, TraceEvent{Kind: "service_tier", Outcome: "fallback", Status: tier})
}

// ProviderErrorMetadata contains only provider fields safe for diagnostic logs.
type ProviderErrorMetadata struct {
	API               string
	Category          string
	StatusCode        int
	RetryAfter        string
	RequestID         string
	StructuredAttempt string
}

// SafeProviderErrorMetadata extracts provider metadata without exposing the
// response body, request payload, endpoint, model, or credentials.
func SafeProviderErrorMetadata(err error) (ProviderErrorMetadata, bool) {
	if err == nil {
		return ProviderErrorMetadata{}, false
	}
	var structured *structuredCompletionError
	if errors.As(err, &structured) {
		metadata := structured.provider
		if final, ok := structured.metadata.FinalAttempt(); ok {
			metadata.StructuredAttempt = string(final.Path)
			if final.ProviderCategory != "" {
				metadata.Category = final.ProviderCategory
			}
			if final.ProviderStatus != 0 {
				metadata.StatusCode = final.ProviderStatus
			}
		}
		if metadata.StructuredAttempt == "" && metadata.StatusCode == 0 && metadata.Category == "" {
			return ProviderErrorMetadata{}, false
		}
		return metadata, true
	}
	return safeProviderErrorMetadataFromCause(err)
}

func safeProviderErrorMetadataFromCause(err error) (ProviderErrorMetadata, bool) {
	if err == nil {
		return ProviderErrorMetadata{}, false
	}
	metadata := ProviderErrorMetadata{Category: traceErrorCode(err)}
	var httpErr *transport.HTTPError
	if errors.As(err, &httpErr) {
		metadata.API = httpErr.API
		metadata.StatusCode = httpErr.StatusCode
		metadata.RetryAfter = httpErr.RetryAfter()
		metadata.RequestID = httpErr.RequestID()
	}
	if metadata.StatusCode == 0 && metadata.Category == "analysis_error" {
		return ProviderErrorMetadata{}, false
	}
	return metadata, true
}

func (c *Client) callModel(ctx context.Context, messages []transport.Message, toolDefs []transport.ToolSchema, parallelToolCalls *bool) (*transport.Response, error) {
	return c.callModelRequest(ctx, transport.Request{
		Model:             c.model,
		Messages:          messages,
		Tools:             toolDefs,
		ParallelToolCalls: parallelToolCalls,
	})
}

func (c *Client) callModelRequest(ctx context.Context, request transport.Request) (*transport.Response, error) {
	if c.reasoningEffortErr != nil {
		return nil, c.reasoningEffortErr
	}
	if c.serviceTierErr != nil {
		return nil, c.serviceTierErr
	}
	if c.maxOutputTokensErr != nil {
		return nil, c.maxOutputTokensErr
	}
	request.ReasoningEffort = c.reasoningEffort
	if len(request.Tools) > 0 {
		if err := ValidateToolCallingConfiguration(c.apiMode, c.model, request.ReasoningEffort); err != nil {
			return nil, err
		}
	}
	if request.MaxOutputTokens == 0 {
		request.MaxOutputTokens = c.maxOutputTokens
	}
	start := time.Now()
	resp, err := c.transport.Complete(ctx, request)
	event := TraceEvent{
		Kind: "model_request", DurationMs: int(time.Since(start) / time.Millisecond),
		MessageCount: len(request.Messages), ReasoningEffort: string(request.ReasoningEffort),
		Bytes: requestSizeEstimate(request.Messages, schemaPayloadBytes(request.Tools)),
	}
	usage := aiusage.TokenUsage{}
	if resp != nil {
		usage = resp.Usage
		event.ResponseID = resp.ResponseID
		event.Status = resp.Status
		event.FinishReason = resp.FinishReason
		event.ServiceTier = resp.ServiceTier
		event.Attempts = resp.Attempts
		event.HTTPStatus = resp.HTTPStatus
		event.UsageReported = resp.Usage.Reported
		event.InputTokens = resp.Usage.InputTokens
		event.CachedInputTokens = resp.Usage.CachedInputTokens
		event.CacheWriteInputTokens = resp.Usage.CacheWriteInputTokens
		event.CacheWriteInputTokensReported = resp.Usage.CacheWriteInputTokensReported
		event.OutputTokens = resp.Usage.OutputTokens
		event.ReasoningTokens = resp.Usage.ReasoningTokens
		event.ToolCallCount = len(resp.Message.ToolCalls)
		event.WireRequestBytes = resp.WireRequestBytes
	}
	aiusage.ObserveModelRequestWithModelAndReasoningEffort(ctx, usage, c.model, c.modelFingerprint(), string(request.ReasoningEffort))
	if err != nil {
		event.Outcome = "error"
		event.ErrorCode = traceErrorCode(err)
	} else {
		event.Outcome = "success"
	}
	recordTrace(ctx, event)
	return resp, err
}

func continuationCalls(api string, message transport.Message, kept []transport.ToolCall) ([]transport.ToolCall, []transport.Message) {
	if api != APIResponses || len(kept) == len(message.ToolCalls) {
		return kept, nil
	}
	skipped := make([]transport.Message, 0, len(message.ToolCalls)-len(kept))
	for _, call := range message.ToolCalls[len(kept):] {
		skipped = append(skipped, transport.Message{Role: "tool", ToolCallID: call.ID, Content: strPtr(`{"error":"skipped by single_tool_call; request again if still needed"}`)})
	}
	return message.ToolCalls, skipped
}

func strPtr(s string) *string { return &s }
