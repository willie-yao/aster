package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func TestResponsesWebSocketContinuesWithIncrementalInput(t *testing.T) {
	shrinkCallDelay(t)
	var mu sync.Mutex
	var payloads [][]byte
	responses := []string{
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":21,"output_tokens":8,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":3}},"output":[{"id":"rs-1","type":"reasoning","encrypted_content":"encrypted-state","summary":[]},{"type":"function_call","call_id":"call-1","name":"read_artifact","arguments":"{\"path\":\"log.txt\"}"}]}}`,
		`{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for _, response := range responses {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			payloads = append(payloads, append([]byte(nil), payload...))
			mu.Unlock()
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()

	messages := []modelMessage{{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("inspect")}}
	first, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if first.Attempts != 1 || first.WireRequestBytes == 0 || first.Usage.InputTokens != 21 || first.Usage.CachedInputTokens != 5 || first.Usage.CacheWriteInputTokens != 2 || !first.Usage.CacheWriteInputTokensReported || first.Usage.ReasoningTokens != 3 {
		t.Fatalf("first response = %+v", first)
	}
	messages = append(messages, first.Message, modelMessage{Role: "tool", ToolCallID: "call-1", Content: strPtr(`{"content":"failure"}`)})
	second, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempts != 1 || second.Message.Content == nil || *second.Message.Content != "done" {
		t.Fatalf("second response = %+v", second)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("payloads = %d", len(payloads))
	}
	if first.WireRequestBytes != len(payloads[0]) || second.WireRequestBytes != len(payloads[1]) {
		t.Fatalf("wire bytes = (%d,%d), payloads = (%d,%d)", first.WireRequestBytes, second.WireRequestBytes, len(payloads[0]), len(payloads[1]))
	}
	firstRequest := decodeWebSocketRequest(t, payloads[0])
	if firstRequest.Type != "response.create" || firstRequest.PreviousResponseID != "" || firstRequest.ServiceTier != "" || firstRequest.Store || len(firstRequest.Input) != 2 {
		t.Fatalf("first request = %+v", firstRequest)
	}
	secondRequest := decodeWebSocketRequest(t, payloads[1])
	if secondRequest.PreviousResponseID != "resp-1" || len(secondRequest.Input) != 1 {
		t.Fatalf("second request = %+v", secondRequest)
	}
	var output struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if err := remarshal(secondRequest.Input[0], &output); err != nil {
		t.Fatal(err)
	}
	if output.Type != "function_call_output" || output.CallID != "call-1" {
		t.Fatalf("incremental input = %+v", output)
	}
}

func TestResponsesWebSocketSanitizedFinalizationStartsNewChain(t *testing.T) {
	shrinkCallDelay(t)
	payloads := make(chan []byte, 2)
	finalJSON := `{"summary":"done"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			payloads <- append([]byte(nil), payload...)
			response := `{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[{"type":"reasoning","encrypted_content":"final-reasoning"},{"type":"function_call","call_id":"submit-1","name":"submit_analysis","arguments":"{\"summary\":\"done\"}"}]}}`
			if i == 1 {
				response = `{"type":"response.completed","response":{"id":"resp-revised","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"revised"}]}]}}`
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()
	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	messages := []modelMessage{{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("finalize")}}
	if _, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages}); err != nil {
		t.Fatal(err)
	}
	messages = append(messages,
		modelMessage{Role: "assistant", Content: strPtr(finalJSON)},
		modelMessage{Role: "user", Content: strPtr("revise")},
	)
	if _, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages}); err != nil {
		t.Fatal(err)
	}
	<-payloads
	secondRaw := <-payloads
	second := decodeWebSocketRequest(t, secondRaw)
	if second.PreviousResponseID != "" || len(second.Input) != 4 {
		t.Fatalf("second request = %+v", second)
	}
	text := string(secondRaw)
	if strings.Contains(text, "final-reasoning") || strings.Contains(text, "submit-1") || !strings.Contains(text, `summary\":\"done`) {
		t.Fatalf("sanitized finalization request = %s", text)
	}
}

func TestResponsesWebSocketPrefixMismatchStartsNewChain(t *testing.T) {
	shrinkCallDelay(t)
	payloads := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			payloads <- append([]byte(nil), payload...)
			response := `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}]}}`
			if i == 1 {
				response = `{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}}`
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	if _, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("first")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("replacement")}}}); err != nil {
		t.Fatal(err)
	}
	<-payloads
	second := decodeWebSocketRequest(t, <-payloads)
	if second.PreviousResponseID != "" || len(second.Input) != 1 {
		t.Fatalf("second request = %+v", second)
	}
}

