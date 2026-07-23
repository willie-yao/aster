package orka

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeContainerResourceClient struct {
	applyErrAt                int
	createExists              bool
	createErr                 error
	getErr                    error
	getCalls                  int
	supersedeClaimOnSecondGet bool
	patchErr                  error
	deleteErr                 error
	deleteVersionErr          error
	deleteVersionConflict     bool
	listErr                   error
	taskErr                   error
	applied                   []string
	created                   []string
	claimed                   []string
	deleted                   []string
	deletedVersion            []string
	listed                    []unstructured.Unstructured
	existing                  *unstructured.Unstructured
	taskStates                map[string]TaskState
	taskStateSequences        map[string][]TaskState
}

func (f *fakeContainerResourceClient) Apply(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) error {
	name := (&unstructured.Unstructured{Object: obj}).GetName()
	f.applied = append(f.applied, gvr.Resource+"/"+name)
	if f.applyErrAt > 0 && len(f.applied) == f.applyErrAt {
		return errors.New("apply failed")
	}
	return nil
}

func (f *fakeContainerResourceClient) CreateIfAbsent(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) (bool, error) {
	name := (&unstructured.Unstructured{Object: obj}).GetName()
	f.created = append(f.created, gvr.Resource+"/"+name)
	if f.createErr != nil {
		return false, f.createErr
	}
	if f.createExists {
		return false, nil
	}
	f.existing = (&unstructured.Unstructured{Object: obj}).DeepCopy()
	f.existing.SetResourceVersion("rv-created")
	return true, nil
}

func (f *fakeContainerResourceClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	f.getCalls++
	if f.supersedeClaimOnSecondGet && f.getCalls == 2 && f.existing != nil {
		annotations := f.existing.GetAnnotations()
		annotations[containerAnalysisClaimAnnotation] = "other-claim"
		f.existing.SetAnnotations(annotations)
	}
	return f.existing, f.getErr
}

func (f *fakeContainerResourceClient) PatchAnnotations(_ context.Context, gvr schema.GroupVersionResource, _, name string, annotations map[string]string) (string, error) {
	f.claimed = append(f.claimed, gvr.Resource+"/"+name)
	if f.patchErr != nil {
		return "", f.patchErr
	}
	if f.existing != nil {
		merged := f.existing.GetAnnotations()
		if merged == nil {
			merged = map[string]string{}
		}
		for key, value := range annotations {
			merged[key] = value
		}
		f.existing.SetAnnotations(merged)
		f.existing.SetResourceVersion("rv-claimed")
	}
	return "rv-claimed", nil
}

func (f *fakeContainerResourceClient) Delete(_ context.Context, gvr schema.GroupVersionResource, _ string, name string) error {
	f.deleted = append(f.deleted, gvr.Resource+"/"+name)
	return f.deleteErr
}

func (f *fakeContainerResourceClient) DeleteIfResourceVersion(_ context.Context, gvr schema.GroupVersionResource, _, name, resourceVersion string) (bool, error) {
	f.deletedVersion = append(f.deletedVersion, gvr.Resource+"/"+name+"@"+resourceVersion)
	if f.deleteVersionErr != nil {
		return false, f.deleteVersionErr
	}
	return !f.deleteVersionConflict, nil
}

func (f *fakeContainerResourceClient) ListByLabel(_ context.Context, gvr schema.GroupVersionResource, _ string, selector string) ([]unstructured.Unstructured, error) {
	if gvr != configMapsGVR || selector != containerAnalysisBundleSelector {
		return nil, errors.New("unexpected list")
	}
	return f.listed, f.listErr
}

func (f *fakeContainerResourceClient) TaskState(_ context.Context, _, name string) (TaskState, error) {
	if f.taskErr != nil {
		return TaskState{}, f.taskErr
	}
	if sequence := f.taskStateSequences[name]; len(sequence) > 0 {
		state := sequence[0]
		f.taskStateSequences[name] = sequence[1:]
		return state, nil
	}
	return f.taskStates[name], nil
}

func lifecycleResources() ContainerAnalysisResources {
	return ContainerAnalysisResources{
		BundleConfigMap: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata": map[string]any{
				"name": "bundle", "namespace": "orka-system",
				"labels": map[string]any{containerAnalysisBundleLabel: "true"},
				"annotations": map[string]any{
					"prow-ai-dashboard/bundle-digest":    "digest",
					"prow-ai-dashboard/contract-version": ContainerAnalysisContractVersion,
					containerAnalysisTaskNameAnnotation:  "task",
				},
			},
			"data": map[string]any{"bundle.json": "{}"},
		},
		Task: map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
			"metadata": map[string]any{"name": "task", "namespace": "orka-system"},
		},
	}
}

