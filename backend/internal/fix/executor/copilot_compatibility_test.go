package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

type copilotFixtureRequest struct {
	path    string
	body    map[string]any
	headers http.Header
}

func TestOpenCode1182CopilotCompatibility(t *testing.T) {
	bin := openCode1182Binary(t)
	const credential = "synthetic-copilot-credential-for-tests"
	for _, tc := range []struct {
		name, api, endpoint, model, path string
		noEnv, genericSDK                bool
	}{
		{"Copilot Responses", modelprovider.APIResponses, "https://api.githubcopilot.com/responses", "gpt-6-sol", "/responses", false, false},
		{"Copilot Claude chat", modelprovider.APIChatCompletions, "https://api.githubcopilot.com/chat/completions", "claude-sonnet-5", "/chat/completions", false, false},
		{"missing env routes to chat", modelprovider.APIResponses, "https://api.githubcopilot.com/responses", "gpt-6-sol", "/chat/completions", true, false},
		{"generic SDK crashes on rotated IDs", modelprovider.APIResponses, "https://api.githubcopilot.com/responses", "gpt-6-sol", "/responses", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(modelprovider.TokenEnv, credential)
			work, readme := fakeCopilotRepository(t)
			var mu sync.Mutex
			var requests []copilotFixtureRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
					http.Error(w, "invalid request body", http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, copilotFixtureRequest{r.URL.Path, body, r.Header.Clone()})
				mu.Unlock()
				switch r.URL.Path {
				case "/responses":
					writeCopilotResponses(w, body, !tc.genericSDK)
				case "/chat/completions":
					if tc.noEnv {
						http.Error(w, `{"error":{"message":"unsupported API for model"}}`, http.StatusBadRequest)
						return
					}
					writeCopilotChat(w, body)
				default:
					http.Error(w, "unexpected operation", http.StatusNotFound)
				}
			}))
			defer server.Close()

			provider := modelprovider.Normalize(modelprovider.Config{
				API: tc.api, Endpoint: tc.endpoint, Model: tc.model,
				Auth: modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
			})
			adapter, err := modelprovider.OpenCodeAdapterFor(provider)
			if err != nil {
				t.Fatal(err)
			}
			adapter.BaseURL = server.URL
			if tc.genericSDK {
				adapter.ProviderID = modelprovider.OpenCodeCustomProviderID
				adapter.NPM = "@ai-sdk/openai"
			}
			spec := OpenCodeSpec{
				Bin: bin, WorkDir: work, HomeDir: t.TempDir(), TempDir: t.TempDir(),
				Provider: provider, Prompt: "Read README and replace Hello World with Hello Copilot.",
				MaxSteps: 6, OutputLimit: 128 << 10,
			}
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			var stdout, stderr string
			if tc.noEnv {
				stdout, stderr, err = runCopilotWithoutEnv(ctx, spec, adapter)
			} else {
				stdout, stderr, err = runOpenCodeAdapter(ctx, spec, adapter)
			}
			mu.Lock()
			received := append([]copilotFixtureRequest(nil), requests...)
			mu.Unlock()
			if len(received) == 0 {
				t.Fatalf("no requests; err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			for _, request := range received {
				if request.path != tc.path {
					t.Fatalf("path = %s, want %s; err=%v stdout=%q stderr=%q", request.path, tc.path, err, stdout, stderr)
				}
				if request.body["model"] != tc.model {
					t.Fatalf("request selected a different model: %v", request.body["model"])
				}
				if request.headers.Get("Authorization") != "Bearer "+credential ||
					request.headers.Get(modelprovider.CopilotIntegrationHeader) != modelprovider.CopilotIntegrationID {
					t.Fatalf("missing fixture auth or integration header: %v", request.headers)
				}
				if tc.path == "/responses" && !tc.genericSDK {
					if _, ok := request.body["max_output_tokens"]; ok {
						t.Fatalf("Copilot Responses retained a hardcoded output limit: %v", request.body)
					}
					if request.body["store"] != false {
						t.Fatalf("Copilot Responses used stateful storage: %v", request.body)
					}
				}
			}
			content, errRead := os.ReadFile(readme)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if tc.noEnv {
				if err == nil || string(content) != "Hello World!\n" {
					t.Fatalf("missing env unexpectedly succeeded: err=%v content=%q stdout=%q stderr=%q", err, content, stdout, stderr)
				}
			} else if tc.genericSDK {
				if err == nil || !strings.Contains(stdout+stderr, "summaryParts") || string(content) != "Hello World!\n" {
					t.Fatalf("stock SDK did not reproduce crash: err=%v content=%q stdout=%q stderr=%q", err, content, stdout, stderr)
				}
			} else if err != nil || len(received) != 3 || string(content) != "Hello Copilot!\n" {
				t.Fatalf("Copilot run did not read, edit, and finish: err=%v requests=%d content=%q stdout=%q stderr=%q", err, len(received), content, stdout, stderr)
			}
		})
	}
}

