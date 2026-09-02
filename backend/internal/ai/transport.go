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

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

// modelTransport executes one model turn. The analysis loops operate only on
// these neutral types; each API adapter owns its wire encoding and response
// conversion.
const (
	APIChatCompletions = modelprovider.APIChatCompletions
	APIResponses       = modelprovider.APIResponses
)

type modelTransport interface {
	Complete(context.Context, modelRequest) (*modelResponse, error)
}

// modelHTTPError retains only bounded provider metadata safe to surface.
type modelHTTPError struct {
	API        string
	StatusCode int
	Category   string
	RetryAfter string
	RequestID  string
}

func (e *modelHTTPError) Error() string {
	message := fmt.Sprintf("%s returned %d", e.API, e.StatusCode)
	if e.Category == "tools_unsupported" {
		message += ": function calling unsupported"
	}
	if e.RequestID != "" {
		message += " request_id=" + e.RequestID
	}
	if e.RetryAfter != "" {
		message += " retry_after=" + e.RetryAfter
	}
	return message
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
	var httpErr *modelHTTPError
	if errors.As(err, &httpErr) {
		metadata.API = httpErr.API
		metadata.StatusCode = httpErr.StatusCode
		metadata.RetryAfter = safeProviderRetryAfter(httpErr.RetryAfter)
		metadata.RequestID = safeProviderRequestID(httpErr.RequestID)
	}
	if metadata.StatusCode == 0 && metadata.Category == "analysis_error" {
		return ProviderErrorMetadata{}, false
	}
	return metadata, true
}

func newModelHTTPError(api string, statusCode int, body string, header http.Header) *modelHTTPError {
	category := "http_error"
	if (statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity) && toolsUnsupportedRe.MatchString(body) {
		category = "tools_unsupported"
	}
	return &modelHTTPError{
		API: api, StatusCode: statusCode, Category: category,
		RetryAfter: safeProviderRetryAfter(header.Get("Retry-After")), RequestID: safeProviderRequestID(providerRequestID(header)),
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
	MaxOutputTokens   int
	OmitReasoning     bool
	ReasoningEffort   ReasoningEffort
	PromptCacheKey    string
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
	Message          modelMessage
	FinishReason     string
	HasMessage       bool
	ResponseID       string
	Status           string
	ServiceTier      string
	Attempts         int
	HTTPStatus       int
	Usage            aiusage.TokenUsage
	WireRequestBytes int
}

// The JSON tags preserve the existing compaction size estimate. API adapters
// still map these neutral messages to their own wire types explicitly.
type modelMessage struct {
	Role          string            `json:"role"`
	Content       *string           `json:"content,omitempty"`
	Name          string            `json:"name,omitempty"`
	Phase         string            `json:"phase,omitempty"`
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
	serviceTier  string
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
	for name, value := range modelprovider.EndpointHeaders(c.endpoint) {
		req.Header.Set(name, value)
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
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
