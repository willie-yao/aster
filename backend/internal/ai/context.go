package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// Context limits are expressed in tokens. The request estimator deliberately
// treats each serialized byte as one token, which is conservative for logs,
// YAML, JSON, paths, and non-ASCII text without binding the engine to one
// provider tokenizer.
const (
	fallbackContextWindowTokens = 64 * 1024
	maxContextWindowTokens      = 1_000_000_000

	completionHeadroomTokens      = 2 * 1024
	finalizationHeadroomTokens    = 1 * 1024
	evidenceLedgerHeadroomTokens  = 1 * 1024
	providerFramingHeadroomTokens = 1 * 1024

	requestSerializationReserveTokens = 4 * 1024
)

const contextReservedTokens = completionHeadroomTokens + finalizationHeadroomTokens + evidenceLedgerHeadroomTokens + providerFramingHeadroomTokens

const minContextWindowTokens = contextReservedTokens + requestSerializationReserveTokens + 1

// ContextBudgets are the internal budgets derived from a provider context
// window. RequestTokenBudget is deliberately smaller than the advertised
// window so retries retain room to finalize safely.
type ContextBudgets struct {
	ContextWindowTokens int
	RequestTokenBudget  int
	ContextByteBudget   int
	ModelByteBudget     int
	UsedFallback        bool
}

// ParseContextWindowTokens parses an optional operator-supplied total context
// window. The override is provider-neutral and takes precedence over metadata
// only when the operator has independent endpoint evidence.
func ParseContextWindowTokens(raw string) (int, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	tokens, err := strconv.Atoi(raw)
	if err != nil || tokens < minContextWindowTokens || tokens > maxContextWindowTokens {
		return 0, false, fmt.Errorf("AI_CONTEXT_WINDOW_TOKENS must be an integer from %d to %d", minContextWindowTokens, maxContextWindowTokens)
	}
	return tokens, true, nil
}

// DeriveContextBudgets returns bounded request and tool budgets. When a
// provider does not disclose its window, the fallback still prevents an
// unbounded conversation from reaching the transport.
func DeriveContextBudgets(providerTokens int) ContextBudgets {
	usedFallback := providerTokens <= 0
	if usedFallback {
		providerTokens = fallbackContextWindowTokens
	}
	requestTokens := providerTokens - contextReservedTokens
	if requestTokens < 1 {
		requestTokens = 1
	}

	// Keep the existing model-byte budget shape for known providers. The
	// request guard below is independent and is always conservative.
	modelBytes := providerTokens * 3 / 2
	if usedFallback {
		modelBytes = 300_000
	}
	return ContextBudgets{
		ContextWindowTokens: providerTokens,
		RequestTokenBudget:  requestTokens,
		// Compaction uses the conservative one-byte-per-token ceiling.
		ContextByteBudget: requestTokens,
		ModelByteBudget:   modelBytes,
		UsedFallback:      usedFallback,
	}
}

type contextHeadroom struct {
	limitTokens    int
	requestTokens  int
	reservedTokens int
}

func contextHeadroomFor(opts AgenticOptions) contextHeadroom {
	derived := DeriveContextBudgets(opts.ContextWindowTokens)
	requestTokens := opts.RequestTokenBudget
	if requestTokens <= 0 {
		requestTokens = derived.RequestTokenBudget
	}
	limitTokens := opts.ContextWindowTokens
	if limitTokens <= 0 {
		limitTokens = derived.ContextWindowTokens
	}
	if max := limitTokens - contextReservedTokens; max > 0 && requestTokens > max {
		requestTokens = max
	}
	if requestTokens < 1 {
		requestTokens = 1
	}
	return contextHeadroom{
		limitTokens:    limitTokens,
		requestTokens:  requestTokens,
		reservedTokens: limitTokens - requestTokens,
	}
}

