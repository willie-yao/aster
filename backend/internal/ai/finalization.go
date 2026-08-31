package ai

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// agForceFinalizePrompt is the user message that forces a JSON-only final round
// when the model has exhausted iterations or returned text without valid JSON.
const agForceFinalizePrompt = `Stop calling tools. Produce the final JSON
analysis now using the evidence you have already gathered, following the
"Response format" section of the system prompt exactly. If you did not find a
definitive root cause, say so explicitly in root_cause (e.g. "Investigation
reached budget; best-evidence hypothesis is X based on Y") rather than
continuing to investigate.

Output ONLY the JSON object: no prose, no explanation, no markdown fences.
Your entire response must start with { and end with }.`

const analysisFinalizeToolName = "submit_analysis"

func analysisFinalizeFormat() ResponseFormat {
	stringArray := func() map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}
	return ResponseFormat{
		Name:        analysisFinalizeToolName,
		Description: "Submit one structured failure analysis.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"summary":            map[string]any{"type": "string"},
				"is_transient":       map[string]any{"type": "boolean"},
				"root_cause":         map[string]any{"type": "string"},
				"severity":           map[string]any{"type": "string", "enum": []string{"Critical", "High", "Medium", "Low"}},
				"suggested_fix":      map[string]any{"type": "string"},
				"relevant_files":     stringArray(),
				"search_suggestions": stringArray(),
				"evidence_citations": map[string]any{
					"type": "array", "maxItems": 20,
					"items": map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{
							"path":       map[string]any{"type": "string"},
							"line_start": map[string]any{"type": "integer", "minimum": 1},
							"line_end":   map[string]any{"type": "integer", "minimum": 1},
							"quote":      map[string]any{"type": "string"},
						},
						"required": []string{"path", "line_start", "line_end", "quote"},
					},
				},
			},
			"required": []string{"summary", "is_transient", "root_cause", "severity", "suggested_fix", "relevant_files", "search_suggestions", "evidence_citations"},
		},
	}
}

const critiqueFinalizationReserve = 5 * time.Second

func (c *Client) runFinalizeRoundTracked(ctx context.Context, state *agentState, messages []modelMessage, headroom contextHeadroom) (string, []json.RawMessage, bool) {
	started := time.Now()
	content, items, safe := c.runFinalizeRound(ctx, messages, headroom)
	state.recentModelRequest = time.Since(started)
	return content, items, safe
}

// runFinalizeRound asks the model for one schema-constrained response containing
// just the final analysis. Used when the agent ran out of iterations or returned
// prose without parseable JSON. Returns raw content; callers handle unparseable
// responses.
func (c *Client) runFinalizeRound(ctx context.Context, messages []modelMessage, headroom contextHeadroom) (string, []json.RawMessage, bool) {
	messages = append(messages, modelMessage{Role: "user", Content: strPtr(agForceFinalizePrompt)})
	format := analysisFinalizeFormat()
	toolDefs := []tools.Schema{{
		Type: "function",
		Function: tools.FunctionDecl{
			Name: format.Name, Description: format.Description,
			Parameters: format.Schema, Strict: true,
		},
	}}
	var safe bool
	messages, safe = prepareContextRequest(ctx, messages, schemaPayloadBytes(toolDefs), headroom, "finalize")
	if !safe {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "headroom_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "unavailable", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		log.Printf("  ⚠ agentic finalize round skipped: request exceeds context headroom")
		return "", nil, false
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "requested"})
	parallel := false
	resp, err := c.callAgenticModelRequest(ctx, modelRequest{
		Model: c.model, Messages: messages, Tools: toolDefs,
		ToolChoice: &ToolChoice{Name: format.Name}, ParallelToolCalls: &parallel,
	})
	if err != nil {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "error", ErrorCode: "model_request_error"})
		log.Printf("  ⚠ agentic finalize round failed: %v", err)
		return "", nil, true
	}
	if !resp.HasMessage {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: "missing_message"})
		return "", resp.Message.ProviderItems, true
	}
	if len(resp.Message.ToolCalls) == 1 && resp.Message.ToolCalls[0].Function.Name == format.Name {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "success", Status: "forced_function"})
		// The forced call encodes the final answer; it is not an application Tool
		// invocation. Replay its arguments as assistant content on later repairs so
		// Responses does not require a synthetic function_call_output.
		content := resp.Message.ToolCalls[0].Function.Arguments
		items := []json.RawMessage(nil)
		phase := ""
		if c.apiMode == APIResponses {
			phase = "final_answer"
			items = responsesAssistantProviderItem(content, phase)
		}
		captureToolLoopContinuation(ctx, c, appendToolsFreeAssistant(messages, modelMessage{
			Role: "assistant", Content: strPtr(content), Phase: phase, ProviderItems: items,
		}))
		return content, items, true
	}
	captureToolLoopContinuation(ctx, c, appendToolsFreeAssistant(messages, resp.Message))
	if resp.Message.Content != nil {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "success", Status: "plain_content"})
		return *resp.Message.Content, resp.Message.ProviderItems, true
	}
	code := "nil_content"
	if len(resp.Message.ToolCalls) > 0 {
		code = "unexpected_tool_call"
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: code})
	return "", resp.Message.ProviderItems, true
}

// tryParseAnalysis extracts and unmarshals the JSON answer, returning ok=false
// if no valid JSON object could be found.
func tryParseAnalysis(s string) (analysisResponse, bool) {
	if strings.TrimSpace(s) == "" {
		return analysisResponse{}, false
	}
	var out analysisResponse
	cleaned := extractJSON(s)
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return analysisResponse{}, false
	}
	if out.RootCause == "" && out.Summary == "" {
		return analysisResponse{}, false
	}
	return captureSemanticSourceRangeHints(out), true
}

var toolsUnsupportedRe = regexp.MustCompile(`(?i)tool[s_]?call|function[s_]?call|tools_choice|tools provided|tools?\s+(?:are\s+)?not supported|function calling`)

func isToolsUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, " 400") && !strings.Contains(msg, " 422") {
		return false
	}
	return toolsUnsupportedRe.MatchString(msg)
}
