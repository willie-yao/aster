package orka

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type compatibleResultKube struct {
	*fakeContainerAnalyzerKube
	listed      []unstructured.Unstructured
	exact       *unstructured.Unstructured
	namespace   string
	name        string
	selector    string
	listErr     error
	exactGetErr error
}

func (k *compatibleResultKube) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if gvr == TasksGVR {
		k.namespace = namespace
		k.name = name
		return k.exact, k.exactGetErr
	}
	return k.fakeContainerAnalyzerKube.Get(ctx, gvr, namespace, name)
}

func (k *compatibleResultKube) ListByLabel(_ context.Context, gvr schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	if gvr != TasksGVR {
		return nil, errors.New("unexpected resource")
	}
	k.namespace = namespace
	k.selector = selector
	return k.listed, k.listErr
}

type compatibleResultAPI struct {
	values map[string]struct {
		raw string
		ok  bool
		err error
	}
	calls []string
}

func (a *compatibleResultAPI) Result(_ context.Context, namespace, taskName string) (string, bool, error) {
	a.calls = append(a.calls, namespace+"/"+taskName)
	value := a.values[taskName]
	return value.raw, value.ok, value.err
}

func TestCompatibleContainerResultCandidatesAreBoundedAndStrict(t *testing.T) {
	now := time.Date(2026, 7, 30, 19, 0, 0, 0, time.UTC)
	const (
		namespace   = "analysis-release"
		workItem    = "0123456789abcdef"
		bundle      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		currentTask = "dashboard-analyzer-current"
	)
	items := []unstructured.Unstructured{}
	for i := 0; i < 6; i++ {
		completed := now.Add(-time.Duration(i+1) * time.Minute)
		items = append(items, compatibleTaskObject(namespace, "valid-"+string(rune('a'+i)), workItem, bundle, fingerprint, "Succeeded", true, completed))
	}
	invalid := []unstructured.Unstructured{
		compatibleTaskObject(namespace, currentTask, workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "running", workItem, bundle, fingerprint, "Running", false, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "pending", workItem, bundle, fingerprint, "Pending", false, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "failed", workItem, bundle, fingerprint, "Failed", true, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "cancelled", workItem, bundle, fingerprint, "Cancelled", true, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "resultless", workItem, bundle, fingerprint, "Succeeded", false, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "expired", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-ContainerAnalysisSucceededTaskRetention-time.Minute)),
		compatibleTaskObject(namespace, "wrong-work", "fedcba9876543210", bundle, fingerprint, "Succeeded", true, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "bad-bundle", workItem, "not-a-digest", fingerprint, "Succeeded", true, now.Add(-time.Minute)),
		compatibleTaskObject(namespace, "wrong-state", workItem, bundle, strings.Repeat("d", 64), "Succeeded", true, now.Add(-time.Minute)),
	}
	wrongContract := compatibleTaskObject(namespace, "wrong-contract", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Minute))
	annotations := wrongContract.GetAnnotations()
	annotations["prow-ai-dashboard/contract-version"] = "old-contract"
	wrongContract.SetAnnotations(annotations)
	invalid = append(invalid, wrongContract)
	unmanaged := compatibleTaskObject(namespace, "unmanaged", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Minute))
	labels := unmanaged.GetLabels()
	labels["app.kubernetes.io/managed-by"] = "other"
	unmanaged.SetLabels(labels)
	invalid = append(invalid, unmanaged)
	items = append(invalid, items...)
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}, listed: items}

	got, err := compatibleContainerResultCandidates(t.Context(), kube, namespace, workItem, fingerprint, currentTask, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"valid-a", "valid-b", "valid-c", "valid-d", "valid-e"}
	names := make([]string, len(got))
	for i := range got {
		names[i] = got[i].Name
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("candidate names = %v, want %v", names, want)
	}
	if kube.namespace != namespace || !strings.Contains(kube.selector, containerWorkItemLabel+"="+workItem) || !strings.Contains(kube.selector, "app.kubernetes.io/managed-by=prow-ai-dashboard") {
		t.Fatalf("lookup namespace=%q selector=%q", kube.namespace, kube.selector)
	}
}

