package ai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

func TestAnalysisPromptCacheKeyUsesStablePromptAndToolSchemas(t *testing.T) {
	base := []tools.Schema{{Type: "function", Function: tools.FunctionDecl{Name: "read_artifact"}}}
	repo := append(append([]tools.Schema(nil), base...), tools.Schema{Type: "function", Function: tools.FunctionDecl{Name: "read_repo_file"}})
	first := analysisPromptCacheKey("stable prompt", base)
	if first != analysisPromptCacheKey("stable prompt", base) {
		t.Fatal("same stable prefix produced different keys")
	}
	if first == analysisPromptCacheKey("changed prompt", base) {
		t.Fatal("changed stable prompt reused a key")
	}
	if first == analysisPromptCacheKey("stable prompt", repo) {
		t.Fatal("repo tools reused the tool-schema shard")
	}
}

func TestPromptCacheKeyReachesBothTransports(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			capture := &capturePromptCacheTransport{}
			client := &Client{transport: capture, model: "model", apiMode: apiMode}
			ctx := withPromptCacheKey(context.Background(), "aster_analysis_v1:workspace:shard")
			if _, err := client.callModel(ctx, []modelMessage{{Role: "user", Content: strPtr("test")}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if capture.request.PromptCacheKey != "aster_analysis_v1:workspace:shard" {
				t.Fatalf("prompt cache key = %q", capture.request.PromptCacheKey)
			}
		})
	}
}

type capturePromptCacheTransport struct{ request modelRequest }

func (t *capturePromptCacheTransport) Complete(_ context.Context, request modelRequest) (*modelResponse, error) {
	t.request = request
	return &modelResponse{HasMessage: true, Message: modelMessage{Role: "assistant", Content: strPtr("ok")}}, nil
}

func TestPromptCacheKeyWireFields(t *testing.T) {
	chat, err := json.Marshal(chatCompletionsRequest{Model: "model", PromptCacheKey: "key"})
	if err != nil || !jsonFieldEquals(chat, "prompt_cache_key", "key") {
		t.Fatalf("chat request = %s, err = %v", chat, err)
	}
	responses, err := json.Marshal(responsesRequest{Model: "model", Store: false, PromptCacheKey: "key"})
	if err != nil || !jsonFieldEquals(responses, "prompt_cache_key", "key") {
		t.Fatalf("responses request = %s, err = %v", responses, err)
	}
}

func jsonFieldEquals(raw []byte, field, want string) bool {
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value[field] == want
}
