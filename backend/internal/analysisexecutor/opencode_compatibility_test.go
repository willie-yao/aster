package analysisexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestOpenCode1182RequestShapeCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	requests := make(chan []byte, 4)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil || len(data) > maxOpenCodeAPIResponseBytes {
			t.Errorf("read provider request: bytes=%d err=%v", len(data), err)
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		requests <- data
		body := []byte(`{"error":{"message":"synthetic bad request","code":"synthetic"}}`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer gateway.Close()

	workDir := t.TempDir()
	for _, dir := range []string{"source", "artifacts", "result"} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "source", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "artifacts", "failure.log"), []byte("synthetic failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	spec := OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Gateway: engineruntime.ModelGatewayConfig{
			Endpoint: gateway.URL + "/v1/chat/completions", Model: "synthetic-model", ProtocolVersion: "openai-chat-completions-v1",
		},
		Prompt: "Read artifacts/failure.log and return one structured result.", MaxSteps: 3,
		ModelContextTokens: 200000, ModelOutputTokens: 8192,
	}
	result, err := defaultRunOpenCode(ctx, spec)
	if err == nil {
		t.Fatal("synthetic HTTP 400 unexpectedly succeeded")
	}
	if result.Telemetry.Error.HTTPStatusCode != http.StatusBadRequest || result.Telemetry.Error.Classification != "api_bad_request" || result.Telemetry.Error.Retryable || !result.Telemetry.Error.RetryableKnown {
		t.Fatalf("err=%v telemetry=%+v", err, result.Telemetry)
	}
	shape := result.Telemetry.RequestShape
	if !shape.Available || !shape.SystemPromptBytesAvailable || !shape.ToolSchemaAvailable || shape.OpenCodeVersion != "1.18.2" {
		t.Fatalf("request shape=%+v", shape)
	}

	var raw []byte
	select {
	case raw = <-requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	observed, err := parseSyntheticProviderRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if observed.model != shape.ModelID || observed.streamingMode != shape.StreamingMode || observed.systemPromptBytes != shape.SystemPromptBytes || observed.userPromptBytes != shape.UserPromptBytes || observed.toolCount != shape.ToolCount || observed.toolSchemaSHA256 != shape.ToolSchemaSHA256 || observed.toolChoiceMode != shape.ToolChoiceMode || observed.outputTokenLimit != shape.OutputTokenLimit {
		t.Fatalf("observed=%+v telemetry=%+v", observed, shape)
	}
}

type syntheticProviderRequestShape struct {
	model             string
	streamingMode     string
	systemPromptBytes int
	userPromptBytes   int
	toolCount         int
	toolSchemaSHA256  string
	toolChoiceMode    string
	outputTokenLimit  int
}

func parseSyntheticProviderRequest(raw []byte) (syntheticProviderRequestShape, error) {
	var request struct {
		Model      string `json:"model"`
		Stream     bool   `json:"stream"`
		ToolChoice string `json:"tool_choice"`
		MaxTokens  int    `json:"max_tokens"`
		Messages   []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return syntheticProviderRequestShape{}, err
	}
	shape := syntheticProviderRequestShape{model: request.Model, toolChoiceMode: request.ToolChoice, outputTokenLimit: request.MaxTokens}
	if request.Stream {
		shape.streamingMode = "streaming"
	}
	for _, message := range request.Messages {
		count, err := providerMessageTextBytes(message.Content)
		if err != nil {
			return syntheticProviderRequestShape{}, err
		}
		switch message.Role {
		case "system":
			shape.systemPromptBytes += count
		case "user":
			shape.userPromptBytes += count
		}
	}
	tools := make([]digestToolSchema, 0, len(request.Tools))
	for _, tool := range request.Tools {
		canonical, err := canonicalJSON(tool.Function.Parameters)
		if err != nil {
			return syntheticProviderRequestShape{}, err
		}
		tools = append(tools, digestToolSchema{Name: tool.Function.Name, Schema: canonical})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	data, err := json.Marshal(tools)
	if err != nil {
		return syntheticProviderRequestShape{}, err
	}
	digest := sha256.Sum256(data)
	shape.toolCount = len(tools)
	shape.toolSchemaSHA256 = hex.EncodeToString(digest[:])
	return shape, nil
}

func providerMessageTextBytes(raw json.RawMessage) (int, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return len([]byte(text)), nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return 0, fmt.Errorf("decode provider message content")
	}
	total := 0
	for _, part := range parts {
		if part.Type == "text" {
			total += len([]byte(part.Text))
		}
	}
	return total, nil
}
