package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// modelTransport executes one model turn. The analysis loops operate only on
// these neutral types; each API adapter owns its wire encoding and response
// conversion.
const (
	APIChatCompletions = "chat_completions"
	APIResponses       = "responses"
)

type modelTransport interface {
	Complete(context.Context, modelRequest) (*modelResponse, error)
}

// modelHTTPError preserves provider response details for existing transport
// callers while allowing domain-specific callers to classify the status code.
type modelHTTPError struct {
	API        string
	StatusCode int
	Body       string
}

func (e *modelHTTPError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.API, e.StatusCode, e.Body)
}

func (c *Client) callModel(ctx context.Context, messages []modelMessage, toolDefs []tools.Schema, parallelToolCalls *bool) (*modelResponse, error) {
	return c.callModelRequest(ctx, modelRequest{
		Model:             c.model,
		Messages:          messages,
		Tools:             toolDefs,
		ParallelToolCalls: parallelToolCalls,
	})
}

func (c *Client) callModelRequest(ctx context.Context, request modelRequest) (*modelResponse, error) {
	start := time.Now()
	resp, err := c.transport.Complete(ctx, request)
	event := TraceEvent{Kind: "model_request", DurationMs: int(time.Since(start) / time.Millisecond), MessageCount: len(request.Messages)}
	if resp != nil {
		event.ResponseID = resp.ResponseID
		event.Status = resp.Status
		event.FinishReason = resp.FinishReason
		event.Attempts = resp.Attempts
		event.HTTPStatus = resp.HTTPStatus
		event.InputTokens = resp.InputTokens
		event.OutputTokens = resp.OutputTokens
		event.ToolCallCount = len(resp.Message.ToolCalls)
	}
	if err != nil {
		event.Outcome = "error"
		event.ErrorCode = traceErrorCode(err)
	} else {
		event.Outcome = "success"
	}
	recordTrace(ctx, event)
	return resp, err
}

type unsupportedTransport struct {
	api string
}

func (t unsupportedTransport) Complete(context.Context, modelRequest) (*modelResponse, error) {
	return nil, fmt.Errorf("unsupported AI API %q", t.api)
}

type modelRequest struct {
	Model             string
	Messages          []modelMessage
	Tools             []tools.Schema
	ParallelToolCalls *bool
	ResponseFormat    *ResponseFormat
	ToolChoice        *ToolChoice
	MaxResponseBytes  int64
	OmitReasoning     bool
}

const defaultModelHTTPResponseBytes int64 = 8 << 20

func readModelResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultModelHTTPResponseBytes
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("model response exceeds %d bytes", limit)
	}
	return raw, nil
}

type modelResponse struct {
	Message      modelMessage
	FinishReason string
	HasMessage   bool
	ResponseID   string
	Status       string
	Attempts     int
	HTTPStatus   int
	InputTokens  int
	OutputTokens int
}

// The JSON tags preserve the existing compaction size estimate. API adapters
// still map these neutral messages to their own wire types explicitly.
type modelMessage struct {
	Role          string            `json:"role"`
	Content       *string           `json:"content,omitempty"`
	Name          string            `json:"name,omitempty"`
	ToolCallID    string            `json:"tool_call_id,omitempty"`
	ToolCalls     []modelToolCall   `json:"tool_calls,omitempty"`
	ProviderItems []json.RawMessage `json:"provider_items,omitempty"`
}

type modelToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function modelFunction `json:"function"`
}

type modelFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// httpAPIClient holds the connection pool and headers shared by model API
// transports and the best-effort /models probe.
type httpAPIClient struct {
	httpClient   *http.Client
	endpoint     string
	token        string
	extraHeaders map[string]string
}

func newHTTPAPIClient(endpoint, token string, extraHeaders map[string]string) *httpAPIClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 16
	return &httpAPIClient{
		// Request deadlines come from the caller's context. A fixed client timeout
		// would override per-failure budgets for slow reasoning endpoints.
		httpClient:   &http.Client{Transport: transport},
		endpoint:     endpoint,
		token:        token,
		extraHeaders: extraHeaders,
	}
}

func (c *httpAPIClient) setRequestHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if isCopilotEndpoint(c.endpoint) {
		req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
}

func isCopilotEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), "githubcopilot.com")
}

func continuationCalls(api string, message modelMessage, kept []modelToolCall) ([]modelToolCall, []modelMessage) {
	if api != APIResponses || len(kept) == len(message.ToolCalls) {
		return kept, nil
	}
	skipped := make([]modelMessage, 0, len(message.ToolCalls)-len(kept))
	for _, call := range message.ToolCalls[len(kept):] {
		skipped = append(skipped, modelMessage{Role: "tool", ToolCallID: call.ID, Content: strPtr(`{"error":"skipped by single_tool_call; request again if still needed"}`)})
	}
	return message.ToolCalls, skipped
}

func strPtr(s string) *string { return &s }
