package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const responsesWebSocketEventEnvelopeBytes int64 = 64 << 10

type responsesWebSocketTransport struct {
	api      *httpAPIClient
	fallback *responsesTransport
}

func newResponsesWebSocketTransport(api *httpAPIClient, fallback *responsesTransport) *responsesWebSocketTransport {
	return &responsesWebSocketTransport{api: api, fallback: fallback}
}

func (t *responsesWebSocketTransport) NewConversation(ctx context.Context) (modelConversation, error) {
	endpoint, err := responsesWebSocketURL(t.api.endpoint)
	if err != nil {
		return nil, err
	}
	headerRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, t.api.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build websocket headers: %w", err)
	}
	t.api.setRequestHeaders(headerRequest)
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: t.api.httpClient,
		HTTPHeader: headerRequest.Header,
	})
	if err != nil {
		if resp != nil {
			body := ""
			if resp.Body != nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 501))
				body = textutil.Truncate(string(raw), 500)
			}
			return nil, newModelHTTPError("responses websocket", resp.StatusCode, body, resp.Header)
		}
		return nil, fmt.Errorf("dial responses websocket: %w", err)
	}
	return &responsesWebSocketConversation{conn: conn, fallback: t.fallback}, nil
}

func responsesWebSocketURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse responses websocket endpoint: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("responses websocket endpoint scheme %q is unsupported", u.Scheme)
	}
	return u.String(), nil
}

type responsesWebSocketConversation struct {
	conn               *websocket.Conn
	fallback           *responsesTransport
	previousResponseID string
	expectedPrefix     []json.RawMessage
	httpOnly           bool
}

type responsesWebSocketEvent struct {
	Type     string                   `json:"type"`
	Status   int                      `json:"status,omitempty"`
	Response json.RawMessage          `json:"response,omitempty"`
	Error    *responsesWebSocketError `json:"error,omitempty"`
}

type responsesWebSocketError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

func (c *responsesWebSocketConversation) Complete(ctx context.Context, req modelRequest) (*modelResponse, error) {
	if c.httpOnly || c.conn == nil {
		return c.fallback.Complete(ctx, req)
	}
	time.Sleep(callDelay)

	request := responsesRequestFor(req)
	fullInput := request.Input
	canonicalInput, err := canonicalResponsesItems(fullInput)
	if err != nil {
		return nil, fmt.Errorf("encode responses websocket input: %w", err)
	}

	previousResponseID := ""
	input := fullInput
	if c.previousResponseID != "" && responsesItemsHavePrefix(canonicalInput, c.expectedPrefix) {
		previousResponseID = c.previousResponseID
		input = fullInput[len(c.expectedPrefix):]
		recordTrace(ctx, TraceEvent{Kind: "responses_websocket", Outcome: "continued"})
	} else if c.previousResponseID != "" {
		c.clearChain()
		recordTrace(ctx, TraceEvent{Kind: "responses_websocket", Outcome: "chain_reset"})
	} else {
		recordTrace(ctx, TraceEvent{Kind: "responses_websocket", Outcome: "chain_start"})
	}

	attempts := 0
	wireRequestBytes := 0
	for {
		request.Type = "response.create"
		request.PreviousResponseID = previousResponseID
		request.Input = input
		payload, err := json.Marshal(request)
		if err != nil {
			return &modelResponse{Attempts: attempts, WireRequestBytes: wireRequestBytes}, fmt.Errorf("marshal responses websocket request: %w", err)
		}
		attempts++
		wireRequestBytes += len(payload)
		if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return c.handleIOFailure(ctx, req, attempts, wireRequestBytes, err)
		}

		event, err := c.readTerminalEvent(ctx, req.MaxResponseBytes)
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) || errors.Is(err, errResponsesWebSocketResponseTooBig) || errors.Is(err, errResponsesWebSocketProtocol) {
				c.disableWebSocket()
				return &modelResponse{Attempts: attempts, WireRequestBytes: wireRequestBytes}, err
			}
			return c.handleIOFailure(ctx, req, attempts, wireRequestBytes, err)
		}

		if event.Type == "error" {
			code := ""
			if event.Error != nil {
				code = event.Error.Code
			}
			if code == "previous_response_not_found" && previousResponseID != "" {
				previousResponseID = ""
				input = fullInput
				c.clearChain()
				recordTrace(ctx, TraceEvent{Kind: "responses_websocket", Outcome: "continuation_recovered"})
				continue
			}
			if event.Status == http.StatusTooManyRequests || code == "websocket_connection_limit_reached" {
				return c.completeHTTPFallback(ctx, req, attempts, wireRequestBytes)
			}
			return websocketEventFailure(event, attempts, wireRequestBytes)
		}

		wire, err := decodeResponsesWebSocketResponse(event, req.MaxResponseBytes)
		if err != nil {
			c.disableWebSocket()
			return &modelResponse{Attempts: attempts, WireRequestBytes: wireRequestBytes}, err
		}
		out := decodeResponsesResponse(wire)
		out.Attempts = attempts
		out.WireRequestBytes = wireRequestBytes
		if event.Type != "response.completed" || wire.Status != "completed" {
			return out, fmt.Errorf("responses websocket status %q", wire.Status)
		}
		c.rememberChain(canonicalInput, wire)
		return out, nil
	}
}

