package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

func TestRunRecordProjectsAnalysisTrace(t *testing.T) {
	started := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	recorded := started.Add(2 * time.Second)
	record := newRunRecord(TraceMetadata{
		JobID: "job", BuildID: "1", TestName: "test https://secret.example",
		APIMode: APIResponses, Model: "model", ReasoningEffort: " HIGH ",
	}, started)
	record.append(TraceEvent{Kind: "model_request", Outcome: "success"})
	record.complete("success", nil, recorded)

	got := record.analysisTrace()
	if got.JobID != "job" || got.BuildID != "1" || !strings.Contains(got.TestName, "[redacted-url]") ||
		got.APIMode != APIResponses || got.Model != "model" || got.ReasoningEffort != "high" ||
		got.StartedAt != started.Format(time.RFC3339Nano) || got.RecordedAt != recorded.Format(time.RFC3339Nano) ||
		got.ElapsedMs != 2000 || got.Outcome != "success" || len(got.Events) != 1 || got.Events[0].Sequence != 1 {
		t.Fatalf("analysis trace projection = %+v", got)
	}
	got.Events[0].Kind = "changed"
	if record.events[0].Kind != "model_request" {
		t.Fatal("trace projection aliases the run record event slice")
	}
}

func TestTraceSessionProjectsRunRecordOnFinish(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job"})
	trace.Record(TraceEvent{Kind: "model_request", Outcome: "success"})
	if len(trace.record.events) != 1 || len(store.Snapshot().Traces) != 0 {
		t.Fatal("run record was projected before the run finished")
	}
	trace.Finish("success", nil)

	got := store.Snapshot().Traces[0]
	if len(got.Events) != 1 || got.Events[0].Sequence != 1 || got.Events[0].Kind != "model_request" || got.Events[0].ElapsedMs < 0 || got.Events[0].ElapsedMs > 60_000 {
		t.Fatalf("projected run record = %+v", got.Events)
	}
}

