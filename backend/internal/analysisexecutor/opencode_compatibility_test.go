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
	"slices"
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
	toolChoice := request.ToolChoice
	if toolChoice == "" {
		toolChoice = "auto"
	}
	shape := syntheticProviderRequestShape{model: request.Model, toolChoiceMode: toolChoice, outputTokenLimit: request.MaxTokens}
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

func TestOpenCode1182TwoPhaseCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	workDir := t.TempDir()
	for _, dir := range []string{"source", "artifacts", "result"} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	artifactPath := filepath.Join(workDir, "artifacts", "failure.log")
	sourcePath := filepath.Join(workDir, "source", "main.go")
	if err := os.WriteFile(artifactPath, []byte("synthetic failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var toolSets [][]string
	var providerShapes []syntheticProviderRequestShape
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil {
			t.Fatal(err)
		}
		shape, err := parseSyntheticProviderRequest(data)
		if err != nil {
			t.Fatal(err)
		}
		providerShapes = append(providerShapes, shape)
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Function.Name)
		}
		sort.Strings(names)
		toolSets = append(toolSets, names)
		calls++
		switch calls {
		case 1:
			writeSyntheticOpenAIToolCalls(t, w, []syntheticOpenAIToolCall{
				{Name: "read", Arguments: map[string]any{"filePath": "artifacts/failure.log"}},
				{Name: "read", Arguments: map[string]any{"filePath": "source/main.go"}},
			})
		case 2:
			writeSyntheticOpenAIText(t, w, "Evidence inspected.")
		case 3:
			var structured any
			if err := json.Unmarshal(executorAnalysisJSON(), &structured); err != nil {
				t.Fatal(err)
			}
			writeSyntheticOpenAIStream(t, w, "StructuredOutput", structured)
		default:
			t.Fatalf("unexpected provider request %d", calls)
		}
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	spec := OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Gateway: engineruntime.ModelGatewayConfig{Endpoint: gateway.URL + "/v1/chat/completions", Model: "synthetic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Prompt:  "Read artifacts/failure.log, inspect source/main.go if relevant, and investigate the failure.", MaxSteps: 6,
		ModelContextTokens: 200000, ModelOutputTokens: 8192,
	}
	result, err := defaultRunOpenCode(ctx, spec)
	if err != nil {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if calls != 3 || len(toolSets) != 3 || slices.Contains(toolSets[0], "StructuredOutput") || slices.Contains(toolSets[1], "StructuredOutput") || !slices.Equal(toolSets[2], []string{"StructuredOutput"}) {
		t.Fatalf("calls=%d toolSets=%v", calls, toolSets)
	}
	finalShape := providerShapes[2]
	if finalShape.toolCount != result.Telemetry.RequestShape.ToolCount || finalShape.toolSchemaSHA256 != result.Telemetry.RequestShape.ToolSchemaSHA256 || finalShape.toolChoiceMode != result.Telemetry.RequestShape.ToolChoiceMode || finalShape.userPromptBytes != result.Telemetry.RequestShape.UserPromptBytes || finalShape.systemPromptBytes != result.Telemetry.RequestShape.SystemPromptBytes {
		t.Fatalf("provider shape=%+v telemetry=%+v", finalShape, result.Telemetry.RequestShape)
	}
	if result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.EvidencePhaseSteps != 2 || result.Telemetry.FinalizationPhaseSteps != 1 || result.Telemetry.StepsUsed != 3 || result.Usage.ModelRequests != 3 || !result.Telemetry.EvidencePhaseCompleted || !result.Telemetry.FinalizationPhaseCompleted || len(result.Structured) == 0 {
		t.Fatalf("result=%+v", result)
	}
}

type syntheticOpenAIToolCall struct {
	Name      string
	Arguments any
}

func writeSyntheticOpenAIStream(t *testing.T, w http.ResponseWriter, toolName string, arguments any) {
	t.Helper()
	writeSyntheticOpenAIToolCalls(t, w, []syntheticOpenAIToolCall{{Name: toolName, Arguments: arguments}})
}

func writeSyntheticOpenAIToolCalls(t *testing.T, w http.ResponseWriter, calls []syntheticOpenAIToolCall) {
	t.Helper()
	toolCalls := make([]any, 0, len(calls))
	for index, call := range calls {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		toolCalls = append(toolCalls, map[string]any{
			"index": index, "id": fmt.Sprintf("call-synthetic-%d", index), "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": string(args)},
		})
	}
	chunks := []any{
		map[string]any{
			"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "synthetic-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": toolCalls}, "finish_reason": nil}},
		},
		map[string]any{
			"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "synthetic-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
		},
	}
	writeSyntheticSSE(t, w, chunks)
}

func writeSyntheticOpenAIText(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	chunks := []any{
		map[string]any{
			"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "synthetic-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}, "finish_reason": nil}},
		},
		map[string]any{
			"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "created": 1, "model": "synthetic-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
		},
	}
	writeSyntheticSSE(t, w, chunks)
}

func writeSyntheticSSE(t *testing.T, w http.ResponseWriter, chunks []any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
