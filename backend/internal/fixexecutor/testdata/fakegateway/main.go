package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	model      = "fixture-model"
	targetFile = "/workspace/repository/README"
)

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/chat/completions", handleCompletion)
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func handleCompletion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	for _, name := range []string{"Authorization", "api-key", "x-api-key"} {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			http.Error(w, "credential headers are forbidden", http.StatusBadRequest)
			return
		}
	}
	defer r.Body.Close() //nolint:errcheck
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	var input request
	if err := decoder.Decode(&input); err != nil || input.Model != model {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	var system string
	toolResults := 0
	for _, value := range input.Messages {
		if value.Role == "system" {
			system += fmt.Sprint(value.Content)
		}
		if value.Role == "tool" {
			toolResults++
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	if strings.Contains(system, "title generator") {
		writeChunk(w, map[string]any{"role": "assistant", "content": "Agent Sandbox fixture"}, nil)
		writeChunk(w, map[string]any{}, "stop")
	} else if toolResults == 0 {
		writeToolCall(w, "call-read", "read", map[string]any{"filePath": targetFile})
	} else if toolResults == 1 {
		writeToolCall(w, "call-edit", "edit", map[string]any{
			"filePath": targetFile, "oldString": "Hello World!", "newString": "Hello Agent Sandbox!",
		})
	} else {
		writeChunk(w, map[string]any{"role": "assistant", "content": "Updated README through the credential-free fixture gateway."}, nil)
		writeChunk(w, map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeToolCall(w http.ResponseWriter, id, name string, arguments map[string]any) {
	data, _ := json.Marshal(arguments)
	writeChunk(w, map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(data)},
		}},
	}, nil)
	writeChunk(w, map[string]any{}, "tool_calls")
}

func writeChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	value := map[string]any{
		"id": "chatcmpl-fixture", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
