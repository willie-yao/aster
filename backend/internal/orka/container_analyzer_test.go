package orka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type fakeContainerAnalyzerKube struct {
	*fakeContainerResourceClient
	mu            sync.Mutex
	taskCalls     int
	phase         string
	terminalDelay time.Duration
	deletedTask   []string
}

func (f *fakeContainerAnalyzerKube) TaskState(ctx context.Context, _ string, _ string) (TaskState, error) {
	f.mu.Lock()
	f.taskCalls++
	call := f.taskCalls
	delay := f.terminalDelay
	f.mu.Unlock()
	if call == 1 {
		return TaskState{}, nil
	}
	if call == 2 && delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return TaskState{}, ctx.Err()
		case <-timer.C:
		}
	}
	return TaskState{
		Exists: true, Phase: f.phase, UID: "task-uid", ResourceVersion: "task-rv",
	}, nil
}

func (f *fakeContainerAnalyzerKube) DeleteTaskIfIdentity(_ context.Context, namespace, name, uid, resourceVersion string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedTask = append(f.deletedTask, namespace+"/"+name+"@"+uid+"@"+resourceVersion)
	return true, nil
}

type generatedContainerResult struct {
	request ai.FailureAnalysisRequest
	key     []byte
	entry   map[string]ai.CacheEntry
	failed  bool
	err     error
	delay   time.Duration
	calls   int
}

func (r *generatedContainerResult) Result(ctx context.Context, namespace, taskName string) (string, bool, error) {
	r.calls++
	if r.delay > 0 {
		timer := time.NewTimer(r.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-timer.C:
		}
	}
	if r.err != nil {
		return "", false, r.err
	}
	identity := analysisruntime.NewContainerStateIdentity(namespace, taskName, r.request)
	state := analysisruntime.ContainerAnalysisState{
		Version: analysisruntime.ContainerStateVersion, TaskNamespace: namespace,
		TaskName: taskName, CacheKey: identity.CacheKey, CacheEntries: r.entry,
		Traces: []ai.AnalysisTrace{{
			JobID: r.request.JobID, BuildID: r.request.Build.BuildID, TestName: r.request.TestCase.Name,
			APIMode: "chat_completions", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "unavailable",
		}},
	}
	result := ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Summary: "container result"},
		Analysis: &models.AIAnalysis{
			RootCause: "cause", SuggestedFix: "fix", Severity: "High", Mode: ai.AgenticMode,
			EvidencePlanCovered: true,
		},
	}
	var logs bytes.Buffer
	if err := analysisruntime.WriteEncryptedContainerAnalysisState(&logs, state, r.key, identity); err != nil {
		return "", false, err
	}
	if !r.failed {
		if err := analysisruntime.WriteFailureAnalysisResult(&logs, result); err != nil {
			return "", false, err
		}
	}
	return logs.String(), true, nil
}

func containerAnalyzerTestOptions(t *testing.T, key []byte) ContainerAnalyzerOptions {
	t.Helper()
	return ContainerAnalyzerOptions{
		Namespace: "orka-system", OrkaAPI: "http://orka.orka-system.svc.cluster.local:8080", Image: "dashboard-analyzer:sha-deadbeef", ProjectDir: containerTaskProject(t), DataDir: t.TempDir(),
		API: "chat_completions", Endpoint: "https://model.invalid/v1/chat/completions", Model: "model",
		ModelSecretName: "model-secret", ModelTokenKey: "token", StateSecretName: "state-secret", StateSecretKey: "state-key", StateKey: key,
		TaskTimeout: time.Minute, PollInterval: time.Millisecond, MaxRetries: 1, MaxConcurrentTasks: 2,
		NodeSelector: map[string]string{"agentpool": "nodepool1"},
	}
}

func TestContainerAnalyzerMergesSuccessfulTaskStateAndCleansUp(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x41}, 32)
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC(),
		Data: json.RawMessage(`{"summary":"container result","root_cause":"cause","suggested_fix":"fix","severity":"High","evidence_plan_covered":true}`),
	}}
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Succeeded"}
	results := &generatedContainerResult{request: request, key: key, entry: entry}
	opts := containerAnalyzerTestOptions(t, key)
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.AnalyzeFailure(context.Background(), nil, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Analysis == nil || !result.Analysis.EvidencePlanCovered || result.Summary == nil {
		t.Fatalf("result = %+v", result)
	}
	if got := store.CacheSeed(request); len(got) != 1 {
		t.Fatalf("cache seed = %+v", got)
	}
	if results.calls != 1 || len(kube.deletedTask) != 0 || len(resources.deletedVersion) != 1 {
		t.Fatalf("result calls=%d deleted Tasks=%v deleted bundles=%v", results.calls, kube.deletedTask, resources.deletedVersion)
	}
}

