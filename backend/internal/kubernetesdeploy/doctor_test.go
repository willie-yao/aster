package kubernetesdeploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type doctorRunner struct {
	releases []string
	manifest string
	commands []recordedCommand
}

func (r *doctorRunner) Run(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
	r.commands = append(r.commands, recordedCommand{name: name, args: append([]string(nil), args...)})
	if name != "helm" || len(args) == 0 {
		return fmt.Errorf("unexpected command")
	}
	switch args[0] {
	case "list":
		items := make([]releaseSummary, 0, len(r.releases))
		for _, release := range r.releases {
			items = append(items, releaseSummary{Name: release})
		}
		return json.NewEncoder(stdout).Encode(items)
	case "template":
		_, err := io.WriteString(stdout, r.manifest)
		return err
	default:
		return fmt.Errorf("doctor attempted non-read-only Helm command %q", args[0])
	}
}

type fakeDoctorCluster struct {
	version     string
	resources   map[schema.GroupVersionResource]bool
	objects     map[string]*unstructured.Unstructured
	lists       map[string]*unstructured.UnstructuredList
	getErrors   map[string]error
	listErrors  map[string]error
	secretNames map[string]bool
	secretLists map[string]*metav1.PartialObjectMetadataList
	calls       []string
}

func (f *fakeDoctorCluster) ServerVersion(context.Context) (string, error) {
	f.calls = append(f.calls, "version")
	if f.version == "" {
		return "", fmt.Errorf("unavailable")
	}
	return f.version, nil
}

func (f *fakeDoctorCluster) HasResource(_ context.Context, gvr schema.GroupVersionResource) (bool, error) {
	f.calls = append(f.calls, "discover "+gvr.String())
	return f.resources[gvr], nil
}

func (f *fakeDoctorCluster) Get(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	key := objectKey(gvr, namespace, name)
	f.calls = append(f.calls, "get "+key)
	if err := f.getErrors[key]; err != nil {
		return nil, err
	}
	if object, ok := f.objects[key]; ok {
		return object.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
}

func (f *fakeDoctorCluster) List(_ context.Context, gvr schema.GroupVersionResource, namespace, selector string) (*unstructured.UnstructuredList, error) {
	key := listKey(gvr, namespace, selector)
	f.calls = append(f.calls, "list "+key)
	if err := f.listErrors[key]; err != nil {
		return nil, err
	}
	if list, ok := f.lists[key]; ok {
		return list.DeepCopy(), nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (f *fakeDoctorCluster) SecretMetadata(_ context.Context, namespace, name string) (*metav1.PartialObjectMetadata, error) {
	f.calls = append(f.calls, "metadata secrets "+namespace+"/"+name)
	if !f.secretNames[namespace+"/"+name] {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}, nil
}

func (f *fakeDoctorCluster) ListSecretMetadata(_ context.Context, namespace, selector string) (*metav1.PartialObjectMetadataList, error) {
	f.calls = append(f.calls, "metadata list secrets "+namespace+" "+selector)
	if list, ok := f.secretLists[namespace+"|"+selector]; ok {
		return list.DeepCopy(), nil
	}
	return &metav1.PartialObjectMetadataList{}, nil
}

func TestKubernetesDoctorValidBaselineAndReadOnlyCommands(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(false))
	cluster := baselineCluster(false)
	runner := baselineDoctorRunner()
	report := runDoctorForTest(t, dir, "install", runner, cluster)
	if report.HasFailures() {
		t.Fatalf("checks = %+v", report.Checks)
	}
	for _, command := range runner.commands {
		if command.name != "helm" || len(command.args) == 0 || command.args[0] != "template" {
			t.Fatalf("non-read-only Helm command: %+v", command)
		}
	}
	for _, call := range cluster.calls {
		if strings.HasPrefix(call, "create ") || strings.HasPrefix(call, "update ") || strings.HasPrefix(call, "patch ") || strings.HasPrefix(call, "delete ") {
			t.Fatalf("Kubernetes write observed: %s", call)
		}
	}
	var output bytes.Buffer
	if err := WriteKubernetesDoctorReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "kubernetes_doctor=pass") || !strings.Contains(output.String(), "unverified_assumptions=") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestKubernetesDoctorValidAgentSandboxBaseline(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(true))
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), baselineCluster(true))
	if report.HasFailures() {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestKubernetesDoctorRequiresExplicitContext(t *testing.T) {
	report := runKubernetesDoctor(context.Background(), KubernetesDoctorOptions{}, kubernetesDoctorDependencies{
		consumerDoctor: passingConsumerDoctor,
	})
	assertDoctorCheck(t, report, "Kubernetes context", KubernetesDoctorFail)
}

func TestKubernetesDoctorReleaseStateConflict(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(false))
	cluster := baselineCluster(false)
	runner := baselineDoctorRunner()
	setHelmRelease(cluster, "capz", "capz", 2, "deployed")
	report := runDoctorForTest(t, dir, "install", runner, cluster)
	assertDoctorCheck(t, report, "Helm release", KubernetesDoctorFail)

	runner = baselineDoctorRunner()
	cluster = baselineCluster(false)
	report = runDoctorForTest(t, dir, "upgrade", runner, cluster)
	assertDoctorCheck(t, report, "Helm release", KubernetesDoctorFail)
}

func TestKubernetesDoctorMissingNamespaceAndStorage(t *testing.T) {
	t.Run("namespace", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(false))
		cluster := baselineCluster(false)
		delete(cluster.objects, objectKey(namespacesGVR, "", "capz"))
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "application namespace", KubernetesDoctorFail)
	})
	t.Run("storage class", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(false))
		cluster := baselineCluster(false)
		delete(cluster.objects, objectKey(storageClassesGVR, "", "rwx"))
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "persistent storage", KubernetesDoctorFail)
	})
	t.Run("invalid existing claim", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		claim := cluster.objects[objectKey(persistentClaimsGVR, "capz", "dashboard-data")]
		_ = unstructured.SetNestedStringSlice(claim.Object, []string{"ReadWriteOnce"}, "spec", "accessModes")
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "persistent storage", KubernetesDoctorFail)
	})
}

func TestKubernetesDoctorAgentSandboxFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeDoctorCluster)
		check  string
	}{
		{name: "missing CRD", mutate: func(c *fakeDoctorCluster) {
			delete(c.objects, objectKey(customResourcesGVR, "", "sandboxes.agents.x-k8s.io"))
		}, check: "Agent Sandbox CRD"},
		{name: "unsupported served version", mutate: func(c *fakeDoctorCluster) {
			crd := c.objects[objectKey(customResourcesGVR, "", "sandboxes.agents.x-k8s.io")]
			_ = unstructured.SetNestedSlice(crd.Object, []any{map[string]any{"name": "v1alpha1", "served": true, "storage": true}}, "spec", "versions")
		}, check: "Agent Sandbox CRD"},
		{name: "controller unavailable", mutate: func(c *fakeDoctorCluster) {
			delete(c.objects, objectKey(deploymentsGVR, agentSandboxSystemNamespace, agentSandboxControllerName))
		}, check: "Agent Sandbox controller"},
		{name: "missing RuntimeClass", mutate: func(c *fakeDoctorCluster) { delete(c.objects, objectKey(runtimeClassesGVR, "", "kata-vm-isolation")) }, check: "Agent Sandbox RuntimeClass"},
		{name: "no matching nodes", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(nodesGVR, "", "")] = objectList(nodeObject("node-a", false, map[string]string{"runtime": "other"}))
		}, check: "secure runtime nodes"},
		{name: "missing execution namespace", mutate: func(c *fakeDoctorCluster) { delete(c.objects, objectKey(namespacesGVR, "", "sandbox")) }, check: "Agent Sandbox execution namespace"},
		{name: "wrong namespace runtime", mutate: func(c *fakeDoctorCluster) {
			namespace := c.objects[objectKey(namespacesGVR, "", "sandbox")]
			annotations := namespace.GetAnnotations()
			annotations["prow-ai-dashboard/runtime-class"] = "runc"
			namespace.SetAnnotations(annotations)
		}, check: "Agent Sandbox execution namespace"},
		{name: "wrong namespace controller version", mutate: func(c *fakeDoctorCluster) {
			namespace := c.objects[objectKey(namespacesGVR, "", "sandbox")]
			annotations := namespace.GetAnnotations()
			annotations["prow-ai-dashboard/agent-sandbox-version"] = "v0.5.2"
			namespace.SetAnnotations(annotations)
		}, check: "Agent Sandbox execution namespace"},
		{name: "missing platform binding", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(configMapsGVR, "capz", "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=platform-binding")] = objectList()
		}, check: "platform binding"},
		{name: "coordinated gateway policy drift", mutate: func(c *fakeDoctorCluster) {
			namespace := c.objects[objectKey(namespacesGVR, "", "sandbox")]
			annotations := namespace.GetAnnotations()
			annotations["prow-ai-dashboard/model-gateway-namespace"] = "attacker"
			annotations["prow-ai-dashboard/model-gateway-target-port"] = "9443"
			allowed := []string{"github.com", "api.github.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
			annotations["prow-ai-dashboard/execution-policy-sha256"] = executionPolicyHash(allowed, annotations)
			namespace.SetAnnotations(annotations)
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			policyAnnotations := policy.GetAnnotations()
			policyAnnotations["prow-ai-dashboard/execution-policy-sha256"] = annotations["prow-ai-dashboard/execution-policy-sha256"]
			policy.SetAnnotations(policyAnnotations)
			egress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
			gateway := egress[1].(map[string]any)
			endpointLabels := gateway["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
			endpointLabels["k8s:io.kubernetes.pod.namespace"] = "attacker"
			gateway["toPorts"].([]any)[0].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"] = "9443"
			_ = unstructured.SetNestedSlice(policy.Object, egress, "spec", "egress")
		}, check: "platform binding"},
		{name: "missing Cilium API", mutate: func(c *fakeDoctorCluster) { c.resources[ciliumPoliciesGVR] = false }, check: "Agent Sandbox Cilium policy"},
		{name: "missing Cilium policy", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")] = objectList()
		}, check: "Agent Sandbox Cilium policy"},
		{name: "empty Cilium policy", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			policy.Object["spec"] = map[string]any{"endpointSelector": map[string]any{}, "ingress": []any{}, "egress": []any{}}
		}, check: "Agent Sandbox Cilium policy"},
		{name: "world Cilium policy", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			egress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
			egress = append(egress, map[string]any{"toEntities": []any{"world"}})
			_ = unstructured.SetNestedSlice(policy.Object, egress, "spec", "egress")
		}, check: "Agent Sandbox Cilium policy"},
		{name: "Cilium policy hash mismatch", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			annotations := policy.GetAnnotations()
			annotations["prow-ai-dashboard/execution-policy-sha256"] = "wrong"
			policy.SetAnnotations(annotations)
		}, check: "Agent Sandbox Cilium policy"},
		{name: "gateway Cilium namespace drift", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			egress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
			gateway := egress[1].(map[string]any)
			endpoints := gateway["toEndpoints"].([]any)
			labels := endpoints[0].(map[string]any)["matchLabels"].(map[string]any)
			labels["k8s:io.kubernetes.pod.namespace"] = "attacker"
			_ = unstructured.SetNestedSlice(policy.Object, egress, "spec", "egress")
		}, check: "Agent Sandbox Cilium policy"},
		{name: "gateway Cilium port drift", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items[0]
			egress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
			gateway := egress[1].(map[string]any)
			toPorts := gateway["toPorts"].([]any)
			ports := toPorts[0].(map[string]any)["ports"].([]any)
			ports[0].(map[string]any)["port"] = "443"
			_ = unstructured.SetNestedSlice(policy.Object, egress, "spec", "egress")
		}, check: "Agent Sandbox Cilium policy"},
		{name: "additional world Cilium policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(ciliumPoliciesGVR, "sandbox", "manual-world", map[string]any{"spec": map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{"toEntities": []any{"world"}}}}})
			c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items = append(c.lists[listKey(ciliumPoliciesGVR, "sandbox", "")].Items, *extra)
		}, check: "Agent Sandbox Cilium policy"},
		{name: "additional Kubernetes egress policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(networkPoliciesGVR, "sandbox", "manual-egress", map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Egress"}, "egress": []any{map[string]any{}}}})
			c.lists[listKey(networkPoliciesGVR, "sandbox", "")].Items = append(c.lists[listKey(networkPoliciesGVR, "sandbox", "")].Items, *extra)
		}, check: "Agent Sandbox network policy"},
		{name: "broadened default deny policy", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(networkPoliciesGVR, "sandbox", "")].Items[0]
			_ = unstructured.SetNestedSlice(policy.Object, []any{map[string]any{}}, "spec", "egress")
		}, check: "Agent Sandbox network policy"},
		{name: "cluster-wide world Cilium policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(ciliumClusterwidePoliciesGVR, "", "world", map[string]any{"spec": map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{"toEntities": []any{"world"}}}}})
			c.lists[listKey(ciliumClusterwidePoliciesGVR, "", "")] = objectList(extra)
		}, check: "Agent Sandbox Cilium policy"},
		{name: "missing quota", mutate: func(c *fakeDoctorCluster) { c.lists[listKey(resourceQuotasGVR, "sandbox", "")] = objectList() }, check: "Agent Sandbox ResourceQuota"},
		{name: "malformed quota", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(resourceQuotasGVR, "sandbox", "")] = objectList(object(resourceQuotasGVR, "sandbox", "bounds", map[string]any{"spec": map[string]any{"hard": map[string]any{"pods": "0"}}}))
		}, check: "Agent Sandbox ResourceQuota"},
		{name: "gateway Service missing", mutate: func(c *fakeDoctorCluster) { delete(c.objects, objectKey(servicesGVR, "gateway-ns", "model-gateway")) }, check: "model gateway Service"},
		{name: "gateway endpoints missing", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(endpointSlicesGVR, "gateway-ns", "kubernetes.io/service-name=model-gateway")] = objectList()
		}, check: "model gateway endpoints"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeDoctorBundle(t, baselineValues(true))
			cluster := baselineCluster(true)
			test.mutate(cluster)
			report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
			assertDoctorCheck(t, report, test.check, KubernetesDoctorFail)
		})
	}
}