func TestContainerAnalyzerReusesExactResultWithoutCreatingTask(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x79}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	resources := &fakeContainerResourceClient{}
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources}}
	results := &compatibleResultAPI{values: map[string]struct {
		raw string
		ok  bool
		err error
	}{}}
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	policy := compatibleResultPolicy(opts)
	result := compatibleFailureResult(policy)
	prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, store.CacheSeed(request), nil))
	if err != nil {
		t.Fatal(err)
	}
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	exact := compatibleTaskObject(opts.Namespace, prepared.Name, workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, time.Now().UTC().Add(-time.Minute))
	kube.exact = &exact
	results.values[prepared.Name] = struct {
		raw string
		ok  bool
		err error
	}{raw: compatibleRawResult(t, opts.Namespace, prepared.Name, request, key, result, nil), ok: true}

	got, reused, err := analyzer.ReuseExactResult(t.Context(), request, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || !sameAgenticResult(got, result) {
		t.Fatalf("reused=%t result=%+v", reused, got)
	}
	if kube.namespace != opts.Namespace || kube.name != prepared.Name || !reflect.DeepEqual(results.calls, []string{opts.Namespace + "/" + prepared.Name}) {
		t.Fatalf("lookup namespace=%q name=%q result calls=%v", kube.namespace, kube.name, results.calls)
	}
	if len(resources.created) != 0 || len(resources.applied) != 0 || kube.selector != "" {
		t.Fatalf("exact reuse mutated resources: created=%v applied=%v selector=%q", resources.created, resources.applied, kube.selector)
	}
	cacheKey := analysisruntime.FailureCacheKey(request)
	accepted, reason := ai.AcceptAgenticCacheEntry(store.CacheSeed(request)[cacheKey], cacheKey, policy)
	if reason != ai.CacheAccepted || !sameAgenticResult(accepted, result) {
		t.Fatalf("promoted cache reason=%q result=%+v", reason, accepted)
	}
}

func TestContainerAnalyzerExactReuseRejectsIneligibleTasks(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x78}, 32)
	opts := containerAnalyzerTestOptions(t, key)
	policy := compatibleResultPolicy(opts)
	for _, tc := range []struct {
		name   string
		phase  string
		result bool
		mutate func(*unstructured.Unstructured)
	}{
		{name: "missing", phase: ""},
		{name: "running", phase: "Running"},
		{name: "failed", phase: "Failed", result: true},
		{name: "cancelled", phase: "Cancelled", result: true},
		{name: "resultless", phase: "Succeeded"},
		{name: "unmanaged", phase: "Succeeded", result: true, mutate: func(task *unstructured.Unstructured) {
			labels := task.GetLabels()
			labels["app.kubernetes.io/managed-by"] = "other"
			task.SetLabels(labels)
		}},
		{name: "wrong-bundle", phase: "Succeeded", result: true, mutate: func(task *unstructured.Unstructured) {
			annotations := task.GetAnnotations()
			annotations["prow-ai-dashboard/bundle-digest"] = strings.Repeat("f", 64)
			task.SetAnnotations(annotations)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := analysisruntime.NewContainerStateStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
			results := &compatibleResultAPI{values: map[string]struct {
				raw string
				ok  bool
				err error
			}{}}
			analyzer, err := newContainerAnalyzer(opts, kube, results, store)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, store.CacheSeed(request), nil))
			if err != nil {
				t.Fatal(err)
			}
			if tc.phase != "" {
				workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
				task := compatibleTaskObject(opts.Namespace, prepared.Name, workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), tc.phase, tc.result, time.Now().UTC().Add(-time.Minute))
				if tc.mutate != nil {
					tc.mutate(&task)
				}
				kube.exact = &task
			}
			_, reused, err := analyzer.ReuseExactResult(t.Context(), request, policy)
			if err != nil || reused || len(results.calls) != 0 {
				t.Fatalf("reused=%t error=%v calls=%v", reused, err, results.calls)
			}
		})
	}
}

