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
	deployments := cluster.lists[listKey(deploymentsGVR, "gateway-ns", "app=gateway")]
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
	deployment := &cluster.lists[listKey(deploymentsGVR, "gateway-ns", "app=gateway")].Items[0]
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
	for _, gvr := range []schema.GroupVersionResource{deploymentsGVR, cronJobsGVR, endpointSlicesGVR, networkPoliciesGVR, storageClassesGVR, runtimeClassesGVR, sandboxesGVR} {
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
	cluster.objects[objectKey(runtimeClassesGVR, "", "kata-vm-isolation")] = object(runtimeClassesGVR, "", "kata-vm-isolation", map[string]any{
		"handler": "kata", "scheduling": map[string]any{"nodeSelector": map[string]any{"runtime": "kata"}},
	})
	cluster.lists[listKey(nodesGVR, "", "")] = objectList(nodeObject("node-a", true, map[string]string{"runtime": "kata"}))
	quota := object(resourceQuotasGVR, "sandbox", "bounds", map[string]any{"spec": map[string]any{"hard": map[string]any{"pods": "4", "count/sandboxes.agents.x-k8s.io": "2"}}})
	cluster.lists[listKey(resourceQuotasGVR, "sandbox", "")] = objectList(quota)
	cluster.lists[listKey(limitRangesGVR, "sandbox", "")] = objectList(object(limitRangesGVR, "sandbox", "bounds", nil))
	policy := object(networkPoliciesGVR, "sandbox", "default-deny", map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}})
	cluster.lists[listKey(networkPoliciesGVR, "sandbox", "")] = objectList(policy)
	cluster.objects[objectKey(serviceAccountsGVR, "sandbox", "fix-workload")] = object(serviceAccountsGVR, "sandbox", "fix-workload", nil)
	cluster.lists[listKey(sandboxesGVR, "sandbox", "")] = objectList()
	gatewayService := object(servicesGVR, "gateway-ns", "model-gateway", map[string]any{"spec": map[string]any{"type": "ClusterIP", "selector": map[string]any{"app": "gateway"}}})
	cluster.objects[objectKey(servicesGVR, "gateway-ns", "model-gateway")] = gatewayService
	cluster.lists[listKey(endpointSlicesGVR, "gateway-ns", "kubernetes.io/service-name=model-gateway")] = readyEndpointList("model-gateway")
	gatewayDeployment := object(deploymentsGVR, "gateway-ns", "model-gateway", map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{modelGatewayTLSSecretAnnotation: "gateway-tls"}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{
				"name": "gateway", "image": "registry.example/gateway@sha256:" + strings.Repeat("b", 64),
				"env":          []any{map[string]any{"name": "AI_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "provider", "key": "token"}}}},
				"volumeMounts": []any{map[string]any{"name": "certificates", "mountPath": "/etc/certs", "readOnly": true}},
			}},
			"volumes": []any{map[string]any{"name": "certificates", "secret": map[string]any{"secretName": "gateway-tls"}}},
		}}}, "status": map[string]any{"availableReplicas": int64(1)},
	})
	cluster.lists[listKey(deploymentsGVR, "gateway-ns", "app=gateway")] = objectList(gatewayDeployment)
	cluster.secretNames["gateway-ns/provider"] = true
	cluster.secretNames["gateway-ns/gateway-tls"] = true
	return cluster
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
