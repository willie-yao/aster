package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// ToolLoopEvent is content-free telemetry for one dispatched tool call.
type ToolLoopEvent struct {
	Name            string
	Path            string
	BytesFetched    int
	ContentBytes    int
	Error           bool
	BudgetExhausted bool
	Forced          bool
}

// ToolLoopPrivateEvent carries tool-owned structured observations to an
// explicitly opted-in caller. It is not logged or added to model messages.
type ToolLoopPrivateEvent struct {
	Name            string
	Error           bool
	BudgetExhausted bool
	Forced          bool
	Observation     any
}

// RequiredTool describes one exact tool call that must succeed before the loop
// accepts a tools-free answer. Requirements are satisfied in slice order.
type RequiredTool struct {
	Name             string
	CorrectivePrompt string
	MaxAttempts      int
	RequireContent   bool
}

// RequiredToolErrorCode classifies a required-tool failure without retaining
// model output, tool arguments, or tool results.
type RequiredToolErrorCode string

const (
	RequiredToolInvalid           RequiredToolErrorCode = "invalid_requirement"
	RequiredToolNotEnabled        RequiredToolErrorCode = "not_enabled"
	RequiredToolAttemptsExhausted RequiredToolErrorCode = "attempts_exhausted"
)

// RequiredToolError is a content-free required-tool failure.
type RequiredToolError struct {
	Tool     string
	Code     RequiredToolErrorCode
	Attempts int
}

func (e *RequiredToolError) Error() string {
	if e.Attempts > 0 {
		return fmt.Sprintf("tool loop: required tool %q failed after %d forced attempt(s): %s", e.Tool, e.Attempts, e.Code)
	}
	return fmt.Sprintf("tool loop: required tool %q failed: %s", e.Tool, e.Code)
}

// ToolLoopOptions tunes the generic tool loop. All fields are optional; a zero
// MaxIters falls back to a small default.
type ToolLoopOptions struct {
	// MaxIters bounds the tool-call rounds. Defaults to 8 when <= 0.
	MaxIters int
	// MinToolCalls, when > 0, nudges the model to investigate with the tools
	// before its first tools-free answer is accepted. The nudge fires at most
	// once, so a model that insists on answering still terminates.
	MinToolCalls int
	// SingleToolCall requests one tool call per turn for endpoints whose chat
	// template rejects parallel calls, and deterministically bounds per-turn
	// tool fan-out.
	SingleToolCall bool
	// ContextByteBudget, when > 0, compacts the message list to fit an
	// approximate request size before each turn.
	ContextByteBudget int
	// PropagateFinalizeError returns a forced-finalization model error instead
	// of preserving the generic tool loop's best-effort empty result.
	PropagateFinalizeError bool
	// RequiredTools is an ordered sequence of exact enabled tool names that must
	// complete successfully before a tools-free answer is accepted.
	RequiredTools []RequiredTool
	// Observe receives content-free telemetry after each tool dispatch.
	Observe func(ToolLoopEvent)
	// ObservePrivate receives tool-owned structured observations. Callers must
	// keep them private and must not log or publish them.
	ObservePrivate func(ToolLoopPrivateEvent)
}

// toolLoopBudget is a large per-dispatch budget handed to tools that gate on
// remaining bytes. The loop bounds work via MaxIters and each tool's own caps,
// not a byte budget, so this is effectively "no budget pressure".
const toolLoopBudget = 1 << 30

