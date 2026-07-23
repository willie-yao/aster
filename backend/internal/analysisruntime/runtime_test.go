package analysisruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

func TestResolveContextBudgets(t *testing.T) {
	var modelCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s, want /v1/models", r.URL.Path)
		}
		modelCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"model","context_window":32768}]}`))
	}))
	defer srv.Close()
	client := ai.NewClientWithOptions(ai.Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "model", Token: "x"})

	t.Run("operator override wins without metadata request", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "128000")
		modelCalls.Store(0)
		budgets, source, err := resolveContextBudgets(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		if source != "operator" || budgets.ContextWindowTokens != 128000 {
			t.Fatalf("source=%q budgets=%+v", source, budgets)
		}
		if got := modelCalls.Load(); got != 0 {
			t.Fatalf("metadata calls=%d, want 0", got)
		}
	})

	t.Run("metadata used without override", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "")
		modelCalls.Store(0)
		budgets, source, err := resolveContextBudgets(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		if source != "detected" || budgets.ContextWindowTokens != 32768 {
			t.Fatalf("source=%q budgets=%+v", source, budgets)
		}
		if got := modelCalls.Load(); got != 1 {
			t.Fatalf("metadata calls=%d, want 1", got)
		}
	})

	t.Run("invalid override fails clearly", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "not-a-number")
		if _, _, err := resolveContextBudgets(context.Background(), client); err == nil {
			t.Fatal("expected invalid override error")
		}
	})
}