func TestKubernetesDoctorPublicCAPrivateDNSGatewayBaseline(t *testing.T) {
	values := strings.Replace(baselineValues(true), "https://model-gateway.gateway-ns.svc.cluster.local/v1/chat/completions", "https://gateway.platform.example.com/v1/chat/completions", 1)
	values = strings.Replace(values, "publicCAPrivateDNS: false", "publicCAPrivateDNS: true", 1)
	dir := writeDoctorBundle(t, values)
	cluster := baselineCluster(true)
	bindingList := cluster.lists[listKey(configMapsGVR, "capz", "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=platform-binding")]
	_ = unstructured.SetNestedField(bindingList.Items[0].Object, "gateway.platform.example.com", "data", "modelGatewayPublicHost")

	selector := "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=model-gateway"
	service := object(servicesGVR, "capz", "platform-model-gateway", map[string]any{"spec": map[string]any{"type": "ClusterIP", "selector": map[string]any{"app.kubernetes.io/name": "prow-ai-dashboard-platform", "app.kubernetes.io/instance": "platform", "app.kubernetes.io/component": "model-gateway"}}})
	service.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "model-gateway"})
	service.SetAnnotations(map[string]string{"prow-ai-dashboard/model-gateway-host": "gateway.platform.example.com"})
	cluster.objects[objectKey(servicesGVR, "capz", "platform-model-gateway")] = service
	cluster.lists[listKey(servicesGVR, "capz", selector)] = objectList(service)
	cluster.lists[listKey(endpointSlicesGVR, "capz", "kubernetes.io/service-name=platform-model-gateway")] = readyEndpointList("platform-model-gateway")

	deployment := cluster.lists[listKey(deploymentsGVR, "gateway-ns", gatewaySelectorForTest())].Items[0].DeepCopy()
	deployment.SetName("platform-model-gateway")
	deployment.SetNamespace("capz")
	annotations := deployment.GetAnnotations()
	annotations["prow-ai-dashboard/model-gateway-host"] = "gateway.platform.example.com"
	annotations[modelGatewayTLSSecretAnnotation] = "gateway-tls"
	deployment.SetAnnotations(annotations)
	cluster.lists[listKey(deploymentsGVR, "capz", gatewaySelectorForTest())] = objectList(deployment)
	podLabels, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "template", "metadata", "labels")
	addGatewayPolicyFixtures(cluster, "capz", deployment, podLabels)
	cluster.secretNames["capz/provider"] = true
	cluster.secretNames["capz/gateway-tls"] = true

	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
	if report.HasFailures() {
		t.Fatalf("checks = %+v", report.Checks)
	}
	assertDoctorCheck(t, report, "model gateway private DNS", KubernetesDoctorUnverified)
}

func TestKubernetesDoctorRejectsInsecureProviderAndMutableExecutor(t *testing.T) {
	values := strings.Replace(baselineValues(true), "https://model-gateway.gateway-ns.svc.cluster.local/v1/chat/completions", "http://model-gateway.gateway-ns.svc.cluster.local/v1/chat/completions", 1)
	values = strings.Replace(values, "digest: sha256:"+strings.Repeat("a", 64), "digest: latest", 1)
	dir := writeDoctorBundle(t, values)
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), baselineCluster(true))
	assertDoctorCheck(t, report, "Agent Sandbox provider endpoint", KubernetesDoctorFail)
	assertDoctorCheck(t, report, "Fix executor image", KubernetesDoctorFail)
}

func TestKubernetesDoctorPublicOriginAndOAuthMismatch(t *testing.T) {
	values := strings.Replace(baselineValues(false), `server:
  security:
    hsts:
      enabled: true
  service:
    type: ClusterIP
`, `server:
  security:
    hsts:
      enabled: true
  actions:
    enabled: true
    mode: oauth
    oauth:
      redirectUrl: https://wrong.example/api/auth/callback
      existingSecret: oauth
  service:
    type: LoadBalancer
ingress:
  enabled: true
`, 1)
	dir := writeDoctorBundle(t, values)
	cluster := baselineCluster(false)
	cluster.secretNames["capz/oauth"] = true
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
	assertDoctorCheck(t, report, "OAuth callback", KubernetesDoctorFail)
	assertDoctorCheck(t, report, "public topology", KubernetesDoctorFail)
	assertDoctorCheck(t, report, "direct origin exposure", KubernetesDoctorFail)
}

func TestKubernetesDoctorSecretMetadataOnly(t *testing.T) {
	values := strings.Replace(baselineValues(false), "accessMode: ReadWriteMany", "accessMode: ReadWriteMany\nai:\n  existingSecret: ai-model", 1)
	dir := writeDoctorBundle(t, values)
	cluster := baselineCluster(false)
	cluster.secretNames["capz/ai-model"] = true
	sentinel := "DO_NOT_EXPOSE_SECRET_VALUE"
	runner := baselineDoctorRunner()
	runner.manifest = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: rendered-secret\ndata:\n  image: " + sentinel + "\nstringData:\n  image: " + sentinel + "\n---\n" + runner.manifest
	report := runDoctorForTest(t, dir, "install", runner, cluster)
	assertDoctorCheck(t, report, "application AI Secret", KubernetesDoctorPass)
	var output bytes.Buffer
	if err := WriteKubernetesDoctorReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), sentinel) || strings.Contains(output.String(), "stringData") {
		t.Fatalf("Secret content leaked: %s", output.String())
	}
	foundMetadata := false
	for _, call := range cluster.calls {
		if call == "metadata secrets capz/ai-model" {
			foundMetadata = true
		}
		if strings.HasPrefix(call, "get /v1, Resource=secrets") {
			t.Fatalf("full Secret GET observed: %s", call)
		}
	}
	if !foundMetadata {
		t.Fatalf("metadata-only Secret lookup not observed: %v", cluster.calls)
	}
}

