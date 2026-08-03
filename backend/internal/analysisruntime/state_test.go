package analysisruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

func stateTestRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID: "job", BuildPrefix: "logs/job/1/", Build: models.BuildInfo{JobName: "job", BuildID: "1"},
		TestCase: models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "boom"},
	}
}

func stateTestKey() []byte { return bytes.Repeat([]byte{0x33}, 32) }

func stateTestIdentity(request ai.FailureAnalysisRequest) ContainerStateIdentity {
	return NewContainerStateIdentity("orka-system", "task", request)
}

func stateTestEntry(request ai.FailureAnalysisRequest, created time.Time, value string) map[string]ai.CacheEntry {
	key := FailureCacheKey(request)
	return map[string]ai.CacheEntry{key: {Key: key, CreatedAt: created, Data: json.RawMessage(value)}}
}

func stateTestTrace(responseID string) ai.AnalysisTrace {
	return ai.AnalysisTrace{
		JobID: "job", BuildID: "1", TestName: "Test A", APIMode: "chat_completions",
		StartedAt: "2026-07-23T00:00:00Z", RecordedAt: "2026-07-23T00:00:01Z", Outcome: "success",
		Events: []ai.TraceEvent{{Sequence: 1, Kind: "model_request", ResponseID: responseID}},
	}
}

func TestContainerAnalysisStateEncryptRoundTripAndMarker(t *testing.T) {
	request := stateTestRequest()
	identity := stateTestIdentity(request)
	state := ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: FailureCacheKey(request),
		CacheEntries: stateTestEntry(request, time.Now().UTC(), `{"summary":"ok"}`),
		Traces:       []ai.AnalysisTrace{stateTestTrace("resp")},
	}
	encoded, err := EncryptContainerAnalysisState(state, stateTestKey(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "resp") || strings.Contains(encoded, "summary") {
		t.Fatal("encrypted state leaked plaintext")
	}
	got, err := DecryptContainerAnalysisState(encoded, stateTestKey(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v, want %+v", got, state)
	}
	var out bytes.Buffer
	if err := WriteEncryptedContainerAnalysisState(&out, state, stateTestKey(), identity); err != nil {
		t.Fatal(err)
	}
	got, err = ParseEncryptedContainerAnalysisState("log before\n"+out.String()+"log after\n", stateTestKey(), identity)
	if err != nil || !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v, error = %v", got, err)
	}
}

func TestContainerAnalysisStateRejectsWrongKeyTamperAndMalformedKey(t *testing.T) {
	request := stateTestRequest()
	identity := stateTestIdentity(request)
	state := ContainerAnalysisState{Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: FailureCacheKey(request)}
	encoded, err := EncryptContainerAnalysisState(state, stateTestKey(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptContainerAnalysisState(encoded, bytes.Repeat([]byte{0x44}, 32), identity); err == nil {
		t.Fatal("wrong key decrypted state")
	}
	wrongIdentity := identity
	wrongIdentity.TaskName = "other-task"
	if _, err := DecryptContainerAnalysisState(encoded, stateTestKey(), wrongIdentity); err == nil {
		t.Fatal("state replayed under another Task identity")
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 1
	if _, err := DecryptContainerAnalysisState(base64.StdEncoding.EncodeToString(payload), stateTestKey(), identity); err == nil {
		t.Fatal("tampered state decrypted")
	}
	oversized := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxContainerStateBytes+64)+4)
	if _, err := DecryptContainerAnalysisState(oversized, stateTestKey(), identity); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized ciphertext error = %v", err)
	}
	for _, raw := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParseContainerStateKey(raw); err == nil {
			t.Fatalf("ParseContainerStateKey(%q) succeeded", raw)
		}
	}
	keyText := base64.StdEncoding.EncodeToString(stateTestKey())
	key, err := ParseContainerStateKey(keyText)
	if err != nil || !bytes.Equal(key, stateTestKey()) {
		t.Fatalf("key = %x, error = %v", key, err)
	}
}

