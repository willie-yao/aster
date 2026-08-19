package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/textutil"
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

// toolLoopNudgePrompt pushes a model that answered from the prompt alone back
// into the tools before its answer is accepted.
const toolLoopNudgePrompt = "Investigate with the tools before answering: grep and read the relevant files, then give your final JSON."

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
	// The generic loop reports a cancelled context without spending a model
	// request on it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	schemas := reg.Schemas(enabled)
	if len(schemas) == 0 {
		return "", fmt.Errorf("tool loop: no tools enabled (got %v); resolve groups with Registry.Enable first", enabled)
	}
	required, err := newRequiredToolState(opts.RequiredTools, schemas)
	if err != nil {
		return "", err
	}

	nudged := false
	result, err := c.runToolLoop(ctx, toolLoopParams{
		messages: []modelMessage{
			{Role: "system", Content: strPtr(sys)},
			{Role: "user", Content: strPtr(user)},
		},
		schemas:           schemas,
		maxIters:          opts.MaxIters,
		singleToolCall:    opts.SingleToolCall,
		contextByteBudget: opts.ContextByteBudget,
		dispatch: func(ctx context.Context, tc modelToolCall) (string, map[string]interface{}, tools.Result) {
			envelope, result := dispatchToolLoop(ctx, reg, env, tc)
			return envelope, result.Payload, result
		},
		onAnswer: func(answer toolLoopAnswer) toolLoopDecision {
			if pending := required.pending(); pending != nil {
				if !required.canForce() {
					return toolLoopStop(required.exhaustedError())
				}
				return toolLoopCorrect(pending.CorrectivePrompt).forcing(pending.Name)
			}
			// Require a minimum of investigation before accepting a final
			// answer, nudging once so a model that finalizes from the prompt
			// alone still goes and reads the source first.
			if opts.MinToolCalls > 0 && answer.Calls < opts.MinToolCalls && !nudged {
				nudged = true
				return toolLoopCorrect(toolLoopNudgePrompt)
			}
			return toolLoopAccept()
		},
		onForcedTurn: func(string) { required.beginForcedAttempt() },
		onDispatch: func(dispatched *toolLoopDispatch) {
			required.observe(dispatched.Call.Function.Name, dispatched.Result)
			_, hasError := dispatched.Result.Payload["error"]
			if opts.Observe != nil {
				opts.Observe(ToolLoopEvent{
					Name: dispatched.Call.Function.Name, Path: toolLoopPath(dispatched.Call.Function.Arguments),
					BytesFetched: dispatched.Result.BytesFetched, ContentBytes: dispatched.Result.ContentBytes,
					Error: hasError, BudgetExhausted: dispatched.Result.BudgetExhausted, Forced: dispatched.Forced,
				})
			}
			if opts.ObservePrivate != nil {
				opts.ObservePrivate(ToolLoopPrivateEvent{
					Name: dispatched.Call.Function.Name, Error: hasError,
					BudgetExhausted: dispatched.Result.BudgetExhausted,
					Forced:          dispatched.Forced, Observation: dispatched.Result.Observation,
				})
			}
		},
	})
	if err != nil {
		var modelErr *toolLoopModelError
		if errors.As(err, &modelErr) && modelErr.iter == 0 && isToolsUnsupportedError(modelErr.err) {
			return "", fmt.Errorf("%w: %v", ErrToolsUnsupported, modelErr.err)
		}
		return "", err
	}
	if !result.BudgetExhausted {
		captureToolLoopContinuation(ctx, c, result.Messages)
		return result.Content, nil
	}
	if required.pending() != nil {
		return "", required.exhaustedError()
	}

	// The model never returned a tools-free answer within the budget. Force one
	// finalize round with tools omitted so the caller still gets a response.
	headroom := contextHeadroomFor(AgenticOptions{ContextByteBudget: opts.ContextByteBudget})
	if opts.PropagateFinalizeError {
		return c.runToolLoopFinalizeRound(ctx, result.Messages, headroom)
	}
	final, _, safe := c.runFinalizeRound(ctx, result.Messages, headroom)
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
	result := dispatchToolCall(ctx, reg, env, tc, toolLoopBudget, toolLoopBudget)
	out, _ := json.Marshal(result.Payload)
	return capJSON(string(out)), result
}

// dispatchToolCall runs one registry tool under the given byte limits and
// returns its result with a non-nil payload. Envelope shaping and any
// budget accounting belong to the caller.
func dispatchToolCall(
	ctx context.Context,
	reg *tools.Registry,
	env *tools.Env,
	tc modelToolCall,
	modelLimit, gcsLimit int,
) tools.Result {
	env.RemainingModelBytes = modelLimit
	env.RemainingGCSBytes = gcsLimit
	result := reg.Dispatch(ctx, env, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if result.Payload == nil {
		// Defensive: registry promises a non-nil Payload, but never trust the
		// edge case. Empty map is safer than a nil deref downstream.
		result.Payload = map[string]interface{}{}
	}
	_, failed := result.Payload["error"]
	traceToolCall(tc, result.BytesFetched, failed)
	return result
}

// traceToolCall logs one dispatch when AGENTIC_TRACE_TOOLS is set, so
// production logs stay clean by default.
func traceToolCall(tc modelToolCall, bytesFetched int, failed bool) {
	if os.Getenv("AGENTIC_TRACE_TOOLS") == "" {
		return
	}
	flag := "ok"
	if failed {
		flag = "ERROR"
	}
	log.Printf("    🔧 %s(%s) -> %d bytes [%s]", tc.Function.Name, textutil.Truncate(tc.Function.Arguments, 140), bytesFetched, flag)
}