func TestKubernetesDoctorModeChangingUpgradeIsWarning(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(false))
	cluster := baselineCluster(false)
	setHelmRelease(cluster, "capz", "capz", 2, "deployed")
	cron := object(cronJobsGVR, "capz", "capz-fetcher", nil)
	cron.SetLabels(map[string]string{"app.kubernetes.io/instance": "capz", "app.kubernetes.io/component": "fetcher"})
	cluster.lists[listKey(cronJobsGVR, "capz", "app.kubernetes.io/instance=capz")] = objectList(cron)
	cluster.objects[objectKey(serviceAccountsGVR, "capz", "app")] = object(serviceAccountsGVR, "capz", "app", nil)
	report := runDoctorForTest(t, dir, "upgrade", baselineDoctorRunner(), cluster)
	assertDoctorCheck(t, report, "writer mode", KubernetesDoctorWarn)
	for _, check := range report.Checks {
		if check.Name == "writer mode" && check.Status == KubernetesDoctorFail {
			t.Fatalf("mode transition was blocked: %+v", report.Checks)
		}
	}
}

func TestKubernetesDoctorRuntimeClassTaints(t *testing.T) {
	t.Run("untolerated", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		node := nodeObject("node-a", true, map[string]string{"runtime": "kata"})
		_ = unstructured.SetNestedSlice(node.Object, []any{map[string]any{"key": "dedicated", "value": "kata", "effect": "NoSchedule"}}, "spec", "taints")
		cluster.lists[listKey(nodesGVR, "", "")] = objectList(node)
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "secure runtime nodes", KubernetesDoctorFail)
	})
	t.Run("tolerated", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		node := nodeObject("node-a", true, map[string]string{"runtime": "kata"})
		_ = unstructured.SetNestedSlice(node.Object, []any{map[string]any{"key": "dedicated", "value": "kata", "effect": "NoSchedule"}}, "spec", "taints")
		cluster.lists[listKey(nodesGVR, "", "")] = objectList(node)
		runtimeClass := cluster.objects[objectKey(runtimeClassesGVR, "", "kata-vm-isolation")]
		_ = unstructured.SetNestedSlice(runtimeClass.Object, []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "kata", "effect": "NoSchedule"}}, "scheduling", "tolerations")
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "secure runtime nodes", KubernetesDoctorPass)
	})
}

func TestKubernetesDoctorActiveSandboxUsesReleaseDedicatedNamespace(t *testing.T) {
	sandbox := object(sandboxesGVR, "sandbox", "active-fix", nil)
	t.Run("missing release label fails", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		cluster.objects[objectKey(namespacesGVR, "", "sandbox")].SetLabels(nil)
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "Agent Sandbox execution namespace", KubernetesDoctorFail)
		assertDoctorCheck(t, report, "active Sandboxes", KubernetesDoctorFail)
	})
	t.Run("unlabeled Sandbox fails", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		cluster.lists[listKey(sandboxesGVR, "sandbox", "")] = objectList(sandbox)
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "active Sandboxes", KubernetesDoctorFail)
	})
	t.Run("terminating Sandbox fails", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(true))
		cluster := baselineCluster(true)
		terminating := sandbox.DeepCopy()
		now := metav1.Now()
		terminating.SetDeletionTimestamp(&now)
		cluster.lists[listKey(sandboxesGVR, "sandbox", "")] = objectList(terminating)
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "active Sandboxes", KubernetesDoctorFail)
	})
}

func TestKubernetesDoctorForbiddenReadsFailClosed(t *testing.T) {
	t.Run("platform workload ServiceAccount", func(t *testing.T) {
		values := strings.Replace(baselineValues(true), "create: false\n      name: fix-workload", "create: true\n      name: fix-workload", 1)
		dir := writeDoctorBundle(t, values)
		cluster := baselineCluster(true)
		key := objectKey(serviceAccountsGVR, "sandbox", "fix-workload")
		cluster.getErrors[key] = apierrors.NewForbidden(serviceAccountsGVR.GroupResource(), "fix-workload", fmt.Errorf("denied"))
		report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "Agent Sandbox workload ServiceAccount", KubernetesDoctorFail)
	})
	t.Run("application ServiceAccount", func(t *testing.T) {
		dir := writeDoctorBundle(t, baselineValues(false))
		cluster := baselineCluster(false)
		setHelmRelease(cluster, "capz", "capz", 1, "deployed")
		key := objectKey(serviceAccountsGVR, "capz", "app")
		cluster.getErrors[key] = apierrors.NewForbidden(serviceAccountsGVR.GroupResource(), "app", fmt.Errorf("denied"))
		report := runDoctorForTest(t, dir, "upgrade", baselineDoctorRunner(), cluster)
		assertDoctorCheck(t, report, "application ServiceAccounts", KubernetesDoctorFail)
	})
}

func TestKubernetesDoctorGatewayProviderSecretDoesNotSatisfyTLS(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(true))
	cluster := baselineCluster(true)
	deployments := cluster.lists[listKey(deploymentsGVR, "gateway-ns", gatewaySelectorForTest())]
	_ = unstructured.SetNestedSlice(deployments.Items[0].Object, nil, "spec", "template", "spec", "volumes")
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
	assertDoctorCheck(t, report, "model gateway TLS", KubernetesDoctorFail)
}

func TestKubernetesDoctorOriginReadsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		gvr      schema.GroupVersionResource
		selector string
		check    string
	}{
		{name: "Service", gvr: servicesGVR, selector: "app.kubernetes.io/instance=capz,app.kubernetes.io/component=server", check: "live Service topology"},
		{name: "Ingress", gvr: ingressesGVR, selector: "app.kubernetes.io/instance=capz", check: "live Ingress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := writeDoctorBundle(t, baselineValues(false))
			cluster := baselineCluster(false)
			setHelmRelease(cluster, "capz", "capz", 1, "deployed")
			cluster.listErrors[listKey(test.gvr, "capz", test.selector)] = apierrors.NewForbidden(test.gvr.GroupResource(), "", fmt.Errorf("denied"))
			report := runDoctorForTest(t, dir, "upgrade", baselineDoctorRunner(), cluster)
			assertDoctorCheck(t, report, test.check, KubernetesDoctorFail)
		})
	}
}