func TestTraceStoreBoundsAndRedacts(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test https://secret.example Authorization: Bearer top-secret", APIMode: APIResponses, ReasoningEffort: " HIGH "})
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

func TestTraceStoreRetainsContentFreeGrepTelemetry(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job"})
	trace.Record(TraceEvent{Kind: "tool_call", Tool: "grep_repo", Outcome: "success", Grep: &tools.GrepCallObservation{
		SelectorID: "latest-client", PathFilter: "find the private failure please",
		PathFilterSupplied: true, PathFilterLength: len("find the private failure please"),
		ContextLines: 2, MaxMatches: 30, MatchCount: 1, FilesScanned: 4, Outcome: tools.GrepOutcomeMatched,
		ReturnedRanges: []tools.GrepRangeObservation{{SelectorID: "latest-client", Path: "pkg/file.go", LineStart: 10, LineEnd: 14}},
	}})
	trace.Finish("success", nil)
	got := store.Snapshot().Traces[0].Events[0].Grep
	if got == nil || got.SelectorID != "latest-client" || got.PathFilter != "" || !got.PathFilterRedacted || got.ContextLines != 2 || got.MatchCount != 1 || len(got.ReturnedRanges) != 1 {
		t.Fatalf("grep telemetry=%+v", got)
	}
	encoded, _ := json.Marshal(store.Snapshot())
	if strings.Contains(string(encoded), "private failure") {
		t.Fatalf("grep telemetry retained prose: %s", encoded)
	}
}

func TestTraceStoreRetainsDraftDecisionsAtEventCap(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	for i := 0; i < analysisTraceMaxEvents; i++ {
		trace.Record(TraceEvent{Kind: "model_request"})
	}
	for i, reason := range []string{draftReasonCandidateNotBetter, draftReasonFallbackPromoted} {
		trace.Record(TraceEvent{
			Kind: "draft_selection",
			DraftDecision: &DraftDecisionTrace{
				CandidateAttempt: i + 1, ReplacementReason: reason,
			},
		})
	}
	trace.Finish("success", nil)

	got := store.Snapshot().Traces[0]
	if !got.Truncated || len(got.Events) != analysisTraceMaxEvents {
		t.Fatalf("trace bounds = truncated:%v events:%d", got.Truncated, len(got.Events))
	}
	var decisions []TraceEvent
	for _, event := range got.Events {
		if event.Kind == "draft_selection" {
			decisions = append(decisions, event)
		}
	}
	if len(decisions) != 2 || decisions[0].DraftDecision == nil || decisions[1].DraftDecision == nil ||
		decisions[0].DraftDecision.ReplacementReason != draftReasonCandidateNotBetter ||
		decisions[1].DraftDecision.ReplacementReason != draftReasonFallbackPromoted ||
		decisions[0].Sequence != analysisTraceMaxEvents-1 || decisions[1].Sequence != analysisTraceMaxEvents {
		t.Fatalf("retained decisions = %+v", decisions)
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
	store.SetEngine(TraceEngine{Version: "v1.2.3", Commit: "0123456789abcdef", ImageTag: "sha-0123456"})
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
	if got.Version != analysisTraceVersion || got.Engine == nil || got.Engine.Commit != "0123456789abcdef" || len(got.Traces) != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
	reloaded, err := LoadTraceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if engine := reloaded.Snapshot().Engine; engine == nil || engine.Version != "v1.2.3" || engine.ImageTag != "sha-0123456" {
		t.Fatalf("reloaded engine = %+v", engine)
	}
	if got.Traces[0].JobID != "job-a" || got.Traces[1].JobID != "job-b" {
		t.Fatalf("trace order = %+v", got.Traces)
	}
	if strings.Contains(string(data), "endpoint") || strings.Contains(string(data), "model") {
		t.Fatalf("trace leaked provider configuration: %s", data)
	}
}

func TestLoadTraceStoreRejectsNoncurrentVersions(t *testing.T) {
	for _, version := range []int{0, analysisTraceVersion + 1} {
		path := filepath.Join(t.TempDir(), "ai_traces.json")
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%d,"traces":[]}`, version)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTraceStore(path); err == nil || !strings.Contains(err.Error(), "is unsupported") {
			t.Fatalf("version %d error = %v", version, err)
		}
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
	old := AnalysisTrace{JobID: "old", BuildID: "old", TestName: "old", StartedAt: "2000-01-01T00:00:00Z", Outcome: "success"}
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
		JobID: "old", BuildID: "1", TestName: "test", StartedAt: "2026-07-22T08:00:00Z", Outcome: "success",
		Events: []TraceEvent{{Kind: "model_request", ResponseID: strings.Repeat("a", 1000)}},
	}
	newer := AnalysisTrace{
		JobID: "new", BuildID: "2", TestName: "test", StartedAt: "2026-07-22T08:01:00Z", Outcome: "success",
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
		JobID: "newest-window-start", RecordedAt: "2026-07-22T09:00:00Z", Outcome: "succeeded",
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

func TestTraceStoreKeepsDistinctInProcessSessions(t *testing.T) {
	store := NewTraceStore()
	for _, startedAt := range []string{"2026-07-22T08:00:00Z", "2026-07-22T08:01:00Z"} {
		store.Upsert(AnalysisTrace{JobID: "job", BuildID: "1", TestName: "same", StartedAt: startedAt, Outcome: "success"})
	}
	if got := len(store.Snapshot().Traces); got != 2 {
		t.Fatalf("analysis sessions = %d, want 2", got)
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

func TestTraceStoreNormalizesReasoningEffortOnUpsert(t *testing.T) {
	store := NewTraceStore()
	if !store.Upsert(AnalysisTrace{
		JobID: "job", StartedAt: "2026-08-12T00:00:00Z", Outcome: "success", ReasoningEffort: " HIGH ",
		Events: []TraceEvent{{Kind: "model_request", ReasoningEffort: " XHIGH "}, {Kind: "model_request", ReasoningEffort: "invalid"}},
	}) {
		t.Fatal("trace was not inserted")
	}
	got := store.Snapshot().Traces[0]
	if got.ReasoningEffort != "high" || got.Events[0].ReasoningEffort != "xhigh" || got.Events[1].ReasoningEffort != "" {
		t.Fatalf("reasoning effort provenance = %+v", got)
	}
}
