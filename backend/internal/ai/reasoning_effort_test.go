package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

func TestReasoningEffortRequestBodies(t *testing.T) {
	allowed := []ReasoningEffort{"", ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax}
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		for _, effort := range allowed {
			t.Run(apiMode+"/"+effortTestName(effort), func(t *testing.T) {
				shrinkCallDelay(t)
				var body []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ = io.ReadAll(r.Body)
					writeReasoningTestFinal(w, apiMode, "ok")
				}))
				defer server.Close()

				client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m", ReasoningEffort: effort})
				if _, err := client.Complete(context.Background(), "system", "user"); err != nil {
					t.Fatal(err)
				}
				want := expectedReasoningRequestBody(apiMode, effort)
				if string(body) != want {
					t.Fatalf("request body = %s\nwant         = %s", body, want)
				}
			})
		}
	}
}

func TestReasoningEffortNormalizesBeforeRequest(t *testing.T) {
	shrinkCallDelay(t)
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		writeReasoningTestFinal(w, APIChatCompletions, "ok")
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{Endpoint: server.URL, Model: "m", ReasoningEffort: "  HiGh  "})
	if _, err := client.Complete(context.Background(), "system", "user"); err != nil {
		t.Fatal(err)
	}
	if got := request["reasoning_effort"]; got != "high" || client.ReasoningEffort() != ReasoningEffortHigh {
		t.Fatalf("request effort = %#v, client effort = %q", got, client.ReasoningEffort())
	}
}

func TestReasoningEffortRejectsInvalidBeforeTransport(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			shrinkCallDelay(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				requests++
			}))
			defer server.Close()

			client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m", ReasoningEffort: "ultra"})
			if _, err := client.Complete(context.Background(), "system", "user"); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
				t.Fatalf("error = %v", err)
			}
			if requests != 0 {
				t.Fatalf("provider requests = %d, want 0", requests)
			}
		})
	}
}

func TestReasoningEffortStructuredFallbackRetainsEffort(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			shrinkCallDelay(t)
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, request)
				if len(requests) == 1 {
					http.Error(w, "schema unsupported", http.StatusBadRequest)
					return
				}
				writeReasoningTestToolCall(w, apiMode, "submit_body", `{"body":"safe"}`)
			}))
			defer server.Close()

			format := structuredBodyFormat()
			format.Name = "submit_body"
			client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m", ReasoningEffort: ReasoningEffortHigh})
			if err := client.CompleteStructured(context.Background(), "system", "user", format, bodyValidator("safe")); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(requests))
			}
			for index, request := range requests {
				assertWireReasoningEffort(t, apiMode, request, "high")
				if apiMode == APIResponses {
					if _, ok := request["include"]; ok {
						t.Fatalf("request %d included encrypted reasoning despite OmitReasoning: %#v", index, request)
					}
				}
			}
			if _, ok := requests[0][reasoningStructuredField(apiMode)]; !ok {
				t.Fatalf("first request did not use structured response format: %#v", requests[0])
			}
			if _, ok := requests[1]["tool_choice"]; !ok {
				t.Fatalf("fallback request did not force a tool: %#v", requests[1])
			}
		})
	}
}

func TestReasoningEffortToolLoopRetainsEffort(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			shrinkCallDelay(t)
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, request)
				if len(requests) == 1 {
					writeReasoningTestToolCall(w, apiMode, "echo", `{"msg":"hi"}`)
					return
				}
				writeReasoningTestFinal(w, apiMode, "done")
			}))
			defer server.Close()

			registry := tools.NewRegistry()
			registry.Register(&stubTool{})
			client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m", ReasoningEffort: ReasoningEffortXHigh})
			if _, err := client.ToolLoop(context.Background(), "system", "user", registry, []string{"echo"}, &tools.Env{}, ToolLoopOptions{MaxIters: 2}); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(requests))
			}
			for _, request := range requests {
				assertWireReasoningEffort(t, apiMode, request, "xhigh")
			}
		})
	}
}

func TestReasoningEffortRetriesRetainExactBody(t *testing.T) {
	for _, apiMode := range []string{APIChatCompletions, APIResponses} {
		t.Run(apiMode, func(t *testing.T) {
			shrinkCallDelay(t)
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(body))
				if len(bodies) == 1 {
					w.Header().Set("Retry-After", "0")
					http.Error(w, "rate limited", http.StatusTooManyRequests)
					return
				}
				writeReasoningTestFinal(w, apiMode, "ok")
			}))
			defer server.Close()

			client := NewClientWithOptions(Options{API: apiMode, Endpoint: server.URL, Model: "m", ReasoningEffort: ReasoningEffortMax})
			if _, err := client.Complete(context.Background(), "system", "user"); err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatalf("retry bodies = %#v", bodies)
			}
			var request map[string]any
			if err := json.Unmarshal([]byte(bodies[0]), &request); err != nil {
				t.Fatal(err)
			}
			assertWireReasoningEffort(t, apiMode, request, "max")
		})
	}
}

