package ai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// toolLoopPhase names a position in one loop turn so callers can report
// progress without the loop knowing their vocabulary.
type toolLoopPhase string

const (
	// toolLoopPhaseTurn opens every turn after the first.
	toolLoopPhaseTurn toolLoopPhase = "turn"
	// toolLoopPhaseDispatch fires once this turn's tool calls are about to run.
	toolLoopPhaseDispatch toolLoopPhase = "dispatch"
)

// toolLoopAnswer is one tools-free model turn handed to the decision hook.
type toolLoopAnswer struct {
	Content string
	Message modelMessage
	Iter    int
	Calls   int
}

// toolLoopDecision is what the caller wants after a tools-free answer: accept
// it, continue with a corrective user message, or stop with an error.
type toolLoopDecision struct {
	stop      bool
	err       error
	prompt    string // corrective user message; empty means accept
	forceTool string // the next turn must call exactly this tool
	grantIter bool   // the corrective round does not consume from maxIters
}

func toolLoopAccept() toolLoopDecision { return toolLoopDecision{} }

func toolLoopStop(err error) toolLoopDecision {
	return toolLoopDecision{stop: true, err: err}
}

// toolLoopCorrect re-enters the loop with prompt as a user message so the model
// can fix whatever the caller rejected.
func toolLoopCorrect(prompt string) toolLoopDecision {
	return toolLoopDecision{prompt: prompt}
}

// forcing pins the corrective turn to one tool call.
func (d toolLoopDecision) forcing(name string) toolLoopDecision {
	d.forceTool = name
	return d
}

func (d toolLoopDecision) corrective() bool { return !d.stop && d.prompt != "" }

// toolLoopDispatch is one dispatched tool call and its outcome. The onDispatch
// hook may rewrite Envelope before it is appended to the conversation.
type toolLoopDispatch struct {
	Call     modelToolCall
	Forced   bool
	Envelope string
	Payload  map[string]interface{}
	Result   tools.Result
}

// toolLoopParams configures one run of the shared tool-calling loop. Only
// messages, schemas, and dispatch are required.
type toolLoopParams struct {
	// messages seeds the conversation and is never mutated in place.
	messages []modelMessage
	schemas  []tools.Schema
	// maxIters bounds the tool-call rounds. Defaults to 8 when <= 0.
	maxIters int
	// maxToolCalls caps dispatched calls across the run. Zero means unlimited.
	maxToolCalls int
	// singleToolCall requests one tool call per turn for endpoints whose chat
	// template rejects parallel calls.
	singleToolCall bool
	// contextByteBudget, when > 0, compacts the message list before each turn.
	contextByteBudget int
	// strictContextBudget fails the run when a compacted request still overruns
	// contextByteBudget instead of sending it anyway.
	strictContextBudget bool
	// dispatch runs one tool call and returns the model-bound envelope, the
	// structured payload behind it, and the raw tool result.
	dispatch func(context.Context, modelToolCall) (string, map[string]interface{}, tools.Result)
	// onTurn observes every model message, including tool-calling ones.
	onTurn func(modelMessage)
	// onAnswer decides what to do with a tools-free answer. A nil hook accepts.
	onAnswer func(toolLoopAnswer) toolLoopDecision
	// onDispatch observes each dispatched call and may rewrite its envelope.
	onDispatch func(*toolLoopDispatch)
	// progress reports loop position for caller-side status reporting.
	progress func(toolLoopPhase)
}

// toolLoopResult is what one loop run produced. It is returned alongside any
// error so callers can report counters from a failed run.
type toolLoopResult struct {
	// Content is the accepted tools-free answer, empty when the loop ended
	// without one.
	Content string
	// Messages is the accumulated conversation, ready for a caller-owned
	// finalization round.
	Messages []modelMessage
	Calls    int
	// ModelCalls counts model turns; ProviderAttempts counts underlying
	// provider requests, which retries can make larger.
	ModelCalls       int
	ProviderAttempts int
	ProviderMs       int
	// BudgetExhausted reports that the loop ended without an accepted
	// tools-free answer, either out of iterations or out of tool calls.
	BudgetExhausted bool
}

// toolLoopModelError reports a failed or empty model turn so each caller can
// classify it in its own vocabulary.
type toolLoopModelError struct {
	iter  int
	empty bool
	err   error
}

func (e *toolLoopModelError) Error() string {
	if e.empty {
		return fmt.Sprintf("tool loop iter %d: empty choices", e.iter+1)
	}
	return fmt.Sprintf("tool loop iter %d: %v", e.iter+1, e.err)
}

func (e *toolLoopModelError) Unwrap() error { return e.err }

// errToolLoopContextBudget reports that a compacted request still exceeds the
// caller's context budget. Only returned when strictContextBudget is set.
var errToolLoopContextBudget = errors.New("tool loop: request exceeds the context byte budget after compaction")

