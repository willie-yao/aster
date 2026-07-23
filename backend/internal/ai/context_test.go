package ai

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseContextWindowTokens(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		present bool
		wantErr bool
	}{
		{name: "unset", raw: "", present: false},
		{name: "valid", raw: "128000", want: 128000, present: true},
		{name: "whitespace", raw: " 65536 ", want: 65536, present: true},
		{name: "zero", raw: "0", wantErr: true},
		{name: "negative", raw: "-1", wantErr: true},
		{name: "text", raw: "many", wantErr: true},
		{name: "too large", raw: "1000000001", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := ParseContextWindowTokens(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want || present != tc.present {
				t.Fatalf("ParseContextWindowTokens(%q)=(%d,%v), want (%d,%v)", tc.raw, got, present, tc.want, tc.present)
			}
		})
	}
}

func TestDeriveContextBudgets_ReservesHeadroom(t *testing.T) {
	budgets := DeriveContextBudgets(128_000)
	if budgets.ContextWindowTokens != 128_000 {
		t.Fatalf("window = %d, want 128000", budgets.ContextWindowTokens)
	}
	if budgets.RequestTokenBudget+contextReservedTokens != budgets.ContextWindowTokens {
		t.Fatalf("request=%d reserved=%d window=%d", budgets.RequestTokenBudget, contextReservedTokens, budgets.ContextWindowTokens)
	}
	if budgets.UsedFallback {
		t.Fatal("detected window unexpectedly marked fallback")
	}
}

func TestDeriveContextBudgets_FallbackIsBounded(t *testing.T) {
	budgets := DeriveContextBudgets(0)
	if !budgets.UsedFallback || budgets.RequestTokenBudget <= 0 || budgets.ContextWindowTokens <= budgets.RequestTokenBudget {
		t.Fatalf("fallback budgets = %+v", budgets)
	}
}

func TestConservativePromptTokenEstimate_CoversDenseData(t *testing.T) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr("system")},
		{Role: "user", Content: strPtr(strings.Repeat("/very/long/artifact/path/日本語/", 600))},
		{Role: "assistant", ToolCalls: []modelToolCall{{ID: "call", Type: "function", Function: modelFunction{Name: "grep_artifact", Arguments: `{"path":"logs/a.yaml","pattern":"é"}`}}}},
		{Role: "tool", ToolCallID: "call", Content: strPtr(strings.Repeat(`{"key":"値","line":"aaaaaaaa"}`+"\n", 1200))},
	}
	bytes := requestSizeEstimate(messages, 2048)
	tokens := conservativePromptTokenEstimate(messages, 2048)
	if tokens <= bytes {
		t.Fatalf("tokens=%d must reserve provider framing above serialized bytes=%d", tokens, bytes)
	}
}

func TestPrepareContextRequest_CompactsBefore128KProviderCall(t *testing.T) {
	budgets := DeriveContextBudgets(128_000)
	messages := conversation(24, 9_000)
	schemas := 4_000
	out, fits := prepareContextRequest(context.Background(), messages, schemas, contextHeadroomFor(AgenticOptions{
		ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget:  budgets.RequestTokenBudget,
	}), "test")
	if !fits {
		t.Fatal("expected compaction to make a tool-heavy request fit")
	}
	if got := conservativePromptTokenEstimate(out, schemas); got > budgets.RequestTokenBudget {
		t.Fatalf("estimate=%d exceeds request budget=%d", got, budgets.RequestTokenBudget)
	}
	for _, message := range out {
		if message.Role == "tool" && message.ToolCallID == "" {
			t.Fatal("compaction broke a tool result pairing")
		}
	}
}

func TestAgentic_ContextHeadroomCompactsLongToolHistory(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	for i := 0; i < 16; i++ {
		srv.push(200, chatRespToolCall("call-"+string(rune('a'+i)), "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	}
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte(strings.Repeat("dense-log-field=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", 1_800))}}
	budgets := DeriveContextBudgets(128_000)
	opts := AgenticOptions{
		MaxIters:            20,
		ModelByteBudget:     1_000_000,
		GCSByteBudget:       1_000_000,
		Timeout:             30 * time.Second,
		ContextByteBudget:   budgets.ContextByteBudget,
		ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget:  budgets.RequestTokenBudget,
	}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "context", BuildID: "1", TestName: "long history", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, _, err := client.doAnalyzeAgentic(ctx, newTestAgenticInputs(t, browser, opts), "agentic:test:context-history", "sys", "user"); err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	trace.Finish("success", nil)

	srv.mu.Lock()
	requests := append([][]byte(nil), srv.requests...)
	srv.mu.Unlock()
	if len(requests) != 17 {
		t.Fatalf("provider requests=%d, want 17", len(requests))
	}
	for i, raw := range requests {
		if len(raw) > budgets.RequestTokenBudget {
			t.Fatalf("request %d wire bytes=%d exceed conservative budget=%d", i, len(raw), budgets.RequestTokenBudget)
		}
	}
	var compacted bool
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "context_compaction" {
			compacted = true
			if event.EstimatedPromptTokens > budgets.RequestTokenBudget || event.ContextLimitTokens != 128_000 || event.ReservedTokens == 0 {
				t.Fatalf("bad compaction telemetry: %+v", event)
			}
		}
	}
	if !compacted {
		t.Fatal("expected context compaction trace")
	}
}

func TestAgentic_ContextHeadroomDeniesCritiqueExpansionAndKeepsDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	large := strings.Repeat("evidence=very-long ", 10_000)
	candidate := `{"summary":"draft","is_transient":false,"root_cause":"` + large + `","severity":"High","suggested_fix":"investigate further","relevant_files":[]}`
	srv.push(200, chatRespFinal(candidate))

	client := newAgenticTestClient(t, srv.URL)
	budgets := DeriveContextBudgets(128_000)
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second,
		ContextByteBudget: budgets.ContextByteBudget, ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget: budgets.RequestTokenBudget, CritiqueMaxRetries: 1,
	}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "context", BuildID: "2", TestName: "critique", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	_, analysis, err := client.doAnalyzeAgentic(ctx, newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:context-critique", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	trace.Finish("success", nil)
	if analysis.CritiquePassed {
		t.Fatal("context-forced draft must remain uncached when critique did not pass")
	}
	if !strings.Contains(analysis.RootCause, "evidence=very-long") {
		t.Fatal("best valid draft was not retained")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("provider calls=%d, want 1 after retry expansion denial", got)
	}
	var denied bool
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "context_headroom" && event.Outcome == "retry_denied" {
			denied = true
		}
	}
	if !denied {
		t.Fatal("missing retry_denied trace")
	}
}

func TestAgentic_ContextHeadroomUncompactableRequestIsNotSent(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	budgets := DeriveContextBudgets(128_000)
	opts := AgenticOptions{
		MaxIters: 1, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second,
		ContextByteBudget: budgets.ContextByteBudget, ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget: budgets.RequestTokenBudget,
	}
	_, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:context-unavailable", strings.Repeat("system ", budgets.RequestTokenBudget), "user")
	if err != ErrContextHeadroom {
		t.Fatalf("error=%v, want ErrContextHeadroom", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Fatalf("provider calls=%d, want 0", got)
	}
}

func TestContextHeadroomTraceIsContentFree(t *testing.T) {
	event := TraceEvent{Kind: "context_headroom", Outcome: "over_budget", Bytes: 123, EstimatedPromptTokens: 456, ContextLimitTokens: 128_000, ReservedTokens: 5_120}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "artifact") || strings.Contains(string(data), "evidence") {
		t.Fatalf("context trace contains content: %s", data)
	}
}