func TestKubernetesDoctorTLSMountMustBeOnGatewayContainer(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(true))
	cluster := baselineCluster(true)
	deployment := &cluster.lists[listKey(deploymentsGVR, "gateway-ns", gatewaySelectorForTest())].Items[0]
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	gateway := containers[0].(map[string]any)
	delete(gateway, "volumeMounts")
	containers = append(containers, map[string]any{
		"name": "sidecar", "image": "registry.example/sidecar@sha256:" + strings.Repeat("c", 64),
		"volumeMounts": []any{map[string]any{"name": "certificates", "mountPath": "/etc/certs", "readOnly": true}},
	})
	_ = unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers")
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
	assertDoctorCheck(t, report, "model gateway TLS", KubernetesDoctorFail)
}

func TestKubernetesDoctorGatewayPolicyFailures(t *testing.T) {
	tests := []struct {
		name   string
		check  string
		mutate func(*fakeDoctorCluster)
	}{
		{name: "missing default deny", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(networkPoliciesGVR, "gateway-ns", "")] = objectList()
		}},
		{name: "broadened default deny", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(networkPoliciesGVR, "gateway-ns", "")].Items[0]
			_ = unstructured.SetNestedSlice(policy.Object, []any{map[string]any{}}, "spec", "egress")
		}},
		{name: "additional Kubernetes policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(networkPoliciesGVR, "gateway-ns", "manual", map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Egress"}, "egress": []any{map[string]any{}}}})
			c.lists[listKey(networkPoliciesGVR, "gateway-ns", "")].Items = append(c.lists[listKey(networkPoliciesGVR, "gateway-ns", "")].Items, *extra)
		}},
		{name: "missing Cilium policy", mutate: func(c *fakeDoctorCluster) {
			c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")] = objectList()
		}},
		{name: "additional world Cilium policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(ciliumPoliciesGVR, "gateway-ns", "world", map[string]any{"spec": map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{"toEntities": []any{"world"}}}}})
			c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items = append(c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items, *extra)
		}},
		{name: "wrong upstream host", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items[0]
			egress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "egress")
			fqdn := egress[1].(map[string]any)["toFQDNs"].([]any)[0].(map[string]any)
			fqdn["matchName"] = "attacker.example"
			_ = unstructured.SetNestedSlice(policy.Object, egress, "spec", "egress")
		}},
		{name: "ingress world source", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items[0]
			ingress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress")
			ingress[0].(map[string]any)["fromEntities"] = []any{"world"}
			_ = unstructured.SetNestedSlice(policy.Object, ingress, "spec", "ingress")
		}},
		{name: "ingress CIDR source", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items[0]
			ingress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress")
			ingress[0].(map[string]any)["fromCIDR"] = []any{"0.0.0.0/0"}
			_ = unstructured.SetNestedSlice(policy.Object, ingress, "spec", "ingress")
		}},
		{name: "ingress CIDR set source", mutate: func(c *fakeDoctorCluster) {
			policy := &c.lists[listKey(ciliumPoliciesGVR, "gateway-ns", "")].Items[0]
			ingress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress")
			ingress[0].(map[string]any)["fromCIDRSet"] = []any{map[string]any{"cidr": "0.0.0.0/0"}}
			_ = unstructured.SetNestedSlice(policy.Object, ingress, "spec", "ingress")
		}},
		{name: "coordinated gateway Deployment drift", check: "model gateway Deployment", mutate: func(c *fakeDoctorCluster) {
			deployment := &c.lists[listKey(deploymentsGVR, "gateway-ns", gatewaySelectorForTest())].Items[0]
			annotations := deployment.GetAnnotations()
			annotations["prow-ai-dashboard/model-gateway-upstream-host"] = "attacker.example"
			annotations["prow-ai-dashboard/model-gateway-policy-sha256"] = gatewayPolicyHash("sandbox", "attacker.example", "8443")
			deployment.SetAnnotations(annotations)
			containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
			env := containers[0].(map[string]any)["env"].([]any)
			env[1].(map[string]any)["value"] = "https://attacker.example/v1/chat/completions"
			_ = unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers")
		}},
		{name: "cluster-wide gateway policy", mutate: func(c *fakeDoctorCluster) {
			extra := object(ciliumClusterwidePoliciesGVR, "", "gateway-world", map[string]any{"spec": map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{"toEntities": []any{"world"}}}}})
			c.lists[listKey(ciliumClusterwidePoliciesGVR, "", "")] = objectList(extra)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeDoctorBundle(t, baselineValues(true))
			cluster := baselineCluster(true)
			test.mutate(cluster)
			report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
			check := test.check
			if check == "" {
				check = "model gateway network policy"
			}
			assertDoctorCheck(t, report, check, KubernetesDoctorFail)
		})
	}
}

func TestKubernetesDoctorMissingReferencedTLSSecretDoesNotPass(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(true))
	cluster := baselineCluster(true)
	delete(cluster.secretNames, "gateway-ns/gateway-tls")
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), cluster)
	assertDoctorCheck(t, report, "model gateway TLS", KubernetesDoctorFail)
	for _, check := range report.Checks {
		if check.Name == "model gateway TLS" && check.Status == KubernetesDoctorPass {
			t.Fatalf("missing TLS Secret also passed: %+v", report.Checks)
		}
	}
}

func TestKubernetesDoctorExistingClaimLeavesRWXUnverified(t *testing.T) {
	dir := writeDoctorBundle(t, baselineValues(true))
	report := runDoctorForTest(t, dir, "install", baselineDoctorRunner(), baselineCluster(true))
	assertDoctorCheck(t, report, "RWX semantics", KubernetesDoctorUnverified)
}

func TestWriteKubernetesDoctorReportBoundsControlCharacters(t *testing.T) {
	report := KubernetesDoctorReport{Checks: []KubernetesDoctorCheck{{Name: "bad\nname", Status: KubernetesDoctorFail, Detail: strings.Repeat("x", 6000) + "\x00token"}}}
	var output bytes.Buffer
	if err := WriteKubernetesDoctorReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x00") || len(output.String()) > 5000 || !strings.Contains(output.String(), "kubernetes_doctor=fail") {
		t.Fatalf("unbounded or unsafe output: %q", output.String())
	}
}

func TestWriteKubernetesDoctorReportBoundsTotalOutput(t *testing.T) {
	report := KubernetesDoctorReport{}
	for i := 0; i < 2000; i++ {
		report.Checks = append(report.Checks, KubernetesDoctorCheck{Name: fmt.Sprintf("check-%04d", i), Status: KubernetesDoctorWarn, Detail: strings.Repeat("x", 1024)})
	}
	var output bytes.Buffer
	if err := WriteKubernetesDoctorReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if output.Len() > maxKubernetesDoctorOutputBytes || !strings.Contains(output.String(), "additional checks omitted") || !strings.Contains(output.String(), "kubernetes_doctor=pass") {
		t.Fatalf("output length=%d tail=%q", output.Len(), output.String()[max(0, output.Len()-500):])
	}
}

