package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestPromptCacheKeyReachesBothProviderRequests(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			shrinkCallDelay(t)
			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if apiMode == APIResponses {
					_, _ = w.Write([]byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer server.Close()

			transport := modelTransport(newChatCompletionsTransport(newHTTPAPIClient(server.URL, "", nil)))
			if apiMode == APIResponses {
				transport = newResponsesTransport(newHTTPAPIClient(server.URL, "", nil))
			}
			_, err := transport.Complete(context.Background(), modelRequest{Model: "model", PromptCacheKey: "aster_analysis_v1:workspace:shard"})
			if err != nil {
				t.Fatal(err)
			}
			if request["prompt_cache_key"] != "aster_analysis_v1:workspace:shard" {
				t.Fatalf("prompt cache key = %#v", request["prompt_cache_key"])
			}
		})
	}
}
