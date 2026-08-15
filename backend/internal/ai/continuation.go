package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

const toolLoopContinuationLifetime = 15 * time.Minute

var (
	ErrToolLoopContinuationInvalid = errors.New("tool loop continuation is invalid")
	ErrToolLoopContinuationUsed    = errors.New("tool loop continuation was already used")
	ErrToolLoopContinuationStale   = errors.New("tool loop continuation is stale")
	ErrToolLoopContinuationPrivate = errors.New("tool loop continuation cannot be serialized")
)

// ToolLoopContinuation is one opaque, single-use in-memory tool conversation.
// It never crosses a cache or public contract boundary.
type ToolLoopContinuation struct {
	state *toolLoopContinuationState
}

type toolLoopContinuationState struct {
	mu                sync.Mutex
	client            *Client
	messages          []modelMessage
	contextByteBudget int
	expiresAt         time.Time
	used              bool
}

// MarshalJSON rejects persistence of private conversation state.
func (ToolLoopContinuation) MarshalJSON() ([]byte, error) {
	return nil, ErrToolLoopContinuationPrivate
}

// MarshalText rejects text-based persistence of private conversation state.
func (ToolLoopContinuation) MarshalText() ([]byte, error) {
	return nil, ErrToolLoopContinuationPrivate
}

// GobEncode rejects binary persistence of private conversation state.
func (ToolLoopContinuation) GobEncode() ([]byte, error) {
	return nil, ErrToolLoopContinuationPrivate
}

// Discard clears an unused continuation without sending another model request.
func (continuation ToolLoopContinuation) Discard() {
	if continuation.state == nil {
		return
	}
	continuation.state.mu.Lock()
	continuation.state.used = true
	continuation.state.messages = nil
	continuation.state.mu.Unlock()
}

// ToolLoopWithContinuation runs ToolLoop and retains its bounded message
// history for one private structured continuation.
func (c *Client) ToolLoopWithContinuation(
	ctx context.Context,
	system, user string,
	registry *tools.Registry,
	enabled []string,
	env *tools.Env,
	options ToolLoopOptions,
) (string, ToolLoopContinuation, error) {
	capture := &toolLoopContinuationCapture{client: c, contextByteBudget: options.ContextByteBudget}
	ctx = context.WithValue(ctx, toolLoopContinuationCaptureContextKey{}, capture)
	memo, err := c.ToolLoop(ctx, system, user, registry, enabled, env, options)
	if err != nil {
		return "", ToolLoopContinuation{}, err
	}
	capture.mu.Lock()
	continuation := capture.continuation
	capture.mu.Unlock()
	if continuation.state == nil {
		return "", ToolLoopContinuation{}, ErrToolLoopContinuationInvalid
	}
	return memo, continuation, nil
}

// ContinueStructured consumes one private tool-loop continuation and appends a
// structured finalization instruction to the same bounded conversation.
func (c *Client) ContinueStructured(ctx context.Context, continuation ToolLoopContinuation, instruction string, format ResponseFormat, validate StructuredValidator) error {
	_, err := c.ContinueStructuredWithMetadata(ctx, continuation, instruction, format, validate)
	return err
}

