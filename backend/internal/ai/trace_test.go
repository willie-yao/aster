package ai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceStoreBoundsAndRedacts(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test https://secret.example Authorization: Bearer top-secret", APIMode: APIResponses})
	for i := 0; i < analysisTraceMaxEvents+2; i++ {
		trace.Record(TraceEvent{Kind: "model_request", ErrorCode: "provider_status"})
	}
	trace.Finish("error", nil)

	snapshot := store.Snapshot()
	if len(snapshot.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(snapshot.Traces))
	}
	got := snapshot.Traces[0]
	if !got.Truncated || len(got.Events) != analysisTraceMaxEvents {
		t.Fatalf("trace bounds = truncated:%v events:%d", got.Truncated, len(got.Events))
	}
	if strings.Contains(got.TestName, "secret.example") || !strings.Contains(got.TestName, "[redacted-url]") {
		t.Fatalf("metadata was not redacted: %q", got.TestName)
	}
	if strings.Contains(got.TestName, "top-secret") {
		t.Fatalf("credential was not redacted: %q", got.TestName)
	}
	if got.Events[0].ErrorCode != "provider_status" {
		t.Fatalf("error code = %q", got.Events[0].ErrorCode)
	}
}

func TestTraceErrorCodeDoesNotPersistProviderBody(t *testing.T) {
	err := errors.New(`responses status "incomplete": {"prompt":"private prompt","arguments":"secret"}`)
	if got := traceErrorCode(err); got != "provider_status" || strings.Contains(got, "private") {
		t.Fatalf("traceErrorCode = %q", got)
	}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job"})
	trace.Finish("error", err)
	raw, marshalErr := json.Marshal(store.Snapshot())
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(raw), "private prompt") || strings.Contains(string(raw), "arguments") {
		t.Fatalf("provider body persisted: %s", raw)
	}
}

func TestTraceStoreSaveUsesPrivateSchema(t *testing.T) {
	store := NewTraceStore()
	second := store.Start(TraceMetadata{JobID: "job-b", BuildID: "2", TestName: "test-b", APIMode: APIChatCompletions})
	second.Record(TraceEvent{Kind: "tool_call", Tool: "read_artifact", Bytes: 42})
	second.Finish("success", nil)
	first := store.Start(TraceMetadata{JobID: "job-a", BuildID: "1", TestName: "test-a", APIMode: APIResponses})
	first.Finish("cache_hit", nil)
	store.mu.Lock()
	for i := range store.traces {
		store.traces[i].StartedAt = "2026-07-22T00:00:00Z"
	}
	store.mu.Unlock()

	path := filepath.Join(t.TempDir(), "ai_traces.json")
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got AnalysisTraceFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != analysisTraceVersion || len(got.Traces) != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Traces[0].Backend != "inprocess" || got.Traces[1].Backend != "inprocess" {
		t.Fatalf("backends = %+v", got.Traces)
	}
	if got.Traces[0].JobID != "job-a" || got.Traces[1].JobID != "job-b" {
		t.Fatalf("trace order = %+v", got.Traces)
	}
	if strings.Contains(string(data), "endpoint") || strings.Contains(string(data), "model") {
		t.Fatalf("trace leaked provider configuration: %s", data)
	}
}

func TestTraceStoreCapsCompletedTraces(t *testing.T) {
	store := NewTraceStore()
	for i := 0; i < analysisTraceMaxTraces+2; i++ {
		trace := store.Start(TraceMetadata{JobID: "job"})
		trace.Finish("success", nil)
	}
	got := store.Snapshot()
	if len(got.Traces) != analysisTraceMaxTraces || got.DroppedTraces != 2 {
		t.Fatalf("traces=%d dropped=%d", len(got.Traces), got.DroppedTraces)
	}
}
