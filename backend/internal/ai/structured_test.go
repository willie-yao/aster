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
	"time"
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
	if metadata.API != "" || metadata.Category != "http_error" || metadata.StatusCode != 429 || metadata.RetryAfter != "" || metadata.RequestID != "" || metadata.StructuredAttempt != string(StructuredAttemptForcedFunction) {
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

type testStructuredValidationError struct{ code string }

func (e testStructuredValidationError) Error() string                    { return "rejected" }
func (e testStructuredValidationError) StructuredValidationCode() string { return e.code }

func codedBodyValidator(want, code string) StructuredValidator {
	return func(raw json.RawMessage) error {
		var value struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return testStructuredValidationError{code: code}
		}
		if value.Body != want {
			return testStructuredValidationError{code: code}
		}
		return nil
	}
}

func structuredContent(content string) *modelResponse {
	return &modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{Content: strPtr(content)}}
}

func structuredFunction(name, arguments string) *modelResponse {
	return &modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{ToolCalls: []modelToolCall{{
		ID: "call-1", Type: "function", Function: modelFunction{Name: name, Arguments: arguments},
	}}}}
}

func TestCompleteStructuredResponseFormatSuccessMetadata(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{{response: structuredContent(`{"body":"safe"}`)}}}
	client := &Client{model: "model", transport: transport}
	ctx := WithStructuredCompletionPhase(t.Context(), "target_extraction_initial")
	metadata, err := client.CompleteStructuredWithMetadata(ctx, "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "invalid_target_extraction"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attempts) != 1 {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
	attempt := metadata.Attempts[0]
	if attempt.Phase != "target_extraction_initial" || attempt.Path != StructuredAttemptResponseFormat || attempt.Outcome != StructuredOutcomeAccepted || !attempt.ValidatorCalled || attempt.ValidationCode != "" || !attempt.ProviderAttemptsKnown || attempt.ProviderAttempts != 1 {
		t.Fatalf("attempt=%+v", attempt)
	}
}

func TestCompleteStructuredProviderRejectionThenForcedFunctionSuccess(t *testing.T) {
	unsupported := newModelHTTPError("responses", 400, "private provider body", http.Header{})
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: unsupported},
		{response: structuredFunction("return_body", `{"body":"safe"}`)},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "invalid_target_extraction"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attempts) != 2 || metadata.Attempts[0].Outcome != StructuredOutcomeProviderError || metadata.Attempts[0].ProviderStatus != 400 || metadata.Attempts[1].Path != StructuredAttemptForcedFunction || metadata.Attempts[1].Outcome != StructuredOutcomeAccepted {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredValidatorRejectionThenForcedFunctionSuccess(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: structuredContent(`{"body":"wrong"}`)},
		{response: structuredFunction("return_body", `{"body":"safe"}`)},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "candidate_kind"))
	if err != nil {
		t.Fatal(err)
	}
	first := metadata.Attempts[0]
	if first.Outcome != StructuredOutcomeValidatorRejected || !first.ValidatorCalled || first.ValidationCode != "candidate_kind" || metadata.Attempts[1].Outcome != StructuredOutcomeAccepted {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredForcedFunctionNotReturned(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: structuredContent(`{"body":"wrong"}`)},
		{response: structuredContent("not a function call")},
		{response: structuredContent(`{"body":"safe"}`)},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "candidate_kind"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Attempts[1].Path != StructuredAttemptForcedFunction || metadata.Attempts[1].Outcome != StructuredOutcomeMissingForcedFunction || metadata.Attempts[1].ValidatorCalled {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredForcedFunctionArgumentsRejected(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: structuredContent(`{"body":"wrong"}`)},
		{response: structuredFunction("return_body", `{"body":"still-wrong"}`)},
		{response: structuredContent(`{"body":"safe"}`)},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "required_call_target"))
	if err != nil {
		t.Fatal(err)
	}
	forced := metadata.Attempts[1]
	if forced.Outcome != StructuredOutcomeValidatorRejected || !forced.ValidatorCalled || forced.ValidationCode != "required_call_target" {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredPlainFallbackAcceptedMetadata(t *testing.T) {
	unsupported := newModelHTTPError("chat", 400, "unsupported", http.Header{})
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: unsupported},
		{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: unsupported},
		{response: structuredContent("text\n{\"body\":\"safe\"}\n")},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "decode"))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := metadata.FinalAttempt()
	if len(metadata.Attempts) != 3 || final.Path != StructuredAttemptPlainFallback || final.Outcome != StructuredOutcomeAccepted || !final.ValidatorCalled {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredAllAttemptsRejectedMetadata(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: structuredContent(`{"body":"wrong"}`)},
		{response: structuredContent("missing function")},
		{response: structuredContent("not json")},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), codedBodyValidator("safe", "invalid_version"))
	if err == nil {
		t.Fatal("all rejected attempts succeeded")
	}
	fromError, ok := StructuredCompletionFailureMetadata(err)
	if !ok || len(metadata.Attempts) != 3 || len(fromError.Attempts) != 3 {
		t.Fatalf("metadata=%+v from_error=%+v ok=%v", metadata, fromError, ok)
	}
	if metadata.Attempts[0].Outcome != StructuredOutcomeValidatorRejected || metadata.Attempts[1].Outcome != StructuredOutcomeMissingForcedFunction || metadata.Attempts[2].Outcome != StructuredOutcomeInvalidJSON {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredProviderFailureOnEachAttemptPath(t *testing.T) {
	tests := []struct {
		name    string
		results []scriptedTransportResult
		path    StructuredAttemptPath
		status  int
	}{
		{name: "response format", results: []scriptedTransportResult{{response: &modelResponse{Attempts: 1, HTTPStatus: 500}, err: newModelHTTPError("responses", 500, "body", http.Header{})}}, path: StructuredAttemptResponseFormat, status: 500},
		{name: "forced function", results: []scriptedTransportResult{
			{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: newModelHTTPError("responses", 400, "body", http.Header{})},
			{response: &modelResponse{Attempts: 1, HTTPStatus: 500}, err: newModelHTTPError("responses", 500, "body", http.Header{})},
		}, path: StructuredAttemptForcedFunction, status: 500},
		{name: "plain fallback", results: []scriptedTransportResult{
			{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: newModelHTTPError("responses", 400, "body", http.Header{})},
			{response: &modelResponse{Attempts: 1, HTTPStatus: 400}, err: newModelHTTPError("responses", 400, "body", http.Header{})},
			{response: &modelResponse{Attempts: 1, HTTPStatus: 503}, err: newModelHTTPError("responses", 503, "body", http.Header{})},
		}, path: StructuredAttemptPlainFallback, status: 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{model: "model", transport: &scriptedTransport{results: tt.results}}
			metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), bodyValidator("safe"))
			if err == nil {
				t.Fatal("provider failure succeeded")
			}
			final, ok := metadata.FinalAttempt()
			if !ok || final.Path != tt.path || final.Outcome != StructuredOutcomeProviderError || final.ProviderCategory != "http_error" || final.ProviderStatus != tt.status {
				t.Fatalf("final=%+v attempts=%+v", final, metadata.Attempts)
			}
		})
	}
}