// conservativePromptTokenEstimate uses one token per serialized byte plus
// a fixed transport allowance. This intentionally overestimates ordinary prose
// and avoids a tokenizer dependency while remaining safe for dense CI data.
func conservativePromptTokenEstimate(messages []modelMessage, schemaBytes int) int {
	// requestSizeEstimate covers the model-visible content and schemas. Reserve
	// additional token-equivalent framing for provider JSON, role encoding, and
	// request metadata that transports add after the neutral messages are built.
	return requestSizeEstimate(messages, schemaBytes) + requestSerializationReserveTokens
}

// prepareContextRequest compacts a request and rejects it before transport when
// the conservative estimate still exceeds the reserved request budget.
func prepareContextRequest(ctx context.Context, messages []modelMessage, schemaBytes int, headroom contextHeadroom, stage string) ([]modelMessage, bool) {
	compactionBudget := headroom.requestTokens - requestSerializationReserveTokens
	if compactionBudget < 1 {
		compactionBudget = 1
	}
	messages, elided := compactMessages(messages, schemaBytes, compactionBudget)
	afterBytes := requestSizeEstimate(messages, schemaBytes)
	afterTokens := conservativePromptTokenEstimate(messages, schemaBytes)
	if elided > 0 {
		recordTrace(ctx, TraceEvent{
			Kind: "context_compaction", Outcome: stage, Elided: elided,
			Bytes: afterBytes, EstimatedPromptTokens: afterTokens,
			ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens,
			MessageCount: len(messages),
		})
	}
	if afterTokens <= headroom.requestTokens {
		return messages, true
	}
	recordTrace(ctx, TraceEvent{
		Kind: "context_headroom", Outcome: "over_budget", Bytes: afterBytes,
		EstimatedPromptTokens: afterTokens, ContextLimitTokens: headroom.limitTokens,
		ReservedTokens: headroom.reservedTokens, MessageCount: len(messages),
	})
	return messages, false
}

const (
	// compactionTargetRatio is the fraction of ContextByteBudget compaction
	// drives toward once triggered, leaving headroom so it does not re-fire
	// every iteration.
	compactionTargetRatio = 0.7
	// compactionKeepRecentTools tool results are kept at full content when
	// possible so the model always has its latest evidence verbatim.
	compactionKeepRecentTools = 3
	// compactionStubHead is how many leading bytes of an elided tool result
	// are retained as a hint before the elision note, usually the envelope head
	// with the artifact path and status.
	compactionStubHead = 160
	// compactionMsgOverhead approximates per-message JSON framing bytes.
	compactionMsgOverhead = 48
)

// elisionMarker tags a stubbed message so compaction is idempotent across
// iterations and tests can detect elision.
const elisionMarker = "bytes elided to fit context"

func isStubbed(c *string) bool {
	return c != nil && strings.Contains(*c, elisionMarker)
}

// stubContent keeps a short head of the original tool result plus an elision
// note that tells the model how to recover the evidence.
func stubContent(orig string) string {
	head := orig
	if len(head) > compactionStubHead {
		head = head[:compactionStubHead]
	}
	return fmt.Sprintf("%s\n...[%d %s; re-call the tool if you need this evidence again]",
		head, len(orig)-len(head), elisionMarker)
}

// schemaPayloadBytes is the serialized size of the tool schemas sent on every
// loop call. Computed once per loop and added to the size estimate so
// compaction accounts for the fixed schema cost, not just message content.
func schemaPayloadBytes(schemas []tools.Schema) int {
	if len(schemas) == 0 {
		return 0
	}
	b, err := json.Marshal(schemas)
	if err != nil {
		return 0
	}
	return len(b)
}

