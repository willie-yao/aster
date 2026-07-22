package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func sysAndTask() []modelMessage {
	return []modelMessage{
		{Role: "system", Content: strPtr("system prompt")},
		{Role: "user", Content: strPtr("analyze this failure")},
	}
}

func toolMsg(id string, n int) modelMessage {
	return modelMessage{Role: "tool", ToolCallID: id, Content: strPtr(strings.Repeat("x", n))}
}

func asstToolCall(id, reasoning string) modelMessage {
	return modelMessage{
		Role:      "assistant",
		Content:   strPtr(reasoning),
		ToolCalls: []modelToolCall{{ID: id, Type: "function", Function: modelFunction{Name: "read_artifact", Arguments: `{"path":"a"}`}}},
	}
}

func conversation(numTools, toolSize int) []modelMessage {
	msgs := sysAndTask()
	for i := 0; i < numTools; i++ {
		id := string(rune('a' + i))
		msgs = append(msgs, asstToolCall(id, "let me read artifact"), toolMsg(id, toolSize))
	}
	return msgs
}

func countStubbed(msgs []modelMessage) int {
	n := 0
	for i := range msgs {
		if isStubbed(msgs[i].Content) {
			n++
		}
	}
	return n
}

func TestCompactMessages_DisabledWhenBudgetZero(t *testing.T) {
	msgs := conversation(8, 4000)
	before := requestSizeEstimate(msgs, 0)
	out, elided := compactMessages(msgs, 0, 0)
	if elided != 0 || requestSizeEstimate(out, 0) != before {
		t.Fatalf("budget=0 must be a no-op: elided=%d", elided)
	}
}

func TestCompactMessages_NoopUnderBudget(t *testing.T) {
	msgs := conversation(2, 1000)
	out, elided := compactMessages(msgs, 0, 1_000_000)
	if elided != 0 {
		t.Fatalf("under-budget conversation must not be compacted: elided=%d", elided)
	}
	if countStubbed(out) != 0 {
		t.Fatalf("no message should be stubbed under budget")
	}
}

func TestCompactMessages_ElidesOldestKeepsRecentAndPreamble(t *testing.T) {
	msgs := conversation(8, 2000) // 8 tool results @2KB
	budget := 8000
	out, elided := compactMessages(msgs, 0, budget)

	if elided == 0 {
		t.Fatal("expected compaction to elide messages")
	}
	if requestSizeEstimate(out, 0) > budget {
		t.Fatalf("post-compaction estimate %d exceeds budget %d", requestSizeEstimate(out, 0), budget)
	}
	if isStubbed(out[0].Content) || *out[0].Content != "system prompt" {
		t.Errorf("system prompt must be preserved")
	}
	if isStubbed(out[1].Content) || *out[1].Content != "analyze this failure" {
		t.Errorf("task must be preserved")
	}
	// The most recent tool result stays full due to the recent-3 preference.
	last := out[len(out)-1]
	if last.Role != "tool" || isStubbed(last.Content) {
		t.Errorf("most recent tool result should be kept verbatim, got stubbed=%v", isStubbed(last.Content))
	}
	if !isStubbed(out[3].Content) { // index 3 = first tool result
		t.Errorf("oldest tool result should be stubbed")
	}
}

func TestCompactMessages_PreservesToolPairing(t *testing.T) {
	msgs := conversation(8, 3000)
	out, _ := compactMessages(msgs, 0, 6000)
	// Every tool message must still carry its ToolCallID, and every
	// assistant tool_calls entry must keep its ID, so the chat protocol
	// stays valid after compaction.
	for i := range out {
		if out[i].Role == "tool" && out[i].ToolCallID == "" {
			t.Errorf("msg %d: tool message lost its ToolCallID", i)
		}
		for _, tc := range out[i].ToolCalls {
			if tc.ID == "" {
				t.Errorf("msg %d: assistant tool_call lost its ID", i)
			}
		}
	}
}