func TestResponsesWebSocketRecoversMissingPreviousResponse(t *testing.T) {
	shrinkCallDelay(t)
	payloads := make(chan []byte, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 3; i++ {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			payloads <- append([]byte(nil), payload...)
			var response string
			switch i {
			case 0:
				response = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"read_artifact","arguments":"{\"path\":\"log.txt\"}"}]}}`
			case 1:
				response = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`
			case 2:
				response = `{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	messages := []modelMessage{{Role: "user", Content: strPtr("inspect")}}
	first, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, first.Message, modelMessage{Role: "tool", ToolCallID: "call-1", Content: strPtr("result")})
	second, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	<-payloads
	incrementalPayload := <-payloads
	recoveryPayload := <-payloads
	if second.Attempts != 2 || second.WireRequestBytes != len(incrementalPayload)+len(recoveryPayload) {
		t.Fatalf("response = %+v", second)
	}
	incremental := decodeWebSocketRequest(t, incrementalPayload)
	recovery := decodeWebSocketRequest(t, recoveryPayload)
	if incremental.PreviousResponseID != "resp-1" || len(incremental.Input) != 1 {
		t.Fatalf("incremental request = %+v", incremental)
	}
	if recovery.PreviousResponseID != "" || len(recovery.Input) != 3 {
		t.Fatalf("recovery request = %+v", recovery)
	}
}

func TestResponsesWebSocketFallsBackToHTTP(t *testing.T) {
	shrinkCallDelay(t)
	for _, tc := range []struct {
		name            string
		webSocketError  string
		httpRateLimited bool
		wantAttempts    int
	}{
		{name: "rate limited", webSocketError: `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, httpRateLimited: true, wantAttempts: 3},
		{name: "connection expired", webSocketError: `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached"}}`, wantAttempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var webSocketRequests atomic.Int32
			var httpRequests atomic.Int32
			var mu sync.Mutex
			var payloadSizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					_, payload, err := conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					webSocketRequests.Add(1)
					mu.Lock()
					payloadSizes = append(payloadSizes, len(payload))
					mu.Unlock()
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(tc.webSocketError))
					return
				}
				body, _ := io.ReadAll(r.Body)
				httpAttempt := httpRequests.Add(1)
				mu.Lock()
				payloadSizes = append(payloadSizes, len(body))
				mu.Unlock()
				if tc.httpRateLimited && httpAttempt == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"rate limited"}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp-http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
			}))
			defer server.Close()

			client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
			conversation, err := client.conversation.NewConversation(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conversation.Close()
			request := modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("first")}}}
			response, err := conversation.Complete(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			wantWireBytes := 0
			for _, size := range payloadSizes {
				wantWireBytes += size
			}
			mu.Unlock()
			if response.Attempts != tc.wantAttempts || response.WireRequestBytes != wantWireBytes {
				t.Fatalf("response = %+v, wire bytes want %d", response, wantWireBytes)
			}
			beforeWS := webSocketRequests.Load()
			beforeHTTP := httpRequests.Load()
			if _, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("second")}}}); err != nil {
				t.Fatal(err)
			}
			if webSocketRequests.Load() != beforeWS || httpRequests.Load() != beforeHTTP+1 {
				t.Fatalf("later requests websocket=%d http=%d", webSocketRequests.Load(), httpRequests.Load())
			}
		})
	}
}

func TestResponsesWebSocketReadFailureAndCancellation(t *testing.T) {
	shrinkCallDelay(t)
	t.Run("read failure falls back", func(t *testing.T) {
		var httpRequests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				_, _, _ = conn.Read(r.Context())
				_ = conn.CloseNow()
				return
			}
			httpRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"resp-http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
		}))
		defer server.Close()
		client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
		conversation, err := client.conversation.NewConversation(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conversation.Close()
		response, err := conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
		if err != nil {
			t.Fatal(err)
		}
		if response.Attempts != 2 || httpRequests.Load() != 1 {
			t.Fatalf("response=%+v http=%d", response, httpRequests.Load())
		}
	})

	t.Run("cancellation does not fall back", func(t *testing.T) {
		seen := make(chan struct{})
		release := make(chan struct{})
		var httpRequests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
				close(seen)
				<-release
				return
			}
			httpRequests.Add(1)
		}))
		defer server.Close()
		client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
		conversation, err := client.conversation.NewConversation(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, err := conversation.Complete(ctx, modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
			result <- err
		}()
		<-seen
		cancel()
		err = <-result
		close(release)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		if httpRequests.Load() != 0 {
			t.Fatalf("http requests = %d", httpRequests.Load())
		}
	})
}

func TestResponsesWebSocketProtocolErrorsDoNotFallback(t *testing.T) {
	shrinkCallDelay(t)
	for _, tc := range []struct {
		name      string
		event     string
		wantError func(error) bool
	}{
		{name: "malformed event", event: `{`, wantError: func(err error) bool { return errors.Is(err, errResponsesWebSocketProtocol) }},
		{name: "unsupported tools message", event: `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"Tools are not supported for this model."}}`, wantError: isToolsUnsupportedError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var httpRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					httpRequests.Add(1)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(tc.event))
			}))
			defer server.Close()
			client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
			conversation, err := client.conversation.NewConversation(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conversation.Close()
			_, err = conversation.Complete(t.Context(), modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
			if err == nil || !tc.wantError(err) {
				t.Fatalf("error = %v", err)
			}
			if httpRequests.Load() != 0 {
				t.Fatalf("http requests = %d", httpRequests.Load())
			}
		})
	}
}

