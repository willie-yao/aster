package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func TestPromptCacheKeyReachesBothProviderRequests(t *testing.T) {
	for _, apiMode := range []string{modelprovider.APIChatCompletions, modelprovider.APIResponses} {
		t.Run(apiMode, func(t *testing.T) {

			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if apiMode == modelprovider.APIResponses {
					_, _ = w.Write([]byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer server.Close()

			provider := NewClient(modelprovider.Config{API: apiMode, Endpoint: server.URL, Model: "model"}, "", nil, "", nil)
			_, err := provider.Complete(context.Background(), Request{Model: "model", PromptCacheKey: "aster_analysis_v1:workspace:shard"})
			if err != nil {
				t.Fatal(err)
			}
			if request["prompt_cache_key"] != "aster_analysis_v1:workspace:shard" {
				t.Fatalf("prompt cache key = %#v", request["prompt_cache_key"])
			}
		})
	}
}
