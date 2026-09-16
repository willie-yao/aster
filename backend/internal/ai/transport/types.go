// Package transport provides provider-neutral model messages and HTTP adapters.
package transport

import (
	"encoding/json"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

type Request struct {
	Model             string
	Messages          []Message
	Tools             []ToolSchema
	ParallelToolCalls *bool
	ResponseFormat    *ResponseFormat
	ToolChoice        *ToolChoice
	MaxResponseBytes  int64
	MaxOutputTokens   int
	OmitReasoning     bool
	ReasoningEffort   modelprovider.ReasoningEffort
	PromptCacheKey    string
}

type Response struct {
	Message          Message
	FinishReason     string
	HasMessage       bool
	ResponseID       string
	Status           string
	ServiceTier      string
	Attempts         int
	HTTPStatus       int
	Usage            aiusage.TokenUsage
	WireRequestBytes int
}

// The JSON tags preserve the existing compaction size estimate. API adapters
// still map these neutral messages to their own wire types explicitly.
type Message struct {
	Role          string            `json:"role"`
	Content       *string           `json:"content,omitempty"`
	Name          string            `json:"name,omitempty"`
	Phase         string            `json:"phase,omitempty"`
	ToolCallID    string            `json:"tool_call_id,omitempty"`
	ToolCalls     []ToolCall        `json:"tool_calls,omitempty"`
	ProviderItems []json.RawMessage `json:"provider_items,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ResponseFormat describes one strict JSON Schema response.
type ResponseFormat struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolChoice forces one named function call.
type ToolChoice struct {
	Name string
}

// ToolSchema is the OpenAI-shape tool definition emitted in the tools array of a
// chat-completion request. Tools own their own schema so the registry can
// build the per-request slice without duplicating the description elsewhere.
type ToolSchema struct {
	Type     string       `json:"type"`
	Function FunctionDecl `json:"function"`
}

// FunctionDecl is the function half of an OpenAI tool definition.
type FunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}