func TestContainerAnalyzerReusesCompatibleResultAndPromotesCache(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7a}, 32)
	stateDir := t.TempDir()
	store, err := analysisruntime.NewContainerStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	resources := &fakeContainerResourceClient{}
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources}}
	results := &compatibleResultAPI{values: map[string]struct {
		raw string
		ok  bool
		err error
	}{}}
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	policy := compatibleResultPolicy(opts)
	result := compatibleFailureResult(policy)
	prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	invalidTask := compatibleTaskObject(opts.Namespace, "old-invalid", workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, time.Now().UTC().Add(-time.Minute))
	validTask := compatibleTaskObject(opts.Namespace, "old-valid", workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, time.Now().UTC().Add(-2*time.Minute))
	kube.listed = []unstructured.Unstructured{validTask, invalidTask}
	results.values["old-invalid"] = struct {
		raw string
		ok  bool
		err error
	}{raw: "invalid result", ok: true}
	results.values["old-valid"] = struct {
		raw string
		ok  bool
		err error
	}{raw: compatibleRawResult(t, opts.Namespace, "old-valid", request, key, result, nil), ok: true}

	got, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || !sameAgenticResult(got, result) {
		t.Fatalf("reused=%t result=%+v", reused, got)
	}
	if !reflect.DeepEqual(results.calls, []string{opts.Namespace + "/old-invalid", opts.Namespace + "/old-valid"}) {
		t.Fatalf("result calls = %v", results.calls)
	}
	if len(resources.created) != 0 || len(resources.applied) != 0 {
		t.Fatalf("compatible reuse created resources: created=%v applied=%v", resources.created, resources.applied)
	}
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry := store.CacheSeed(request)[cacheKey]
	accepted, reason := ai.AcceptAgenticCacheEntry(entry, cacheKey, policy)
	if reason != ai.CacheAccepted || !sameAgenticResult(accepted, result) {
		t.Fatalf("promoted cache reason=%q result=%+v", reason, accepted)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := analysisruntime.NewContainerStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.CacheSeed(request)) != 1 {
		t.Fatal("promoted cache did not survive restart")
	}
}

func TestContainerAnalyzerCompatibleResultSafetyFailures(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7b}, 32)
	for _, tc := range []struct {
		name            string
		resultErr       error
		wrongTask       bool
		wrongCache      bool
		wrongGeneration bool
		minTools        int
		wantErr         bool
		wantAuth        bool
	}{
		{name: "authorization", resultErr: &ResultHTTPError{StatusCode: http.StatusUnauthorized}, wantErr: true, wantAuth: true},
		{name: "candidate result unavailable", resultErr: &ResultHTTPError{StatusCode: http.StatusBadGateway}},
		{name: "encrypted identity", wrongTask: true, wantErr: true},
		{name: "cache identity", wrongCache: true, wantErr: true},
		{name: "cache generation identity", wrongGeneration: true, wantErr: true},
		{name: "below floor", minTools: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := analysisruntime.NewContainerStateStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := containerAnalyzerTestOptions(t, key)
			resources := &fakeContainerResourceClient{}
			kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: resources}}
			results := &compatibleResultAPI{values: map[string]struct {
				raw string
				ok  bool
				err error
			}{}}
			analyzer, err := newContainerAnalyzer(opts, kube, results, store)
			if err != nil {
				t.Fatal(err)
			}
			policy := compatibleResultPolicy(opts)
			if tc.minTools > 0 {
				policy.MinToolCalls = tc.minTools
			}
			result := compatibleFailureResult(compatibleResultPolicy(opts))
			prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
			candidateName := "old-candidate"
			candidateBundle := prepared.BundleDigest
			kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, workItem, candidateBundle, containerStateKeyFingerprint(key), "Succeeded", true, time.Now().UTC().Add(-time.Minute))}
			identityName := candidateName
			if tc.wrongTask {
				identityName = "other-task"
			}
			resultRequest := request
			if tc.wrongCache {
				resultRequest.TestCase.FailureMessage = "different failure"
			}
			if tc.wrongGeneration {
				resultRequest.CacheGeneration = "0123456789abcdef"
			}
			results.values[candidateName] = struct {
				raw string
				ok  bool
				err error
			}{raw: compatibleRawResult(t, opts.Namespace, identityName, resultRequest, key, result, nil), ok: tc.resultErr == nil, err: tc.resultErr}

			_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
			if (err != nil) != tc.wantErr || reused {
				t.Fatalf("reused=%t error=%v", reused, err)
			}
			if tc.wantAuth && !IsResultAuthorizationError(err) {
				t.Fatalf("authorization error = %v", err)
			}
			if len(store.CacheSeed(request)) != 0 {
				t.Fatal("unsafe result was promoted")
			}
		})
	}
}

