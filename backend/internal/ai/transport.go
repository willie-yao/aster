package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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

func (c *Client) callModel(ctx context.Context, messages []modelMessage, toolDefs []tools.Schema, parallelToolCalls *bool) (*modelResponse, error) {
	return c.transport.Complete(ctx, modelRequest{
		Model:             c.model,
		Messages:          messages,
		Tools:             toolDefs,
		ParallelToolCalls: parallelToolCalls,
	})
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
}

type modelResponse struct {
	Message      modelMessage
	FinishReason string
	HasMessage   bool
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
