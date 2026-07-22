package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

func TestResponsesTransportToolRoundTrip(t *testing.T) {
	shrinkCallDelay(t)
	var requests []map[string]any
	responses := []string{
		`{"id":"resp-1","status":"completed","output":[{"id":"rs-1","type":"reasoning","encrypted_content":"encrypted-state","summary":[]},{"type":"function_call","call_id":"call-1","name":"read_artifact","arguments":"{\"path\":\"log.txt\"}"}]}`,
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
	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "model", Token: "token"})
	messages := []modelMessage{{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("inspect")}}
	first, err := client.callModel(context.Background(), messages, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) != 1 || len(first.Message.ProviderItems) != 2 {
		t.Fatalf("first response = %+v", first)
	}
	messages = append(messages, first.Message, modelMessage{Role: "tool", ToolCallID: "call-1", Content: strPtr(`{"ok":true}`)})
	second, err := client.callModel(context.Background(), messages, nil, nil)
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

func TestResponsesTransportFlattensTools(t *testing.T) {
	schemas := []tools.Schema{{Type: "function", Function: tools.FunctionDecl{Name: "read", Description: "read", Parameters: map[string]any{"type": "object"}}}}
	got := encodeResponsesTools(schemas)
	if len(got) != 1 || got[0].Name != "read" || got[0].Type != "function" || got[0].Strict {
		t.Fatalf("tools = %+v", got)
	}
}

func TestResponsesTransportRejectsIncomplete(t *testing.T) {
	shrinkCallDelay(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","status":"incomplete","output":[{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}]}`))
	}))
	defer s.Close()
	c := NewClientWithOptions(Options{API: APIResponses, Endpoint: s.URL, Model: "m"})
	if _, err := c.callModel(context.Background(), nil, nil, nil); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v", err)
	}
}