func TestContainerAnalyzerFailedTaskDoesNotReplaceAcceptedCache(t *testing.T) {
	request := containerTaskRequest()
	request.TestCase.AISummary = &models.AISummary{Summary: "accepted published result"}
	request.TestCase.AIAnalysis = &models.AIAnalysis{RootCause: "accepted cause", Mode: ai.AgenticMode}
	key := bytes.Repeat([]byte{0x52}, 32)
	cacheKey := analysisruntime.FailureCacheKey(request)
	accepted := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC().Add(-time.Minute), Data: json.RawMessage(`{"summary":"accepted"}`),
	}}
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := analysisruntime.NewContainerStateIdentity("orka-system", "seed-task", request)
	if err := store.Merge(analysisruntime.ContainerAnalysisState{
		Version: analysisruntime.ContainerStateVersion, TaskNamespace: identity.TaskNamespace,
		TaskName: identity.TaskName, CacheKey: cacheKey, CacheEntries: accepted,
	}); err != nil {
		t.Fatal(err)
	}
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Failed"}
	failedEntry := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC().Add(time.Minute), Data: json.RawMessage(`{"summary":"must-not-merge"}`),
	}}
	results := &generatedContainerResult{request: request, key: key, entry: failedEntry, failed: true}
	analyzer, err := newContainerAnalyzer(containerAnalyzerTestOptions(t, key), kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.AnalyzeFailure(context.Background(), nil, request)
	if err == nil || result.Summary == nil || result.Summary.Summary != "accepted published result" || result.Analysis == nil || result.Analysis.RootCause != "accepted cause" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if results.calls != 1 {
		t.Fatalf("failed Task result calls = %d, want 1", results.calls)
	}
	got := store.CacheSeed(request)[cacheKey]
	if !bytes.Contains(got.Data, []byte(`"accepted"`)) {
		t.Fatalf("accepted cache was replaced: %s", got.Data)
	}
	if traces := store.TraceStore().Snapshot().Traces; len(traces) != 1 || traces[0].Outcome != "unavailable" {
		t.Fatalf("failed Task traces = %+v", traces)
	}
}

func TestValidateContainerAnalyzerOptionsRejectsMutableImageTag(t *testing.T) {
	opts := containerAnalyzerTestOptions(t, bytes.Repeat([]byte{0x61}, 32))
	opts.Image = "ghcr.io/example/analyzer:main"
	if err := ValidateContainerAnalyzerOptions(opts); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("error = %v", err)
	}
	opts.Image = "ghcr.io/example/analyzer:v1.2.3+build.4"
	if err := ValidateContainerAnalyzerOptions(opts); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("build metadata error = %v", err)
	}
	opts.Image = "ghcr.io/example/analyzer:v1.2.3"
	if err := ValidateContainerAnalyzerOptions(opts); err != nil {
		t.Fatalf("semantic version rejected: %v", err)
	}
}

func TestContainerAnalyzerRetainsResourcesUntilResultIsConsumed(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x73}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Succeeded"}
	results := &generatedContainerResult{request: request, key: key, err: errors.New("result unavailable")}
	analyzer, err := newContainerAnalyzer(containerAnalyzerTestOptions(t, key), kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := analyzer.AnalyzeFailure(context.Background(), nil, request); err == nil {
		t.Fatal("AnalyzeFailure succeeded")
	}
	if len(kube.deletedTask) != 0 || len(resources.deletedVersion) != 0 {
		t.Fatalf("unconsumed result cleanup: Tasks=%v bundles=%v", kube.deletedTask, resources.deletedVersion)
	}
}

func TestContainerAnalyzerMaintainPrunesWithoutAnalysisWork(t *testing.T) {
	key := bytes.Repeat([]byte{0x74}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := taskObject("old-terminal", "Succeeded", time.Now().UTC().Add(-2*ContainerAnalysisTaskRetention))
	resources := &fakeContainerResourceClient{listedTasks: []unstructured.Unstructured{old}}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Succeeded"}
	analyzer, err := newContainerAnalyzer(containerAnalyzerTestOptions(t, key), kube, &generatedContainerResult{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kube.deletedTask) != 1 {
		t.Fatalf("maintenance deleted Tasks = %v", kube.deletedTask)
	}
}

func TestContainerAnalyzerAllowsSchedulingAndFreshResultGrace(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x75}, 32)
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"summary":"ok"}`),
	}}
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Succeeded", terminalDelay: 15 * time.Millisecond}
	results := &generatedContainerResult{request: request, key: key, entry: entry, delay: 20 * time.Millisecond}
	opts := containerAnalyzerTestOptions(t, key)
	opts.TaskTimeout = 5 * time.Millisecond
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.AnalyzeFailure(context.Background(), nil, request)
	if err != nil || result.Summary == nil {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestContainerAnalyzerReturnsValidResultWhenStatePersistenceFails(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x76}, 32)
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"summary":"ok"}`),
	}}
	stateDir := t.TempDir()
	store, err := analysisruntime.NewContainerStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources, phase: "Succeeded"}
	results := &generatedContainerResult{request: request, key: key, entry: entry}
	analyzer, err := newContainerAnalyzer(containerAnalyzerTestOptions(t, key), kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.AnalyzeFailure(context.Background(), nil, request)
	if err != nil || result.Summary == nil || result.Summary.Summary != "container result" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if len(kube.deletedTask) != 0 || len(resources.deletedVersion) != 0 {
		t.Fatalf("persistence failure cleaned resources: Tasks=%v bundles=%v", kube.deletedTask, resources.deletedVersion)
	}
}