var (
	errResponsesWebSocketProtocol       = errors.New("responses websocket protocol error")
	errResponsesWebSocketResponseTooBig = errors.New("responses websocket response exceeds byte limit")
)

func (c *responsesWebSocketConversation) readTerminalEvent(ctx context.Context, responseLimit int64) (responsesWebSocketEvent, error) {
	limit := responseLimit
	if limit <= 0 {
		limit = defaultModelHTTPResponseBytes
	}
	eventLimit := limit + responsesWebSocketEventEnvelopeBytes
	if limit > math.MaxInt64-responsesWebSocketEventEnvelopeBytes {
		eventLimit = math.MaxInt64
	}
	c.conn.SetReadLimit(eventLimit)
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return responsesWebSocketEvent{}, err
		}
		var event responsesWebSocketEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return responsesWebSocketEvent{}, fmt.Errorf("%w: decode event: %v", errResponsesWebSocketProtocol, err)
		}
		switch event.Type {
		case "response.completed", "response.failed", "response.incomplete", "error":
			if int64(len(event.Response)) > limit {
				return responsesWebSocketEvent{}, fmt.Errorf("%w: %d bytes exceeds %d", errResponsesWebSocketResponseTooBig, len(event.Response), limit)
			}
			return event, nil
		}
	}
}

func decodeResponsesWebSocketResponse(event responsesWebSocketEvent, responseLimit int64) (responsesResponse, error) {
	if len(event.Response) == 0 {
		return responsesResponse{}, fmt.Errorf("responses websocket event %q has no response", event.Type)
	}
	limit := responseLimit
	if limit <= 0 {
		limit = defaultModelHTTPResponseBytes
	}
	if int64(len(event.Response)) > limit {
		return responsesResponse{}, fmt.Errorf("%w: %d bytes exceeds %d", errResponsesWebSocketResponseTooBig, len(event.Response), limit)
	}
	var wire responsesResponse
	if err := json.Unmarshal(event.Response, &wire); err != nil {
		return responsesResponse{}, fmt.Errorf("decode responses websocket response: %w", err)
	}
	return wire, nil
}

func websocketEventFailure(event responsesWebSocketEvent, attempts, wireRequestBytes int) (*modelResponse, error) {
	code := event.Type
	body := code
	if event.Error != nil {
		if event.Error.Code != "" {
			code = event.Error.Code
		}
		body = strings.TrimSpace(code + ": " + event.Error.Message)
	}
	body = textutil.Truncate(body, 500)
	response := &modelResponse{Attempts: attempts, HTTPStatus: event.Status, WireRequestBytes: wireRequestBytes}
	return response, newModelHTTPError("responses websocket", event.Status, body, http.Header{})
}

func (c *responsesWebSocketConversation) handleIOFailure(ctx context.Context, req modelRequest, attempts, wireRequestBytes int, err error) (*modelResponse, error) {
	if ctx.Err() != nil {
		c.disableWebSocket()
		return &modelResponse{Attempts: attempts, WireRequestBytes: wireRequestBytes}, ctx.Err()
	}
	return c.completeHTTPFallback(ctx, req, attempts, wireRequestBytes)
}

func (c *responsesWebSocketConversation) completeHTTPFallback(ctx context.Context, req modelRequest, attempts, wireRequestBytes int) (*modelResponse, error) {
	if ctx.Err() != nil {
		c.disableWebSocket()
		return &modelResponse{Attempts: attempts, WireRequestBytes: wireRequestBytes}, ctx.Err()
	}
	c.disableWebSocket()
	recordTrace(ctx, TraceEvent{Kind: "responses_websocket", Outcome: "http_fallback"})
	response, err := c.fallback.Complete(ctx, req)
	if response == nil {
		response = &modelResponse{}
	}
	response.Attempts += attempts
	response.WireRequestBytes += wireRequestBytes
	return response, err
}

func (c *responsesWebSocketConversation) rememberChain(input []json.RawMessage, response responsesResponse) {
	output := make([]any, len(response.Output))
	for i := range response.Output {
		output[i] = response.Output[i]
	}
	canonicalOutput, err := canonicalResponsesItems(output)
	if err != nil || response.ID == "" {
		c.clearChain()
		return
	}
	c.previousResponseID = response.ID
	c.expectedPrefix = append(append([]json.RawMessage(nil), input...), canonicalOutput...)
}

func (c *responsesWebSocketConversation) clearChain() {
	c.previousResponseID = ""
	c.expectedPrefix = nil
}

func (c *responsesWebSocketConversation) disableWebSocket() {
	c.httpOnly = true
	c.clearChain()
	if c.conn != nil {
		_ = c.conn.CloseNow()
		c.conn = nil
	}
}

func (c *responsesWebSocketConversation) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.conn = nil
	return err
}

func canonicalResponsesItems(items []any) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, err
		}
		out[i] = append(json.RawMessage(nil), compact.Bytes()...)
	}
	return out, nil
}

func responsesItemsHavePrefix(items, prefix []json.RawMessage) bool {
	if len(prefix) == 0 || len(items) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !bytes.Equal(items[i], prefix[i]) {
			return false
		}
	}
	return true
}