func TestPruneContainerAnalysisBundlesRemovesOnlyExpiredInactiveBundles(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	client := &fakeContainerResourceClient{
		listed: []unstructured.Unstructured{
			bundleObject("old", "old-task", now.Add(-ContainerAnalysisBundleRetention-time.Minute)),
			bundleObject("running", "running-task", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("missing", "missing-task", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("recent", "recent-task", now.Add(-time.Hour)),
			bundleObject("legacy", "", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("unknown", "unknown-task", time.Time{}),
		},
		taskStates: map[string]TaskState{
			"old-task":     {Exists: true, Phase: "Succeeded"},
			"running-task": {Exists: true, Phase: "Running"},
		},
	}
	deleted, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}
	wantDeleted := []string{"configmaps/old@rv-old", "configmaps/missing@rv-missing", "configmaps/legacy@rv-legacy"}
	if !reflect.DeepEqual(client.deletedVersion, wantDeleted) {
		t.Fatalf("versioned deletes = %v, want %v", client.deletedVersion, wantDeleted)
	}
	if len(client.created) != 0 || len(client.claimed) != 0 || len(client.applied) != 0 {
		t.Fatalf("prune applied resources: created=%v claimed=%v applied=%v", client.created, client.claimed, client.applied)
	}
}

func TestReconcileContainerAnalysisResourcesAppliesOneTaskWithoutListingBundles(t *testing.T) {
	client := &fakeContainerResourceClient{listErr: errors.New("list must not be called")}
	if err := ReconcileContainerAnalysisResources(context.Background(), client, lifecycleResources()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.created, []string{"configmaps/bundle"}) || !reflect.DeepEqual(client.claimed, []string{"configmaps/bundle"}) {
		t.Fatalf("created=%v claimed=%v", client.created, client.claimed)
	}
	if !reflect.DeepEqual(client.applied, []string{"tasks/task"}) {
		t.Fatalf("applied = %v", client.applied)
	}
}

func TestReconcileContainerAnalysisResourcesSkipsTerminalTask(t *testing.T) {
	resources := lifecycleResources()
	existing := (&unstructured.Unstructured{Object: resources.BundleConfigMap}).DeepCopy()
	existing.SetResourceVersion("rv-terminal")
	client := &fakeContainerResourceClient{
		existing:   existing,
		taskStates: map[string]TaskState{"task": {Exists: true, Phase: "Succeeded", UID: "uid-task"}},
	}
	if err := ReconcileContainerAnalysisResources(context.Background(), client, resources); err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 0 || len(client.claimed) != 0 || len(client.applied) != 0 {
		t.Fatalf("terminal Task recreated resources: created=%v claimed=%v applied=%v", client.created, client.claimed, client.applied)
	}
	if !reflect.DeepEqual(client.deletedVersion, []string{"configmaps/bundle@rv-terminal"}) {
		t.Fatalf("versioned deletes = %v", client.deletedVersion)
	}
}

func TestApplyContainerAnalysisResourcesDoesNotDeleteOnTaskFailure(t *testing.T) {
	resources := lifecycleResources()
	client := &fakeContainerResourceClient{applyErrAt: 1}
	err := ApplyContainerAnalysisResources(context.Background(), client, resources)
	if err == nil || !strings.Contains(err.Error(), "apply container analysis Task") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.deletedVersion) != 0 {
		t.Fatalf("failed reconciliation deleted bundle: %v", client.deletedVersion)
	}

	existing := (&unstructured.Unstructured{Object: resources.BundleConfigMap}).DeepCopy()
	existing.SetResourceVersion("rv-existing")
	client = &fakeContainerResourceClient{applyErrAt: 1, createExists: true, existing: existing}
	err = ApplyContainerAnalysisResources(context.Background(), client, resources)
	if err == nil || !strings.Contains(err.Error(), "apply container analysis Task") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.deletedVersion) != 0 {
		t.Fatalf("existing bundle was deleted: %v", client.deletedVersion)
	}
}

func TestApplyContainerAnalysisResourcesStopsWhenClaimIsSuperseded(t *testing.T) {
	client := &fakeContainerResourceClient{supersedeClaimOnSecondGet: true}
	err := ApplyContainerAnalysisResources(context.Background(), client, lifecycleResources())
	if err == nil || !strings.Contains(err.Error(), "claim was superseded") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.applied) != 0 || len(client.deletedVersion) != 0 {
		t.Fatalf("superseded claimant changed resources: applied=%v deleted=%v", client.applied, client.deletedVersion)
	}
}

func TestPruneContainerAnalysisBundlesSkipsActiveMissingTaskClaim(t *testing.T) {
	now := time.Now().UTC()
	active := bundleObject("active", "missing-task", now.Add(-2*ContainerAnalysisBundleRetention))
	annotations := active.GetAnnotations()
	annotations[containerAnalysisClaimAnnotation] = "claim"
	annotations[containerAnalysisClaimTimeAnnotation] = now.Format(time.RFC3339Nano)
	active.SetAnnotations(annotations)
	client := &fakeContainerResourceClient{listed: []unstructured.Unstructured{active}}
	deleted, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || len(client.deletedVersion) != 0 {
		t.Fatalf("active claim was pruned: deleted=%d calls=%v", deleted, client.deletedVersion)
	}

	annotations[containerAnalysisClaimTimeAnnotation] = now.Add(-ContainerAnalysisClaimTTL - time.Second).Format(time.RFC3339Nano)
	active.SetAnnotations(annotations)
	client = &fakeContainerResourceClient{listed: []unstructured.Unstructured{active}}
	deleted, err = PruneContainerAnalysisBundles(context.Background(), client, "orka-system", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expired claim deleted = %d, want 1", deleted)
	}

	annotations[containerAnalysisClaimTimeAnnotation] = "not-a-time"
	active.SetAnnotations(annotations)
	client = &fakeContainerResourceClient{listed: []unstructured.Unstructured{active}}
	deleted, err = PruneContainerAnalysisBundles(context.Background(), client, "orka-system", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("malformed claim deleted = %d, want 1", deleted)
	}
}