func TestResponsesWebSocketCanceledWriteDoesNotFallback(t *testing.T) {
	shrinkCallDelay(t)
	var httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpRequests.Add(1)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = conversation.Complete(ctx, modelRequest{Model: "model", Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if httpRequests.Load() != 0 {
		t.Fatalf("http requests = %d", httpRequests.Load())
	}
}

func TestResponsesWebSocketCanceledDial(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	api := newHTTPAPIClient(server.URL, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newResponsesWebSocketTransport(api, newResponsesTransport(api)).NewConversation(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestResponsesWebSocketResponseByteLimit(t *testing.T) {
	shrinkCallDelay(t)
	for _, tc := range []struct {
		name      string
		padding   int
		limitMode string
		wantError bool
	}{
		{name: "event envelope may exceed response limit", padding: 32, limitMode: "exact", wantError: false},
		{name: "nested response too large", padding: 256, limitMode: "smaller", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := `{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("x", tc.padding) + `"}]}]}`
			event := `{"type":"response.completed","response":` + response + `}`
			var httpRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					httpRequests.Add(1)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(event))
			}))
			defer server.Close()
			limit := int64(len(response))
			if tc.limitMode == "smaller" {
				limit--
			}
			client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
			conversation, err := client.conversation.NewConversation(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conversation.Close()
			_, err = conversation.Complete(t.Context(), modelRequest{Model: "model", MaxResponseBytes: limit, Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
			if tc.wantError {
				if !errors.Is(err, errResponsesWebSocketResponseTooBig) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if httpRequests.Load() != 0 {
				t.Fatalf("http requests = %d", httpRequests.Load())
			}
		})
	}
}

func TestResponsesWebSocketRejectsOversizedEventEnvelope(t *testing.T) {
	shrinkCallDelay(t)
	response := `{"id":"resp-1","status":"completed","output":[]}`
	event := `{"type":"response.completed","padding":"` + strings.Repeat("x", int(responsesWebSocketEventEnvelopeBytes)+1024) + `","response":` + response + `}`
	var httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpRequests.Add(1)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(event))
	}))
	defer server.Close()
	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", ResponsesWebSocket: true})
	conversation, err := client.conversation.NewConversation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	_, err = conversation.Complete(t.Context(), modelRequest{Model: "model", MaxResponseBytes: int64(len(response)), Messages: []modelMessage{{Role: "user", Content: strPtr("inspect")}}})
	if !errors.Is(err, websocket.ErrMessageTooBig) {
		t.Fatalf("error = %v", err)
	}
	if httpRequests.Load() != 0 {
		t.Fatalf("http requests = %d", httpRequests.Load())
	}
}

func TestResponsesWebSocketHandshakeHeadersAndRedirects(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		headers := make(chan http.Header, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Clone()
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}))
		defer server.Close()
		localURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, localURL.Host)
		}}
		api := newHTTPAPIClient("http://api.githubcopilot.com/responses", "token", map[string]string{"X-Extra": "value"})
		api.httpClient.Transport = transport
		conversation, err := newResponsesWebSocketTransport(api, newResponsesTransport(api)).NewConversation(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = conversation.Close()
		got := <-headers
		if got.Get("Authorization") != "Bearer token" || got.Get("X-Extra") != "value" || got.Get(modelprovider.CopilotIntegrationHeader) != modelprovider.CopilotIntegrationID {
			t.Fatalf("headers = %v", got)
		}
	})

	t.Run("cross origin redirect", func(t *testing.T) {
		var redirected atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			redirected.Add(1)
		}))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer server.Close()
		api := newHTTPAPIClient(server.URL, "", nil)
		_, err := newResponsesWebSocketTransport(api, newResponsesTransport(api)).NewConversation(t.Context())
		if err == nil || !strings.Contains(err.Error(), "different origin") {
			t.Fatalf("error = %v", err)
		}
		if redirected.Load() != 0 {
			t.Fatalf("redirected requests = %d", redirected.Load())
		}
	})
}

func TestNewAgenticConversationCancellationDoesNotFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &Client{conversation: failingConversationTransport{err: errors.New("dial failed")}}
	_, _, err := client.newAgenticConversation(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

type failingConversationTransport struct{ err error }

func (f failingConversationTransport) NewConversation(context.Context) (modelConversation, error) {
	return nil, f.err
}

func decodeWebSocketRequest(t *testing.T, raw []byte) responsesRequest {
	t.Helper()
	var request responsesRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func remarshal(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
