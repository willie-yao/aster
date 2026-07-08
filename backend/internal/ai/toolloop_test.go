package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aitest"
)

// stubTool is a trivial tool for exercising the loop plumbing. It records how
// many times it was dispatched and echoes its "msg" arg back.
type stubTool struct{ calls int }

func (*stubTool) Name() string  { return "echo" }
func (*stubTool) Group() string { return "stub" }
func (*stubTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "echo",
			Description: "echo the msg arg",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"msg": map[string]interface{}{"type": "string"}},
				"required":   []string{"msg"},
			},
		},
	}
}
func (s *stubTool) Dispatch(_ context.Context, _ *tools.Env, raw json.RawMessage) tools.Result {
	s.calls++
	var a struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &a)
	return tools.Result{Payload: map[string]interface{}{"echo": a.Msg}}
}

func newLoopClient(url string) *Client {
	return NewClientWithOptions(Options{Token: "t", Endpoint: url, Model: "m"})
}

func TestToolLoop_ToolThenFinal(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "echo", map[string]any{"msg": "hi"})
	script.PushFinal(`{"files":["pkg/a.go"]}`)

	stub := &stubTool{}
	reg := tools.NewRegistry()
	reg.Register(stub)

	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MaxIters: 5},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["pkg/a.go"]}` {
		t.Errorf("final = %q, want the files JSON", out)
	}
	if stub.calls != 1 {
		t.Errorf("tool dispatched %d times, want 1", stub.calls)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (tool turn + final)", script.ChatCalls())
	}
}

func TestToolLoop_ExhaustsItersThenForceFinalize(t *testing.T) {
	script := aitest.NewScriptServer(t)
	// One tool call fills the only iteration; the loop then forces a finalize
	// round (tools omitted) which the second response answers.
	script.PushToolCall("c1", "echo", map[string]any{"msg": "again"})
	script.PushFinal(`{"files":["x.yaml"]}`)

	stub := &stubTool{}
	reg := tools.NewRegistry()
	reg.Register(stub)

	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MaxIters: 1},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["x.yaml"]}` {
		t.Errorf("finalize result = %q, want the files JSON", out)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (tool turn + forced finalize)", script.ChatCalls())
	}
}

func TestToolLoop_ImmediateFinal(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushFinal(`done`)

	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != "done" {
		t.Errorf("out = %q, want done", out)
	}
}

// TestToolLoop_GroupNameNotResolved guards the bug where a caller passes a group
// alias instead of resolved tool names: reg.Schemas returns nothing, so the loop
// would silently run with no tools. ToolLoop now errors instead.
func TestToolLoop_GroupNameNotResolved(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&stubTool{}) // group "stub", tool name "echo"
	_, err := newLoopClient("http://unused").ToolLoop(
		context.Background(), "sys", "user", reg, []string{"stub"}, &tools.Env{},
		ToolLoopOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "no tools enabled") {
		t.Errorf("expected no-tools-enabled error for a group alias, got %v", err)
	}
}

// TestToolLoop_MinToolCallsNudgesOnce verifies a premature tools-free answer is
// nudged once, then a second insistent answer is accepted so the loop still
// terminates.
func TestToolLoop_MinToolCallsNudgesOnce(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushFinal(`{"files":["a.go"]}`) // premature: no tool calls yet
	script.PushFinal(`{"files":["a.go"]}`) // after the nudge, model insists

	reg := tools.NewRegistry()
	reg.Register(&stubTool{})
	out, err := newLoopClient(script.URL).ToolLoop(
		context.Background(), "sys", "user", reg, []string{"echo"}, &tools.Env{},
		ToolLoopOptions{MinToolCalls: 1},
	)
	if err != nil {
		t.Fatalf("ToolLoop error: %v", err)
	}
	if out != `{"files":["a.go"]}` {
		t.Errorf("out = %q", out)
	}
	if script.ChatCalls() != 2 {
		t.Errorf("chat calls = %d, want 2 (premature answer + nudge retry)", script.ChatCalls())
	}
}
