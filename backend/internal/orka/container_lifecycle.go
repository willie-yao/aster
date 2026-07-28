package orka

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var configMapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

const (
	containerAnalysisBundleLabel          = "prow-ai-dashboard/bundle"
	containerAnalysisBundleSelector       = containerAnalysisBundleLabel + "=true"
	containerAnalysisTaskSelector         = "prow-ai-dashboard/adapter=container-analyzer"
	containerAnalysisTaskNameAnnotation   = "prow-ai-dashboard/task-name"
	containerAnalysisClaimAnnotation      = "prow-ai-dashboard/bundle-claim"
	containerAnalysisClaimTimeAnnotation  = "prow-ai-dashboard/bundle-claimed-at"
	containerAnalysisConsumedAtAnnotation = "prow-ai-dashboard/failure-consumed-at"
	// ContainerAnalysisBundleRetention bounds orphaned private input bundles.
	ContainerAnalysisBundleRetention = 24 * time.Hour
	// ContainerAnalysisClaimTTL protects an active resource-application claim.
	ContainerAnalysisClaimTTL     = 10 * time.Minute
	containerTaskDeleteWait       = 30 * time.Second
	containerFailureReuseGrace    = 30 * time.Second
	containerStaleActiveTaskGrace = 10 * time.Minute
	// ContainerAnalysisBundlePruneLimit bounds cleanup work per fetch run.
	ContainerAnalysisBundlePruneLimit = 100
	// ContainerAnalysisTaskRetention bounds terminal Task history.
	ContainerAnalysisTaskRetention = 24 * time.Hour
	// ContainerAnalysisTaskPruneLimit bounds terminal Task cleanup per fetch run.
	ContainerAnalysisTaskPruneLimit = 100
)

// ContainerAnalysisResourceClient applies and removes analyzer resources.
type ContainerAnalysisResourceClient interface {
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	CreateIfAbsent(context.Context, schema.GroupVersionResource, string, map[string]any) (bool, error)
	Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error)
	PatchAnnotations(context.Context, schema.GroupVersionResource, string, string, map[string]string) (string, error)
	DeleteIfResourceVersion(context.Context, schema.GroupVersionResource, string, string, string) (bool, error)
	ListByLabel(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error)
	TaskState(context.Context, string, string) (TaskState, error)
}

// ContainerAnalysisMaintenanceClient also deletes exact Task identities.
type ContainerAnalysisMaintenanceClient interface {
	ContainerAnalysisResourceClient
	DeleteTaskIfIdentity(context.Context, string, string, string, string) (bool, error)
}

// ContainerAnalysisReconcileResult reports whether an existing Task was adopted.
type ContainerAnalysisReconcileResult struct {
	Adopted bool
}

// ReconcileContainerAnalysisResources applies one bundle and Task without batch GC.
func ReconcileContainerAnalysisResources(ctx context.Context, client ContainerAnalysisMaintenanceClient, resources ContainerAnalysisResources) error {
	_, err := ReconcileContainerAnalysisResourcesWithResult(ctx, client, resources)
	return err
}

