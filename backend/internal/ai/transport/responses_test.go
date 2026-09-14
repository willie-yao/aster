package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func TestResponsesTransportToolRoundTrip(t *testing.T) {

	var requests []map[string]any
	responses := []string{
		`{"id":"resp-1","status":"completed","usage":{"input_tokens":21,"output_tokens":8,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":3}},"output":[{"id":"rs-1","type":"reasoning","encrypted_content":"encrypted-state","summary":[]},{"type":"function_call","call_id":"call-1","name":"read_artifact","arguments":"{\"path\":\"log.txt\"}"}]}`,
		`{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responses[len(requests)-1]))
	}))
	defer server.Close()
	client := NewClient(modelprovider.Config{API: modelprovider.APIResponses, Endpoint: server.URL, Model: "model"}, "token", nil, "", nil)
	messages := []Message{{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("inspect")}}
	first, err := client.Complete(context.Background(), Request{Model: client.model, Messages: messages, Tools: nil, ParallelToolCalls: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) != 1 || len(first.Message.ProviderItems) != 2 {
		t.Fatalf("first response = %+v", first)
	}
	if first.ResponseID != "resp-1" || first.Status != "completed" || !first.Usage.Reported || first.Usage.InputTokens != 21 || first.Usage.CachedInputTokens != 5 || !first.Usage.CacheWriteInputTokensReported || first.Usage.CacheWriteInputTokens != 2 || first.Usage.OutputTokens != 8 || first.Usage.ReasoningTokens != 3 || first.Attempts != 1 || first.WireRequestBytes == 0 {
		t.Fatalf("first metadata = %+v", first)
	}
	messages = append(messages, first.Message, Message{Role: "tool", ToolCallID: "call-1", Content: strPtr(`{"ok":true}`)})
	second, err := client.Complete(context.Background(), Request{Model: client.model, Messages: messages, Tools: nil, ParallelToolCalls: nil})
	if err != nil || second.Message.Content == nil || *second.Message.Content != "done" {
		t.Fatalf("second response = %+v, err = %v", second, err)
	}
	include := requests[0]["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", include)
	}
	if store, ok := requests[0]["store"].(bool); !ok || store {
		t.Fatalf("store = %#v, want false", requests[0]["store"])
	}
	if _, ok := requests[0]["service_tier"]; ok {
		t.Fatalf("default request included service_tier: %#v", requests[0])
	}
	input := requests[1]["input"].([]any)
	var reasoning, call, output bool
	for _, raw := range input {
		item := raw.(map[string]any)
		switch item["type"] {
		case "reasoning":
			reasoning = item["encrypted_content"] == "encrypted-state"
		case "function_call":
			call = item["call_id"] == "call-1"
		case "function_call_output":
			output = item["call_id"] == "call-1"
		}
	}
	if !reasoning || !call || !output {
		t.Fatalf("second input missing continuation items: %#v", input)
	}
}

func TestResponsesTokenUsageDistinguishesCacheWriteAbsentZeroAndPositive(t *testing.T) {
	if got := responsesTokenUsage(nil); got.Reported {
		t.Fatalf("absent usage = %+v", got)
	}
	if got := responsesTokenUsage(&responsesUsage{}); !got.Reported || got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheWriteInputTokensReported {
		t.Fatalf("absent cache write = %+v", got)
	}
	zero := 0
	if got := responsesTokenUsage(&responsesUsage{InputTokensDetails: responsesInputTokenDetails{CacheWriteTokens: &zero}}); !got.CacheWriteInputTokensReported || got.CacheWriteInputTokens != 0 {
		t.Fatalf("explicit zero cache write = %+v", got)
	}
	positive := 7
	if got := responsesTokenUsage(&responsesUsage{InputTokensDetails: responsesInputTokenDetails{CacheWriteTokens: &positive}}); !got.CacheWriteInputTokensReported || got.CacheWriteInputTokens != 7 {
		t.Fatalf("positive cache write = %+v", got)
	}
}

func TestResponsesAssistantPhaseRoundTrip(t *testing.T) {
	response := responsesResponse{
		ID: "response", Status: "completed",
		Output: []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","phase":"analysis","content":[{"type":"output_text","text":"draft"}]}`)},
	}
	decoded := decodeResponsesResponse(response)
	if decoded.Message.Phase != "analysis" || decoded.Message.Content == nil || *decoded.Message.Content != "draft" {
		t.Fatalf("decoded message = %+v", decoded.Message)
	}
	decoded.Message.ProviderItems = nil
	input := encodeResponsesInput([]Message{decoded.Message})
	item := input[0].(map[string]any)
	if item["phase"] != "analysis" {
		t.Fatalf("encoded assistant = %#v", item)
	}
}

func TestResponsesTransportFlattensTools(t *testing.T) {
	schemas := []ToolSchema{{Type: "function", Function: FunctionDecl{Name: "read", Description: "read", Parameters: map[string]any{"type": "object"}}}}
	got := encodeResponsesTools(schemas)
	if len(got) != 1 || got[0].Name != "read" || got[0].Type != "function" || got[0].Strict {
		t.Fatalf("tools = %+v", got)
	}
}

func TestResponsesTransportRejectsIncomplete(t *testing.T) {

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","status":"incomplete","output":[{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}]}`))
	}))
	defer s.Close()
	c := NewClient(modelprovider.Config{API: modelprovider.APIResponses, Endpoint: s.URL, Model: "m"}, "", nil, "", nil)
	if _, err := c.Complete(context.Background(), Request{Model: c.model, Messages: nil, Tools: nil, ParallelToolCalls: nil}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v", err)
	}
}

func TestResponsesFlexRequiresConsecutiveCapacityResponses(t *testing.T) {

	for _, tc := range []struct {
		name   string
		bodies []string
	}{
		{name: "capacity then rate limit", bodies: []string{"Resource unavailable, try again later", "rate limited"}},
		{name: "rate limit then capacity", bodies: []string{"rate limited", "Resource unavailable, try again later"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tiers []string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				tiers = append(tiers, request["service_tier"].(string))
				if calls < len(tc.bodies) {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(tc.bodies[calls]))
					calls++
					return
				}
				_, _ = w.Write([]byte(`{"id":"resp-flex","status":"completed","service_tier":"flex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
			}))
			defer server.Close()
			api := newHTTPAPIClient(server.URL, "", nil)
			api.serviceTier = modelprovider.ServiceTierFlex
			provider := newResponsesTransport(api)
			if _, err := provider.Complete(context.Background(), Request{Model: "m"}); err != nil {
				t.Fatal(err)
			}
			if want := []string{modelprovider.ServiceTierFlex, modelprovider.ServiceTierFlex, modelprovider.ServiceTierFlex}; !slices.Equal(tiers, want) {
				t.Fatalf("tiers = %v, want %v", tiers, want)
			}
		})
	}
}