func runDoctorForTest(t *testing.T, dir, action string, runner *doctorRunner, cluster *fakeDoctorCluster) KubernetesDoctorReport {
	t.Helper()
	return runKubernetesDoctor(context.Background(), KubernetesDoctorOptions{
		Action: action, ProjectDir: dir, ValuesFile: filepath.Join("deploy", "values.yaml"),
		Release: "capz", Namespace: "capz", KubeContext: "test", Chart: "chart",
	}, kubernetesDoctorDependencies{
		runner:         runner,
		clusterFactory: func(string) (clusterReader, error) { return cluster, nil },
		consumerDoctor: passingConsumerDoctor,
		readFile:       os.ReadFile,
	})
}

func passingConsumerDoctor(context.Context, onboard.DoctorOptions) onboard.DoctorReport {
	return onboard.DoctorReport{Checks: []onboard.DoctorCheck{{Name: "bundle", Status: onboard.DoctorPass, Detail: "valid"}}}
}

func setHelmRelease(cluster *fakeDoctorCluster, namespace, release string, revision int, status string) {
	item := metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "sh.helm.release.v1." + release + ".v" + fmt.Sprint(revision), Namespace: namespace,
		Labels: map[string]string{"owner": "helm", "name": release, "version": fmt.Sprint(revision), "status": status},
	}}
	cluster.secretLists[namespace+"|owner=helm,name="+release] = &metav1.PartialObjectMetadataList{Items: []metav1.PartialObjectMetadata{item}}
}

func baselineDoctorRunner() *doctorRunner {
	return &doctorRunner{manifest: "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: app\n  namespace: capz\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: rendered\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: registry.example/dashboard:sha-abcdef1234567\n"}
}