// ReconcileContainerAnalysisResourcesWithResult also reports Task adoption.
func ReconcileContainerAnalysisResourcesWithResult(ctx context.Context, client ContainerAnalysisMaintenanceClient, resources ContainerAnalysisResources) (ContainerAnalysisReconcileResult, error) {
	namespace, _, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return ContainerAnalysisReconcileResult{}, err
	}
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return ContainerAnalysisReconcileResult{}, err
	}
	if taskNamespace != namespace {
		return ContainerAnalysisReconcileResult{}, fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	state, err := client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return ContainerAnalysisReconcileResult{}, fmt.Errorf("read container analysis Task %s: %w", taskName, err)
	}
	result := ContainerAnalysisReconcileResult{Adopted: state.Exists}
	if state.Exists && state.Deleting {
		disappeared, err := waitForDeletingContainerAnalysisTask(ctx, client, taskNamespace, taskName, state.UID)
		if err != nil {
			return result, err
		}
		if !disappeared {
			return result, nil
		}
		result.Adopted = false
	} else if state.Exists && TerminalPhase(state.Phase) {
		if state.Phase == "Succeeded" {
			return result, nil
		}
		replace, err := consumedFailureReadyForReplacement(ctx, client, resources, time.Now().UTC())
		if err != nil {
			return result, err
		}
		if !replace {
			return result, nil
		}
		removed, err := client.DeleteTaskIfIdentity(ctx, taskNamespace, taskName, state.UID, state.ResourceVersion)
		if err != nil {
			return result, fmt.Errorf("delete consumed container analysis Task %s: %w", taskName, err)
		}
		if !removed {
			return result, nil
		}
		disappeared, err := waitForDeletingContainerAnalysisTask(ctx, client, taskNamespace, taskName, state.UID)
		if err != nil {
			return result, err
		}
		if !disappeared {
			return result, nil
		}
		result.Adopted = false
	}
	if err := ApplyContainerAnalysisResources(ctx, client, resources); err != nil {
		return result, err
	}
	return result, nil
}

func waitForDeletingContainerAnalysisTask(ctx context.Context, client ContainerAnalysisResourceClient, namespace, taskName, expectedUID string) (bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, containerTaskDeleteWait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := client.TaskState(waitCtx, namespace, taskName)
		if err != nil {
			return false, fmt.Errorf("wait for deleting container analysis Task %s: %w", taskName, err)
		}
		if !state.Exists {
			return true, nil
		}
		if state.UID != expectedUID {
			return false, nil
		}
		select {
		case <-waitCtx.Done():
			return false, fmt.Errorf("wait for deleting container analysis Task %s: %w", taskName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func consumedFailureReadyForReplacement(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources, now time.Time) (bool, error) {
	namespace, name, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return false, err
	}
	existing, err := client.Get(ctx, configMapsGVR, namespace, name)
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read consumed container analysis bundle %s: %w", name, err)
	}
	if err := validateExistingContainerAnalysisBundle(existing, resources.BundleConfigMap); err != nil {
		return false, err
	}
	consumedAt, err := time.Parse(time.RFC3339Nano, existing.GetAnnotations()[containerAnalysisConsumedAtAnnotation])
	if err != nil {
		return false, nil
	}
	return !now.Before(consumedAt.Add(containerFailureReuseGrace)), nil
}

// MarkContainerAnalysisFailureConsumed marks a failed result safe to replace on a later pass.
func MarkContainerAnalysisFailureConsumed(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources, expectedTaskUID string) error {
	namespace, name, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	state, err := client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return fmt.Errorf("read consumed container analysis Task %s: %w", taskName, err)
	}
	if !state.Exists || state.UID != expectedTaskUID || (state.Phase != "Failed" && state.Phase != "Cancelled") {
		return nil
	}
	existing, err := client.Get(ctx, configMapsGVR, namespace, name)
	if err != nil {
		return fmt.Errorf("read consumed container analysis bundle %s: %w", name, err)
	}
	if err := validateExistingContainerAnalysisBundle(existing, resources.BundleConfigMap); err != nil {
		return err
	}
	if _, err := client.PatchAnnotations(ctx, configMapsGVR, namespace, name, map[string]string{
		containerAnalysisConsumedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return fmt.Errorf("mark container analysis failure consumed: %w", err)
	}
	state, err = client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return fmt.Errorf("recheck consumed container analysis Task %s: %w", taskName, err)
	}
	if !state.Exists || state.UID != expectedTaskUID {
		return fmt.Errorf("container analysis Task %s changed while marking failure consumed", taskName)
	}
	return nil
}