// ContinueStructuredWithMetadata uses the shared message-aware structured
// finalizer while preserving bounded attempt telemetry.
func (c *Client) ContinueStructuredWithMetadata(ctx context.Context, continuation ToolLoopContinuation, instruction string, format ResponseFormat, validate StructuredValidator) (StructuredCompletionMetadata, error) {
	metadata := StructuredCompletionMetadata{Attempts: []StructuredAttemptMetadata{}}
	if validate == nil {
		return metadata, fmt.Errorf("structured completion validator is required")
	}
	if strings.TrimSpace(format.Name) == "" || len(format.Schema) == 0 {
		return metadata, fmt.Errorf("structured completion schema is required")
	}
	if strings.TrimSpace(instruction) == "" {
		return metadata, fmt.Errorf("structured continuation instruction is required")
	}
	if err := ctx.Err(); err != nil {
		return metadata, err
	}
	messages, contextByteBudget, err := c.consumeToolLoopContinuation(continuation)
	if err != nil {
		return metadata, err
	}
	messages = append(messages, modelMessage{Role: "user", Content: strPtr(instruction)})
	if contextByteBudget > 0 {
		toolSchema := tools.Schema{Type: "function", Function: tools.FunctionDecl{
			Name: format.Name, Description: format.Description, Parameters: format.Schema, Strict: true,
		}}
		schemaBytes := schemaPayloadBytes([]tools.Schema{toolSchema})
		var elided int
		messages, elided = compactMessages(messages, schemaBytes, contextByteBudget)
		afterBytes := requestSizeEstimate(messages, schemaBytes)
		if elided > 0 {
			log.Printf("  ✂ structured continuation: elided %d message(s) to fit ~%d-byte window", elided, contextByteBudget)
			recordTrace(ctx, TraceEvent{
				Kind: "context_compaction", Outcome: "structured_continuation", Elided: elided,
				Bytes: afterBytes, MessageCount: len(messages),
			})
		}
		if afterBytes > contextByteBudget {
			recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "structured_continuation", Bytes: afterBytes, MessageCount: len(messages)})
			return metadata, ErrContextHeadroom
		}
	}
	result, err := c.completeStructuredMessagesWithMetadata(
		ctx, messages, format, defaultStructuredResponseBytes, true,
		func(raw string) structuredValidationResult { return evaluateStructuredCandidates(raw, validate) },
	)
	return result.Metadata, err
}

func (c *Client) consumeToolLoopContinuation(continuation ToolLoopContinuation) ([]modelMessage, int, error) {
	if continuation.state == nil {
		return nil, 0, ErrToolLoopContinuationInvalid
	}
	state := continuation.state
	state.mu.Lock()
	defer state.mu.Unlock()
	switch {
	case state.client != c:
		return nil, 0, ErrToolLoopContinuationInvalid
	case state.used:
		return nil, 0, ErrToolLoopContinuationUsed
	case time.Now().After(state.expiresAt):
		state.used = true
		state.messages = nil
		return nil, 0, ErrToolLoopContinuationStale
	case len(state.messages) == 0:
		state.used = true
		return nil, 0, ErrToolLoopContinuationInvalid
	}
	state.used = true
	messages := cloneModelMessages(state.messages)
	state.messages = nil
	return messages, state.contextByteBudget, nil
}

type toolLoopContinuationCapture struct {
	mu                sync.Mutex
	client            *Client
	contextByteBudget int
	continuation      ToolLoopContinuation
}

type toolLoopContinuationCaptureContextKey struct{}

func captureToolLoopContinuation(ctx context.Context, client *Client, messages []modelMessage) {
	if ctx == nil {
		return
	}
	capture, _ := ctx.Value(toolLoopContinuationCaptureContextKey{}).(*toolLoopContinuationCapture)
	if capture == nil || capture.client != client {
		return
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.continuation.state != nil {
		return
	}
	capture.continuation = ToolLoopContinuation{state: &toolLoopContinuationState{
		client: client, messages: cloneModelMessages(messages),
		contextByteBudget: capture.contextByteBudget,
		expiresAt:         time.Now().Add(toolLoopContinuationLifetime),
	}}
}

func cloneModelMessages(messages []modelMessage) []modelMessage {
	out := make([]modelMessage, len(messages))
	for index, message := range messages {
		out[index] = message
		if message.Content != nil {
			content := *message.Content
			out[index].Content = &content
		}
		out[index].ToolCalls = append([]modelToolCall(nil), message.ToolCalls...)
		out[index].ProviderItems = make([]json.RawMessage, len(message.ProviderItems))
		for itemIndex, item := range message.ProviderItems {
			out[index].ProviderItems[itemIndex] = append(json.RawMessage(nil), item...)
		}
	}
	return out
}