func TestContainerAnalyzerCompatibleLookupFailureFallsBack(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7d}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	kube := &compatibleResultKube{
		fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}},
		listErr:                   errors.New("temporary list failure"),
	}
	analyzer, err := newContainerAnalyzer(opts, kube, &compatibleResultAPI{}, store)
	if err != nil {
		t.Fatal(err)
	}
	_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, compatibleResultPolicy(opts))
	if err != nil || reused {
		t.Fatalf("reused=%t error=%v", reused, err)
	}
}

func compatibleTaskObject(namespace, name, workItem, bundleDigest, stateFingerprint, phase string, resultAvailable bool, completedAt time.Time) unstructured.Unstructured {
	createdAt := completedAt.Add(-time.Minute)
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "prow-ai-dashboard",
				"prow-ai-dashboard/adapter":    "container-analyzer",
				containerWorkItemLabel:         workItem,
			},
			"annotations": map[string]any{
				"prow-ai-dashboard/contract-version":      ContainerAnalysisContractVersion,
				"prow-ai-dashboard/bundle-digest":         bundleDigest,
				"prow-ai-dashboard/state-key-fingerprint": stateFingerprint,
			},
		},
		"spec": map[string]any{"image": "dashboard-analyzer:sha-oldimage"},
		"status": map[string]any{
			"phase": phase, "completionTime": completedAt.Format(time.RFC3339Nano),
			"resultRef": map[string]any{"available": resultAvailable},
		},
	}}
	object.SetUID(types.UID("uid-" + name))
	object.SetResourceVersion("rv-" + name)
	object.SetCreationTimestamp(metav1.NewTime(createdAt))
	return object
}

func compatibleResultPolicy(opts ContainerAnalyzerOptions) ai.AgenticCachePolicy {
	return ai.AgenticCachePolicy{
		MinToolCalls: 2, MinGCSBytes: 50, Model: opts.Model,
		ModelHash: ai.ModelFingerprint(opts.API, opts.Endpoint, opts.Model), PromptHash: "prompt-hash",
	}
}

func compatibleFailureResult(policy ai.AgenticCachePolicy) ai.FailureAnalysisResult {
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	return ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: generatedAt, Summary: "summary"},
		Analysis: &models.AIAnalysis{
			GeneratedAt: generatedAt, Mode: ai.AgenticMode, Model: policy.Model,
			RootCause: "root", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"a.go"},
			SearchSuggestions: []string{"search/a.go"}, EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 7, LineEnd: 7, Quote: "failure"}},
			ToolCalls: 2, ContextBytes: 100, GCSBytes: 50, CritiquePassed: true, CritiqueVersion: 999,
			ModelHash: policy.ModelHash, PromptHash: policy.PromptHash,
		},
	}
}