func baselineCluster(agentSandbox bool) *fakeDoctorCluster {
	cluster := &fakeDoctorCluster{
		version:     "v1.34.1",
		resources:   map[schema.GroupVersionResource]bool{},
		objects:     map[string]*unstructured.Unstructured{},
		lists:       map[string]*unstructured.UnstructuredList{},
		getErrors:   map[string]error{},
		listErrors:  map[string]error{},
		secretNames: map[string]bool{},
		secretLists: map[string]*metav1.PartialObjectMetadataList{},
	}
	for _, gvr := range []schema.GroupVersionResource{deploymentsGVR, cronJobsGVR, endpointSlicesGVR, networkPoliciesGVR, storageClassesGVR, runtimeClassesGVR, sandboxesGVR, ciliumPoliciesGVR, ciliumClusterwidePoliciesGVR} {
		cluster.resources[gvr] = true
	}
	cluster.objects[objectKey(namespacesGVR, "", "capz")] = object(namespacesGVR, "", "capz", nil)
	cluster.objects[objectKey(storageClassesGVR, "", "rwx")] = object(storageClassesGVR, "", "rwx", nil)
	cluster.lists[listKey(deploymentsGVR, "capz", "app.kubernetes.io/instance=capz")] = objectList()
	cluster.lists[listKey(cronJobsGVR, "capz", "app.kubernetes.io/instance=capz")] = objectList()
	cluster.lists[listKey(podsGVR, "capz", "")] = objectList()
	cluster.lists[listKey(ingressesGVR, "capz", "app.kubernetes.io/instance=capz")] = objectList()
	if !agentSandbox {
		return cluster
	}
	cluster.objects[objectKey(persistentClaimsGVR, "capz", "dashboard-data")] = object(persistentClaimsGVR, "capz", "dashboard-data", map[string]any{
		"spec": map[string]any{"accessModes": []any{"ReadWriteMany"}}, "status": map[string]any{"phase": "Bound"},
	})
	cluster.objects[objectKey(customResourcesGVR, "", "sandboxes.agents.x-k8s.io")] = object(customResourcesGVR, "", "sandboxes.agents.x-k8s.io", map[string]any{
		"spec": map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": true, "storage": true}}},
	})
	cluster.objects[objectKey(deploymentsGVR, agentSandboxSystemNamespace, agentSandboxControllerName)] = object(deploymentsGVR, agentSandboxSystemNamespace, agentSandboxControllerName, map[string]any{
		"spec":   map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": agentSandboxControllerName, "image": supportedAgentSandboxControllerImage}}}}},
		"status": map[string]any{"availableReplicas": int64(1)},
	})
	cluster.objects[objectKey(serviceAccountsGVR, agentSandboxSystemNamespace, agentSandboxControllerName)] = object(serviceAccountsGVR, agentSandboxSystemNamespace, agentSandboxControllerName, nil)
	for _, service := range []string{agentSandboxControllerName, agentSandboxWebhookServiceName} {
		cluster.objects[objectKey(servicesGVR, agentSandboxSystemNamespace, service)] = object(servicesGVR, agentSandboxSystemNamespace, service, nil)
		cluster.lists[listKey(endpointSlicesGVR, agentSandboxSystemNamespace, "kubernetes.io/service-name="+service)] = readyEndpointList(service)
	}
	controllerPod := object(podsGVR, agentSandboxSystemNamespace, "controller", map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app": "agent-sandbox-controller"}},
		"status":   map[string]any{"containerStatuses": []any{map[string]any{"name": agentSandboxControllerName, "imageID": "registry@" + supportedAgentSandboxAMD64Digest}}},
	})
	cluster.lists[listKey(podsGVR, agentSandboxSystemNamespace, "app=agent-sandbox-controller")] = objectList(controllerPod)
	cluster.objects[objectKey(namespacesGVR, "", "sandbox")] = object(namespacesGVR, "", "sandbox", nil)
	cluster.objects[objectKey(namespacesGVR, "", "sandbox")].SetLabels(map[string]string{"prow-ai-dashboard/release": "capz"})
	allowedFQDNs := []string{"github.com", "api.github.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
	platformAnnotations := map[string]string{
		"prow-ai-dashboard/runtime-class":             "kata-vm-isolation",
		"prow-ai-dashboard/agent-sandbox-version":     supportedAgentSandboxVersion,
		"prow-ai-dashboard/network-policy-mode":       "cilium",
		"prow-ai-dashboard/default-deny-policy-name":  "platform-prow-ai-dashboard-platform-execution-default-deny",
		"prow-ai-dashboard/execution-policy-name":     "platform-prow-ai-dashboard-platform-execution-egress",
		"prow-ai-dashboard/platform-release":          "platform",
		"prow-ai-dashboard/model-gateway-enabled":     "true",
		"prow-ai-dashboard/model-gateway-namespace":   "gateway-ns",
		"prow-ai-dashboard/model-gateway-name":        "prow-ai-dashboard-platform",
		"prow-ai-dashboard/model-gateway-target-port": "8443",
	}
	policyHash := executionPolicyHash(allowedFQDNs, platformAnnotations)
	platformAnnotations["prow-ai-dashboard/execution-policy-sha256"] = policyHash
	cluster.objects[objectKey(namespacesGVR, "", "sandbox")].SetAnnotations(platformAnnotations)
	gatewayHash := gatewayPolicyHash("sandbox", "provider.example", "8443")
	binding := object(configMapsGVR, "capz", "platform-prow-ai-dashboard-platform-binding", map[string]any{"data": map[string]any{
		"applicationReleaseName": "capz", "executionNamespace": "sandbox", "runtimeClassName": "kata-vm-isolation", "executionPolicySHA256": policyHash,
		"modelGatewayEnabled": "true", "modelGatewayPublicHost": "", "modelGatewayUpstreamHost": "provider.example", "modelGatewayExecutionNamespace": "sandbox", "modelGatewayTargetPort": "8443", "modelGatewayPolicySHA256": gatewayHash,
	}})
	binding.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "platform-binding", "app.kubernetes.io/instance": "platform"})
	cluster.lists[listKey(configMapsGVR, "capz", "app.kubernetes.io/part-of=prow-ai-dashboard-platform,app.kubernetes.io/component=platform-binding")] = objectList(binding)
	fqdnEntries := make([]any, 0, len(allowedFQDNs))
	for _, name := range allowedFQDNs {
		fqdnEntries = append(fqdnEntries, map[string]any{"matchName": name})
	}
	ciliumPolicy := object(ciliumPoliciesGVR, "sandbox", "platform-prow-ai-dashboard-platform-execution-egress", map[string]any{"spec": map[string]any{
		"endpointSelector": map[string]any{},
		"ingress":          []any{},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}},
				"toPorts":     []any{map[string]any{"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}}, "rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}}}},
			},
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
					"k8s:io.kubernetes.pod.namespace": "gateway-ns",
					"k8s:app.kubernetes.io/name":      "prow-ai-dashboard-platform",
					"k8s:app.kubernetes.io/instance":  "platform",
					"k8s:app.kubernetes.io/component": "model-gateway",
				}}},
				"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "8443", "protocol": "TCP"}}}},
			},
			map[string]any{
				"toFQDNs": fqdnEntries,
				"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
			},
		},
	}})
	ciliumPolicy.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "agent-sandbox-execution", "app.kubernetes.io/instance": "platform"})
	ciliumPolicy.SetAnnotations(map[string]string{"prow-ai-dashboard/execution-policy-sha256": policyHash})
	cluster.objects[objectKey(ciliumPoliciesGVR, "sandbox", "platform-prow-ai-dashboard-platform-execution-egress")] = ciliumPolicy
	cluster.lists[listKey(ciliumPoliciesGVR, "sandbox", "")] = objectList(ciliumPolicy)
	cluster.lists[listKey(ciliumClusterwidePoliciesGVR, "", "")] = objectList()
	cluster.objects[objectKey(runtimeClassesGVR, "", "kata-vm-isolation")] = object(runtimeClassesGVR, "", "kata-vm-isolation", map[string]any{
		"handler": "kata", "scheduling": map[string]any{"nodeSelector": map[string]any{"runtime": "kata"}},
	})
	cluster.lists[listKey(nodesGVR, "", "")] = objectList(nodeObject("node-a", true, map[string]string{"runtime": "kata"}))
	quota := object(resourceQuotasGVR, "sandbox", "bounds", map[string]any{"spec": map[string]any{"hard": map[string]any{"pods": "4", "count/sandboxes.agents.x-k8s.io": "2"}}})
	cluster.lists[listKey(resourceQuotasGVR, "sandbox", "")] = objectList(quota)
	cluster.lists[listKey(limitRangesGVR, "sandbox", "")] = objectList(object(limitRangesGVR, "sandbox", "bounds", nil))
	policy := object(networkPoliciesGVR, "sandbox", "platform-prow-ai-dashboard-platform-execution-default-deny", map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}})
	policy.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "agent-sandbox-execution", "app.kubernetes.io/instance": "platform"})
	cluster.lists[listKey(networkPoliciesGVR, "sandbox", "")] = objectList(policy)
	cluster.objects[objectKey(serviceAccountsGVR, "sandbox", "fix-workload")] = object(serviceAccountsGVR, "sandbox", "fix-workload", nil)
	cluster.lists[listKey(sandboxesGVR, "sandbox", "")] = objectList()
	gatewayPodLabels := map[string]string{
		"app.kubernetes.io/name":      "prow-ai-dashboard-platform",
		"app.kubernetes.io/instance":  "platform",
		"app.kubernetes.io/component": "model-gateway",
		"app.kubernetes.io/part-of":   "prow-ai-dashboard-platform",
	}
	gatewaySelector := map[string]any{
		"app.kubernetes.io/name":      "prow-ai-dashboard-platform",
		"app.kubernetes.io/instance":  "platform",
		"app.kubernetes.io/component": "model-gateway",
	}
	gatewayService := object(servicesGVR, "gateway-ns", "model-gateway", map[string]any{"spec": map[string]any{"type": "ClusterIP", "selector": gatewaySelector}})
	cluster.objects[objectKey(servicesGVR, "gateway-ns", "model-gateway")] = gatewayService
	cluster.lists[listKey(endpointSlicesGVR, "gateway-ns", "kubernetes.io/service-name=model-gateway")] = readyEndpointList("model-gateway")
	gatewayAnnotations := map[string]string{
		modelGatewayTLSSecretAnnotation:                       "gateway-tls",
		"prow-ai-dashboard/model-gateway-default-deny-policy": "platform-model-gateway-default-deny",
		"prow-ai-dashboard/model-gateway-cilium-policy":       "platform-model-gateway",
		"prow-ai-dashboard/model-gateway-execution-namespace": "sandbox",
		"prow-ai-dashboard/model-gateway-upstream-host":       "provider.example",
		"prow-ai-dashboard/model-gateway-target-port":         "8443",
	}
	gatewayAnnotations["prow-ai-dashboard/model-gateway-policy-sha256"] = gatewayHash
	gatewayDeployment := object(deploymentsGVR, "gateway-ns", "model-gateway", map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": stringMapAny(gatewayPodLabels)}, "spec": map[string]any{
			"containers": []any{map[string]any{
				"name": "gateway", "image": "registry.example/gateway@sha256:" + strings.Repeat("b", 64),
				"env": []any{
					map[string]any{"name": "AI_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "provider", "key": "token"}}},
					map[string]any{"name": "UPSTREAM_URL", "value": "https://provider.example/v1/chat/completions"},
				},
				"volumeMounts": []any{map[string]any{"name": "certificates", "mountPath": "/etc/certs", "readOnly": true}},
			}},
			"volumes": []any{map[string]any{"name": "certificates", "secret": map[string]any{"secretName": "gateway-tls"}}},
		}}}, "status": map[string]any{"availableReplicas": int64(1)},
	})
	gatewayDeployment.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "model-gateway", "app.kubernetes.io/instance": "platform"})
	gatewayDeployment.SetAnnotations(gatewayAnnotations)
	cluster.lists[listKey(deploymentsGVR, "gateway-ns", gatewaySelectorForTest())] = objectList(gatewayDeployment)
	addGatewayPolicyFixtures(cluster, "gateway-ns", gatewayDeployment, gatewayPodLabels)
	cluster.secretNames["gateway-ns/provider"] = true
	cluster.secretNames["gateway-ns/gateway-tls"] = true
	return cluster
}