// ApplyContainerAnalysisResources claims the bundle before applying its Task.
func ApplyContainerAnalysisResources(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources) error {
	bundleNamespace, bundleName, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	if taskNamespace != bundleNamespace {
		return fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	claim, err := newContainerAnalysisBundleClaim()
	if err != nil {
		return err
	}
	if _, err := client.CreateIfAbsent(ctx, configMapsGVR, bundleNamespace, resources.BundleConfigMap); err != nil {
		return fmt.Errorf("create container analysis bundle %s: %w", bundleName, err)
	}
	existing, err := client.Get(ctx, configMapsGVR, bundleNamespace, bundleName)
	if err != nil {
		return fmt.Errorf("read container analysis bundle %s: %w", bundleName, err)
	}
	if err := validateExistingContainerAnalysisBundle(existing, resources.BundleConfigMap); err != nil {
		return err
	}
	if _, err := client.PatchAnnotations(ctx, configMapsGVR, bundleNamespace, bundleName, map[string]string{
		containerAnalysisClaimAnnotation:      claim,
		containerAnalysisClaimTimeAnnotation:  time.Now().UTC().Format(time.RFC3339Nano),
		containerAnalysisConsumedAtAnnotation: "",
	}); err != nil {
		return fmt.Errorf("claim container analysis bundle %s: %w", bundleName, err)
	}
	claimed, err := client.Get(ctx, configMapsGVR, bundleNamespace, bundleName)
	if err != nil {
		return fmt.Errorf("verify container analysis bundle claim %s: %w", bundleName, err)
	}
	if err := validateExistingContainerAnalysisBundle(claimed, resources.BundleConfigMap); err != nil {
		return err
	}
	if claimed.GetAnnotations()[containerAnalysisClaimAnnotation] != claim {
		return waitForClaimedContainerAnalysisTask(ctx, client, taskNamespace, taskName, resources.Task)
	}
	if err := client.Apply(ctx, TasksGVR, taskNamespace, resources.Task); err != nil {
		return fmt.Errorf("apply container analysis Task: %w", err)
	}
	return nil
}

func waitForClaimedContainerAnalysisTask(ctx context.Context, client ContainerAnalysisResourceClient, namespace, taskName string, expected map[string]any) error {
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	expectedExecution, _, err := unstructured.NestedMap(expected, "spec", "execution")
	if err != nil {
		return fmt.Errorf("read expected container analysis Task execution: %w", err)
	}
	for {
		state, err := client.TaskState(waitCtx, namespace, taskName)
		if err != nil {
			return fmt.Errorf("observe claimed container analysis Task %s: %w", taskName, err)
		}
		if state.Exists {
			if !reflect.DeepEqual(state.Execution, expectedExecution) {
				return fmt.Errorf("claimed container analysis Task %s has mismatched execution", taskName)
			}
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("observe claimed container analysis Task %s: %w", taskName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func newContainerAnalysisBundleClaim() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create container analysis bundle claim: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validateExistingContainerAnalysisBundle(existing *unstructured.Unstructured, expected map[string]any) error {
	if existing == nil {
		return fmt.Errorf("existing container analysis bundle is missing")
	}
	expectedObject := &unstructured.Unstructured{Object: expected}
	immutable, found, err := unstructured.NestedBool(existing.Object, "immutable")
	if err != nil || !found || !immutable {
		return fmt.Errorf("existing container analysis bundle %s is not immutable", existing.GetName())
	}
	existingData, found, err := unstructured.NestedStringMap(existing.Object, "data")
	if err != nil || !found {
		return fmt.Errorf("existing container analysis bundle %s has invalid data", existing.GetName())
	}
	expectedData, found, err := unstructured.NestedStringMap(expectedObject.Object, "data")
	if err != nil || !found || !maps.Equal(existingData, expectedData) {
		return fmt.Errorf("existing container analysis bundle %s does not match the requested content", existing.GetName())
	}
	for _, key := range []string{"prow-ai-dashboard/bundle-digest", "prow-ai-dashboard/contract-version", containerAnalysisTaskNameAnnotation} {
		if existing.GetAnnotations()[key] != expectedObject.GetAnnotations()[key] {
			return fmt.Errorf("existing container analysis bundle %s has mismatched identity", existing.GetName())
		}
	}
	if existing.GetLabels()[containerAnalysisBundleLabel] != "true" {
		return fmt.Errorf("existing container analysis bundle %s is missing the retention label", existing.GetName())
	}
	return nil
}

// CleanupContainerAnalysisBundle deletes private inputs for the observed terminal Task UID.
func CleanupContainerAnalysisBundle(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources, expectedTaskUID string) error {
	namespace, name, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	if taskNamespace != namespace {
		return fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	if expectedTaskUID == "" {
		return fmt.Errorf("container analysis terminal Task UID is required")
	}
	state, err := client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return fmt.Errorf("read terminal container analysis Task %s: %w", taskName, err)
	}
	if !sameTerminalTask(state, expectedTaskUID) {
		return nil
	}
	existing, err := client.Get(ctx, configMapsGVR, namespace, name)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read terminal container analysis bundle %s: %w", name, err)
	}
	if err := validateExistingContainerAnalysisBundle(existing, resources.BundleConfigMap); err != nil {
		return err
	}
	resourceVersion := existing.GetResourceVersion()
	if resourceVersion == "" {
		return fmt.Errorf("terminal container analysis bundle %s has no resource version", name)
	}
	state, err = client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return fmt.Errorf("recheck terminal container analysis Task %s: %w", taskName, err)
	}
	if !sameTerminalTask(state, expectedTaskUID) {
		return nil
	}
	if _, err := client.DeleteIfResourceVersion(ctx, configMapsGVR, namespace, name, resourceVersion); err != nil {
		return fmt.Errorf("delete terminal container analysis bundle %s: %w", name, err)
	}
	return nil
}

func sameTerminalTask(state TaskState, expectedUID string) bool {
	return state.Exists && state.UID == expectedUID && TerminalPhase(state.Phase)
}

// PruneContainerAnalysisBundles removes orphaned bundles older than the retention window.
func PruneContainerAnalysisBundles(ctx context.Context, client ContainerAnalysisResourceClient, namespace string, now time.Time) (int, error) {
	if namespace == "" {
		return 0, fmt.Errorf("container analysis bundle namespace is required")
	}
	items, err := client.ListByLabel(ctx, configMapsGVR, namespace, containerAnalysisBundleSelector)
	if err != nil {
		return 0, fmt.Errorf("list container analysis bundles: %w", err)
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].GetCreationTimestamp().Time, items[j].GetCreationTimestamp().Time
		if left.Equal(right) {
			return items[i].GetName() < items[j].GetName()
		}
		return left.Before(right)
	})
	cutoff := now.Add(-ContainerAnalysisBundleRetention)
	deleted := 0
	deleteCandidates := 0
	for i := range items {
		created := items[i].GetCreationTimestamp()
		if created.IsZero() || created.Time.After(cutoff) {
			continue
		}
		taskName := items[i].GetAnnotations()[containerAnalysisTaskNameAnnotation]
		terminalTask := false
		if taskName != "" {
			state, err := client.TaskState(ctx, namespace, taskName)
			if err != nil {
				return deleted, fmt.Errorf("read Task for expired container analysis bundle %s: %w", items[i].GetName(), err)
			}
			if state.Exists && !TerminalPhase(state.Phase) {
				continue
			}
			terminalTask = state.Exists && TerminalPhase(state.Phase)
		}
		if !terminalTask && activeContainerAnalysisClaim(items[i].GetAnnotations(), now) {
			continue
		}
		resourceVersion := items[i].GetResourceVersion()
		if resourceVersion == "" {
			continue
		}
		if deleteCandidates >= ContainerAnalysisBundlePruneLimit {
			break
		}
		deleteCandidates++
		removed, err := client.DeleteIfResourceVersion(ctx, configMapsGVR, namespace, items[i].GetName(), resourceVersion)
		if err != nil {
			return deleted, fmt.Errorf("delete expired container analysis bundle %s: %w", items[i].GetName(), err)
		}
		if removed {
			deleted++
		}
	}
	return deleted, nil
}

// PruneContainerAnalysisTasks removes old terminal Tasks owned by this adapter.
func PruneContainerAnalysisTasks(ctx context.Context, client ContainerAnalysisMaintenanceClient, namespace string, now time.Time) (int, error) {
	if namespace == "" {
		return 0, fmt.Errorf("container analysis Task namespace is required")
	}
	items, err := client.ListByLabel(ctx, TasksGVR, namespace, containerAnalysisTaskSelector)
	if err != nil {
		return 0, fmt.Errorf("list container analysis Tasks: %w", err)
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].GetCreationTimestamp().Time, items[j].GetCreationTimestamp().Time
		if left.Equal(right) {
			return items[i].GetName() < items[j].GetName()
		}
		return left.Before(right)
	})
	cutoff := now.Add(-ContainerAnalysisTaskRetention)
	deleted := 0
	deleteCandidates := 0
	for i := range items {
		created := items[i].GetCreationTimestamp()
		if created.IsZero() {
			continue
		}
		state, err := taskStateFromObject(&items[i])
		if err != nil {
			return deleted, fmt.Errorf("read expired container analysis Task %s: %w", items[i].GetName(), err)
		}
		if state.Deleting || state.UID == "" || state.ResourceVersion == "" {
			continue
		}
		terminalExpired := TerminalPhase(state.Phase) && !created.Time.After(cutoff)
		activeExpired := !TerminalPhase(state.Phase) && staleActiveContainerAnalysisTask(&items[i], now)
		if !terminalExpired && !activeExpired {
			continue
		}
		if deleteCandidates >= ContainerAnalysisTaskPruneLimit {
			break
		}
		deleteCandidates++
		removed, err := client.DeleteTaskIfIdentity(ctx, namespace, items[i].GetName(), state.UID, state.ResourceVersion)
		if err != nil {
			return deleted, fmt.Errorf("delete expired container analysis Task %s: %w", items[i].GetName(), err)
		}
		if removed {
			deleted++
		}
	}
	return deleted, nil
}