func TestSameAgenticResultIncludesPublicationEvidence(t *testing.T) {
	policy := compatibleResultPolicy(ContainerAnalyzerOptions{API: ai.APIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"})
	left := compatibleFailureResult(policy)
	right := compatibleFailureResult(policy)
	if !sameAgenticResult(left, right) {
		t.Fatal("matching results were not equal")
	}
	right.Analysis.SearchSuggestions = []string{"different.go"}
	if sameAgenticResult(left, right) {
		t.Fatal("different search suggestions were treated as equal")
	}
	right = compatibleFailureResult(policy)
	right.Analysis.EvidenceCitations[0].LineStart = 8
	if sameAgenticResult(left, right) {
		t.Fatal("different evidence citations were treated as equal")
	}
}

func compatibleRawResult(t *testing.T, namespace, taskName string, request ai.FailureAnalysisRequest, key []byte, result ai.FailureAnalysisResult, entries map[string]ai.CacheEntry) string {
	t.Helper()
	identity := analysisruntime.NewContainerStateIdentity(namespace, taskName, request)
	state := analysisruntime.ContainerAnalysisState{
		Version: analysisruntime.ContainerStateVersion, TaskNamespace: namespace, TaskName: taskName,
		CacheKey: identity.CacheKey, CacheEntries: entries,
	}
	var raw bytes.Buffer
	if err := analysisruntime.WriteEncryptedContainerAnalysisState(&raw, state, key, identity); err != nil {
		t.Fatal(err)
	}
	if err := analysisruntime.WriteFailureAnalysisResult(&raw, result); err != nil {
		t.Fatal(err)
	}
	return raw.String()
}

func TestPruneContainerAnalysisTasksRetainsBoundedSucceededReuseWindow(t *testing.T) {
	now := time.Date(2026, 7, 30, 19, 0, 0, 0, time.UTC)
	const (
		namespace   = "orka-system"
		workItem    = "0123456789abcdef"
		bundle      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	items := make([]unstructured.Unstructured, 0, 10)
	for i := 0; i < containerCompatibleResultCandidateLimit+1; i++ {
		items = append(items, compatibleTaskObject(namespace, "reuse-"+string(rune('a'+i)), workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Duration(i+1)*time.Minute)))
	}
	items = append(items,
		compatibleTaskObject(namespace, "expired-reuse", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-ContainerAnalysisSucceededTaskRetention-time.Minute)),
		taskObject("old-failed", "Failed", now.Add(-2*ContainerAnalysisTaskRetention)),
		taskObject("old-unlabeled", "Succeeded", now.Add(-2*ContainerAnalysisTaskRetention)),
	)
	previousContract := compatibleTaskObject(namespace, "recent-previous-contract", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Hour))
	annotations := previousContract.GetAnnotations()
	annotations["prow-ai-dashboard/contract-version"] = "previous-contract"
	previousContract.SetAnnotations(annotations)
	items = append(items, previousContract)
	missingCompletion := compatibleTaskObject(namespace, "recent-missing-completion", workItem, bundle, fingerprint, "Succeeded", true, now.Add(-time.Hour))
	delete(missingCompletion.Object["status"].(map[string]any), "completionTime")
	futureCompletion := compatibleTaskObject(namespace, "recent-future-completion", workItem, bundle, fingerprint, "Succeeded", true, now.Add(containerResultClockSkew+time.Minute))
	items = append(items, missingCompletion, futureCompletion)
	client := &fakeContainerResourceClient{listedTasks: items}
	deleted, err := PruneContainerAnalysisTasks(t.Context(), client, namespace, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, calls = %v", deleted, client.deletedTasks)
	}
	deletedNames := map[string]bool{}
	for _, call := range client.deletedTasks {
		deletedNames[strings.SplitN(call, "@", 2)[0]] = true
	}
	for _, name := range []string{"reuse-f", "expired-reuse", "old-failed", "old-unlabeled"} {
		if !deletedNames[name] {
			t.Fatalf("missing deletion for %s: %v", name, client.deletedTasks)
		}
	}
	for _, name := range []string{"reuse-a", "reuse-b", "reuse-c", "reuse-d", "reuse-e"} {
		if deletedNames[name] {
			t.Fatalf("retained candidate %s was deleted", name)
		}
	}
	if deletedNames["recent-previous-contract"] {
		t.Fatal("recent non-reusable Task bypassed normal retention")
	}
	if deletedNames["recent-missing-completion"] || deletedNames["recent-future-completion"] {
		t.Fatal("Task with incomplete timing status bypassed normal retention")
	}
}

