package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

func TestOrkaContainerAnalyzerKind(t *testing.T) {
	if os.Getenv("RUN_ORKA_CONTAINER_ANALYZER_KIND") == "" {
		t.Skip("set RUN_ORKA_CONTAINER_ANALYZER_KIND=1 with ORKA_CONTAINER_CONTEXT and ORKA_CONTAINER_IMAGE")
	}
	kubeContext := strings.TrimSpace(os.Getenv("ORKA_CONTAINER_CONTEXT"))
	image := strings.TrimSpace(os.Getenv("ORKA_CONTAINER_IMAGE"))
	if kubeContext == "" || image == "" {
		t.Fatal("ORKA_CONTAINER_CONTEXT and ORKA_CONTAINER_IMAGE are required")
	}
	const namespace = "orka-system"
	id := "container-analyzer-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	labels := map[string]string{"prow-ai-dashboard/smoke": id}
	modelName := "script-model-" + strings.TrimPrefix(id, "container-analyzer-")
	modelImage := strings.TrimSpace(os.Getenv("ORKA_CONTAINER_MODEL_IMAGE"))
	if modelImage == "" {
		modelImage = "python:3.12-alpine"
	}
	secretName := "analyzer-secret-" + strings.TrimPrefix(id, "container-analyzer-")
	cleanup := func() {
		containerKubectlIgnore(t, kubeContext, "delete", "task,job,pod,deployment,service,configmap,secret", "-n", namespace, "-l", "prow-ai-dashboard/smoke="+id, "--wait=true", "--timeout=2m")
	}
	cleanup()
	t.Cleanup(cleanup)

	applyContainerModelServer(t, kubeContext, namespace, modelName, modelImage, id)
	applyContainerSecret(t, kubeContext, namespace, secretName, id)
	containerKubectl(t, kubeContext, nil, "wait", "-n", namespace, "--for=condition=Available", "deployment/"+modelName, "--timeout=3m")

	bc := flatcarBenchCase(t)
	request := flatcarFailureRequest(bc)
	endpoint := "http://" + modelName + "." + namespace + ".svc.cluster.local/v1/chat/completions"

	t.Run("scripted-flatcar-result", func(t *testing.T) {
		task := buildKindContainerTask(t, namespace, image, id+"-flatcar", endpoint, "script-success", secretName, request, labels)
		name := applyContainerTask(t, kubeContext, task)
		status := waitContainerTask(t, kubeContext, namespace, name, 4*time.Minute)
		if status.Phase != "Succeeded" || status.Attempts != 1 || status.JobName == "" {
			t.Fatalf("Task status = %+v", status)
		}
		assertContainerJobPlacement(t, kubeContext, namespace, status.JobName)
		raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
		t.Logf("raw Task result:\n%s", raw)
		result, err := orka.ParseContainerAnalysisResult(raw)
		if err != nil {
			t.Fatalf("parse Task result: %v\nraw result:\n%s", err, raw)
		}
		tc := models.TestCase{Name: request.TestCase.Name, Status: "failed"}
		if err := orka.ApplyContainerAnalysisResult(&tc, result); err != nil {
			t.Fatal(err)
		}
		scoreBenchCase(t, bc, &tc, 0, "Orka container")
		if !strings.Contains(raw, "starting failure analysis") {
			t.Fatal("Task result did not demonstrate pinned-controller combined log capture")
		}
	})

	t.Run("retry-after-analyzer-failure", func(t *testing.T) {
		task := buildKindContainerTask(t, namespace, image, id+"-retry", endpoint, "script-retry", secretName, request, labels)
		name := applyContainerTask(t, kubeContext, task)
		status := waitContainerTask(t, kubeContext, namespace, name, 5*time.Minute)
		if status.Phase != "Succeeded" || status.Attempts < 2 {
			raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
			t.Fatalf("Task status = %+v, want succeeded after retry\nraw result:\n%s", status, raw)
		}
		assertContainerJobPlacement(t, kubeContext, namespace, status.JobName)
		raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
		if _, err := orka.ParseContainerAnalysisResult(raw); err != nil {
			t.Fatalf("parse retried Task result: %v\n%s", err, raw)
		}
	})
	assertNoPodsOnGPUNode(t, kubeContext, namespace)
	cleanup()
	waitForSmokeCleanup(t, kubeContext, namespace, id, 2*time.Minute)
}

