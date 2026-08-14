package ai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

type evidenceSourceTool struct{ source EvidenceReadSource }

func (t *evidenceSourceTool) Name() string  { return "evidence_source" }
func (t *evidenceSourceTool) Group() string { return "test" }
func (t *evidenceSourceTool) Schema() tools.Schema {
	return tools.Schema{Type: "function", Function: tools.FunctionDecl{Name: t.Name(), Parameters: map[string]interface{}{"type": "object"}}}
}
func (t *evidenceSourceTool) Dispatch(ctx context.Context, _ *tools.Env, _ json.RawMessage) tools.Result {
	t.source = EvidenceReadSourceFromContext(ctx)
	return tools.Result{Payload: map[string]interface{}{"ok": true}}
}

func TestDispatchAgenticToolMarksModelToolEvidenceSource(t *testing.T) {
	tool := &evidenceSourceTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)
	state := &agentState{
		registry: registry, enabledTools: []string{tool.Name()}, opts: AgenticOptions{ModelByteBudget: 1024, GCSByteBudget: 1024},
	}
	dispatchAgenticTool(t.Context(), state, modelToolCall{Function: modelFunction{Name: tool.Name(), Arguments: `{}`}})
	if tool.source != EvidenceReadSourceModelTool {
		t.Fatalf("source = %q, want %q", tool.source, EvidenceReadSourceModelTool)
	}
}

func TestEvidenceReadSourceContext(t *testing.T) {
	if got := EvidenceReadSourceFromContext(t.Context()); got != "" {
		t.Fatalf("default source = %q", got)
	}
	ctx := withEvidenceReadSource(t.Context(), EvidenceReadSourceRepairInjection)
	if got := EvidenceReadSourceFromContext(ctx); got != EvidenceReadSourceRepairInjection {
		t.Fatalf("source = %q", got)
	}
}
