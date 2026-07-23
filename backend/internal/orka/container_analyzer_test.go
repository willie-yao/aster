package orka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeContainerAnalyzerKube struct {
	*fakeContainerResourceClient
	mu          sync.Mutex
	taskCalls   int
	phase       string
	deletedTask []string
}

func (f *fakeContainerAnalyzerKube) TaskState(context.Context, string, string) (TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskCalls++
	if f.taskCalls == 1 {
		return TaskState{}, nil
	}
	return TaskState{
		Exists: true, Phase: f.phase, UID: "task-uid", ResourceVersion: "task-rv",
	}, nil
}

func (f *fakeContainerAnalyzerKube) DeleteTask(_ context.Context, namespace, name, resourceVersion string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedTask = append(f.deletedTask, namespace+"/"+name+"@"+resourceVersion)
	return nil
}

type generatedContainerResult struct {
	request ai.FailureAnalysisRequest
	key     []byte
	entry   map[string]ai.CacheEntry
	failed  bool
	calls   int
}

func (r *generatedContainerResult) Result(_ context.Context, namespace, taskName string) (string, bool, error) {
	r.calls++
	if r.failed {
		return "", false, fmt.Errorf("result should not be read")
	}
	identity := analysisruntime.NewContainerStateIdentity(namespace, taskName, r.request)
	state := analysisruntime.ContainerAnalysisState{
		Version: analysisruntime.ContainerStateVersion, TaskNamespace: namespace,
		TaskName: taskName, CacheKey: identity.CacheKey, CacheEntries: r.entry,
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
	if err := analysisruntime.WriteFailureAnalysisResult(&logs, result); err != nil {
		return "", false, err
	}
	return logs.String(), true, nil
}

func containerAnalyzerTestOptions(t *testing.T, key []byte) ContainerAnalyzerOptions {
	t.Helper()
	return ContainerAnalyzerOptions{
		Namespace: "orka-system", Image: "dashboard-analyzer:sha", ProjectDir: containerTaskProject(t), DataDir: t.TempDir(),
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
	if results.calls != 1 || len(kube.deletedTask) != 1 || len(resources.deletedVersion) != 1 {
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
	results := &generatedContainerResult{request: request, key: key, failed: true}
	analyzer, err := newContainerAnalyzer(containerAnalyzerTestOptions(t, key), kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.AnalyzeFailure(context.Background(), nil, request)
	if err == nil || result.Summary == nil || result.Summary.Summary != "accepted published result" || result.Analysis == nil || result.Analysis.RootCause != "accepted cause" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if results.calls != 0 {
		t.Fatalf("failed Task result calls = %d, want 0", results.calls)
	}
	got := store.CacheSeed(request)[cacheKey]
	if !bytes.Contains(got.Data, []byte(`"accepted"`)) {
		t.Fatalf("accepted cache was replaced: %s", got.Data)
	}
}