func TestPruneContainerAnalysisBundlesSkipsConcurrentClaim(t *testing.T) {
	now := time.Now()
	client := &fakeContainerResourceClient{
		listed:                []unstructured.Unstructured{bundleObject("raced", "missing-task", now.Add(-2*ContainerAnalysisBundleRetention))},
		deleteVersionConflict: true,
	}
	deleted, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || !reflect.DeepEqual(client.deletedVersion, []string{"configmaps/raced@rv-raced"}) {
		t.Fatalf("deleted=%d versioned deletes=%v", deleted, client.deletedVersion)
	}
}

func TestApplyContainerAnalysisResourcesRejectsMismatchedExistingBundle(t *testing.T) {
	resources := lifecycleResources()
	existing := &unstructured.Unstructured{Object: deepCopyMap(resources.BundleConfigMap)}
	existing.SetResourceVersion("rv-existing")
	existing.Object["data"].(map[string]any)["bundle.json"] = "different"
	client := &fakeContainerResourceClient{createExists: true, existing: existing}
	if err := ApplyContainerAnalysisResources(context.Background(), client, resources); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.applied) != 0 || len(client.deletedVersion) != 0 {
		t.Fatalf("mismatched existing bundle changed resources: applied=%v deleted=%v", client.applied, client.deletedVersion)
	}
}

func TestCleanupContainerAnalysisBundleUsesTaskUIDAndResourceVersion(t *testing.T) {
	resources := lifecycleResources()
	existing := (&unstructured.Unstructured{Object: resources.BundleConfigMap}).DeepCopy()
	existing.SetResourceVersion("rv-terminal")
	terminal := TaskState{Exists: true, Phase: "Succeeded", UID: "uid-old"}
	client := &fakeContainerResourceClient{
		existing:           existing,
		taskStateSequences: map[string][]TaskState{"task": {terminal, terminal}},
	}
	if err := CleanupContainerAnalysisBundle(context.Background(), client, resources, "uid-old"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.deletedVersion, []string{"configmaps/bundle@rv-terminal"}) {
		t.Fatalf("versioned deletes = %v", client.deletedVersion)
	}

	client = &fakeContainerResourceClient{
		existing: existing,
		taskStateSequences: map[string][]TaskState{"task": {
			terminal,
			{Exists: true, Phase: "Running", UID: "uid-replacement"},
		}},
	}
	if err := CleanupContainerAnalysisBundle(context.Background(), client, resources, "uid-old"); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedVersion) != 0 {
		t.Fatalf("replacement Task bundle was deleted: %v", client.deletedVersion)
	}

	client = &fakeContainerResourceClient{
		existing:              existing,
		deleteVersionConflict: true,
		taskStateSequences:    map[string][]TaskState{"task": {terminal, terminal}},
	}
	if err := CleanupContainerAnalysisBundle(context.Background(), client, resources, "uid-old"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.deletedVersion, []string{"configmaps/bundle@rv-terminal"}) {
		t.Fatalf("versioned deletes = %v", client.deletedVersion)
	}
}

func TestPruneContainerAnalysisBundlesFailsClosed(t *testing.T) {
	client := &fakeContainerResourceClient{listErr: errors.New("forbidden")}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
	client = &fakeContainerResourceClient{
		listed:           []unstructured.Unstructured{bundleObject("old", "old-task", time.Now().Add(-2*ContainerAnalysisBundleRetention))},
		taskStates:       map[string]TaskState{"old-task": {Exists: true, Phase: "Failed"}},
		deleteVersionErr: errors.New("forbidden"),
	}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "delete expired") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
	client = &fakeContainerResourceClient{
		listed:  []unstructured.Unstructured{bundleObject("old", "old-task", time.Now().Add(-2*ContainerAnalysisBundleRetention))},
		taskErr: errors.New("forbidden"),
	}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "read Task") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
}

func bundleObject(name, taskName string, created time.Time) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": "orka-system",
			"annotations": map[string]any{containerAnalysisTaskNameAnnotation: taskName},
		},
	}}
	object.SetResourceVersion("rv-" + name)
	if !created.IsZero() {
		object.SetCreationTimestamp(metav1.NewTime(created))
	}
	return object
}

func deepCopyMap(in map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: in}).DeepCopy().Object
}
