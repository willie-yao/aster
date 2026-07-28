package orka

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

func TestContainerAnalysisCorrelationLabelsAreSafeAndDoNotChangeIdentity(t *testing.T) {
	progress := fetchprogress.New(t.TempDir(), "sha-test")
	progress.StartPass(fetchprogress.PassLightweightWatch)
	request := containerTaskRequest()
	workItem, labels := containerAnalysisCorrelation(progress, request)
	if len(labels) != 4 || labels[containerWorkItemLabel] != workItem {
		t.Fatalf("correlation labels = %+v", labels)
	}
	for key, value := range labels {
		if errs := k8svalidation.IsQualifiedName(key); len(errs) > 0 {
			t.Fatalf("label key %q: %v", key, errs)
		}
		if errs := k8svalidation.IsValidLabelValue(value); len(errs) > 0 {
			t.Fatalf("label value %q: %v", value, errs)
		}
		for _, private := range []string{request.JobID, request.TestCase.Name, request.BuildPrefix} {
			if private != "" && strings.Contains(value, private) {
				t.Fatalf("label %s leaked %q", key, private)
			}
		}
	}

	baseSpec := containerTaskSpec(t)
	base, err := BuildContainerAnalysisResources(baseSpec)
	if err != nil {
		t.Fatal(err)
	}
	withLabels := containerTaskSpec(t)
	withLabels.TaskLabels = labels
	correlated, err := BuildContainerAnalysisResources(withLabels)
	if err != nil {
		t.Fatal(err)
	}
	baseName := base.Task["metadata"].(map[string]any)["name"]
	correlatedName := correlated.Task["metadata"].(map[string]any)["name"]
	if correlatedName != baseName {
		t.Fatalf("correlation changed content-addressed Task name: %v != %v", correlatedName, baseName)
	}
	baseBundle := base.BundleConfigMap["metadata"].(map[string]any)["name"]
	correlatedBundle := correlated.BundleConfigMap["metadata"].(map[string]any)["name"]
	if correlatedBundle != baseBundle {
		t.Fatalf("correlation changed bundle identity: %v != %v", correlatedBundle, baseBundle)
	}
	taskLabels := correlated.Task["metadata"].(map[string]any)["labels"].(map[string]any)
	if taskLabels[containerPassIDLabel] != labels[containerPassIDLabel] || taskLabels["prow-ai-dashboard/test"] != "bundle" {
		t.Fatalf("Task labels = %+v", taskLabels)
	}
	bundleLabels := correlated.BundleConfigMap["metadata"].(map[string]any)["labels"].(map[string]any)
	if _, found := bundleLabels[containerPassIDLabel]; found {
		t.Fatalf("immutable bundle received pass label: %+v", bundleLabels)
	}
}

func TestContainerAnalyzerAccountsAdoptionAttemptsRetriesAndCacheHit(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x81}, 32)
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry := map[string]ai.CacheEntry{cacheKey: {
		Key: cacheKey, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"summary":"cached"}`),
	}}
	dataDir := t.TempDir()
	store, err := analysisruntime.NewContainerStateStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	seedIdentity := analysisruntime.NewContainerStateIdentity("orka-system", "seed-task", request)
	if err := store.Merge(analysisruntime.ContainerAnalysisState{
		Version: analysisruntime.ContainerStateVersion, TaskNamespace: seedIdentity.TaskNamespace,
		TaskName: seedIdentity.TaskName, CacheKey: cacheKey, CacheEntries: entry,
	}); err != nil {
		t.Fatal(err)
	}
	progress := fetchprogress.New(dataDir, "sha-test")
	progress.StartPass(fetchprogress.PassLightweightWatch)
	progress.PlanAnalyses(1)
	resources := &fakeContainerResourceClient{}
	kube := &fakeContainerAnalyzerKube{
		fakeContainerResourceClient: resources,
		phase:                       "Succeeded", attempts: 2, existsInitially: true,
	}
	results := &generatedContainerResult{
		request: request, key: key, entry: entry, notReady: 1, traceOutcome: "ai_cache_hit",
	}
	opts := containerAnalyzerTestOptions(t, key)
	opts.Progress = progress
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := analyzer.AnalyzeFailure(t.Context(), nil, request); err != nil {
		t.Fatal(err)
	}
	status := progress.Snapshot()
	if status.Analyses.TaskAttempts != 2 || status.Analyses.Retries != 1 || status.Analyses.ExistingTasksAdopted != 1 {
		t.Fatalf("Task accounting = %+v", status.Analyses)
	}
	if status.Analyses.ResultsRetrieved != 1 || status.Analyses.ResultRetrievalRetries != 1 {
		t.Fatalf("result accounting = %+v", status.Analyses)
	}
	if status.Analyses.AcceptedCacheHits != 1 || status.Analyses.NewWork != 0 || status.Analyses.StaleWork != 0 {
		t.Fatalf("cache accounting = %+v", status.Analyses)
	}
	if len(status.CurrentTasks) != 1 || !status.CurrentTasks[0].Adopted || status.CurrentTasks[0].Attempts != 2 || status.CurrentTasks[0].Phase != "Succeeded" {
		t.Fatalf("current Task mapping = %+v", status.CurrentTasks)
	}
}

func TestContainerAnalyzerRecordsTaskTimeoutAndCancellation(t *testing.T) {
	for name, cancel := range map[string]func(context.CancelFunc){
		"timeout":      func(context.CancelFunc) {},
		"cancellation": func(cancel context.CancelFunc) { time.AfterFunc(5*time.Millisecond, cancel) },
	} {
		t.Run(name, func(t *testing.T) {
			key := bytes.Repeat([]byte{0x82}, 32)
			dataDir := t.TempDir()
			store, err := analysisruntime.NewContainerStateStore(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			progress := fetchprogress.New(dataDir, "sha-test")
			progress.StartPass(fetchprogress.PassLightweightWatch)
			progress.PlanAnalyses(1)
			resources := &fakeContainerResourceClient{}
			kube := &fakeContainerAnalyzerKube{
				fakeContainerResourceClient: resources,
				phase:                       "Unknown", attempts: 1, existsInitially: true,
			}
			opts := containerAnalyzerTestOptions(t, key)
			opts.Progress = progress
			analyzer, err := newContainerAnalyzer(opts, kube, &generatedContainerResult{}, store)
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer stop()
			cancel(stop)
			_, err = analyzer.AnalyzeFailure(ctx, nil, containerTaskRequest())
			if err == nil {
				t.Fatal("AnalyzeFailure succeeded")
			}
			status := progress.Snapshot()
			if len(status.CurrentTasks) != 1 {
				t.Fatalf("Task mappings = %+v", status.CurrentTasks)
			}
			want := "TimedOut"
			if name == "cancellation" {
				want = "Cancelled"
			}
			if status.CurrentTasks[0].Phase != want {
				t.Fatalf("Task phase = %q, want %q", status.CurrentTasks[0].Phase, want)
			}
		})
	}
}
