package analysisexecutor

import (
	"bytes"
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
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestOpenCode1182RequestShapeCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	requests := make(chan []byte, 4)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "api-key", "x-api-key"} {
			if strings.TrimSpace(r.Header.Get(name)) != "" {
				t.Error("direct unauthenticated Chat request carried a credential header")
			}
		}
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
		Provider: testOpenCodeProvider(gateway.URL+"/v1/chat/completions", "synthetic-model"),
		Prompt:   "Read artifacts/failure.log and return one structured result.", MaxSteps: 3,
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

func TestOpenCode1182DirectBearerCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	requests := make(chan struct{}, 1)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("direct bearer request did not carry the configured credential")
		}
		requests <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic bad request"}}`))
	}))
	defer providerServer.Close()
	workDir := t.TempDir()
	for _, dir := range []string{"source", "artifacts", "result"} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := defaultRunOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testDirectBearerProvider(providerServer.URL+"/v1/chat/completions", "synthetic-model"),
		Prompt:   "Return a structured result.", MaxSteps: 3, ModelContextTokens: 200000, ModelOutputTokens: 8192,
	})
	if err == nil {
		t.Fatal("synthetic provider failure unexpectedly succeeded")
	}
	select {
	case <-requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestOpenCode1182ResponsesRequestShapeCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	credential := strings.Repeat("fixture-responses-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	requests := make(chan []byte, 2)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("Responses request did not carry direct bearer authentication")
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("Responses path = %q", r.URL.Path)
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil || len(data) > maxOpenCodeAPIResponseBytes {
			t.Errorf("read Responses request: bytes=%d err=%v", len(data), err)
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		if bytes.Contains(data, []byte(credential)) {
			t.Error("Responses request body contained the provider credential")
		}
		requests <- data
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic bad request","code":"synthetic"}}`))
	}))
	defer providerServer.Close()
	workDir := t.TempDir()
	for _, dir := range []string{"source", "artifacts", "result"} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	spec := OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testResponsesProvider(providerServer.URL+"/v1/responses", "synthetic-model"),
		Prompt:   "Return a structured result.", MaxSteps: 3, ModelContextTokens: 200000, ModelOutputTokens: 8192,
	}
	result, err := defaultRunOpenCode(ctx, spec)
	if err == nil {
		t.Fatal("synthetic Responses HTTP 400 unexpectedly succeeded")
	}
	if result.Telemetry.Error.HTTPStatusCode != http.StatusBadRequest || result.Telemetry.Error.Classification != "api_bad_request" {
		t.Fatalf("err=%v telemetry=%+v", err, result.Telemetry)
	}
	var raw []byte
	select {
	case raw = <-requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	observed, err := parseSyntheticResponsesRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Model != "synthetic-model" || !observed.Stream || observed.Store == nil || *observed.Store || observed.HasPreviousResponseID || len(observed.Tools) == 0 || observed.MaxOutputTokens != 8192 {
		t.Fatalf("Responses request shape = %+v", observed)
	}
}

type syntheticResponsesRequest struct {
	Model                 string
	Stream                bool
	Store                 *bool
	HasPreviousResponseID bool
	Instructions          string
	Input                 []map[string]any
	Tools                 []map[string]any
	ToolChoice            any
	MaxOutputTokens       int
}

