package fixexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
)

func TestOpenCode1182FixResponsesCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact OpenCode 1.18.2 executable")
	}
	credential := strings.Repeat("fixture-fix-responses-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	workDir := t.TempDir()
	readme := filepath.Join(workDir, "README")
	if err := os.WriteFile(readme, []byte("Hello World!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "init", "-q")
	runGit(t, workDir, "config", "commit.gpgsign", "false")
	runGit(t, workDir, "config", "user.name", "Fixture")
	runGit(t, workDir, "config", "user.email", "fixture@example.test")
	runGit(t, workDir, "add", "README")
	runGit(t, workDir, "commit", "-qm", "fixture")
	requests := 0
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("Responses path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Fatal("Fix Responses request did not carry direct bearer authentication")
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(credential)) {
			t.Fatal("Fix Responses request body contained the provider credential")
		}
		var request map[string]any
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true || request["store"] != false || request["previous_response_id"] != nil {
			t.Fatalf("stateful Fix Responses request = %v", request)
		}
		requests++
		toolNames := fixResponsesToolNames(request)
		toolOutputs := fixResponsesInputTypeCount(request, "function_call_output")
		switch {
		case len(toolNames) == 0:
			writeFixResponsesText(t, w, "Fixture title")
		case toolOutputs == 0:
			writeFixResponsesToolCall(t, w, "read", map[string]any{"filePath": "README"})
		case toolOutputs == 1:
			writeFixResponsesToolCall(t, w, "edit", map[string]any{"filePath": "README", "oldString": "Hello World!", "newString": "Hello Responses!"})
		default:
			writeFixResponsesText(t, w, "Updated README through Responses.")
		}
	}))
	defer providerServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	stdout, stderr, err := defaultRunOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testResponsesProvider(providerServer.URL+"/v1/responses", "synthetic-model"),
		Prompt:   "Read README and replace Hello World with Hello Responses.", MaxSteps: 6, OutputLimit: 128 << 10,
	})
	if err != nil {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	content, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Hello Responses!\n" || requests < 3 {
		t.Fatalf("content=%q requests=%d", content, requests)
	}
	if strings.Contains(stdout, credential) || strings.Contains(stderr, credential) {
		t.Fatal("Fix Responses output retained the provider credential")
	}
}

func fixResponsesToolNames(request map[string]any) []string {
	var names []string
	values, _ := request["tools"].([]any)
	for _, raw := range values {
		if tool, ok := raw.(map[string]any); ok {
			if name, ok := tool["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

func fixResponsesInputTypeCount(request map[string]any, want string) int {
	count := 0
	values, _ := request["input"].([]any)
	for _, raw := range values {
		if item, ok := raw.(map[string]any); ok && item["type"] == want {
			count++
		}
	}
	return count
}

func writeFixResponsesToolCall(t *testing.T, w http.ResponseWriter, name string, arguments any) {
	t.Helper()
	data, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"type": "function_call", "id": "fc-0", "call_id": "call-0", "name": name, "arguments": string(data), "status": "completed"}
	writeFixResponsesSSE(t, w, []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc-0", "call_id": "call-0", "name": name, "arguments": ""}},
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc-0", "output_index": 0, "delta": string(data)},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		fixResponsesCompleted(),
	})
}

func writeFixResponsesText(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	writeFixResponsesSSE(t, w, []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-0", "phase": "final_answer"}},
		map[string]any{"type": "response.output_text.delta", "item_id": "msg-0", "delta": text, "logprobs": []any{}},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-0", "phase": "final_answer"}},
		fixResponsesCompleted(),
	})
}

func fixResponsesCompleted() map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{
		"usage": map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 2},
			"output_tokens": 3, "output_tokens_details": map[string]any{"reasoning_tokens": 1},
		},
	}}
}

func writeFixResponsesSSE(t *testing.T, w http.ResponseWriter, events []any) {
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
