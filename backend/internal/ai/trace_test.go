package ai

import (
	"encoding/json"
	"errors"
	"fmt"
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
		store.traces[i].RecordedAt = "2026-07-22T00:00:00Z"
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
		trace := store.Start(TraceMetadata{JobID: "job", BuildID: fmt.Sprintf("%d", i)})
		trace.Finish("success", nil)
	}
	got := store.Snapshot()
	if len(got.Traces) != analysisTraceMaxTraces || got.DroppedTraces != 2 {
		t.Fatalf("traces=%d dropped=%d", len(got.Traces), got.DroppedTraces)
	}
	builds := map[string]bool{}
	for _, trace := range got.Traces {
		builds[trace.BuildID] = true
	}
	if builds["0"] || builds["1"] || !builds[fmt.Sprintf("%d", analysisTraceMaxTraces+1)] {
		t.Fatalf("rolling trace window kept wrong builds: first=%v second=%v newest=%v", builds["0"], builds["1"], builds[fmt.Sprintf("%d", analysisTraceMaxTraces+1)])
	}
	old := AnalysisTrace{Backend: "inprocess", JobID: "old", BuildID: "old", TestName: "old", StartedAt: "2000-01-01T00:00:00Z", Outcome: "success"}
	if store.Upsert(old) {
		t.Fatal("delayed old trace displaced the rolling window")
	}
	got = store.Snapshot()
	if len(got.Traces) != analysisTraceMaxTraces || got.DroppedTraces != 3 {
		t.Fatalf("after delayed trace: traces=%d dropped=%d", len(got.Traces), got.DroppedTraces)
	}
}

func TestTraceStoreSnapshotWithinLimitEvictsOldest(t *testing.T) {
	older := AnalysisTrace{
		Backend: "inprocess", JobID: "old", BuildID: "1", TestName: "test", StartedAt: "2026-07-22T08:00:00Z", Outcome: "success",
		Events: []TraceEvent{{Kind: "model_request", ResponseID: strings.Repeat("a", 1000)}},
	}
	newer := AnalysisTrace{
		Backend: "inprocess", JobID: "new", BuildID: "2", TestName: "test", StartedAt: "2026-07-22T08:01:00Z", Outcome: "success",
		Events: []TraceEvent{{Kind: "model_request", ResponseID: strings.Repeat("b", 1000)}},
	}
	one := NewTraceStore()
	one.Upsert(newer)
	oneEncoded, err := json.MarshalIndent(one.Snapshot(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	store := NewTraceStore()
	store.Upsert(older)
	store.Upsert(newer)
	limit := len(oneEncoded) + 256
	snapshot, err := store.snapshotWithinLimit(limit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > limit || len(snapshot.Traces) != 1 || snapshot.Traces[0].JobID != "new" || snapshot.DroppedTraces != 1 {
		t.Fatalf("bounded snapshot = traces:%+v dropped:%d bytes:%d limit:%d", snapshot.Traces, snapshot.DroppedTraces, len(encoded), limit)
	}
}

func TestTraceStoreRetentionBoundary(t *testing.T) {
	store := NewTraceStore()
	store.traces = []AnalysisTrace{{
		Backend: "orka", TaskName: "newest-window-start", RecordedAt: "2026-07-22T09:00:00Z", Outcome: "succeeded",
	}}
	store.dropped = 3
	if !store.BeforeRetention("2026-07-22T08:59:59Z") {
		t.Fatal("analysis older than the retention boundary was not recognized")
	}
	if store.BeforeRetention("2026-07-22T09:00:01Z") {
		t.Fatal("analysis newer than the retention boundary was treated as evicted")
	}
	if got := store.Snapshot().RetainedSince; got != "2026-07-22T09:00:00Z" {
		t.Fatalf("retained_since = %q", got)
	}
}

func TestTraceStoreRejectsStaleTaskReplacement(t *testing.T) {
	store := NewTraceStore()
	current := AnalysisTrace{
		Backend: "orka", TaskNamespace: "orka-system", TaskName: "task", Outcome: "succeeded", ElapsedMs: 2000,
		Events: []TraceEvent{{Kind: "task", Outcome: "started"}, {Kind: "model_request", Outcome: "success", ResponseID: "resp"}, {Kind: "task", Outcome: "succeeded", ElapsedMs: 2000}},
	}
	if !store.Upsert(current) {
		t.Fatal("initial trace was not stored")
	}
	stale := AnalysisTrace{
		Backend: "orka", TaskNamespace: "orka-system", TaskName: "task", Outcome: "unknown", ElapsedMs: 1000,
		Events: []TraceEvent{{Kind: "task", Outcome: "started"}, {Kind: "model_request", Outcome: "success"}},
	}
	if store.Upsert(stale) {
		t.Fatal("stale Task trace replaced a terminal trace")
	}
	got := store.Snapshot().Traces[0]
	if got.Outcome != "succeeded" || got.Events[1].ResponseID != "resp" || len(got.Events) != 3 {
		t.Fatalf("stored trace regressed: %+v", got)
	}
}

func TestTraceStoreLoadAndUpsertOrkaTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai_traces.json")
	store := NewTraceStore()
	if !store.Upsert(AnalysisTrace{Backend: "orka", TaskName: "task-a", ContractHash: "contract", JobID: "job", BuildID: "1", TestName: "test", Outcome: "succeeded", Events: []TraceEvent{{Kind: "model_request", ResponseID: "resp-old"}}}) {
		t.Fatal("initial upsert failed")
	}
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTraceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasTerminalTask("", "task-a", "contract") {
		t.Fatal("loaded store lost task identity")
	}
	loaded.Upsert(AnalysisTrace{Backend: "orka", TaskName: "task-a", JobID: "job", BuildID: "1", TestName: "test", Outcome: "succeeded", Events: []TraceEvent{{Kind: "model_request", ResponseID: "resp-new"}}})
	got := loaded.Snapshot()
	if len(got.Traces) != 1 || got.Traces[0].Events[0].ResponseID != "resp-new" || got.Traces[0].Events[0].Sequence != 1 {
		t.Fatalf("upserted traces = %+v", got.Traces)
	}
}

func TestTraceStoreKeepsDistinctInProcessSessions(t *testing.T) {
	store := NewTraceStore()
	for _, startedAt := range []string{"2026-07-22T08:00:00Z", "2026-07-22T08:01:00Z"} {
		store.Upsert(AnalysisTrace{Backend: "inprocess", JobID: "job", BuildID: "1", TestName: "same", StartedAt: startedAt, Outcome: "success"})
	}
	if got := len(store.Snapshot().Traces); got != 2 {
		t.Fatalf("in-process sessions = %d, want 2", got)
	}
}

func TestTraceStorePreservesLongResponseID(t *testing.T) {
	responseID := strings.Repeat("Ab+/", 300)
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job"})
	trace.Record(TraceEvent{Kind: "model_request", ResponseID: responseID})
	trace.Finish("success", nil)
	got := store.Snapshot().Traces[0].Events[0].ResponseID
	if got != responseID {
		t.Fatalf("response ID length = %d, want exact %d-byte value", len(got), len(responseID))
	}
}
