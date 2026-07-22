package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildOrkaAnalysisTraceIsContentFree(t *testing.T) {
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	events := []executionEvent{
		{Seq: 1, Type: "TaskStarted", CreatedAt: base},
		{Seq: 2, Type: "ToolCallStarted", ToolName: "read-artifact-bscope", ToolCallID: "call-1", Content: json.RawMessage(`{"arguments":"private args"}`), CreatedAt: base.Add(time.Second)},
		{Seq: 3, Type: "ToolCallCompleted", ToolName: "read-artifact-bscope", ToolCallID: "call-1", Content: json.RawMessage(`{"resultLength":42,"result":"private result"}`), CreatedAt: base.Add(2 * time.Second)},
		{Seq: 4, Type: "ModelRequestCompleted", Provider: "private-provider", Model: "private-model", InputTokens: 10, OutputTokens: 5, StopReason: "stop", Content: json.RawMessage(`{"apiMode":"responses","responseID":"resp-1","prompt":"private prompt","reasoning":"private reasoning"}`), CreatedAt: base.Add(3 * time.Second)},
		{Seq: 5, Type: "ContextTruncated", Content: json.RawMessage(`{"messages":"private messages"}`), CreatedAt: base.Add(4 * time.Second)},
		{Seq: 6, Type: "TaskSucceeded", CreatedAt: base.Add(5 * time.Second)},
	}
	telemetry := summarizeEvents(events)
	trace := buildOrkaAnalysisTrace(orkaTraceIdentity{
		Namespace: "orka-system", TaskName: "task-a", ContractHash: "contract",
		JobID: "job", BuildID: "1", TestName: "test",
	}, telemetry)
	if trace.Backend != "orka" || trace.TaskName != "task-a" || trace.TaskNamespace != "orka-system" || trace.APIMode != "responses" || trace.Outcome != "succeeded" {
		t.Fatalf("trace metadata = %+v", trace)
	}
	if len(trace.Events) != 6 || trace.Events[2].Bytes != 42 || trace.Events[3].ResponseID != "resp-1" || trace.Events[3].InputTokens != 10 {
		t.Fatalf("trace events = %+v", trace.Events)
	}
	raw, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private args", "private result", "private prompt", "private reasoning", "private-provider", "private-model", "private messages"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("trace persisted %q: %s", private, raw)
		}
	}
}

func TestBuildOrkaAnalysisTraceRecordsTaskRetry(t *testing.T) {
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	telemetry := summarizeEvents([]executionEvent{
		{Seq: 1, Type: "TaskStarted", CreatedAt: base},
		{Seq: 2, Type: "TaskStarted", CreatedAt: base.Add(time.Second)},
		{Seq: 3, Type: "TaskFailed", CreatedAt: base.Add(2 * time.Second)},
	})
	trace := buildOrkaAnalysisTrace(orkaTraceIdentity{TaskName: "task"}, telemetry)
	if trace.ErrorCode != "task_failed" || len(trace.Events) != 3 || trace.Events[1].Retry != 1 {
		t.Fatalf("trace = %+v", trace)
	}
}