func TestResponsesReasoningEffortIndependentFromEncryptedReasoning(t *testing.T) {
	shrinkCallDelay(t)
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		writeReasoningTestFinal(w, APIResponses, `{"body":"safe"}`)
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL, Model: "m", ReasoningEffort: ReasoningEffortMedium})
	if _, err := client.Complete(context.Background(), "system", "user"); err != nil {
		t.Fatal(err)
	}
	if err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		assertWireReasoningEffort(t, APIResponses, request, "medium")
	}
	if _, ok := requests[0]["include"]; !ok {
		t.Fatalf("ordinary request omitted encrypted reasoning inclusion: %#v", requests[0])
	}
	if _, ok := requests[1]["include"]; ok {
		t.Fatalf("structured request retained encrypted reasoning inclusion: %#v", requests[1])
	}
}

func TestModelFingerprintReasoningEffortCompatibility(t *testing.T) {
	const apiMode = APIResponses
	const endpoint = "https://provider.invalid/v1/responses"
	const model = "gpt-test"
	legacySum := sha256.Sum256([]byte(model + "\x00" + endpoint + "\x00" + apiMode))
	historical := hex.EncodeToString(legacySum[:8])
	if got := ModelFingerprint(apiMode, endpoint, model); got != historical {
		t.Fatalf("base fingerprint = %q, want historical %q", got, historical)
	}
	if got := ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, ""); got != historical {
		t.Fatalf("empty effort fingerprint = %q, want historical %q", got, historical)
	}
	if got := ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, "  HIGH "); got != ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, ReasoningEffortHigh) {
		t.Fatal("fingerprint did not normalize effort")
	}
	seen := map[string]bool{historical: true}
	for _, effort := range []ReasoningEffort{ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax} {
		fingerprint := ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, effort)
		if seen[fingerprint] {
			t.Fatalf("effort %q reused fingerprint %q", effort, fingerprint)
		}
		seen[fingerprint] = true
	}
	client := NewClientWithOptions(Options{API: apiMode, Endpoint: endpoint, Model: model, ReasoningEffort: " HIGH "})
	if client.ModelFingerprint() != ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, ReasoningEffortHigh) {
		t.Fatalf("client fingerprint = %q", client.ModelFingerprint())
	}
}

func effortTestName(effort ReasoningEffort) string {
	if effort == "" {
		return "default"
	}
	return string(effort)
}

func expectedReasoningRequestBody(apiMode string, effort ReasoningEffort) string {
	if apiMode == APIResponses {
		body := `{"model":"m","input":[{"content":"system","role":"system"},{"content":"user","role":"user"}]`
		if effort != "" {
			body += `,"reasoning":{"effort":"` + string(effort) + `"}`
		}
		return body + `,"store":false,"include":["reasoning.encrypted_content"]}`
	}
	body := `{"model":"m","messages":[{"role":"system","content":"system"},{"role":"user","content":"user"}]`
	if effort != "" {
		body += `,"reasoning_effort":"` + string(effort) + `"`
	}
	return body + `}`
}

func reasoningStructuredField(apiMode string) string {
	if apiMode == APIResponses {
		return "text"
	}
	return "response_format"
}

func assertWireReasoningEffort(t *testing.T, apiMode string, request map[string]any, want string) {
	t.Helper()
	if apiMode == APIResponses {
		reasoning, ok := request["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != want {
			t.Fatalf("reasoning = %#v, want effort %q", request["reasoning"], want)
		}
		return
	}
	if got := request["reasoning_effort"]; got != want {
		t.Fatalf("reasoning_effort = %#v, want %q", got, want)
	}
}

func writeReasoningTestFinal(w http.ResponseWriter, apiMode, content string) {
	w.Header().Set("Content-Type", "application/json")
	if apiMode == APIResponses {
		_, _ = io.WriteString(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+mustJSONString(content)+`}]}]}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"c","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":`+mustJSONString(content)+`}}]}`)
}

func writeReasoningTestToolCall(w http.ResponseWriter, apiMode, name, arguments string) {
	w.Header().Set("Content-Type", "application/json")
	if apiMode == APIResponses {
		_, _ = io.WriteString(w, `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":`+mustJSONString(name)+`,"arguments":`+mustJSONString(arguments)+`}]}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"c","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":`+mustJSONString(name)+`,"arguments":`+mustJSONString(arguments)+`}}]}}]}`)
}

func mustJSONString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
