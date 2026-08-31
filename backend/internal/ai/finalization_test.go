package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponsesForcedFinalizationReplaysAsAssistantContent(t *testing.T) {
	shrinkCallDelay(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if requests == 2 {
			if hasOrphanResponsesFunctionCall(request.Input, "submit-1") {
				http.Error(w, `{"error":{"message":"No tool output found for function call submit-1.","code":"invalid_request_body"}}`, http.StatusBadRequest)
				return
			}
			if !hasResponsesAssistantPhase(request.Input, "final_answer") {
				http.Error(w, `{"error":{"message":"assistant phase missing","code":"invalid_request_body"}}`, http.StatusBadRequest)
				return
			}
		}
		callID := fmt.Sprintf("submit-%d", requests)
		_, _ = fmt.Fprintf(w, `{"id":"resp-%d","status":"completed","output":[{"id":"reason-%d","type":"reasoning","encrypted_content":"state-%d","summary":[]},{"type":"function_call","call_id":%q,"name":"submit_analysis","arguments":%q}]}`, requests, requests, requests, callID, cleanFinalJSON)
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model"})
	headroom := contextHeadroomFor(AgenticOptions{ContextWindowTokens: 128_000, RequestTokenBudget: 120_000})
	base := []modelMessage{{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("analyze")}}
	first, providerItems, safe := client.runFinalizeRound(t.Context(), base, headroom)
	if !safe || first != cleanFinalJSON {
		t.Fatalf("first finalization = %q, safe=%t", first, safe)
	}
	repair := append(base,
		modelMessage{Role: "assistant", Content: strPtr(first), ProviderItems: providerItems},
		modelMessage{Role: "user", Content: strPtr("fix the draft")},
	)
	second, _, safe := client.runFinalizeRound(t.Context(), repair, headroom)
	if !safe || second != cleanFinalJSON {
		t.Fatalf("second finalization = %q, safe=%t, requests=%d", second, safe, requests)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want 2", requests)
	}
}

func hasResponsesAssistantPhase(items []json.RawMessage, phase string) bool {
	for _, raw := range items {
		var item struct {
			Role  string `json:"role"`
			Phase string `json:"phase"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Role == "assistant" && item.Phase == phase {
			return true
		}
	}
	return false
}

func hasOrphanResponsesFunctionCall(items []json.RawMessage, callID string) bool {
	functionCall := false
	functionOutput := false
	for _, raw := range items {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(raw, &item) != nil || item.CallID != callID {
			continue
		}
		functionCall = functionCall || item.Type == "function_call"
		functionOutput = functionOutput || item.Type == "function_call_output"
	}
	return functionCall && !functionOutput
}

func TestChatForcedFinalizationStillReturnsArguments(t *testing.T) {
	shrinkCallDelay(t)
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"submit-1","type":"function","function":{"name":"submit_analysis","arguments":%q}}]}}]}`, cleanFinalJSON)
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: server.URL, Model: "model"})
	headroom := contextHeadroomFor(AgenticOptions{ContextWindowTokens: 128_000, RequestTokenBudget: 120_000})
	content, providerItems, safe := client.runFinalizeRound(t.Context(), []modelMessage{{Role: "user", Content: strPtr("analyze")}}, headroom)
	if !safe || content != cleanFinalJSON || len(providerItems) != 0 {
		t.Fatalf("finalization = %q, provider_items=%d, safe=%t", content, len(providerItems), safe)
	}
	if request["tool_choice"] == nil || request["tools"] == nil {
		t.Fatalf("chat finalization request = %#v", request)
	}
	if _, ok := request["input"]; ok {
		t.Fatalf("chat request included Responses input: %#v", request)
	}
}
