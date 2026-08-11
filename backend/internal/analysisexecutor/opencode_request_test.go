package analysisexecutor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestNewOpenCodeRequestShapeRecordsBoundedFacts(t *testing.T) {
	spec := OpenCodeSpec{
		Gateway:            engineruntime.ModelGatewayConfig{Model: "claude-sonnet-4.6"},
		Prompt:             "synthetic prompt",
		ModelContextTokens: 200000,
		ModelOutputTokens:  8192,
	}
	got := newOpenCodeRequestShape(spec, "1.18.2")
	wantSystemBytes, err := openCodeSystemPromptBytes(spec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.StreamingMode != "streaming" || got.ModelID != spec.Gateway.Model || !got.SystemPromptBytesAvailable || got.SystemPromptBytes != wantSystemBytes || got.UserPromptBytes != len(spec.Prompt) || got.ResponseSchemaSHA256 != agentanalysis.WorkspaceResultSchemaHash() || got.ToolChoiceMode != "required" || got.ContextLimit != 200000 || got.OutputTokenLimit != 8192 || got.OpenCodeVersion != "1.18.2" || got.ToolSchemaAvailable {
		t.Fatalf("shape=%+v", got)
	}
}

func TestFetchOpenCodeToolSchemaDigestUsesOnlyAnalysisTools(t *testing.T) {
	response := []map[string]any{
		{"id": "write", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
		{"id": "grep", "parameters": map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}},
		{"id": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}}}},
		{"id": "glob", "parameters": map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}},
		{"id": "bash", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/experimental/tool" || r.URL.Query().Get("provider") != "engine" || r.URL.Query().Get("model") != "test-model" || r.URL.Query().Get("directory") != "/workspace" {
			t.Fatalf("request=%s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: "/workspace", Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}}
	count, digest, err := fetchOpenCodeToolSchemaDigest(t.Context(), server.Client(), server.URL, spec)
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 || len(digest) != 64 {
		t.Fatalf("count=%d digest=%q", count, digest)
	}
	response[0], response[4] = response[4], response[0]
	count2, digest2, err := fetchOpenCodeToolSchemaDigest(t.Context(), server.Client(), server.URL, spec)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count || digest2 != digest {
		t.Fatalf("digest changed with response order: %s != %s", digest2, digest)
	}
}

func TestFetchOpenCodeToolSchemaDigestRejectsMissingAnalysisTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"read","parameters":{"type":"object"}}]`)
	}))
	defer server.Close()
	_, _, err := fetchOpenCodeToolSchemaDigest(t.Context(), server.Client(), server.URL, OpenCodeSpec{Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}})
	if err == nil {
		t.Fatal("incomplete tool schema set was accepted")
	}
}