// requestSizeEstimate approximates the serialized chat-request size in bytes:
// message content + tool-call arguments + per-message framing + the fixed
// schema payload.
func requestSizeEstimate(messages []modelMessage, schemaBytes int) int {
	total := schemaBytes + 64 // request framing
	for i := range messages {
		total += compactionMsgOverhead
		if messages[i].Content != nil && (messages[i].Role != "assistant" || len(messages[i].ProviderItems) == 0) {
			total += len(*messages[i].Content)
		}
		for _, tc := range messages[i].ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments) + 32
		}
		for _, item := range messages[i].ProviderItems {
			total += len(item)
		}
	}
	return total
}

// compactMessages elides accumulated tool results and, if needed, assistant
// reasoning so the estimated request stays under budgetBytes. Disabled when
// budgetBytes <= 0. Preserves the system prompt, task, message order, and
// tool_call_id wiring so OpenAI tool-call pairing stays valid. Returns the
// slice and the number of messages elided this call.
func compactMessages(messages []modelMessage, schemaBytes, budgetBytes int) ([]modelMessage, int) {
	if budgetBytes <= 0 || requestSizeEstimate(messages, schemaBytes) <= budgetBytes {
		return messages, 0
	}
	target := int(float64(budgetBytes) * compactionTargetRatio)
	elided := 0

	// Tool-result messages, oldest first, that are not already stubbed.
	var toolIdx []int
	for i := 2; i < len(messages); i++ {
		if messages[i].Role == "tool" && messages[i].Content != nil && !isStubbed(messages[i].Content) {
			toolIdx = append(toolIdx, i)
		}
	}
	stub := func(i int) {
		messages[i].Content = strPtr(stubContent(*messages[i].Content))
		elided++
	}
	// Stage 1: stub older tool results, preferring to keep the most recent
	// compactionKeepRecentTools verbatim.
	keepFrom := len(toolIdx) - compactionKeepRecentTools
	for p := 0; p < keepFrom && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		stub(toolIdx[p])
	}
	// Stage 2: still over target, so stub the recent tool results too.
	for p := 0; p < len(toolIdx) && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		if !isStubbed(messages[toolIdx[p]].Content) {
			stub(toolIdx[p])
		}
	}
	// Stage 3: still over target, so stub older assistant reasoning, keeping
	// the tool_calls wiring intact.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; i++ {
		m := &messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		elidedMessage := false
		if m.Content != nil && !isStubbed(m.Content) && len(*m.Content) > compactionStubHead {
			m.Content = strPtr(stubContent(*m.Content))
			elidedMessage = true
		}
		if elidedMessage {
			elided++
		}
	}
	// Stage 4: replace tools-free Responses turns with replayable text.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; i++ {
		m := &messages[i]
		if m.Role != "assistant" || len(m.ProviderItems) == 0 || len(m.ToolCalls) > 0 || m.Content == nil {
			continue
		}
		replay := responsesAssistantMessagesFromProviderItems(m.ProviderItems)
		if len(replay) == 0 {
			if m.Phase == "" {
				m.Phase = responsesPhaseFromProviderItems(m.ProviderItems)
			}
			m.ProviderItems = nil
		} else {
			replacement := make([]modelMessage, 0, len(messages)+len(replay)-1)
			replacement = append(replacement, messages[:i]...)
			replacement = append(replacement, replay...)
			replacement = append(replacement, messages[i+1:]...)
			messages = replacement
			i += len(replay) - 1
		}
		elided++
	}
	// Stage 5: remove older Responses assistant turns and their paired tool
	// outputs atomically when continuation state keeps the request over budget.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; {
		m := messages[i]
		if m.Role != "assistant" || len(m.ProviderItems) == 0 || len(m.ToolCalls) == 0 {
			i++
			continue
		}
		ids := map[string]bool{}
		for _, call := range m.ToolCalls {
			ids[call.ID] = true
		}
		end := i + 1
		for end < len(messages) && messages[end].Role == "tool" && ids[messages[end].ToolCallID] {
			end++
		}
		if end == i+1 {
			i++
			continue
		}
		elided += end - i
		messages = append(messages[:i], messages[end:]...)
	}
	return messages, elided
}