func TestContainerStateStorePersistsCacheAndPrivateTrace(t *testing.T) {
	dir := t.TempDir()
	request := stateTestRequest()
	created := time.Now().UTC().Add(-time.Minute)
	identity := stateTestIdentity(request)
	state := ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: FailureCacheKey(request),
		CacheEntries: stateTestEntry(request, created, `{"summary":"old"}`),
		Traces:       []ai.AnalysisTrace{stateTestTrace("resp-old")},
	}
	store, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Merge(state); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed := reloaded.CacheSeed(request)
	if len(seed) != 1 || !jsonMessagesEqual(seed[state.CacheKey].Data, json.RawMessage(`{"summary":"old"}`)) {
		t.Fatalf("cache seed = %+v", seed)
	}
	traces, err := ai.LoadTraceStore(filepath.Join(dir, output.AITraceFilename))
	if err != nil {
		t.Fatal(err)
	}
	if got := traces.Snapshot().Traces; len(got) != 1 || got[0].Events[0].ResponseID != "resp-old" {
		t.Fatalf("traces = %+v", got)
	}
	state.CacheEntries = stateTestEntry(request, created.Add(-time.Minute), `{"summary":"stale"}`)
	if err := reloaded.Merge(state); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.CacheSeed(request); !jsonMessagesEqual(got[state.CacheKey].Data, json.RawMessage(`{"summary":"old"}`)) {
		t.Fatalf("stale cache replaced entry: %+v", got)
	}
}

func TestContainerStateStoreDoesNotAdvanceCacheWhenTracePersistenceFails(t *testing.T) {
	dir := t.TempDir()
	request := stateTestRequest()
	identity := stateTestIdentity(request)
	state := ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: identity.CacheKey,
		CacheEntries: stateTestEntry(request, time.Now().UTC(), `{"summary":"recovered"}`),
		Traces:       []ai.AnalysisTrace{stateTestTrace("resp-recovered")},
	}
	store, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(dir, output.AITraceFilename)
	if err := os.Mkdir(tracePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Merge(state); err == nil {
		t.Fatal("merge succeeded with an unreadable trace path")
	}
	if got := store.CacheSeed(request); len(got) != 0 {
		t.Fatalf("in-memory cache advanced after trace failure: %+v", got)
	}
	if got := ai.NewCache(dir).Entries(identity.CacheKey); len(got) != 0 {
		t.Fatalf("persisted cache advanced after trace failure: %+v", got)
	}
	if err := os.RemoveAll(tracePath); err != nil {
		t.Fatal(err)
	}
	if err := store.Merge(state); err != nil {
		t.Fatal(err)
	}
	if got := store.CacheSeed(request); len(got) != 1 {
		t.Fatalf("cache entries after recovery = %d, want 1", len(got))
	}
	persisted, err := ai.LoadTraceStore(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Snapshot().Traces; len(got) != 1 || got[0].Events[0].ResponseID != "resp-recovered" {
		t.Fatalf("persisted traces = %+v", got)
	}
}

func jsonMessagesEqual(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func TestRestoreAndSnapshotContainerAnalysisState(t *testing.T) {
	dir := t.TempDir()
	request := stateTestRequest()
	entries := stateTestEntry(request, time.Now().UTC(), `{"summary":"seed"}`)
	if err := RestoreContainerCache(dir, request, entries); err != nil {
		t.Fatal(err)
	}
	cache := ai.NewCache(dir)
	traces := ai.NewTraceStore()
	traces.Upsert(stateTestTrace("resp"))
	state, err := SnapshotContainerAnalysisState(cache, traces, request, stateTestIdentity(request))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.CacheEntries) != 1 || len(state.Traces) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if err := traces.Save(filepath.Join(dir, output.AITraceFilename)); err != nil {
		t.Fatal(err)
	}
	if err := RemoveContainerLocalState(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", name, err)
		}
	}
}

