package ai

import (
	"encoding/json"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/transport"
)

func TestAnalysisPromptCacheKeySerializedIdentity(t *testing.T) {
	schemas := []transport.ToolSchema{{
		Type: "function",
		Function: transport.FunctionDecl{
			Name: "read_artifact", Description: "Read a build log", Strict: true,
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			},
		},
	}}
	raw, err := json.Marshal(schemas)
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `[{"type":"function","function":{"name":"read_artifact","description":"Read a build log","parameters":{"properties":{"path":{"type":"string"}},"type":"object"},"strict":true}}]`
	if string(raw) != wantJSON {
		t.Fatalf("serialized schemas = %s, want %s", raw, wantJSON)
	}
	const wantKey = "aster_analysis_v1:ef7195539cdb15aa:5958feff4385fca0"
	if got := analysisPromptCacheKey("stable prompt", schemas); got != wantKey {
		t.Fatalf("prompt cache key = %q, want %q", got, wantKey)
	}
}

func TestAnalysisPromptCacheKeyUsesStablePromptAndToolSchemas(t *testing.T) {
	base := []transport.ToolSchema{{Type: "function", Function: transport.FunctionDecl{Name: "read_artifact"}}}
	repo := append(append([]transport.ToolSchema(nil), base...), transport.ToolSchema{Type: "function", Function: transport.FunctionDecl{Name: "read_repo_file"}})
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