func staleActiveContainerAnalysisTask(task *unstructured.Unstructured, now time.Time) bool {
	created := task.GetCreationTimestamp()
	if created.IsZero() {
		return false
	}
	timeoutText, found, err := unstructured.NestedString(task.Object, "spec", "timeout")
	if err != nil || !found {
		return false
	}
	taskTimeout, err := time.ParseDuration(timeoutText)
	if err != nil || taskTimeout <= 0 {
		return false
	}
	retries, found, err := unstructured.NestedInt64(task.Object, "spec", "retryPolicy", "maxRetries")
	if err != nil || !found {
		retries = 0
	}
	if retries < 0 || retries > 1<<31-1 {
		return false
	}
	deadline := created.Time.Add(containerTaskWaitTimeout(taskTimeout, int(retries))).Add(containerStaleActiveTaskGrace)
	return !now.Before(deadline)
}

func activeContainerAnalysisClaim(annotations map[string]string, now time.Time) bool {
	if annotations[containerAnalysisClaimAnnotation] == "" {
		return false
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, annotations[containerAnalysisClaimTimeAnnotation])
	if err != nil {
		return false
	}
	return now.Before(claimedAt.Add(ContainerAnalysisClaimTTL))
}

func containerResourceRef(resource map[string]any) (string, string, error) {
	object := &unstructured.Unstructured{Object: resource}
	if object.GetNamespace() == "" || object.GetName() == "" {
		return "", "", fmt.Errorf("container analysis resource requires namespace and name")
	}
	return object.GetNamespace(), object.GetName(), nil
}