func TestContainerStateStoreConcurrentMergesRetainDistinctIdentities(t *testing.T) {
	dir := t.TempDir()
	store, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	requests := []ai.FailureAnalysisRequest{stateTestRequest(), stateTestRequest()}
	requests[1].TestCase.Name = "Test B"
	requests[1].TestCase.FailureMessage = "different"
	states := make([]ContainerAnalysisState, 0, len(requests))
	for i, request := range requests {
		identity := NewContainerStateIdentity("orka-system", fmt.Sprintf("task-%d", i), request)
		trace := stateTestTrace(fmt.Sprintf("resp-%d", i))
		trace.TestName = request.TestCase.Name
		states = append(states, ContainerAnalysisState{
			Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace,
			TaskName: identity.TaskName, CacheKey: identity.CacheKey,
			CacheEntries: stateTestEntry(request, time.Now().UTC().Add(time.Duration(i)*time.Second), fmt.Sprintf(`{"summary":"%d"}`, i)),
			Traces:       []ai.AnalysisTrace{trace},
		})
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(states))
	for _, state := range states {
		state := state
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Merge(state)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, request := range requests {
		if got := len(reloaded.CacheSeed(request)); got != 1 {
			t.Fatalf("cache seed %d entries = %d, want 1", i, got)
		}
	}
	traces := reloaded.TraceStore().Snapshot().Traces
	if len(traces) != 2 || traces[0].TestName == traces[1].TestName {
		t.Fatalf("traces = %+v", traces)
	}
}

func TestFailureCacheKeyGeneration(t *testing.T) {
	request := stateTestRequest()
	base := FailureCacheKey(request)
	request.CacheGeneration = "0123456789abcdef"
	generated := FailureCacheKey(request)
	if generated == base || !strings.Contains(generated, ":g:0123456789abcdef:") {
		t.Fatalf("generated key = %q, base = %q", generated, base)
	}
	request.CacheGeneration = ""
	if got := FailureCacheKey(request); got != base {
		t.Fatalf("empty generation changed key: %q vs %q", got, base)
	}
}

func TestContainerStateRejectsCrossGenerationIdentity(t *testing.T) {
	request := stateTestRequest()
	request.CacheGeneration = "0123456789abcdef"
	identity := NewContainerStateIdentity("ns", "task", request)
	state := ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: "ns", TaskName: "task",
		CacheKey: identity.CacheKey, CacheEntries: map[string]ai.CacheEntry{},
	}
	other := request
	other.CacheGeneration = "fedcba9876543210"
	if err := validateContainerStateIdentity(state, NewContainerStateIdentity("ns", "task", other)); err == nil {
		t.Fatal("cross-generation state identity was accepted")
	}
}

func TestContainerStateStoreMergePreservesStagedFanoutEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fanoutRequest := stateTestRequest()
	fanoutRequest.TestCase.Name = "fanout"
	fanoutRequest.TestCase.FailureMessage = "shared"
	fanoutKey := FailureCacheKey(fanoutRequest)
	fanoutEntry := ai.CacheEntry{Key: fanoutKey, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"summary":"fanout"}`)}
	if err := store.StageCacheEntry(fanoutEntry); err != nil {
		t.Fatal(err)
	}

	mergedRequest := stateTestRequest()
	mergedRequest.TestCase.Name = "task"
	mergedRequest.TestCase.FailureMessage = "other"
	identity := NewContainerStateIdentity("orka-system", "task", mergedRequest)
	mergedEntries := stateTestEntry(mergedRequest, time.Now().UTC().Add(time.Second), `{"summary":"task"}`)
	if err := store.Merge(ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName,
		CacheKey: identity.CacheKey, CacheEntries: mergedEntries,
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewContainerStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.CacheSeed(fanoutRequest)); got != 1 {
		t.Fatalf("fanout cache entries = %d, want 1", got)
	}
	if got := len(reloaded.CacheSeed(mergedRequest)); got != 1 {
		t.Fatalf("merged cache entries = %d, want 1", got)
	}
}

func TestContainerStateStoreRecordsUsageFromTrace(t *testing.T) {
	dir := t.TempDir()
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewContainerStateStore(dir, usage)
	if err != nil {
		t.Fatal(err)
	}
	request := stateTestRequest()
	identity := NewContainerStateIdentity("orka-system", "task-usage", request)
	trace := ai.AnalysisTrace{JobID: "job", BuildID: "1", TestName: "Test A", StartedAt: "2026-08-03T12:00:00Z", RecordedAt: "2026-08-03T12:00:01Z", Outcome: "success", Events: []ai.TraceEvent{{Kind: "model_request", UsageReported: true, InputTokens: 12, OutputTokens: 4}}}
	state := ContainerAnalysisState{Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: identity.CacheKey, Traces: []ai.AnalysisTrace{trace}}
	if err := store.MergeTraces(state); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeTraces(state); err != nil {
		t.Fatal(err)
	}
	snapshot := usage.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.Days[0].Totals.Operations != 1 || snapshot.Days[0].Totals.InputTokens != 12 || snapshot.RecentOperations[0].Origin != aiusage.OriginAnalyzer {
		t.Fatalf("usage = %+v", snapshot)
	}
}