func flatcarBenchCase(t *testing.T) benchCase {
	t.Helper()
	for _, bc := range benchCases {
		if bc.name == "flatcar-worker-dns-providerid" {
			return bc
		}
	}
	t.Fatal("Flatcar benchmark case is missing")
	return benchCase{}
}

func flatcarFailureRequest(bc benchCase) ai.FailureAnalysisRequest {
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo},
		JobName:     bc.jobName, BuildID: bc.buildID, PullNumber: bc.pullNumber,
	}
	return ai.FailureAnalysisRequest{
		JobID:       models.JobIDFor(bc.jobType, bc.repo, bc.jobName),
		BuildPrefix: loc.BuildPath(),
		Build: models.BuildInfo{
			BuildID: bc.buildID, JobName: bc.jobName, PullNumber: bc.pullNumber, WebURL: bc.webURL,
		},
		TestCase:            *benchTestCase(bc),
		ConsecutiveFailures: bc.consecutiveFailures,
	}
}

func buildKindContainerTask(t *testing.T, namespace, image, prefix, endpoint, model, secretName string, request ai.FailureAnalysisRequest, labels map[string]string) map[string]any {
	t.Helper()
	task, err := orka.BuildContainerAnalysisTask(orka.ContainerAnalysisTaskSpec{
		Namespace: namespace, NamePrefix: prefix, Image: image,
		Command: []string{"/app"}, Args: []string{"-project-dir=/project", "-data-dir=/tmp/analyzer"},
		Timeout: "2m", MaxRetries: 1, Request: request, Labels: labels,
		Environment: map[string]string{"AI_ENDPOINT": endpoint, "AI_MODEL": model},
		SecretEnv:   []orka.SecretEnvVar{{Name: "AI_TOKEN", SecretName: secretName, SecretKey: "token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func applyContainerTask(t *testing.T, kubeContext string, task map[string]any) string {
	t.Helper()
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	containerKubectl(t, kubeContext, data, "apply", "-f", "-")
	return task["metadata"].(map[string]any)["name"].(string)
}

type containerTaskStatus struct {
	Phase    string `json:"phase"`
	Attempts int    `json:"attempts"`
	JobName  string `json:"jobName"`
}

func waitContainerTask(t *testing.T, kubeContext, namespace, name string, timeout time.Duration) containerTaskStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status containerTaskStatus
	for time.Now().Before(deadline) {
		out := containerKubectl(t, kubeContext, nil, "get", "task", name, "-n", namespace, "-o", "jsonpath={.status}")
		if strings.TrimSpace(out) != "" {
			if err := json.Unmarshal([]byte(out), &status); err != nil {
				t.Fatalf("decode Task status %q: %v", out, err)
			}
			if status.Phase == "Succeeded" || status.Phase == "Failed" || status.Phase == "Cancelled" {
				return status
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Task %s did not finish in %s", name, timeout)
	return status
}

func assertContainerJobPlacement(t *testing.T, kubeContext, namespace, jobName string) {
	t.Helper()
	out := containerKubectl(t, kubeContext, nil, "get", "job", jobName, "-n", namespace, "-o", "json")
	var job struct {
		Spec struct {
			BackoffLimit *int `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					NodeSelector                 map[string]string `json:"nodeSelector"`
					AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("Job backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.NodeSelector["agentpool"] != "nodepool1" {
		t.Fatalf("Job nodeSelector = %+v", job.Spec.Template.Spec.NodeSelector)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("custom container automountServiceAccountToken = %v, want false", job.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	podNode := strings.TrimSpace(containerKubectl(t, kubeContext, nil, "get", "pod", "-n", namespace, "-l", "job-name="+jobName, "-o", "jsonpath={.items[0].spec.nodeName}"))
	if podNode == "" {
		t.Fatal("analyzer pod has no scheduled node")
	}
	nodePool := strings.TrimSpace(containerKubectl(t, kubeContext, nil, "get", "node", podNode, "-o", "jsonpath={.metadata.labels.agentpool}"))
	if nodePool != "nodepool1" {
		t.Fatalf("analyzer pod scheduled on node pool %q, want nodepool1", nodePool)
	}
}

func waitForSmokeCleanup(t *testing.T, kubeContext, namespace, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := containerKubectl(t, kubeContext, nil, "get", "task,job,pod,deployment,service,configmap,secret", "-n", namespace, "-l", "prow-ai-dashboard/smoke="+id, "-o", "name")
		if strings.TrimSpace(out) == "" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("smoke resources with label %s were not cleaned up", id)
}

func assertNoPodsOnGPUNode(t *testing.T, kubeContext, namespace string) {
	t.Helper()
	out := containerKubectl(t, kubeContext, nil, "get", "pods", "-n", namespace, "-o", "json")
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		pool := strings.TrimSpace(containerKubectl(t, kubeContext, nil, "get", "node", pod.Spec.NodeName, "-o", "jsonpath={.metadata.labels.agentpool}"))
		if pool == "h100" {
			t.Fatalf("pod %s scheduled on mock GPU node %s", pod.Metadata.Name, pod.Spec.NodeName)
		}
	}
}

func fetchContainerTaskResult(t *testing.T, kubeContext, namespace, taskName string) string {
	t.Helper()
	token := strings.TrimSpace(containerKubectl(t, kubeContext, nil, "create", "token", "orka-container-worker", "-n", namespace, "--duration=10m"))
	if token == "" {
		t.Fatal("empty Orka API token")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "-n", namespace, "port-forward", "svc/orka", fmt.Sprintf("%d:8080", port))
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		request, _ := http.NewRequest(http.MethodGet, base+"/healthz", nil)
		resp, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Orka port-forward did not become ready: %v\n%s", requestErr, logs.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
	client := orka.NewResultClient(base, token)
	resultCtx, resultCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer resultCancel()
	result, err := waitContainerTaskResult(resultCtx, client, namespace, taskName, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("fetch Task result: %v", err)
	}
	return result
}

type containerTaskResultReader interface {
	Result(context.Context, string, string) (string, bool, error)
}

func waitContainerTaskResult(ctx context.Context, reader containerTaskResultReader, namespace, taskName string, poll time.Duration) (string, error) {
	for {
		result, ok, err := reader.Result(ctx, namespace, taskName)
		if err != nil {
			return "", err
		}
		if ok {
			return result, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("wait for durable Task result: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

type delayedContainerResult struct {
	calls int
}

func (d *delayedContainerResult) Result(context.Context, string, string) (string, bool, error) {
	d.calls++
	if d.calls < 3 {
		return "", false, nil
	}
	return "result", true, nil
}

func TestWaitContainerTaskResultPollsUntilAvailable(t *testing.T) {
	reader := &delayedContainerResult{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := waitContainerTaskResult(ctx, reader, "orka-system", "task", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result != "result" || reader.calls != 3 {
		t.Fatalf("result = %q after %d calls", result, reader.calls)
	}
}

func applyContainerModelServer(t *testing.T, kubeContext, namespace, name, image, id string) {
	t.Helper()
	script := `import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Lock
counts = {}
lock = Lock()
def response(content=None, tool=None):
    message = {"role":"assistant","content":content}
    finish = "stop"
    if tool:
        finish = "tool_calls"
        message = {"role":"assistant","content":None,"tool_calls":[{"id":tool[0],"type":"function","function":{"name":tool[1],"arguments":json.dumps(tool[2])}}]}
    return {"choices":[{"finish_reason":finish,"message":message}]}
analysis = json.dumps({"summary":"The Flatcar worker Node registered but remained cloud-provider uninitialized without a providerID because cloud-node-manager could not reach the Kubernetes API Service ClusterIP 10.96.0.1.","is_transient":True,"root_cause":"The worker Node existed and became Ready, but it had no providerID and retained the cloud-provider uninitialized taint. cloud-node-manager crash-looped because the API Service ClusterIP 10.96.0.1 was unreachable; the preceding kube-proxy bootstrap failed to synchronize after DNS queries to the loopback resolver [::1]:53 were refused.","severity":"Transient-Ignore","suggested_fix":"Add a bootstrap readiness check that blocks kube-proxy and cloud-node-manager until the node loopback DNS resolver accepts queries, and preserve those logs when the check fails.","relevant_files":[]})
sequence = [response(tool=("c1","read_artifact",{"path":"build-log.txt"})), response(tool=("c2","tail_artifact",{"path":"build-log.txt"})), response(tool=("c3","read_artifact",{"path":"artifacts/junit.e2e_suite.1.xml"})), response(content=analysis), response(content=json.dumps({"objections":[]}))]
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(404); self.end_headers()
    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        model = body.get("model", "")
        with lock:
            count = counts.get(model, 0)
            counts[model] = count + 1
        if model == "script-retry" and count < 1:
            self.send_response(500); self.end_headers(); self.wfile.write(b"retry failure"); return
        offset = 1 if model == "script-retry" else 0
        index = count - offset
        if index < 0 or index >= len(sequence):
            self.send_response(500); self.end_headers(); self.wfile.write(b"script exhausted"); return
        data = json.dumps(sequence[index]).encode()
        self.send_response(200); self.send_header("content-type", "application/json"); self.send_header("content-length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self, format, *args):
        return
ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
`
	resources := []map[string]any{
		{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"prow-ai-dashboard/smoke": id}},
			"data":     map[string]any{"server.py": script},
		},
		{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"prow-ai-dashboard/smoke": id}},
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": name, "prow-ai-dashboard/smoke": id}},
					"spec": map[string]any{
						"nodeSelector": map[string]any{"agentpool": "nodepool1"},
						"containers": []any{map[string]any{
							"name": "model", "image": image, "command": []string{"python", "/script/server.py"},
							"ports":          []any{map[string]any{"containerPort": 8080}},
							"readinessProbe": containerModelReadinessProbe(),
							"volumeMounts":   []any{map[string]any{"name": "script", "mountPath": "/script"}},
						}},
						"volumes": []any{map[string]any{"name": "script", "configMap": map[string]any{"name": name}}},
					},
				},
			},
		},
		{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"prow-ai-dashboard/smoke": id}},
			"spec":     map[string]any{"selector": map[string]any{"app": name}, "ports": []any{map[string]any{"port": 80, "targetPort": 8080}}},
		},
	}
	for _, resource := range resources {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		containerKubectl(t, kubeContext, data, "apply", "-f", "-")
	}
}

func containerModelReadinessProbe() map[string]any {
	return map[string]any{
		"tcpSocket":           map[string]any{"port": 8080},
		"periodSeconds":       1,
		"failureThreshold":    30,
		"initialDelaySeconds": 0,
	}
}

func TestContainerModelReadinessProbeUsesTCPPort(t *testing.T) {
	probe := containerModelReadinessProbe()
	tcp := probe["tcpSocket"].(map[string]any)
	if tcp["port"] != 8080 || probe["periodSeconds"] != 1 || probe["failureThreshold"] != 30 {
		t.Fatalf("readiness probe = %+v", probe)
	}
}

func applyContainerSecret(t *testing.T, kubeContext, namespace, name, id string) {
	t.Helper()
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"prow-ai-dashboard/smoke": id}},
		"stringData": map[string]any{"token": "script-token"},
	}
	data, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	containerKubectl(t, kubeContext, data, "apply", "-f", "-")
}

func containerKubectl(t *testing.T, kubeContext string, stdin []byte, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"--context", kubeContext}, args...)
	cmd := exec.Command("kubectl", commandArgs...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func containerKubectlIgnore(t *testing.T, kubeContext string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"--context", kubeContext}, args...)
	_ = exec.Command("kubectl", commandArgs...).Run()
}
