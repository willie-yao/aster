package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

type chatCompletionsTransport struct {
	api *httpAPIClient
}

func newChatCompletionsTransport(api *httpAPIClient) *chatCompletionsTransport {
	return &chatCompletionsTransport{api: api}
}

type chatCompletionsMessage struct {
	Role       string                    `json:"role"`
	Content    *string                   `json:"content,omitempty"`
	Name       string                    `json:"name,omitempty"`
	ToolCallID string                    `json:"tool_call_id,omitempty"`
	ToolCalls  []chatCompletionsToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionsToolCall struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Function chatCompletionsFunction `json:"function"`
}

type chatCompletionsFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionsRequest struct {
	Model    string                   `json:"model"`
	Messages []chatCompletionsMessage `json:"messages"`
	Tools    []tools.Schema           `json:"tools,omitempty"`

	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

type chatCompletionsResponse struct {
	ID      string                  `json:"id"`
	Choices []chatCompletionsChoice `json:"choices"`
	Usage   chatCompletionsUsage    `json:"usage"`
}

type chatCompletionsUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

type chatCompletionsChoice struct {
	FinishReason string                 `json:"finish_reason"`
	Message      chatCompletionsMessage `json:"message"`
}

func (t *chatCompletionsTransport) Complete(ctx context.Context, req modelRequest) (*modelResponse, error) {
	time.Sleep(callDelay)

	body, err := json.Marshal(chatCompletionsRequest{
		Model:             req.Model,
		Messages:          encodeChatMessages(req.Messages),
		Tools:             req.Tools,
		ParallelToolCalls: req.ParallelToolCalls,
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
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &modelResponse{Attempts: attempts, HTTPStatus: resp.StatusCode}, &modelHTTPError{API: "chat", StatusCode: resp.StatusCode, Body: textutil.Truncate(string(raw), 500)}
	}
	var wire chatCompletionsResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return &modelResponse{Attempts: attempts, HTTPStatus: resp.StatusCode}, fmt.Errorf("decode response: %w; body=%s", err, textutil.Truncate(string(raw), 500))
	}
	out := decodeChatResponse(wire)
	out.Attempts = attempts
	out.HTTPStatus = resp.StatusCode
	return out, nil
}

func encodeChatMessages(messages []modelMessage) []chatCompletionsMessage {
	if messages == nil {
		return nil
	}
	out := make([]chatCompletionsMessage, len(messages))
	for i, message := range messages {
		out[i] = chatCompletionsMessage{
			Role:       message.Role,
			Content:    message.Content,
			Name:       message.Name,
			ToolCallID: message.ToolCallID,
			ToolCalls:  encodeChatToolCalls(message.ToolCalls),
		}
	}
	return out
}

func encodeChatToolCalls(calls []modelToolCall) []chatCompletionsToolCall {
	if calls == nil {
		return nil
	}
	out := make([]chatCompletionsToolCall, len(calls))
	for i, call := range calls {
		out[i] = chatCompletionsToolCall{
			ID:   call.ID,
			Type: call.Type,
			Function: chatCompletionsFunction{
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			},
		}
	}
	return out
}

func decodeChatResponse(resp chatCompletionsResponse) *modelResponse {
	if len(resp.Choices) == 0 {
		return &modelResponse{ResponseID: resp.ID, InputTokens: chatInputTokens(resp.Usage), OutputTokens: chatOutputTokens(resp.Usage)}
	}
	choice := resp.Choices[0]
	return &modelResponse{
		HasMessage:   true,
		FinishReason: choice.FinishReason,
		ResponseID:   resp.ID,
		InputTokens:  chatInputTokens(resp.Usage),
		OutputTokens: chatOutputTokens(resp.Usage),
		Message: modelMessage{
			Role:       choice.Message.Role,
			Content:    choice.Message.Content,
			Name:       choice.Message.Name,
			ToolCallID: choice.Message.ToolCallID,
			ToolCalls:  decodeChatToolCalls(choice.Message.ToolCalls),
		},
	}
}

func chatInputTokens(usage chatCompletionsUsage) int {
	if usage.InputTokens > 0 {
		return usage.InputTokens
	}
	return usage.PromptTokens
}

func chatOutputTokens(usage chatCompletionsUsage) int {
	if usage.OutputTokens > 0 {
		return usage.OutputTokens
	}
	return usage.CompletionTokens
}

func decodeChatToolCalls(calls []chatCompletionsToolCall) []modelToolCall {
	if calls == nil {
		return nil
	}
	out := make([]modelToolCall, len(calls))
	for i, call := range calls {
		out[i] = modelToolCall{
			ID:   call.ID,
			Type: call.Type,
			Function: modelFunction{
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			},
		}
	}
	return out
}

func retryAfter(value string, fallback time.Duration) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return fallback
}