func parseSyntheticResponsesRequest(raw []byte) (syntheticResponsesRequest, error) {
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		return syntheticResponsesRequest{}, fmt.Errorf("decode Responses request: %w", err)
	}
	result := syntheticResponsesRequest{
		Model:                 fmt.Sprint(wire["model"]),
		Stream:                wire["stream"] == true,
		HasPreviousResponseID: wire["previous_response_id"] != nil,
		Instructions:          fmt.Sprint(wire["instructions"]),
		ToolChoice:            wire["tool_choice"],
	}
	if value, ok := wire["store"].(bool); ok {
		result.Store = &value
	}
	if value, ok := wire["max_output_tokens"].(float64); ok {
		result.MaxOutputTokens = int(value)
	}
	if values, ok := wire["input"].([]any); ok {
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				result.Input = append(result.Input, item)
			}
		}
	}
	if values, ok := wire["tools"].([]any); ok {
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				result.Tools = append(result.Tools, item)
			}
		}
	}
	return result, nil
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
	roles             []string
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
		shape.roles = append(shape.roles, message.Role)
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
		for _, name := range []string{"Authorization", "api-key", "x-api-key"} {
			if strings.TrimSpace(r.Header.Get(name)) != "" {
				t.Fatal("gateway Chat request carried a provider credential header")
			}
		}
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
			if err := json.Unmarshal(compatibilityAnalysisJSON(), &structured); err != nil {
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
		Provider: testGatewayProvider(gateway.URL+"/v1/chat/completions", "synthetic-model"),
		Prompt:   "Read artifacts/failure.log, inspect source/main.go if relevant, and investigate the failure.", MaxSteps: 6,
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
	if len(finalShape.roles) == 0 || finalShape.roles[len(finalShape.roles)-1] != "user" {
		t.Fatalf("final provider roles=%v", finalShape.roles)
	}
	if finalShape.toolCount != result.Telemetry.RequestShape.ToolCount || finalShape.toolSchemaSHA256 != result.Telemetry.RequestShape.ToolSchemaSHA256 || finalShape.toolChoiceMode != result.Telemetry.RequestShape.ToolChoiceMode || finalShape.userPromptBytes != result.Telemetry.RequestShape.UserPromptBytes || finalShape.systemPromptBytes != result.Telemetry.RequestShape.SystemPromptBytes {
		t.Fatalf("provider shape=%+v telemetry=%+v", finalShape, result.Telemetry.RequestShape)
	}
	if result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.EvidencePhaseSteps != 2 || result.Telemetry.FinalizationPhaseSteps != 1 || result.Telemetry.StepsUsed != 3 || result.Usage.ModelRequests != 3 || !result.Telemetry.EvidencePhaseCompleted || !result.Telemetry.FinalizationPhaseCompleted || len(result.Structured) == 0 {
		t.Fatalf("result=%+v", result)
	}
	wantHandles := []agentanalysis.WorkspaceEvidenceHandle{
		{ID: "artifact-001", Root: agentanalysis.WorkspaceArtifactsDir, Path: "failure.log", LineStart: 1, LineEnd: 1},
		{ID: "source-001", Root: agentanalysis.WorkspaceSourceDir, Path: "main.go", LineStart: 1, LineEnd: 1},
	}
	if !slices.Equal(result.EvidenceHandles, wantHandles) {
		t.Fatalf("evidence handles=%+v want=%+v", result.EvidenceHandles, wantHandles)
	}
	assertCompatibilityAnalysis(t, result, filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir), filepath.Join(workDir, agentanalysis.WorkspaceSourceDir))
}