func runCopilotWithoutEnv(ctx context.Context, spec OpenCodeSpec, adapter modelprovider.OpenCodeAdapter) (string, string, error) {
	if err := writeOpenCodeConfig(spec.HomeDir, spec.Provider, adapter, spec.MaxSteps); err != nil {
		return "", "", err
	}
	path := filepath.Join(spec.HomeDir, ".config", "opencode", "opencode.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", err
	}
	delete(config["provider"].(map[string]any)[adapter.ProviderID].(map[string]any), "env")
	data, err = json.Marshal(config)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", "", err
	}
	env, err := openCodeEnvironment(spec.HomeDir, spec.TempDir, spec.Provider)
	if err != nil {
		return "", "", err
	}
	guard, err := modelprovider.NewCredentialGuard(spec.Provider, os.LookupEnv)
	if err != nil {
		return "", "", err
	}
	return runOpenCodeCommand(ctx, spec.WorkDir, env, guard, maxCapturedStream, spec.MaxSteps,
		spec.Bin, "run", "--dir", spec.WorkDir, "--format", "json", "--agent", "build",
		"--model", adapter.ProviderID+"/"+spec.Provider.Model, spec.Prompt)
}

func fakeCopilotRepository(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	readme := filepath.Join(work, "README")
	if err := os.WriteFile(readme, []byte("Hello World!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-q")
	runGit(t, work, "config", "commit.gpgsign", "false")
	runGit(t, work, "config", "user.name", "Fixture")
	runGit(t, work, "config", "user.email", "fixture@example.test")
	runGit(t, work, "add", "README")
	runGit(t, work, "commit", "-qm", "fixture")
	return work, readme
}

func writeCopilotResponses(w http.ResponseWriter, body map[string]any, includeSummary bool) {
	outputs := fixResponsesInputTypeCount(body, "function_call_output")
	model, _ := body["model"].(string)
	events := []any{
		map[string]any{"type": "response.created", "response": map[string]any{"id": "response_fixture", "model": model}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "reasoning_added", "summary": []any{}, "encrypted_content": "encrypted"}},
	}
	summary := []any{}
	if includeSummary {
		summary = []any{map[string]any{"type": "summary_text", "text": "Reading README."}}
		events = append(events,
			map[string]any{"type": "response.reasoning_summary_part.added", "item_id": "reasoning_summary_rotated", "output_index": 0, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}},
			map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "reasoning_summary_rotated", "output_index": 0, "summary_index": 0, "delta": "Reading README."},
			map[string]any{"type": "response.reasoning_summary_part.done", "item_id": "reasoning_summary_rotated", "output_index": 0, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": "Reading README."}},
		)
	}
	events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "reasoning_done", "summary": summary, "encrypted_content": "encrypted"}})
	switch outputs {
	case 0:
		events = append(events, copilotToolCall(1, "read", map[string]any{"filePath": "README"})...)
	case 1:
		events = append(events, copilotToolCall(1, "apply_patch", map[string]any{"patchText": "*** Begin Patch\n*** Update File: README\n@@\n-Hello World!\n+Hello Copilot!\n*** End Patch"})...)
	default:
		events = append(events,
			map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "message_added", "role": "assistant", "content": []any{}}},
			map[string]any{"type": "response.output_text.delta", "item_id": "message_rotated", "output_index": 1, "content_index": 0, "delta": "Updated README.", "logprobs": []any{}},
			map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "message", "id": "message_done", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Updated README.", "annotations": []any{}}}}},
		)
	}
	events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "response_fixture", "model": model,
		"usage": map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 3, "output_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	}})
	writeCopilotSSE(w, events, false)
}

func copilotToolCall(index int, name string, arguments any) []any {
	data, _ := json.Marshal(arguments)
	callID := "call_" + name
	return []any{
		map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "function_call", "id": "fc_" + name, "call_id": callID, "name": name, "arguments": ""}},
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_" + name, "output_index": index, "delta": string(data)},
		map[string]any{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"type": "function_call", "id": "fc_" + name, "call_id": callID, "name": name, "arguments": string(data), "status": "completed"}},
	}
}

func writeCopilotChat(w http.ResponseWriter, body map[string]any) {
	toolReplies := 0
	for _, value := range body["messages"].([]any) {
		if message, ok := value.(map[string]any); ok && message["role"] == "tool" {
			toolReplies++
		}
	}
	model, _ := body["model"].(string)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chat_fixture", "object": "chat.completion.chunk", "created": 1790000000, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	call := func(name string, arguments any) []any {
		data, _ := json.Marshal(arguments)
		return []any{
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_" + name, "type": "function", "function": map[string]any{"name": name, "arguments": string(data)}}}}, nil),
			chunk(map[string]any{}, "tool_calls"),
		}
	}
	var events []any
	switch toolReplies {
	case 0:
		events = call("read", map[string]any{"filePath": "README"})
	case 1:
		events = call("edit", map[string]any{"filePath": "README", "oldString": "Hello World!", "newString": "Hello Copilot!"})
	default:
		events = []any{chunk(map[string]any{"role": "assistant", "content": "Updated README."}, nil), chunk(map[string]any{}, "stop")}
	}
	events[len(events)-1].(map[string]any)["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	writeCopilotSSE(w, events, true)
}

func writeCopilotSSE(w http.ResponseWriter, events []any, done bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if done {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}