func TestContainerAnalyzerReusesAuthenticatedCacheEntry(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7c}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
	results := &compatibleResultAPI{values: map[string]struct {
		raw string
		ok  bool
		err error
	}{}}
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	policy := compatibleResultPolicy(opts)
	result := compatibleFailureResult(policy)
	prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().UTC().Add(-time.Minute)
	candidateName := "old-cached"
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, completedAt)}
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry, err := ai.NewAgenticCacheEntry(cacheKey, result, completedAt)
	if err != nil {
		t.Fatal(err)
	}
	_, seededBundleDigest, err := analysisruntime.BuildProjectBundleWithCache(opts.ProjectDir, ContainerAnalysisContractVersion, request, map[string]ai.CacheEntry{cacheKey: entry})
	if err != nil {
		t.Fatal(err)
	}
	kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, workItem, seededBundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, completedAt)}
	results.values[candidateName] = struct {
		raw string
		ok  bool
		err error
	}{raw: compatibleRawResult(t, opts.Namespace, candidateName, request, key, result, map[string]ai.CacheEntry{cacheKey: entry}), ok: true}

	_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
	if err != nil || !reused {
		t.Fatalf("reused=%t error=%v", reused, err)
	}
	got := store.CacheSeed(request)[cacheKey]
	if !got.CreatedAt.Equal(entry.CreatedAt) {
		t.Fatalf("promoted cache time = %s, want %s", got.CreatedAt, entry.CreatedAt)
	}
}