func TestOpenCode1182RequiredSourceCorrectionCompatibility(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(workDir, "artifacts", "failure.log"), []byte("synthetic failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "source", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var toolSets [][]string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil {
			t.Fatal(err)
		}
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
			writeSyntheticOpenAIStream(t, w, "read", map[string]any{"filePath": "artifacts/failure.log"})
		case 2:
			writeSyntheticOpenAIText(t, w, "Artifact evidence inspected.")
		case 3:
			writeSyntheticOpenAIStream(t, w, "read", map[string]any{"filePath": "source/main.go"})
		case 4:
			writeSyntheticOpenAIText(t, w, "Source evidence inspected.")
		case 5:
			var structured any
			if err := json.Unmarshal(compatibilityAnalysisJSON(), &structured); err != nil {
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
		Provider:              testGatewayProvider(gateway.URL+"/v1/chat/completions", "synthetic-model"),
		Prompt:                "Read artifacts/failure.log and investigate the failure.",
		MaxSteps:              8,
		ModelContextTokens:    200000,
		ModelOutputTokens:     8192,
		RequireSourceEvidence: true,
	}
	result, err := defaultRunOpenCode(ctx, spec)
	if err != nil {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if calls != 5 || len(toolSets) != 5 || slices.Contains(toolSets[0], "StructuredOutput") || slices.Contains(toolSets[1], "StructuredOutput") || slices.Contains(toolSets[2], "StructuredOutput") || slices.Contains(toolSets[3], "StructuredOutput") || !slices.Equal(toolSets[4], []string{"StructuredOutput"}) {
		t.Fatalf("calls=%d toolSets=%v", calls, toolSets)
	}
	if result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceEvidenceAccepted || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.SourceEvidenceCorrectionReason != agentanalysis.WorkspaceSourceToolSkipped || result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.EvidencePhaseSteps != 4 || result.Telemetry.FinalizationPhaseSteps != 1 || result.Telemetry.StepsUsed != 5 || result.Usage.ModelRequests != 5 {
		t.Fatalf("result=%+v", result)
	}
	assertCompatibilityAnalysis(t, result, filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir), filepath.Join(workDir, agentanalysis.WorkspaceSourceDir))
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

func TestOpenCode1182ResponsesTwoPhaseCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	credential := strings.Repeat("fixture-responses-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	workDir := t.TempDir()
	for _, dir := range []string{"source", "artifacts", "result"} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "artifacts", "failure.log"), []byte("synthetic failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "source", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var requests []syntheticResponsesRequest
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("Responses path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Fatal("Responses request did not carry direct bearer authentication")
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil || len(data) > maxOpenCodeAPIResponseBytes {
			t.Fatalf("read Responses request: bytes=%d err=%v", len(data), err)
		}
		if bytes.Contains(data, []byte(credential)) {
			t.Fatal("Responses request body contained the provider credential")
		}
		request, err := parseSyntheticResponsesRequest(data)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		calls++
		switch calls {
		case 1:
			writeSyntheticResponsesToolCalls(t, w, []syntheticResponsesToolCall{
				{Name: "read", Arguments: map[string]any{"filePath": "artifacts/failure.log"}},
				{Name: "read", Arguments: map[string]any{"filePath": "source/main.go"}},
			}, true)
		case 2:
			writeSyntheticResponsesText(t, w, "Evidence inspected.", true)
		case 3:
			var structured any
			if err := json.Unmarshal(compatibilityAnalysisJSON(), &structured); err != nil {
				t.Fatal(err)
			}
			writeSyntheticResponsesToolCalls(t, w, []syntheticResponsesToolCall{{Name: "StructuredOutput", Arguments: structured}}, true)
		default:
			t.Fatalf("unexpected Responses request %d", calls)
		}
	}))
	defer providerServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	result, err := defaultRunOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testResponsesProvider(providerServer.URL+"/v1/responses", "synthetic-model"),
		Prompt:   "Read artifacts/failure.log, inspect source/main.go if relevant, and investigate the failure.", MaxSteps: 6,
		ModelContextTokens: 200000, ModelOutputTokens: 8192,
	})
	if err != nil {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if calls != 3 || len(requests) != 3 {
		t.Fatalf("calls=%d requests=%d", calls, len(requests))
	}
	for index, request := range requests {
		if request.Store == nil || *request.Store || request.HasPreviousResponseID || !request.Stream {
			t.Fatalf("request %d stateful Responses fields = %+v", index+1, request)
		}
	}
	if responseInputTypeCount(requests[0], "function_call_output") != 0 || responseInputTypeCount(requests[1], "function_call_output") < 2 || responseInputTypeCount(requests[2], "function_call_output") < 2 {
		t.Fatalf("multi-turn Responses history was not preserved: %#v", requests)
	}
	if slices.Contains(responseToolNames(requests[0]), "StructuredOutput") || slices.Contains(responseToolNames(requests[1]), "StructuredOutput") || !slices.Equal(responseToolNames(requests[2]), []string{"StructuredOutput"}) {
		t.Fatalf("Responses tool sets = %v %v %v", responseToolNames(requests[0]), responseToolNames(requests[1]), responseToolNames(requests[2]))
	}
	if role := responseLastMessageRole(requests[2]); role != "user" {
		t.Fatalf("final Responses message role = %q sequence=%v", role, responseInputRoleSequence(requests[2]))
	}
	if result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Usage.ModelRequests != 3 || !result.Usage.Available || result.Usage.InputTokens != 24 || result.Usage.CachedInputTokens != 6 || result.Usage.OutputTokens != 6 || !result.Telemetry.EvidencePhaseCompleted || !result.Telemetry.FinalizationPhaseCompleted || len(result.Structured) == 0 {
		t.Fatalf("Responses result=%+v", result)
	}
	wantHandles := []agentanalysis.WorkspaceEvidenceHandle{
		{ID: "artifact-001", Root: agentanalysis.WorkspaceArtifactsDir, Path: "failure.log", LineStart: 1, LineEnd: 1},
		{ID: "source-001", Root: agentanalysis.WorkspaceSourceDir, Path: "main.go", LineStart: 1, LineEnd: 1},
	}
	if !slices.Equal(result.EvidenceHandles, wantHandles) {
		t.Fatalf("Responses evidence handles=%+v want=%+v", result.EvidenceHandles, wantHandles)
	}
	assertCompatibilityAnalysis(t, result, filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir), filepath.Join(workDir, agentanalysis.WorkspaceSourceDir))
}