type contextTransport struct{}

func (contextTransport) Complete(ctx context.Context, _ modelRequest) (*modelResponse, error) {
	<-ctx.Done()
	return &modelResponse{Attempts: 1}, ctx.Err()
}

func TestCompleteStructuredCancellationMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &Client{model: "model", transport: contextTransport{}}
	metadata, err := client.CompleteStructuredWithMetadata(ctx, "system", "user", structuredBodyFormat(), bodyValidator("safe"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	final, ok := metadata.FinalAttempt()
	if !ok || final.Path != StructuredAttemptResponseFormat || final.Outcome != StructuredOutcomeProviderError || final.ProviderCategory != "context_canceled" {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestCompleteStructuredDeadlineMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	client := &Client{model: "model", transport: contextTransport{}}
	metadata, err := client.CompleteStructuredWithMetadata(ctx, "system", "user", structuredBodyFormat(), bodyValidator("safe"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	final, ok := metadata.FinalAttempt()
	if !ok || final.Outcome != StructuredOutcomeProviderError || final.ProviderCategory != "deadline_exceeded" {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestCompleteStructuredRecordsContentFreeTraceEvent(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{{response: structuredContent(`{"body":"wrong"}`)}, {response: structuredFunction("return_body", `{"body":"safe"}`)}}}
	client := &Client{model: "model", transport: transport}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "build", TestName: "test", APIMode: APIResponses})
	ctx := withAnalysisTrace(WithStructuredCompletionPhase(t.Context(), "target_extraction_initial"), trace)
	if _, err := client.CompleteStructuredWithMetadata(ctx, "private prompt", "private target", structuredBodyFormat(), codedBodyValidator("safe", "candidate_kind")); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	snapshot := store.Snapshot()
	var events []TraceEvent
	for _, event := range snapshot.Traces[0].Events {
		if event.Kind == "structured_completion" {
			events = append(events, event)
		}
	}
	if len(events) != 2 || events[0].StructuredPhase != "target_extraction_initial" || events[0].StructuredAttempt != string(StructuredAttemptResponseFormat) || events[0].StructuredOutcome != string(StructuredOutcomeValidatorRejected) || events[0].ValidatorCalled == nil || !*events[0].ValidatorCalled || events[0].ValidationCode != "candidate_kind" {
		t.Fatalf("events=%+v", events)
	}
	encoded, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{"private prompt", "private target", `\"body\":\"wrong\"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("trace leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestCompleteStructuredEmptyResponseOutcome(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: &modelResponse{HasMessage: true, Attempts: 1, Message: modelMessage{Content: strPtr(" ")}}},
		{response: structuredContent("missing function")},
		{response: &modelResponse{HasMessage: false, Attempts: 1}},
	}}
	client := &Client{model: "model", transport: transport}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), bodyValidator("safe"))
	if err == nil {
		t.Fatal("empty responses succeeded")
	}
	if metadata.Attempts[0].Outcome != StructuredOutcomeEmptyResponse || metadata.Attempts[2].Outcome != StructuredOutcomeEmptyResponse {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}

func TestCompleteStructuredConflictingCandidatesAreNoCandidate(t *testing.T) {
	transport := &scriptedTransport{results: []scriptedTransportResult{
		{response: structuredContent(`{"body":"one"}{"body":"two"}`)},
		{response: structuredContent("missing function")},
		{response: structuredContent(`{"body":"one"}{"body":"two"}`)},
	}}
	client := &Client{model: "model", transport: transport}
	validator := func(raw json.RawMessage) error {
		var value struct {
			Body string `json:"body"`
		}
		return json.Unmarshal(raw, &value)
	}
	metadata, err := client.CompleteStructuredWithMetadata(t.Context(), "system", "user", structuredBodyFormat(), validator)
	if err == nil {
		t.Fatal("conflicting candidates succeeded")
	}
	if metadata.Attempts[0].Outcome != StructuredOutcomeNoCandidate || metadata.Attempts[2].Outcome != StructuredOutcomeNoCandidate || !metadata.Attempts[2].ValidatorCalled {
		t.Fatalf("attempts=%+v", metadata.Attempts)
	}
}
