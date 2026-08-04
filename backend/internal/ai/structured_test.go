package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type scriptedTransportResult struct {
	response *modelResponse
	err      error
}

type scriptedTransport struct {
	requests []modelRequest
	results  []scriptedTransportResult
}

func (t *scriptedTransport) Complete(_ context.Context, request modelRequest) (*modelResponse, error) {
	t.requests = append(t.requests, request)
	result := t.results[len(t.requests)-1]
	return result.response, result.err
}

func structuredBodyFormat() ResponseFormat {
	return ResponseFormat{Name: "return_body", Description: "Return a body.", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"body": map[string]any{"type": "string"},
		},
		"required": []string{"body"}, "additionalProperties": false,
	}}
}

func bodyValidator(want string) StructuredValidator {
	return func(raw json.RawMessage) error {
		var value struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Body != want {
			return errors.New("unexpected body")
		}
		return nil
	}
}

func TestCompleteStructuredFallsBackToForcedTool(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: &modelResponse{HasMessage: true, Message: modelMessage{Content: strPtr(`{"body":"unsafe"}`)}}},
		{response: &modelResponse{
			HasMessage: true,
			Message: modelMessage{ToolCalls: []modelToolCall{{
				ID: "call-1", Type: "function", Function: modelFunction{Name: "return_body", Arguments: `{"body":"safe"}`},
			}}},
		}},
	}}
	client := &Client{model: "model", transport: transport}
	if err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(transport.requests))
	}
	if transport.requests[0].ResponseFormat == nil || transport.requests[0].ToolChoice != nil || !transport.requests[0].OmitReasoning {
		t.Fatalf("strict request = %+v", transport.requests[0])
	}
	forced := transport.requests[1]
	if forced.ToolChoice == nil || forced.ToolChoice.Name != "return_body" || len(forced.Tools) != 1 || !forced.Tools[0].Function.Strict {
		t.Fatalf("forced request = %+v", forced)
	}
}

func TestCompleteStructuredUsesBoundedExtractorFallback(t *testing.T) {
	unsupported := &modelHTTPError{API: "chat", StatusCode: 400, Body: "unsupported"}
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{err: unsupported},
		{err: unsupported},
		{response: &modelResponse{HasMessage: true, Message: modelMessage{Content: strPtr("planning text\n{\"body\":\"safe\"}\n")}}},
	}}
	client := &Client{model: "model", transport: transport}
	if err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), bodyValidator("safe")); err != nil {
		t.Fatal(err)
	}
	if len(transport.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(transport.requests))
	}
}

func TestCompleteStructuredRejectsConflictingCandidates(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: &modelResponse{HasMessage: true, Message: modelMessage{Content: strPtr(`{"body":"one"}{"body":"two"}`)}}},
		{response: &modelResponse{HasMessage: true, Message: modelMessage{Content: strPtr("missing tool call")}}},
		{response: &modelResponse{HasMessage: true, Message: modelMessage{Content: strPtr(`{"body":"one"}{"body":"two"}`)}}},
	}}
	client := &Client{model: "model", transport: transport}
	validator := func(raw json.RawMessage) error {
		var value struct {
			Body string `json:"body"`
		}
		return json.Unmarshal(raw, &value)
	}
	if err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), validator); err == nil {
		t.Fatal("conflicting candidates were accepted")
	}
}

func TestValidateStructuredCandidatesRejectsOversizedResponse(t *testing.T) {
	raw := strings.Repeat("x", int(defaultStructuredResponseBytes)+1)
	if err := validateStructuredCandidates(raw, bodyValidator("safe")); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestStructuredWireMappings(t *testing.T) {
	format := structuredBodyFormat()
	chatFormat := encodeChatResponseFormat(&format)
	if chatFormat == nil || chatFormat.Type != "json_schema" || !chatFormat.JSONSchema.Strict || chatFormat.JSONSchema.Name != format.Name {
		t.Fatalf("chat response format = %+v", chatFormat)
	}
	chatChoice := encodeChatToolChoice(&ToolChoice{Name: format.Name})
	if chatChoice == nil || chatChoice.Type != "function" || chatChoice.Function.Name != format.Name {
		t.Fatalf("chat tool choice = %+v", chatChoice)
	}
	responsesText := encodeResponsesText(&format)
	if responsesText == nil || responsesText.Format.Type != "json_schema" || !responsesText.Format.Strict || responsesText.Format.Name != format.Name {
		t.Fatalf("responses text format = %+v", responsesText)
	}
	responsesChoice := encodeResponsesToolChoice(&ToolChoice{Name: format.Name})
	if responsesChoice == nil || responsesChoice.Type != "function" || responsesChoice.Name != format.Name {
		t.Fatalf("responses tool choice = %+v", responsesChoice)
	}
}

func TestSafeProviderErrorMetadataExcludesProviderBody(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "12")
	headers.Set("X-GitHub-Request-Id", "request-123")
	cause := newModelHTTPError("responses", 429, "private provider body with model output", headers)
	err := structuredFailureAt("provider request failed", "forced-function", cause)
	metadata, ok := SafeProviderErrorMetadata(err)
	if !ok {
		t.Fatal("provider metadata was not available")
	}
	if metadata.API != "responses" || metadata.StatusCode != 429 || metadata.RetryAfter != "12" || metadata.RequestID != "request-123" || metadata.StructuredAttempt != "forced-function" {
		t.Fatalf("metadata = %+v", metadata)
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", metadata)} {
		if strings.Contains(text, cause.Body) || strings.Contains(text, "model output") {
			t.Fatalf("safe metadata exposed provider body: %s", text)
		}
	}
}

func TestSafeProviderErrorMetadataRejectsUnsafeHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "private response text")
	headers.Set("X-Request-Id", "request with spaces and source text")
	err := structuredFailureAt("provider request failed", "json-schema", newModelHTTPError("chat", 500, "body", headers))
	metadata, ok := SafeProviderErrorMetadata(err)
	if !ok {
		t.Fatal("structured attempt metadata was not available")
	}
	if metadata.RetryAfter != "" || metadata.RequestID != "" {
		t.Fatalf("unsafe headers were retained: %+v", metadata)
	}
}

func TestCompleteStructuredDoesNotSendTokenAcrossRedirect(t *testing.T) {
	const token = "fixture-ai-secret"
	received := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Fatalf("redirect destination received authorization: %q", auth)
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/v1/responses", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := NewClientWithOptions(Options{Token: token, API: APIResponses, Endpoint: origin.URL + "/v1/responses", Model: "model"})
	err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), bodyValidator("safe"))
	if err == nil {
		t.Fatal("cross-origin redirect was accepted")
	}
	if received != 0 {
		t.Fatalf("redirect destination requests = %d", received)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), destination.URL) {
		t.Fatalf("redirect error exposed sensitive details: %v", err)
	}
}

func TestCompleteStructuredStopsSameOriginRedirectLoop(t *testing.T) {
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, server.URL+"/v1/responses", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := NewClientWithOptions(Options{Token: "fixture-token", API: APIResponses, Endpoint: server.URL + "/v1/responses", Model: "model"})
	err := client.CompleteStructured(context.Background(), "system", "user", structuredBodyFormat(), bodyValidator("safe"))
	if err == nil {
		t.Fatal("same-origin redirect loop was accepted")
	}
	if requests != 10 {
		t.Fatalf("redirect requests = %d, want 10", requests)
	}
}