func gatewaySelectorForTest() string {
	return selectorString(map[string]string{
		"app.kubernetes.io/name":      "prow-ai-dashboard-platform",
		"app.kubernetes.io/instance":  "platform",
		"app.kubernetes.io/component": "model-gateway",
	})
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func addGatewayPolicyFixtures(cluster *fakeDoctorCluster, namespace string, deployment *unstructured.Unstructured, podLabels map[string]string) {
	annotations := deployment.GetAnnotations()
	defaultDeny := object(networkPoliciesGVR, namespace, annotations["prow-ai-dashboard/model-gateway-default-deny-policy"], map[string]any{"spec": map[string]any{
		"podSelector": map[string]any{"matchLabels": stringMapAny(podLabels)}, "policyTypes": []any{"Ingress", "Egress"},
	}})
	defaultDeny.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "model-gateway", "app.kubernetes.io/instance": "platform"})
	cluster.lists[listKey(networkPoliciesGVR, namespace, "")] = objectList(defaultDeny)
	selectorLabels := map[string]any{
		"app.kubernetes.io/name":      podLabels["app.kubernetes.io/name"],
		"app.kubernetes.io/instance":  podLabels["app.kubernetes.io/instance"],
		"app.kubernetes.io/component": podLabels["app.kubernetes.io/component"],
	}
	policy := object(ciliumPoliciesGVR, namespace, annotations["prow-ai-dashboard/model-gateway-cilium-policy"], map[string]any{"spec": map[string]any{
		"endpointSelector": map[string]any{"matchLabels": selectorLabels},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": annotations["prow-ai-dashboard/model-gateway-execution-namespace"]}}},
			"toPorts":       []any{map[string]any{"ports": []any{map[string]any{"port": annotations["prow-ai-dashboard/model-gateway-target-port"], "protocol": "TCP"}}}},
		}},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}},
				"toPorts":     []any{map[string]any{"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}}, "rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}}}},
			},
			map[string]any{
				"toFQDNs": []any{map[string]any{"matchName": annotations["prow-ai-dashboard/model-gateway-upstream-host"]}},
				"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
			},
		},
	}})
	policy.SetLabels(map[string]string{"app.kubernetes.io/part-of": "prow-ai-dashboard-platform", "app.kubernetes.io/component": "model-gateway", "app.kubernetes.io/instance": "platform"})
	policy.SetAnnotations(map[string]string{"prow-ai-dashboard/model-gateway-policy-sha256": annotations["prow-ai-dashboard/model-gateway-policy-sha256"]})
	existing := cluster.lists[listKey(ciliumPoliciesGVR, namespace, "")]
	if existing == nil {
		existing = objectList()
	}
	existing.Items = append(existing.Items, *policy)
	cluster.lists[listKey(ciliumPoliciesGVR, namespace, "")] = existing
}

func baselineValues(agentSandbox bool) string {
	storage := "storageClass: rwx\n  accessMode: ReadWriteMany"
	if agentSandbox {
		storage = "existingClaim: dashboard-data\n  accessMode: ReadWriteMany"
	}
	values := `global:
  imageTag: sha-abcdef1234567
image:
  repository: registry.example/dashboard
mode: watch
persistence:
  ` + storage + `
server:
  security:
    hsts:
      enabled: true
  service:
    type: ClusterIP
`
	if !agentSandbox {
		return values
	}
	return values + `agentSandbox:
  fixRuntime:
    enabled: true
    namespace: sandbox
    runtimeClassName: kata-vm-isolation
    image:
      repository: registry.example/fix
      digest: sha256:` + strings.Repeat("a", 64) + `
    dashboardImage:
      repository: registry.example/remote-fixer
      tag: sha-abcdef1234567
    workloadServiceAccount:
      create: false
      name: fix-workload
    modelProvider:
      credentialMode: gateway
      api: chat_completions
      endpoint: https://model-gateway.gateway-ns.svc.cluster.local/v1/chat/completions
      model: test-model
      auth:
        type: none
      publicCAPrivateDNS: false
`
}

func writeDoctorBundle(t *testing.T, values string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "project.yaml"), minimalProject)
	writeFile(t, filepath.Join(dir, "prompts", "system.md"), "prompt")
	writeFile(t, filepath.Join(dir, "deploy", "values.yaml"), values)
	return dir
}

func object(gvr schema.GroupVersionResource, namespace, name string, extra map[string]any) *unstructured.Unstructured {
	apiVersion := gvr.Version
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + gvr.Version
	}
	kind := strings.TrimSuffix(gvr.Resource, "s")
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	base := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
	for key, value := range extra {
		if key == "metadata" {
			for metadataKey, metadataValue := range value.(map[string]any) {
				metadata[metadataKey] = metadataValue
			}
			continue
		}
		base[key] = value
	}
	return &unstructured.Unstructured{Object: base}
}

func objectList(objects ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	for _, object := range objects {
		list.Items = append(list.Items, *object.DeepCopy())
	}
	return list
}

func nodeObject(name string, ready bool, labels map[string]string) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	node := object(nodesGVR, "", name, map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": status}}}})
	node.SetLabels(labels)
	return node
}

func readyEndpointList(service string) *unstructured.UnstructuredList {
	return objectList(object(endpointSlicesGVR, agentSandboxSystemNamespace, service+"-slice", map[string]any{"endpoints": []any{map[string]any{"conditions": map[string]any{"ready": true}}}}))
}

func objectKey(gvr schema.GroupVersionResource, namespace, name string) string {
	return gvr.String() + "|" + namespace + "|" + name
}

func listKey(gvr schema.GroupVersionResource, namespace, selector string) string {
	return gvr.String() + "|" + namespace + "|" + selector
}

func assertDoctorCheck(t *testing.T, report KubernetesDoctorReport, name string, status KubernetesDoctorStatus) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name && check.Status == status {
			return
		}
	}
	t.Fatalf("missing %s=%s in %+v", name, status, report.Checks)
}
