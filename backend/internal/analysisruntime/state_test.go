package analysisruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
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
		Backend: "orka", TaskNamespace: "orka-system", TaskName: "task", ContractHash: ContainerAnalyzerContractVersion,
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

func TestContainerAnalysisStateRejectsMismatchedTraceIdentity(t *testing.T) {
	request := stateTestRequest()
	identity := stateTestIdentity(request)
	trace := stateTestTrace("resp")
	trace.TaskName = "other-task"
	state := ContainerAnalysisState{
		Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName,
		CacheKey: identity.CacheKey, Traces: []ai.AnalysisTrace{trace},
	}
	if _, err := EncryptContainerAnalysisState(state, stateTestKey(), identity); err == nil || !strings.Contains(err.Error(), "trace identity") {
		t.Fatalf("EncryptContainerAnalysisState error = %v", err)
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
