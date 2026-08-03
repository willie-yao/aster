package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

type responsesTransport struct {
	api *httpAPIClient
}

func newResponsesTransport(api *httpAPIClient) *responsesTransport {
	return &responsesTransport{api: api}
}

type responsesRequest struct {
	Model             string               `json:"model"`
	Input             []any                `json:"input"`
	Tools             []responsesTool      `json:"tools,omitempty"`
	Text              *responsesTextConfig `json:"text,omitempty"`
	ToolChoice        *responsesToolChoice `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
	Store             bool                 `json:"store"`
	Include           []string             `json:"include,omitempty"`
}

type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

type responsesTextFormat struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Strict      bool           `json:"strict"`
	Schema      map[string]any `json:"schema"`
}

type responsesToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type responsesResponse struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  *responsesUsage   `json:"usage"`
}

type responsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	OutputTokens        int                         `json:"output_tokens"`
	InputTokensDetails  responsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokensDetails responsesOutputTokenDetails `json:"output_tokens_details"`
}

type responsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responsesOutputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

func (t *responsesTransport) Complete(ctx context.Context, req modelRequest) (*modelResponse, error) {
	time.Sleep(callDelay)
	include := []string{"reasoning.encrypted_content"}
	if req.OmitReasoning {
		include = nil
	}
	body, err := json.Marshal(responsesRequest{
		Model: req.Model, Input: encodeResponsesInput(req.Messages),
		Tools: encodeResponsesTools(req.Tools), Text: encodeResponsesText(req.ResponseFormat),
		ToolChoice: encodeResponsesToolChoice(req.ToolChoice), ParallelToolCalls: req.ParallelToolCalls,
		Store: false, Include: include,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var resp *http.Response
	attempts := 0
	for attempt := 0; attempt < 3; attempt++ {
		attempts = attempt + 1
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api.endpoint, bytes.NewReader(body))
		if err != nil {
			return &modelResponse{Attempts: attempts}, fmt.Errorf("build request: %w", err)
		}
		t.api.setRequestHeaders(httpReq)
		resp, err = t.api.httpClient.Do(httpReq)
		if err != nil {
			return &modelResponse{Attempts: attempts}, fmt.Errorf("post: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt == 2 {
				break
			}
			wait := retryAfter(resp.Header.Get("Retry-After"), time.Duration(2<<attempt)*time.Second)
			_ = resp.Body.Close()
			select {
			case <-ctx.Done():
				return &modelResponse{Attempts: attempts}, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	raw, err := readModelResponseBody(resp.Body, req.MaxResponseBytes)
	if err != nil {
		return &modelResponse{Attempts: attempts, HTTPStatus: resp.StatusCode}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &modelResponse{Attempts: attempts, HTTPStatus: resp.StatusCode}, &modelHTTPError{API: "responses", StatusCode: resp.StatusCode, Body: textutil.Truncate(string(raw), 500)}
	}
	var wire responsesResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return &modelResponse{Attempts: attempts, HTTPStatus: resp.StatusCode}, fmt.Errorf("decode response: %w; body=%s", err, textutil.Truncate(string(raw), 500))
	}
	if wire.Status != "completed" {
		return &modelResponse{ResponseID: wire.ID, Status: wire.Status, Attempts: attempts, HTTPStatus: resp.StatusCode, Usage: responsesTokenUsage(wire.Usage)}, fmt.Errorf("responses status %q: %s", wire.Status, textutil.Truncate(string(raw), 500))
	}
	out := decodeResponsesResponse(wire)
	out.Attempts = attempts
	out.HTTPStatus = resp.StatusCode
	return out, nil
}

func encodeResponsesInput(messages []modelMessage) []any {
	if messages == nil {
		return nil
	}
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		for _, raw := range message.ProviderItems {
			items = append(items, raw)
		}
		if message.Role == "assistant" && len(message.ProviderItems) > 0 {
			continue
		}
		switch message.Role {
		case "tool":
			output := ""
			if message.Content != nil {
				output = *message.Content
			}
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": message.ToolCallID, "output": output,
			})
		case "assistant":
			for _, call := range message.ToolCalls {
				items = append(items, map[string]any{
					"type": "function_call", "call_id": call.ID,
					"name": call.Function.Name, "arguments": call.Function.Arguments,
				})
			}
			if message.Content != nil {
				items = append(items, map[string]any{"role": "assistant", "content": *message.Content})
			}
		default:
			if message.Content != nil {
				items = append(items, map[string]any{"role": message.Role, "content": *message.Content})
			}
		}
	}
	return items
}

func encodeResponsesTools(schemas []tools.Schema) []responsesTool {
	if schemas == nil {
		return nil
	}
	out := make([]responsesTool, len(schemas))
	for i, schema := range schemas {
		out[i] = responsesTool{
			Type: "function", Name: schema.Function.Name,
			Description: schema.Function.Description, Parameters: schema.Function.Parameters,
			Strict: schema.Function.Strict,
		}
	}
	return out
}

func responsesTokenUsage(usage *responsesUsage) aiusage.TokenUsage {
	if usage == nil {
		return aiusage.TokenUsage{}
	}
	return aiusage.TokenUsage{
		Reported: true, InputTokens: usage.InputTokens, CachedInputTokens: usage.InputTokensDetails.CachedTokens,
		OutputTokens: usage.OutputTokens, ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens,
	}
}

func decodeResponsesResponse(resp responsesResponse) *modelResponse {
	message := modelMessage{Role: "assistant"}
	var text string
	for _, raw := range resp.Output {
		message.ProviderItems = append(message.ProviderItems, append(json.RawMessage(nil), raw...))
		var item responsesOutputItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		switch item.Type {
		case "function_call":
			message.ToolCalls = append(message.ToolCalls, modelToolCall{
				ID: item.CallID, Type: "function",
				Function: modelFunction{Name: item.Name, Arguments: item.Arguments},
			})
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" {
					text += content.Text
				}
				if content.Type == "refusal" {
					text += content.Refusal
				}
			}
		}
	}
	if text != "" {
		message.Content = strPtr(text)
	}
	finish := resp.Status
	if len(message.ToolCalls) > 0 {
		finish = "tool_calls"
	} else if finish == "completed" {
		finish = "stop"
	}
	return &modelResponse{
		Message: message, FinishReason: finish, ResponseID: resp.ID, Status: resp.Status,
		Usage:      responsesTokenUsage(resp.Usage),
		HasMessage: len(resp.Output) > 0,
	}
}

func encodeResponsesText(format *ResponseFormat) *responsesTextConfig {
	if format == nil {
		return nil
	}
	return &responsesTextConfig{Format: responsesTextFormat{
		Type: "json_schema", Name: format.Name, Description: format.Description,
		Strict: true, Schema: format.Schema,
	}}
}

func encodeResponsesToolChoice(choice *ToolChoice) *responsesToolChoice {
	if choice == nil {
		return nil
	}
	return &responsesToolChoice{Type: "function", Name: choice.Name}
}