func TestContainerAnalyzerReusesAuthenticatedCacheAcrossPolicyChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(*ai.FailureAnalysisResult, *ai.AgenticCachePolicy)
		changeProject func(*testing.T, string)
	}{
		{name: "skill set", mutate: func(result *ai.FailureAnalysisResult, policy *ai.AgenticCachePolicy) {
			result.Analysis.SkillSetHash = "old-skills"
			policy.SkillSetHash = "current-skills"
		}, changeProject: func(t *testing.T, projectDir string) {
			if err := os.MkdirAll(filepath.Join(projectDir, "skills"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "skills", "current.yaml"), []byte("id: current\ntriggers: [current]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "model and endpoint", mutate: func(result *ai.FailureAnalysisResult, policy *ai.AgenticCachePolicy) {
			result.Analysis.ModelHash = ai.ModelFingerprint(ai.APIChatCompletions, "https://old-model.invalid/v1/chat/completions", "old-model")
			policy.ModelHash = ai.ModelFingerprint(ai.APIChatCompletions, "https://current-model.invalid/v1/chat/completions", "current-model")
		}},
		{name: "prompt", mutate: func(result *ai.FailureAnalysisResult, policy *ai.AgenticCachePolicy) {
			result.Analysis.PromptHash = "old-prompt"
			policy.PromptHash = "current-prompt"
		}, changeProject: func(t *testing.T, projectDir string) {
			if err := os.WriteFile(filepath.Join(projectDir, "prompts", "system.md"), []byte("Changed prompt.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "transient persistence", mutate: func(result *ai.FailureAnalysisResult, policy *ai.AgenticCachePolicy) {
			result.Summary.IsTransient = true
			policy.ConsecutiveFailures = 3
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := containerTaskRequest()
			key := bytes.Repeat([]byte{0x70}, 32)
			store, err := analysisruntime.NewContainerStateStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := containerAnalyzerTestOptions(t, key)
			kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
			results := &compatibleResultAPI{values: map[string]struct {
				raw string
				ok  bool
				err error
			}{}}
			analyzer, err := newContainerAnalyzer(opts, kube, results, store)
			if err != nil {
				t.Fatal(err)
			}
			policy := compatibleResultPolicy(opts)
			result := compatibleFailureResult(policy)
			tc.mutate(&result, &policy)
			completedAt := time.Now().UTC().Add(-time.Minute)
			cacheKey := analysisruntime.FailureCacheKey(request)
			entry, err := ai.NewAgenticCacheEntry(cacheKey, result, completedAt)
			if err != nil {
				t.Fatal(err)
			}
			_, bundleDigest, err := analysisruntime.BuildProjectBundleWithCache(opts.ProjectDir, ContainerAnalysisContractVersion, request, map[string]ai.CacheEntry{cacheKey: entry})
			if err != nil {
				t.Fatal(err)
			}
			if tc.changeProject != nil {
				tc.changeProject(t, opts.ProjectDir)
			}
			candidateName := "old-policy"
			workItem := fetchprogress.WorkItemID(cacheKey)
			kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, workItem, bundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, completedAt)}
			results.values[candidateName] = struct {
				raw string
				ok  bool
				err error
			}{raw: compatibleRawResult(t, opts.Namespace, candidateName, request, key, result, map[string]ai.CacheEntry{cacheKey: entry}), ok: true}

			got, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
			if err != nil || !reused || !sameAgenticResult(got, result) {
				t.Fatalf("reused=%t result=%+v error=%v", reused, got, err)
			}
			accepted, reason := ai.AcceptAgenticCacheEntry(store.CacheSeed(request)[cacheKey], cacheKey, policy)
			if reason != ai.CacheAccepted || !sameAgenticResult(accepted, result) {
				t.Fatalf("promoted cache reason=%q result=%+v", reason, accepted)
			}
		})
	}
}

func TestContainerAnalyzerRejectsInconsistentAuthenticatedCacheResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ai.FailureAnalysisResult)
	}{
		{name: "provenance", mutate: func(result *ai.FailureAnalysisResult) { result.Analysis.ModelHash = "different-model" }},
		{name: "generation time", mutate: func(result *ai.FailureAnalysisResult) {
			result.Summary.GeneratedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			result.Analysis.GeneratedAt = result.Summary.GeneratedAt
		}},
		{name: "analysis content", mutate: func(result *ai.FailureAnalysisResult) { result.Analysis.RootCause = "different root cause" }},
		{name: "same-failure provenance", mutate: func(result *ai.FailureAnalysisResult) {
			result.Analysis.SameFailureReuse = !result.Analysis.SameFailureReuse
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := containerTaskRequest()
			key := bytes.Repeat([]byte{0x71}, 32)
			store, err := analysisruntime.NewContainerStateStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := containerAnalyzerTestOptions(t, key)
			kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
			results := &compatibleResultAPI{values: map[string]struct {
				raw string
				ok  bool
				err error
			}{}}
			analyzer, err := newContainerAnalyzer(opts, kube, results, store)
			if err != nil {
				t.Fatal(err)
			}
			policy := compatibleResultPolicy(opts)
			published := compatibleFailureResult(policy)
			cached := compatibleFailureResult(policy)
			tc.mutate(&cached)
			completedAt := time.Now().UTC().Add(-time.Minute)
			cacheKey := analysisruntime.FailureCacheKey(request)
			entry, err := ai.NewAgenticCacheEntry(cacheKey, cached, completedAt)
			if err != nil {
				t.Fatal(err)
			}
			_, bundleDigest, err := analysisruntime.BuildProjectBundleWithCache(opts.ProjectDir, ContainerAnalysisContractVersion, request, map[string]ai.CacheEntry{cacheKey: entry})
			if err != nil {
				t.Fatal(err)
			}
			candidateName := "old-inconsistent"
			kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, fetchprogress.WorkItemID(cacheKey), bundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, completedAt)}
			results.values[candidateName] = struct {
				raw string
				ok  bool
				err error
			}{raw: compatibleRawResult(t, opts.Namespace, candidateName, request, key, published, map[string]ai.CacheEntry{cacheKey: entry}), ok: true}

			_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
			if err != nil || reused {
				t.Fatalf("reused=%t error=%v", reused, err)
			}
			if len(store.CacheSeed(request)) != 0 {
				t.Fatal("inconsistent result was promoted")
			}
		})
	}
}