type syntheticResponsesToolCall struct {
	Name      string
	Arguments any
}

func responseToolNames(request syntheticResponsesRequest) []string {
	var names []string
	for _, tool := range request.Tools {
		if name, ok := tool["name"].(string); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func responseInputRoleSequence(request syntheticResponsesRequest) []string {
	result := make([]string, 0, len(request.Input))
	for _, item := range request.Input {
		result = append(result, fmt.Sprintf("%v:%v", item["type"], item["role"]))
	}
	return result
}

func responseLastMessageRole(request syntheticResponsesRequest) string {
	for index := len(request.Input) - 1; index >= 0; index-- {
		if role, ok := request.Input[index]["role"].(string); ok && role != "" {
			return role
		}
	}
	return ""
}

func responseInputTypeCount(request syntheticResponsesRequest, want string) int {
	count := 0
	for _, item := range request.Input {
		if item["type"] == want {
			count++
		}
	}
	return count
}

func writeSyntheticResponsesText(t *testing.T, w http.ResponseWriter, text string, usage bool) {
	t.Helper()
	writeSyntheticResponsesSSE(t, w, []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-1", "phase": "final_answer"}},
		map[string]any{"type": "response.output_text.delta", "item_id": "msg-1", "delta": text, "logprobs": []any{}},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-1", "phase": "final_answer"}},
		responsesCompletedEvent("resp-text", usage),
	})
}

func writeSyntheticResponsesToolCalls(t *testing.T, w http.ResponseWriter, calls []syntheticResponsesToolCall, usage bool) {
	t.Helper()
	var events []any
	for index, call := range calls {
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		itemID := fmt.Sprintf("fc-%d", index)
		callID := fmt.Sprintf("call-%d", index)
		item := map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": call.Name, "arguments": string(arguments), "status": "completed"}
		events = append(events,
			map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": call.Name, "arguments": ""}},
			map[string]any{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": index, "delta": string(arguments)},
			map[string]any{"type": "response.output_item.done", "output_index": index, "item": item},
		)
	}
	events = append(events, responsesCompletedEvent("resp-tools", usage))
	writeSyntheticResponsesSSE(t, w, events)
}

func responsesCompletedEvent(id string, usage bool) map[string]any {
	response := map[string]any{"id": id, "status": "completed"}
	if usage {
		response["usage"] = map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 2},
			"output_tokens": 3, "output_tokens_details": map[string]any{"reasoning_tokens": 1}, "total_tokens": 13,
		}
	}
	return map[string]any{"type": "response.completed", "response": response}
}

func writeSyntheticResponsesSSE(t *testing.T, w http.ResponseWriter, events []any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
}

func TestOpenCode1182ResponsesFailureModesAreSanitized(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	credential := strings.Repeat("fixture-responses-credential-", 2)
	for _, tc := range []struct {
		name     string
		wantCode string
		write    func(*testing.T, http.ResponseWriter)
	}{
		{name: "missing usage", wantCode: agentanalysis.WorkspaceEvidenceArtifactHandlesMissing, write: func(t *testing.T, w http.ResponseWriter) { writeSyntheticResponsesText(t, w, "No usage.", false) }},
		{name: "malformed event", wantCode: "serialization", write: func(_ *testing.T, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {not-json}\n\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(modelprovider.TokenEnv, credential)
			providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+credential {
					t.Error("Responses failure request did not carry direct bearer authentication")
				}
				tc.write(t, w)
			}))
			defer providerServer.Close()
			workDir := t.TempDir()
			for _, dir := range []string{"source", "artifacts", "result"} {
				if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			result, err := defaultRunOpenCode(ctx, OpenCodeSpec{
				Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
				Provider: testResponsesProvider(providerServer.URL+"/v1/responses", "synthetic-model"),
				Prompt:   "Investigate.", MaxSteps: 3, ModelContextTokens: 200000, ModelOutputTokens: 8192,
			})
			if err == nil || result.Usage.Available || result.Usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || result.Telemetry.FailureCode != tc.wantCode {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, forbidden := range []string{credential, "No usage.", "not-json"} {
				if strings.Contains(err.Error(), forbidden) || bytes.Contains(encoded, []byte(forbidden)) {
					t.Fatal("Responses failure diagnostics retained provider content")
				}
			}
		})
	}
}

