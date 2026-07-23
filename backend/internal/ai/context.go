package ai

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