func TestCompactMessages_Idempotent(t *testing.T) {
	msgs := conversation(8, 2000)
	out, first := compactMessages(msgs, 0, 8000)
	stubbedAfterFirst := countStubbed(out)
	out, second := compactMessages(out, 0, 8000)
	if second != 0 {
		t.Errorf("second compaction at the same budget should elide nothing, got %d", second)
	}
	if countStubbed(out) != stubbedAfterFirst {
		t.Errorf("idempotent compaction changed stub count: %d -> %d", stubbedAfterFirst, countStubbed(out))
	}
	_ = first
}

func TestCompactMessages_FallsBackToRecentToolsThenReasoning(t *testing.T) {
	// Budget so tight that even keeping the recent tools overflows, forcing
	// the graduated fallback into recent tool results and then assistant
	// reasoning.
	msgs := conversation(5, 3000)
	budget := 2500
	out, elided := compactMessages(msgs, 0, budget)
	if elided == 0 {
		t.Fatal("expected aggressive compaction")
	}
	for i := range out {
		if out[i].Role == "tool" && !isStubbed(out[i].Content) {
			t.Errorf("msg %d: tool result should be stubbed under a tight budget", i)
		}
	}
}

func TestStubContent_KeepsHeadAndMarker(t *testing.T) {
	orig := "PATH=artifacts/foo/bar.log STATUS=ok " + strings.Repeat("y", 5000)
	stub := stubContent(orig)
	if !strings.HasPrefix(stub, "PATH=artifacts/foo/bar.log") {
		t.Errorf("stub should keep the head hint, got %q", stub[:40])
	}
	if !strings.Contains(stub, elisionMarker) {
		t.Errorf("stub should carry the elision marker")
	}
	if len(stub) >= len(orig) {
		t.Errorf("stub (%d) should be shorter than original (%d)", len(stub), len(orig))
	}
}

func TestRequestSizeEstimate_IncludesSchemaBytes(t *testing.T) {
	msgs := sysAndTask()
	base := requestSizeEstimate(msgs, 0)
	withSchema := requestSizeEstimate(msgs, 1000)
	if withSchema-base != 1000 {
		t.Errorf("schema bytes should add to the estimate: delta=%d want 1000", withSchema-base)
	}
}

func TestRequestSizeEstimateCountsProviderItems(t *testing.T) {
	small := []modelMessage{{Role: "assistant"}}
	large := []modelMessage{{Role: "assistant", ToolCalls: []modelToolCall{{ID: "c", Type: "function", Function: modelFunction{Name: "read", Arguments: "{}"}}}, ProviderItems: []json.RawMessage{json.RawMessage(strings.Repeat("x", 1024))}}}
	if requestSizeEstimate(large, 0) < requestSizeEstimate(small, 0)+1024 {
		t.Fatal("provider items were not counted")
	}
	compacted, _ := compactMessages(large, 0, 1)
	if len(compacted[0].ProviderItems) != 1 {
		t.Fatal("compaction dropped provider state")
	}
}

func TestCompactMessagesRemovesResponsesRoundAtomically(t *testing.T) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("user")},
		{Role: "assistant", ToolCalls: []modelToolCall{{ID: "call-1"}}, ProviderItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", 2000) + `"}`)}},
		{Role: "tool", ToolCallID: "call-1", Content: strPtr(strings.Repeat("y", 1000))},
		{Role: "user", Content: strPtr("continue")},
	}
	got, _ := compactMessages(messages, 0, 300)
	for _, message := range got {
		if message.ToolCallID == "call-1" || (message.Role == "assistant" && len(message.ProviderItems) > 0) {
			t.Fatalf("orphaned Responses round: %+v", got)
		}
	}
}

func TestCompactMessagesDropsNoToolResponsesState(t *testing.T) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr("system")}, {Role: "user", Content: strPtr("user")},
		{Role: "assistant", Content: strPtr("draft"), ProviderItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", 2000) + `"}`)}},
		{Role: "user", Content: strPtr("revise")},
	}
	got, elided := compactMessages(messages, 0, 600)
	if elided != 1 {
		t.Fatalf("elided = %d, want 1", elided)
	}
	if len(got[2].ProviderItems) != 0 || got[2].Content == nil || *got[2].Content != "draft" {
		t.Fatalf("tools-free Responses turn was not reduced to text: %+v", got[2])
	}
}