func TestContainerAnalyzerReusesGeneratedEntryFromUnseededTask(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7e}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
	results := &compatibleResultAPI{values: map[string]struct {
		raw string
		ok  bool
		err error
	}{}}
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	policy := compatibleResultPolicy(opts)
	result := compatibleFailureResult(policy)
	prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().UTC().Add(-time.Minute)
	candidateName := "old-generated"
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	kube.listed = []unstructured.Unstructured{compatibleTaskObject(opts.Namespace, candidateName, workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, completedAt)}
	cacheKey := analysisruntime.FailureCacheKey(request)
	entry, err := ai.NewAgenticCacheEntry(cacheKey, result, completedAt)
	if err != nil {
		t.Fatal(err)
	}
	results.values[candidateName] = struct {
		raw string
		ok  bool
		err error
	}{raw: compatibleRawResult(t, opts.Namespace, candidateName, request, key, result, map[string]ai.CacheEntry{cacheKey: entry}), ok: true}

	_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, policy)
	if err != nil || !reused {
		t.Fatalf("reused=%t error=%v", reused, err)
	}
}

type deadlineCompatibleResultAPI struct {
	deadlines []time.Time
}

func (a *deadlineCompatibleResultAPI) Result(ctx context.Context, _, _ string) (string, bool, error) {
	deadline, _ := ctx.Deadline()
	a.deadlines = append(a.deadlines, deadline)
	return "", false, &ResultHTTPError{StatusCode: http.StatusBadGateway}
}

func TestContainerAnalyzerCompatibleCandidatesShareOneTimeoutBudget(t *testing.T) {
	request := containerTaskRequest()
	key := bytes.Repeat([]byte{0x7f}, 32)
	store, err := analysisruntime.NewContainerStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := containerAnalyzerTestOptions(t, key)
	results := &deadlineCompatibleResultAPI{}
	kube := &compatibleResultKube{fakeContainerAnalyzerKube: &fakeContainerAnalyzerKube{fakeContainerResourceClient: &fakeContainerResourceClient{}}}
	analyzer, err := newContainerAnalyzer(opts, kube, results, store)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareContainerAnalysisTask(analyzer.taskSpec(request, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	workItem := fetchprogress.WorkItemID(analysisruntime.FailureCacheKey(request))
	now := time.Now().UTC()
	for i := 0; i < containerCompatibleResultCandidateLimit; i++ {
		kube.listed = append(kube.listed, compatibleTaskObject(opts.Namespace, "old-budget-"+string(rune('a'+i)), workItem, prepared.BundleDigest, containerStateKeyFingerprint(key), "Succeeded", true, now.Add(-time.Duration(i+1)*time.Minute)))
	}
	_, reused, err := analyzer.ReuseCompatibleResult(t.Context(), request, compatibleResultPolicy(opts))
	if err != nil || reused || len(results.deadlines) != containerCompatibleResultCandidateLimit {
		t.Fatalf("reused=%t error=%v deadlines=%v", reused, err, results.deadlines)
	}
	for _, deadline := range results.deadlines[1:] {
		if !deadline.Equal(results.deadlines[0]) {
			t.Fatalf("candidate deadlines differ: %v", results.deadlines)
		}
	}
}
