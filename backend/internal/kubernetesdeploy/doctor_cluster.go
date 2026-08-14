package kubernetesdeploy

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	namespacesGVR                = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	podsGVR                      = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	servicesGVR                  = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	serviceAccountsGVR           = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	configMapsGVR                = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	persistentClaimsGVR          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	resourceQuotasGVR            = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}
	limitRangesGVR               = schema.GroupVersionResource{Version: "v1", Resource: "limitranges"}
	nodesGVR                     = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	secretsGVR                   = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	deploymentsGVR               = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	cronJobsGVR                  = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	endpointSlicesGVR            = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
	ingressesGVR                 = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	networkPoliciesGVR           = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
	storageClassesGVR            = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	runtimeClassesGVR            = schema.GroupVersionResource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"}
	customResourcesGVR           = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	sandboxesGVR                 = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	ciliumPoliciesGVR            = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	ciliumClusterwidePoliciesGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumclusterwidenetworkpolicies"}
)

type clusterReader interface {
	ServerVersion(context.Context) (string, error)
	HasResource(context.Context, schema.GroupVersionResource) (bool, error)
	Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error)
	List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error)
	SecretMetadata(context.Context, string, string) (*metav1.PartialObjectMetadata, error)
	ListSecretMetadata(context.Context, string, string) (*metav1.PartialObjectMetadataList, error)
}

type readOnlyCluster struct {
	dynamic   dynamic.Interface
	metadata  metadata.Interface
	discovery discovery.DiscoveryInterface
}

func newReadOnlyCluster(contextName string) (clusterReader, error) {
	contextName = strings.TrimSpace(contextName)
	if contextName == "" {
		return nil, fmt.Errorf("--kube-context is required; the current default context is never used implicitly")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	deferred := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := deferred.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if _, ok := raw.Contexts[contextName]; !ok {
		return nil, fmt.Errorf("kube context %q does not exist", contextName)
	}
	cfg, err := deferred.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube context %q: %w", contextName, err)
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes reader: %w", err)
	}
	metadataClient, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes metadata reader: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes discovery reader: %w", err)
	}
	return &readOnlyCluster{dynamic: dynamicClient, metadata: metadataClient, discovery: discoveryClient}, nil
}

func (r *readOnlyCluster) ServerVersion(_ context.Context) (string, error) {
	version, err := r.discovery.ServerVersion()
	if err != nil {
		return "", err
	}
	return version.GitVersion, nil
}

func (r *readOnlyCluster) HasResource(_ context.Context, gvr schema.GroupVersionResource) (bool, error) {
	resources, err := r.discovery.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return false, err
	}
	for _, resource := range resources.APIResources {
		if resource.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}

func (r *readOnlyCluster) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	resource := r.dynamic.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return resource.Get(ctx, name, metav1.GetOptions{})
}

func (r *readOnlyCluster) List(ctx context.Context, gvr schema.GroupVersionResource, namespace, selector string) (*unstructured.UnstructuredList, error) {
	options := metav1.ListOptions{LabelSelector: selector}
	resource := r.dynamic.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).List(ctx, options)
	}
	return resource.List(ctx, options)
}

func (r *readOnlyCluster) SecretMetadata(ctx context.Context, namespace, name string) (*metav1.PartialObjectMetadata, error) {
	return r.metadata.Resource(secretsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *readOnlyCluster) ListSecretMetadata(ctx context.Context, namespace, selector string) (*metav1.PartialObjectMetadataList, error) {
	return r.metadata.Resource(secretsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
}
