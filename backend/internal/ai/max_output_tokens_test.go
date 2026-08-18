package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type captureMaxOutputTransport struct {
	request modelRequest
}

func (t *captureMaxOutputTransport) Complete(_ context.Context, request modelRequest) (*modelResponse, error) {
	t.request = request
	return &modelResponse{HasMessage: true, Message: modelMessage{Role: "assistant", Content: strPtr("ok")}}, nil
}

func TestClientMaxOutputTokensIsOptionalAndFingerprinting(t *testing.T) {
	base := NewClientWithOptions(Options{Endpoint: "https://example.invalid/v1/chat/completions", Model: "model"})
	limited := NewClientWithOptions(Options{Endpoint: "https://example.invalid/v1/chat/completions", Model: "model", MaxOutputTokens: 8192})
	capture := &captureMaxOutputTransport{}
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
	chat, err := json.Marshal(chatCompletionsRequest{Model: "model", MaxTokens: 8192})
	if err != nil || !strings.Contains(string(chat), `"max_tokens":8192`) {
		t.Fatalf("chat=%s err=%v", chat, err)
	}
	responses, err := json.Marshal(responsesRequest{Model: "model", MaxOutputTokens: 8192})
	if err != nil || !strings.Contains(string(responses), `"max_output_tokens":8192`) {
		t.Fatalf("responses=%s err=%v", responses, err)
	}
}