// runToolLoop drives the shared tool-calling round: compact, call the model,
// hand a tools-free answer to onAnswer, otherwise dispatch this turn's tool
// calls and repeat. It never finalizes; a caller that gets BudgetExhausted
// decides how to wring an answer out of the returned Messages.
func (c *Client) runToolLoop(ctx context.Context, params toolLoopParams) (toolLoopResult, error) {
	maxIters := params.maxIters
	if maxIters <= 0 {
		maxIters = 8
	}
	schemaBytes := schemaPayloadBytes(params.schemas)
	messages := append([]modelMessage(nil), params.messages...)
	result := toolLoopResult{}

	var parallelToolCalls *bool
	if params.singleToolCall {
		value := false
		parallelToolCalls = &value
	}

	forcedName := ""
	for iter := 0; iter < maxIters; iter++ {
		if err := ctx.Err(); err != nil {
			result.Messages = messages
			return result, err
		}
		if iter > 0 && params.progress != nil {
			params.progress(toolLoopPhaseTurn)
		}
		if params.contextByteBudget > 0 {
			var elided int
			messages, elided = compactMessages(messages, schemaBytes, params.contextByteBudget)
			if elided > 0 {
				log.Printf("  ✂ tool loop: elided %d message(s) to fit ~%d-byte window", elided, params.contextByteBudget)
			}
			if params.strictContextBudget && requestSizeEstimate(messages, schemaBytes) > params.contextByteBudget {
				result.Messages = messages
				return result, errToolLoopContextBudget
			}
		}

		request := modelRequest{
			Model: c.model, Messages: messages, Tools: params.schemas,
			ParallelToolCalls: parallelToolCalls,
		}
		if forcedName != "" {
			request.ToolChoice = &ToolChoice{Name: forcedName}
			parallel := false
			request.ParallelToolCalls = &parallel
		}
		providerStart := time.Now()
		response, err := c.callModelRequest(ctx, request)
		result.ProviderMs += int(time.Since(providerStart) / time.Millisecond)
		result.ModelCalls++
		result.ProviderAttempts += modelResponseAttempts(response)
		result.Messages = messages
		if err != nil {
			return result, &toolLoopModelError{iter: iter, err: err}
		}
		if response == nil || !response.HasMessage {
			return result, &toolLoopModelError{iter: iter, empty: true}
		}
		message := response.Message
		if params.onTurn != nil {
			params.onTurn(message)
		}

		if len(message.ToolCalls) == 0 {
			content := ""
			if message.Content != nil {
				content = *message.Content
			}
			decision := toolLoopAccept()
			if params.onAnswer != nil {
				decision = params.onAnswer(toolLoopAnswer{
					Content: content, Message: message, Iter: iter, Calls: result.Calls,
				})
			}
			if decision.stop {
				result.Messages = messages
				return result, decision.err
			}
			if decision.corrective() {
				messages = appendToolsFreeAssistant(messages, message)
				messages = append(messages, modelMessage{Role: "user", Content: strPtr(decision.prompt)})
				forcedName = decision.forceTool
				if decision.grantIter {
					maxIters++
				}
				continue
			}
			messages = appendToolsFreeAssistant(messages, message)
			result.Messages = messages
			result.Content = content
			return result, nil
		}
		forced := forcedName != ""
		forcedName = ""

		if params.progress != nil {
			params.progress(toolLoopPhaseDispatch)
		}
		toolCalls, dropped := limitToolCalls(message.ToolCalls, params.singleToolCall || forced)
		if dropped > 0 {
			log.Printf("  ⤵ single_tool_call: executing 1 of %d tool calls, dropping %d", len(message.ToolCalls), dropped)
		}
		if params.maxToolCalls > 0 {
			remaining := params.maxToolCalls - result.Calls
			if remaining <= 0 {
				result.BudgetExhausted = true
				break
			}
			if len(toolCalls) > remaining {
				toolCalls = toolCalls[:remaining]
				result.BudgetExhausted = true
			}
		}

		echoCalls, skippedOutputs := continuationCalls(c.apiMode, message, toolCalls)
		echo := modelMessage{Role: "assistant", ToolCalls: echoCalls, ProviderItems: message.ProviderItems}
		if message.Content != nil {
			echo.Content = message.Content
		}
		messages = append(messages, echo)
		messages = append(messages, skippedOutputs...)

		for _, toolCall := range toolCalls {
			envelope, payload, toolResult := params.dispatch(ctx, toolCall)
			result.Calls++
			dispatched := toolLoopDispatch{
				Call: toolCall, Forced: forced, Envelope: envelope,
				Payload: payload, Result: toolResult,
			}
			if params.onDispatch != nil {
				params.onDispatch(&dispatched)
			}
			messages = append(messages, modelMessage{
				Role: "tool", ToolCallID: toolCall.ID, Content: strPtr(dispatched.Envelope),
			})
		}
		if err := ctx.Err(); err != nil {
			result.Messages = messages
			return result, err
		}
	}

	result.Messages = messages
	result.BudgetExhausted = true
	return result, nil
}

// modelResponseAttempts reports how many provider requests a model turn cost.
// Transports that do not count attempts report a single one.
func modelResponseAttempts(response *modelResponse) int {
	if response == nil {
		return 0
	}
	if response.Attempts > 0 {
		return response.Attempts
	}
	return 1
}
