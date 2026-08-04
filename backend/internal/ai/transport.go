package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
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
	RetryAfter string
	RequestID  string
}

func (e *modelHTTPError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.API, e.StatusCode, e.Body)
}

// ProviderErrorMetadata contains only provider fields safe for diagnostic logs.
type ProviderErrorMetadata struct {
	API               string
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
	metadata := ProviderErrorMetadata{}
	var structured *structuredCompletionError
	if errors.As(err, &structured) {
		metadata.StructuredAttempt = structured.attempt
	}
	var httpErr *modelHTTPError
	if errors.As(err, &httpErr) {
		metadata.API = httpErr.API
		metadata.StatusCode = httpErr.StatusCode
		metadata.RetryAfter = safeProviderRetryAfter(httpErr.RetryAfter)
		metadata.RequestID = safeProviderRequestID(httpErr.RequestID)
	}
	if metadata.StructuredAttempt == "" && metadata.StatusCode == 0 {
		return ProviderErrorMetadata{}, false
	}
	return metadata, true
}

func newModelHTTPError(api string, statusCode int, body string, header http.Header) *modelHTTPError {
	return &modelHTTPError{
		API: api, StatusCode: statusCode, Body: body,
		RetryAfter: header.Get("Retry-After"), RequestID: providerRequestID(header),
	}
}

func providerRequestID(header http.Header) string {
	for _, name := range []string{"X-GitHub-Request-Id", "OpenAI-Request-Id", "X-Request-Id", "Request-Id"} {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func safeProviderRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 && seconds <= 86_400 {
		return strconv.Itoa(seconds)
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.UTC().Format(http.TimeFormat)
	}
	return ""
}

func safeProviderRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/", r) {
			continue
		}
		return ""
	}
	return value
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
	usage := aiusage.TokenUsage{}
	if resp != nil {
		usage = resp.Usage
		event.ResponseID = resp.ResponseID
		event.Status = resp.Status
		event.FinishReason = resp.FinishReason
		event.Attempts = resp.Attempts
		event.HTTPStatus = resp.HTTPStatus
		event.UsageReported = resp.Usage.Reported
		event.InputTokens = resp.Usage.InputTokens
		event.CachedInputTokens = resp.Usage.CachedInputTokens
		event.OutputTokens = resp.Usage.OutputTokens
		event.ReasoningTokens = resp.Usage.ReasoningTokens
		event.ToolCallCount = len(resp.Message.ToolCalls)
	}
	aiusage.ObserveModelRequest(ctx, usage)
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
	Usage        aiusage.TokenUsage
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
		httpClient: &http.Client{
			Transport:     transport,
			CheckRedirect: modelRedirectPolicy(endpoint),
		},
		endpoint:     endpoint,
		token:        token,
		extraHeaders: extraHeaders,
	}
}

func modelRedirectPolicy(endpoint string) func(*http.Request, []*http.Request) error {
	configured, err := url.Parse(endpoint)
	return func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("model endpoint stopped after 10 redirects")
		}
		if err != nil || !strings.EqualFold(next.URL.Scheme, configured.Scheme) || !strings.EqualFold(next.URL.Host, configured.Host) {
			return fmt.Errorf("model endpoint redirected to a different origin")
		}
		return nil
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
