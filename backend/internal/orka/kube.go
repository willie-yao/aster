package orka

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// GroupVersionResources for the Orka CRDs the pipeline creates.
var (
	TasksGVR = schema.GroupVersionResource{Group: "core.orka.ai", Version: "v1alpha1", Resource: "tasks"}
	ToolsGVR = schema.GroupVersionResource{Group: "core.orka.ai", Version: "v1alpha1", Resource: "tools"}
)

const fieldManager = "orka-producer"

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

// KubeClient applies Orka objects and reads Task status via the dynamic client,
// so the pipeline needs no Orka Go types.
type KubeClient struct{ dyn dynamic.Interface }

// NewKubeClient builds a dynamic client from cfg.
func NewKubeClient(cfg *rest.Config) (*KubeClient, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &KubeClient{dyn: dyn}, nil
}

// Apply server-side applies an unstructured object (create or update).
// Idempotent, which matches the content-addressed run-once Task contract.
func (k *KubeClient) Apply(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj map[string]any) error {
	u := &unstructured.Unstructured{Object: obj}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	force := true
	_, err = k.dyn.Resource(gvr).Namespace(ns).Patch(ctx, u.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s/%s: %w", gvr.Resource, u.GetName(), err)
	}
	return nil
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
