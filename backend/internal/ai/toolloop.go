package ai

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

func appendToolsFreeAssistant(messages []modelMessage, msg modelMessage) []modelMessage {
	if msg.Content == nil && len(msg.ProviderItems) == 0 {
		return messages
	}
	return append(messages, modelMessage{Role: "assistant", Content: msg.Content, Phase: msg.Phase, ProviderItems: msg.ProviderItems})
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