func compatibilityAnalysisJSON() []byte {
	return []byte(`{
  "version": 1,
  "contract_version": "agent-analysis-workspace-v8",
  "summary": "The synthetic failure is grounded.",
  "is_transient": false,
  "root_cause": "The artifact and source contain the same synthetic marker.",
  "severity": "Low",
  "suggested_fix": "",
  "relevant_file_ids": ["source-001"],
  "artifact_evidence_ids": ["artifact-001"],
  "source_evidence_ids": ["source-001"],
  "unresolved_details": []
}`)
}

func assertCompatibilityAnalysis(t *testing.T, result OpenCodeRunResult, artifactRoot, sourceRoot string) {
	t.Helper()
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(
		ai.FailureAnalysisRequest{JobID: "periodic::synthetic", BuildPrefix: "logs/synthetic/1/", Build: models.BuildInfo{BuildID: "1", JobName: "synthetic"}, TestCase: models.TestCase{Name: "TestSynthetic", Status: "failed", FailureMessage: "synthetic failure"}},
		sourceinvestigation.Repository{Owner: "synthetic", Name: "workspace", Revision: strings.Repeat("a", 40)},
		"Inspect the synthetic evidence.", files,
	)
	if err != nil {
		t.Fatal(err)
	}
	analysis, validation, err := agentanalysis.ParseWorkspaceAnalysis(string(result.Structured), result.EvidenceHandles, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != agentanalysis.WorkspaceResultAccepted || analysis.SuggestedFix != "" || len(analysis.EvidenceCitations) != 1 || analysis.EvidenceCitations[0].Path != "failure.log" || analysis.EvidenceCitations[0].Quote != "synthetic failure" || len(analysis.SourceCitations) != 1 || analysis.SourceCitations[0].Path != "main.go" || analysis.SourceCitations[0].Quote != "package main" {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestOpenCode1182ReasoningEffortWireCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	for _, tc := range []struct {
		name   string
		api    string
		path   string
		effort modelprovider.ReasoningEffort
	}{
		{name: "chat_completions/high", api: modelprovider.APIChatCompletions, path: "/v1/chat/completions", effort: modelprovider.ReasoningEffortHigh},
		{name: "chat_completions/empty", api: modelprovider.APIChatCompletions, path: "/v1/chat/completions"},
		{name: "responses/high", api: modelprovider.APIResponses, path: "/v1/responses", effort: modelprovider.ReasoningEffortHigh},
		{name: "responses/empty", api: modelprovider.APIResponses, path: "/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := "fixture-reasoning-effort-token"
			t.Setenv(modelprovider.TokenEnv, credential)
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("provider path = %q, want %q", r.URL.Path, tc.path)
				}
				var request map[string]any
				if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
					t.Errorf("decode provider request: %v", err)
				}
				requests <- request
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":{"message":"synthetic stop"}}`, http.StatusBadRequest)
			}))
			defer server.Close()

			workDir := t.TempDir()
			for _, dir := range []string{"source", "artifacts", "result"} {
				if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			provider := modelprovider.Normalize(modelprovider.Config{
				CredentialMode: modelprovider.CredentialModeDirect, API: tc.api,
				Endpoint: server.URL + tc.path, Model: "synthetic-model", ReasoningEffort: tc.effort,
				Auth: modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
			})
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			_, _ = defaultRunOpenCode(ctx, OpenCodeSpec{
				Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(), Provider: provider,
				Prompt: "Inspect the fixture.", MaxSteps: 3, ModelContextTokens: 200000, ModelOutputTokens: 8192,
			})

			select {
			case request := <-requests:
				reasoningEffort, hasReasoningEffort := request["reasoning_effort"]
				reasoning, hasReasoning := request["reasoning"]
				if tc.effort == "" {
					if hasReasoningEffort || hasReasoning {
						t.Fatalf("empty effort added reasoning fields: %#v", request)
					}
					return
				}
				if tc.api == modelprovider.APIResponses {
					reasoningObject, ok := reasoning.(map[string]any)
					if !hasReasoning || !ok || reasoningObject["effort"] != string(tc.effort) || hasReasoningEffort {
						t.Fatalf("Responses reasoning fields = %#v", request)
					}
				} else if !hasReasoningEffort || reasoningEffort != string(tc.effort) || hasReasoning {
					t.Fatalf("Chat reasoning fields = %#v", request)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
