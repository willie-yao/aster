package orka

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// TasksGVR identifies the Orka Task resource.
var TasksGVR = schema.GroupVersionResource{Group: "core.orka.ai", Version: "v1alpha1", Resource: "tasks"}

const fieldManager = "prow-ai-dashboard"

// RESTConfig returns in-cluster config when running in a pod, otherwise the
// default kubeconfig loading rules (KUBECONFIG / ~/.kube/config). A non-empty
// context selects a kubeconfig context for local runs.
func RESTConfig(context string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		overrides.CurrentContext = context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

// KubeClient applies Orka Tasks and reads their status through the dynamic client.
type KubeClient struct {
	dyn     dynamic.Interface
	Manager string
}

// NewKubeClient builds a dynamic client from cfg.
func NewKubeClient(cfg *rest.Config) (*KubeClient, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &KubeClient{dyn: dyn}, nil
}

// Apply server-side applies an unstructured object (create or update).
// It is idempotent for content-addressed Tasks and immutable bundle ownership.
func (k *KubeClient) Apply(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj map[string]any) error {
	u := &unstructured.Unstructured{Object: obj}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	manager := k.Manager
	if manager == "" {
		manager = fieldManager
	}
	force := true
	_, err = k.dyn.Resource(gvr).Namespace(ns).Patch(ctx, u.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: manager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s/%s: %w", gvr.Resource, u.GetName(), err)
	}
	return nil
}

// CreateIfAbsent creates an object and reports whether this call created it.
func (k *KubeClient) CreateIfAbsent(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj map[string]any) (bool, error) {
	u := &unstructured.Unstructured{Object: obj}
	if _, err := k.dyn.Resource(gvr).Namespace(ns).Create(ctx, u, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("create %s/%s: %w", gvr.Resource, u.GetName(), err)
	}
	return true, nil
}

// Get returns one unstructured object.
func (k *KubeClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	u, err := k.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// PatchAnnotations updates metadata annotations and returns the new resource version.
func (k *KubeClient) PatchAnnotations(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, annotations map[string]string) (string, error) {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return "", err
	}
	u, err := k.dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	if err != nil {
		return "", fmt.Errorf("patch %s/%s annotations: %w", gvr.Resource, name, err)
	}
	return u.GetResourceVersion(), nil
}

// DeleteIfResourceVersion deletes only when the object has not changed.
func (k *KubeClient) DeleteIfResourceVersion(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string) (bool, error) {
	preconditions := &metav1.Preconditions{ResourceVersion: &resourceVersion}
	err := k.dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: preconditions})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TaskState is the existing execution state needed before reapplying a Task.
type TaskState struct {
	Exists          bool
	Phase           string
	Execution       map[string]any
	ResourceVersion string
	UID             string
	Attempts        int
	Deleting        bool
	ResultAvailable bool
	CompletionTime  time.Time
	Annotations     map[string]string
}

// TaskState returns a Task's phase and execution placement. A missing Task has
// Exists=false and no error.
func (k *KubeClient) TaskState(ctx context.Context, ns, name string) (TaskState, error) {
	u, err := k.dyn.Resource(TasksGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return TaskState{}, nil
	}
	if err != nil {
		return TaskState{}, err
	}
	state, err := taskStateFromObject(u)
	if err != nil {
		return TaskState{}, fmt.Errorf("read Task %s state: %w", name, err)
	}
	return state, nil
}

func taskStateFromObject(u *unstructured.Unstructured) (TaskState, error) {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	attempts, foundAttempts, err := unstructured.NestedInt64(u.Object, "status", "attempts")
	if err != nil || (foundAttempts && (attempts < 0 || attempts > 1<<31-1)) {
		return TaskState{}, fmt.Errorf("invalid status.attempts")
	}
	execution, found, err := unstructured.NestedMap(u.Object, "spec", "execution")
	if err != nil {
		return TaskState{}, err
	}
	if !found {
		execution = nil
	}
	resultAvailable, foundResult, err := unstructured.NestedBool(u.Object, "status", "resultRef", "available")
	if err != nil {
		return TaskState{}, fmt.Errorf("invalid status.resultRef.available")
	}
	if !foundResult {
		resultAvailable = false
	}
	var completionTime time.Time
	completionText, foundCompletion, err := unstructured.NestedString(u.Object, "status", "completionTime")
	if err != nil {
		return TaskState{}, fmt.Errorf("invalid status.completionTime")
	}
	if foundCompletion && strings.TrimSpace(completionText) != "" {
		completionTime, err = time.Parse(time.RFC3339Nano, completionText)
		if err != nil {
			return TaskState{}, fmt.Errorf("invalid status.completionTime")
		}
	}
	return TaskState{
		Exists: true, Phase: phase, Execution: execution,
		ResourceVersion: u.GetResourceVersion(), UID: string(u.GetUID()), Attempts: int(attempts),
		Deleting: u.GetDeletionTimestamp() != nil, ResultAvailable: resultAvailable, CompletionTime: completionTime,
		Annotations: u.GetAnnotations(),
	}, nil
}

// DeleteTaskIfIdentity deletes only the exact observed Task identity.
func (k *KubeClient) DeleteTaskIfIdentity(ctx context.Context, ns, name, uid, resourceVersion string) (bool, error) {
	uidValue := types.UID(uid)
	preconditions := &metav1.Preconditions{UID: &uidValue, ResourceVersion: &resourceVersion}
	err := k.dyn.Resource(TasksGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: preconditions})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TaskPhase returns a Task's status.phase, or "" if unset.
func (k *KubeClient) TaskPhase(ctx context.Context, ns, name string) (string, error) {
	u, err := k.dyn.Resource(TasksGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	return phase, nil
}

// ListByLabel returns the objects of gvr in ns matching selector.
func (k *KubeClient) ListByLabel(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) ([]unstructured.Unstructured, error) {
	l, err := k.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Delete removes a named object, ignoring not-found.
func (k *KubeClient) Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) error {
	err := k.dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// TerminalPhase reports whether a Task phase is final (no further transitions).
func TerminalPhase(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Cancelled":
		return true
	default:
		return false
	}
}

// IsNotFound reports whether err is a Kubernetes not-found error.
func IsNotFound(err error) bool { return apierrors.IsNotFound(err) }
