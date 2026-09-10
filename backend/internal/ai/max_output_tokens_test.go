package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestClientMaxOutputTokensIsOptionalAndFingerprinting(t *testing.T) {
	base := NewClientWithOptions(Options{Endpoint: "https://example.invalid/v1/chat/completions", Model: "model"})
	limited := NewClientWithOptions(Options{Endpoint: "https://example.invalid/v1/chat/completions", Model: "model", MaxOutputTokens: 8192})
	capture := &recordingTransport{result: &modelResponse{
		HasMessage: true, Message: modelMessage{Role: "assistant", Content: strPtr("ok")},
	}}
	limited.transport = capture
	if _, err := limited.callModel(t.Context(), []modelMessage{{Role: "user", Content: strPtr("test")}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if capture.request.MaxOutputTokens != 8192 || base.ModelFingerprint() == limited.ModelFingerprint() {
		t.Fatalf("request=%+v base=%s limited=%s", capture.request, base.ModelFingerprint(), limited.ModelFingerprint())
	}
	if err := NewClientWithOptions(Options{Endpoint: "https://example.invalid/v1/chat/completions", Model: "model", MaxOutputTokens: -1}).ValidateConfiguration(); err == nil {
		t.Fatal("negative output limit was accepted")
	}
}

func TestMaxOutputTokenWireFields(t *testing.T) {
	for _, api := range []struct {
		mode, field, otherField string
	}{
		{APIChatCompletions, "max_tokens", "max_output_tokens"},
		{APIResponses, "max_output_tokens", "max_tokens"},
	} {
		for _, limit := range []int{0, 8192} {
			t.Run(fmt.Sprintf("%s/%d", api.mode, limit), func(t *testing.T) {
				shrinkCallDelay(t)
				var mu sync.Mutex
				var requests []map[string]json.RawMessage
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					mu.Lock()
					requests = append(requests, request)
					mu.Unlock()
					writeReasoningTestFinal(w, api.mode, "ok")
				}))
				t.Cleanup(server.Close)

				client := NewClientWithOptions(Options{
					API: api.mode, Endpoint: server.URL, Model: "model", MaxOutputTokens: limit,
				})
				result, err := client.Complete(t.Context(), "system", "user")
				if err != nil || result != "ok" {
					t.Fatalf("Complete = %q, %v", result, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(requests) != 1 {
					t.Fatalf("provider requests = %d, want 1", len(requests))
				}
				request := requests[0]
				if _, exists := request[api.otherField]; exists {
					t.Errorf("request contains other API field %q", api.otherField)
				}
				raw, exists := request[api.field]
				if limit == 0 {
					if exists {
						t.Errorf("default request contains %q: %s", api.field, raw)
					}
					return
				}
				var got int
				if err := json.Unmarshal(raw, &got); err != nil || got != limit {
					t.Errorf("%s = %s, decode error = %v, want %d", api.field, raw, err, limit)
				}
			})
		}
	}
}