// ToolLoop runs a bounded, read-only tool-calling loop and returns the model's
// final tools-free message. It is domain-agnostic: the caller supplies the tool
// registry, the enabled tool names, and a tools.Env carrying whatever backend
// those tools need (an artifact Browser, a source RepoReader, or both).
//
// Unlike doAnalyzeAgentic it has no critique gate, investigation floors, skills,
// or cache: it is the plain transport-plus-dispatch core, reused by callers
// (such as the fix-PR locate step) that run their own downstream validation.
func (c *Client) ToolLoop(
	ctx context.Context,
	sys, user string,
	reg *tools.Registry,
	enabled []string,
	env *tools.Env,
	opts ToolLoopOptions,
) (string, error) {
	maxIters := opts.MaxIters
	if maxIters <= 0 {
		maxIters = 8
	}

	messages := []modelMessage{
		{Role: "system", Content: strPtr(sys)},
		{Role: "user", Content: strPtr(user)},
	}
	schemas := reg.Schemas(enabled)
	if len(schemas) == 0 {
		return "", fmt.Errorf("tool loop: no tools enabled (got %v); resolve groups with Registry.Enable first", enabled)
	}
	schemaBytes := schemaPayloadBytes(schemas)
	required, err := newRequiredToolState(opts.RequiredTools, schemas)
	if err != nil {
		return "", err
	}

	var parallelToolCalls *bool
	if opts.SingleToolCall {
		f := false
		parallelToolCalls = &f
	}

	calls := 0
	nudged := false
	forceRequired := false
	for iter := 0; iter < maxIters; iter++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if opts.ContextByteBudget > 0 {
			var elided int
			messages, elided = compactMessages(messages, schemaBytes, opts.ContextByteBudget)
			if elided > 0 {
				log.Printf("  ✂ tool loop: elided %d message(s) to fit ~%d-byte window", elided, opts.ContextByteBudget)
			}
		}
		request := modelRequest{
			Model: c.model, Messages: messages, Tools: schemas,
			ParallelToolCalls: parallelToolCalls,
		}
		forcedName := ""
		if forceRequired {
			pending := required.pending()
			if pending == nil {
				return "", fmt.Errorf("tool loop: required-tool state is inconsistent")
			}
			required.beginForcedAttempt()
			forcedName = pending.Name
			request.ToolChoice = &ToolChoice{Name: forcedName}
			parallel := false
			request.ParallelToolCalls = &parallel
			forceRequired = false
		}
		resp, err := c.callModelRequest(ctx, request)
		if err != nil {
			if iter == 0 && isToolsUnsupportedError(err) {
				return "", fmt.Errorf("%w: %v", ErrToolsUnsupported, err)
			}
			return "", fmt.Errorf("tool loop iter %d: %w", iter+1, err)
		}
		if !resp.HasMessage {
			return "", fmt.Errorf("tool loop iter %d: empty choices", iter+1)
		}
		msg := resp.Message

		if len(msg.ToolCalls) == 0 {
			if pending := required.pending(); pending != nil {
				if !required.canForce() {
					return "", required.exhaustedError()
				}
				messages = appendToolsFreeAssistant(messages, msg)
				messages = append(messages, modelMessage{Role: "user", Content: strPtr(pending.CorrectivePrompt)})
				forceRequired = true
				continue
			}
			// Require a minimum of investigation before accepting a final
			// answer, nudging once so a model that finalizes from the prompt
			// alone still goes and reads the source first.
			if opts.MinToolCalls > 0 && calls < opts.MinToolCalls && !nudged {
				nudged = true
				if msg.Content != nil {
					messages = append(messages, modelMessage{Role: "assistant", Content: msg.Content, ProviderItems: msg.ProviderItems})
				}
				messages = append(messages, modelMessage{
					Role:    "user",
					Content: strPtr("Investigate with the tools before answering: grep and read the relevant files, then give your final JSON."),
				})
				continue
			}
			messages = appendToolsFreeAssistant(messages, msg)
			captureToolLoopContinuation(ctx, c, messages)
			if msg.Content != nil {
				return *msg.Content, nil
			}
			return "", nil
		}

		toolCalls, dropped := limitToolCalls(msg.ToolCalls, opts.SingleToolCall || forcedName != "")
		if dropped > 0 {
			log.Printf("  ⤵ single_tool_call: executing 1 of %d tool calls, dropping %d", len(msg.ToolCalls), dropped)
		}
		echoCalls, skippedOutputs := continuationCalls(c.apiMode, msg, toolCalls)
		echo := modelMessage{Role: "assistant", ToolCalls: echoCalls, ProviderItems: msg.ProviderItems}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		messages = append(messages, skippedOutputs...)

		for _, tc := range toolCalls {
			payload, result := dispatchToolLoop(ctx, reg, env, tc)
			calls++
			required.observe(tc.Function.Name, result)
			_, hasError := result.Payload["error"]
			if opts.Observe != nil {
				opts.Observe(ToolLoopEvent{
					Name: tc.Function.Name, Path: toolLoopPath(tc.Function.Arguments),
					BytesFetched: result.BytesFetched, ContentBytes: result.ContentBytes, Error: hasError,
					BudgetExhausted: result.BudgetExhausted, Forced: forcedName != "",
				})
			}
			if opts.ObservePrivate != nil {
				opts.ObservePrivate(ToolLoopPrivateEvent{
					Name: tc.Function.Name, Error: hasError, BudgetExhausted: result.BudgetExhausted,
					Forced: forcedName != "", Observation: result.Observation,
				})
			}
			messages = append(messages, modelMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    strPtr(payload),
			})
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if required.pending() != nil {
		return "", required.exhaustedError()
	}

	// The model never returned a tools-free answer within the budget. Force one
	// finalize round with tools omitted so the caller still gets a response.
	headroom := contextHeadroomFor(AgenticOptions{ContextByteBudget: opts.ContextByteBudget})
	if opts.PropagateFinalizeError {
		return c.runToolLoopFinalizeRound(ctx, messages, headroom)
	}
	final, _, safe := c.runFinalizeRound(ctx, messages, headroom)
	if !safe {
		return "", ErrContextHeadroom
	}
	return final, nil
}

type requiredToolState struct {
	requirements []RequiredTool
	attempts     []int
	index        int
}

func newRequiredToolState(requirements []RequiredTool, schemas []tools.Schema) (*requiredToolState, error) {
	enabled := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		enabled[schema.Function.Name] = true
	}
	state := &requiredToolState{
		requirements: append([]RequiredTool(nil), requirements...),
		attempts:     make([]int, len(requirements)),
	}
	for index := range state.requirements {
		requirement := &state.requirements[index]
		if requirement.Name == "" || strings.TrimSpace(requirement.Name) != requirement.Name || strings.TrimSpace(requirement.CorrectivePrompt) == "" {
			return nil, &RequiredToolError{Tool: requirement.Name, Code: RequiredToolInvalid}
		}
		if !enabled[requirement.Name] {
			return nil, &RequiredToolError{Tool: requirement.Name, Code: RequiredToolNotEnabled}
		}
		if requirement.MaxAttempts <= 0 {
			requirement.MaxAttempts = 1
		}
	}
	return state, nil
}

func (s *requiredToolState) pending() *RequiredTool {
	if s == nil || s.index >= len(s.requirements) {
		return nil
	}
	return &s.requirements[s.index]
}

func (s *requiredToolState) canForce() bool {
	pending := s.pending()
	return pending != nil && s.attempts[s.index] < pending.MaxAttempts
}

func (s *requiredToolState) beginForcedAttempt() {
	if s.pending() != nil {
		s.attempts[s.index]++
	}
}

func (s *requiredToolState) observe(name string, result tools.Result) {
	pending := s.pending()
	if pending == nil || name != pending.Name {
		return
	}
	if _, hasError := result.Payload["error"]; hasError || result.BudgetExhausted {
		return
	}
	if pending.RequireContent && result.ContentBytes <= 0 {
		return
	}
	s.index++
}

func (s *requiredToolState) exhaustedError() error {
	pending := s.pending()
	if pending == nil {
		return nil
	}
	return &RequiredToolError{
		Tool: pending.Name, Code: RequiredToolAttemptsExhausted,
		Attempts: s.attempts[s.index],
	}
}

func appendToolsFreeAssistant(messages []modelMessage, msg modelMessage) []modelMessage {
	if msg.Content == nil && len(msg.ProviderItems) == 0 {
		return messages
	}
	return append(messages, modelMessage{Role: "assistant", Content: msg.Content, ProviderItems: msg.ProviderItems})
}

func (c *Client) runToolLoopFinalizeRound(ctx context.Context, messages []modelMessage, headroom contextHeadroom) (string, error) {
	messages = append(messages, modelMessage{Role: "user", Content: strPtr(agForceFinalizePrompt)})
	prepared, safe := prepareContextRequest(ctx, messages, 0, headroom, "finalize")
	if !safe {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "headroom_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "unavailable", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		return "", ErrContextHeadroom
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "requested"})
	resp, err := c.callModel(ctx, prepared, nil, nil)
	if err != nil {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "error", ErrorCode: "model_request_error"})
		return "", err
	}
	if !resp.HasMessage {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: "missing_message"})
		return "", nil
	}
	captureToolLoopContinuation(ctx, c, appendToolsFreeAssistant(prepared, resp.Message))
	if resp.Message.Content == nil {
		code := "nil_content"
		if len(resp.Message.ToolCalls) > 0 {
			code = "unexpected_tool_call"
		}
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: code})
		return "", nil
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "success"})
	return *resp.Message.Content, nil
}

func toolLoopPath(arguments string) string {
	var value struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return ""
	}
	return value.Path
}

// dispatchToolLoop routes one tool call through the registry and returns the
// capped JSON payload to hand back to the model. Tools that gate on remaining
// bytes see a large budget since the loop bounds work by iteration count.
func dispatchToolLoop(ctx context.Context, reg *tools.Registry, env *tools.Env, tc modelToolCall) (string, tools.Result) {
	env.RemainingModelBytes = toolLoopBudget
	env.RemainingGCSBytes = toolLoopBudget
	result := reg.Dispatch(ctx, env, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if result.Payload == nil {
		result.Payload = map[string]interface{}{}
	}
	if os.Getenv("AGENTIC_TRACE_TOOLS") != "" {
		flag := "ok"
		if _, hasErr := result.Payload["error"]; hasErr {
			flag = "ERROR"
		}
		log.Printf("    🔧 %s(%s) [%s]", tc.Function.Name, textutil.Truncate(tc.Function.Arguments, 140), flag)
	}
	out, _ := json.Marshal(result.Payload)
	return capJSON(string(out)), result
}
