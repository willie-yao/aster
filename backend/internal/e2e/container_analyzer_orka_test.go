package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"k8s.io/client-go/rest"
)

func containerAnalyzerStateKey() []byte { return bytes.Repeat([]byte{0x5a}, 32) }

func containerAnalyzerStateKeyText() string {
	return base64.StdEncoding.EncodeToString(containerAnalyzerStateKey())
}

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
	liveEndpoint := strings.TrimSpace(os.Getenv("ORKA_CONTAINER_LIVE_ENDPOINT"))
	liveModel := strings.TrimSpace(os.Getenv("ORKA_CONTAINER_LIVE_MODEL"))
	liveToken := os.Getenv("ORKA_CONTAINER_LIVE_TOKEN")
	secretToken := "script-token"
	if liveEndpoint != "" && liveToken != "" {
		secretToken = liveToken
	}
	cleanup := func() {
		containerKubectlIgnore(t, kubeContext, "delete", "task,job,pod,deployment,service,configmap,secret", "-n", namespace, "-l", "prow-ai-dashboard/smoke="+id, "--wait=true", "--timeout=2m")
	}
	cleanup()
	t.Cleanup(cleanup)

	applyContainerModelServer(t, kubeContext, namespace, modelName, modelImage, id)
	applyContainerSecret(t, kubeContext, namespace, secretName, id, secretToken)
	containerKubectl(t, kubeContext, nil, "wait", "-n", namespace, "--for=condition=Available", "deployment/"+modelName, "--timeout=3m")
	pruneContainerBundles(t, kubeContext, namespace)

	bc := flatcarBenchCase(t)
	request := flatcarFailureRequest(bc)
	endpoint := "http://" + modelName + "." + namespace + ".svc.cluster.local/v1/chat/completions"

	t.Run("scripted-flatcar-result", func(t *testing.T) {
		resources := buildKindContainerTask(t, namespace, image, id+"-flatcar", endpoint, "script-success", secretName, request, labels, nil, "2m", false)
		name := applyContainerResources(t, kubeContext, resources)
		status := waitContainerTask(t, kubeContext, namespace, name, 4*time.Minute)
		if status.Phase != "Succeeded" || status.Attempts != 1 || status.JobName == "" {
			t.Fatalf("Task status = %+v", status)
		}
		assertContainerJobPlacement(t, kubeContext, namespace, status.JobName, resources)
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
		if !strings.Contains(raw, analysisruntime.FailureAnalysisResultMarker) {
			t.Fatal("Task result did not contain the framed dashboard result")
		}
		state, err := analysisruntime.ParseEncryptedContainerAnalysisState(raw, containerAnalyzerStateKey(), containerStateIdentity(resources, request))
		if err != nil || len(state.Traces) != 1 || state.Traces[0].Backend != "orka" || state.Traces[0].TaskName != name {
			t.Fatalf("container state = %+v, error = %v", state, err)
		}
		cleanupContainerBundle(t, kubeContext, resources)
		assertTerminalReconcileDoesNotRecreateBundle(t, kubeContext, resources)
	})

	t.Run("retry-after-analyzer-failure", func(t *testing.T) {
		resources := buildKindContainerTask(t, namespace, image, id+"-retry", endpoint, "script-retry", secretName, request, labels, nil, "2m", false)
		name := applyContainerResources(t, kubeContext, resources)
		status := waitContainerTask(t, kubeContext, namespace, name, 5*time.Minute)
		if status.Phase != "Succeeded" || status.Attempts < 2 {
			raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
			t.Fatalf("Task status = %+v, want succeeded after retry\nraw result:\n%s", status, raw)
		}
		assertContainerJobPlacement(t, kubeContext, namespace, status.JobName, resources)
		raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
		if _, err := orka.ParseContainerAnalysisResult(raw); err != nil {
			t.Fatalf("parse retried Task result: %v\n%s", err, raw)
		}
		if _, err := analysisruntime.ParseEncryptedContainerAnalysisState(raw, containerAnalyzerStateKey(), containerStateIdentity(resources, request)); err != nil {
			t.Fatalf("parse retried Task state: %v", err)
		}
		cleanupContainerBundle(t, kubeContext, resources)
	})

	t.Run("persistent-cache-hit", func(t *testing.T) {
		store, err := analysisruntime.NewContainerStateStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		first := buildKindContainerTask(t, namespace, image, id+"-cache-a", endpoint, "script-cache", secretName, request, labels, store.CacheSeed(request), "2m", false)
		firstName := applyContainerResources(t, kubeContext, first)
		firstStatus := waitContainerTask(t, kubeContext, namespace, firstName, 4*time.Minute)
		if firstStatus.Phase != "Succeeded" {
			t.Fatalf("first cache Task status = %+v", firstStatus)
		}
		firstRaw := fetchContainerTaskResult(t, kubeContext, namespace, firstName)
		firstState, err := analysisruntime.ParseEncryptedContainerAnalysisState(firstRaw, containerAnalyzerStateKey(), containerStateIdentity(first, request))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Merge(firstState); err != nil {
			t.Fatal(err)
		}
		cleanupContainerBundle(t, kubeContext, first)
		seed := store.CacheSeed(request)
		if len(seed) != 1 {
			t.Fatalf("cache seed entries = %d, want 1", len(seed))
		}
		second := buildKindContainerTask(t, namespace, image, id+"-cache-b", endpoint, "script-cache", secretName, request, labels, seed, "2m", false)
		secondName := applyContainerResources(t, kubeContext, second)
		secondStatus := waitContainerTask(t, kubeContext, namespace, secondName, 4*time.Minute)
		if secondStatus.Phase != "Succeeded" {
			raw := fetchContainerTaskResult(t, kubeContext, namespace, secondName)
			t.Fatalf("cached Task status = %+v\n%s", secondStatus, raw)
		}
		secondRaw := fetchContainerTaskResult(t, kubeContext, namespace, secondName)
		result, err := orka.ParseContainerAnalysisResult(secondRaw)
		if err != nil || result.Analysis == nil || !result.Analysis.CacheHit {
			t.Fatalf("cached result = %+v, error = %v", result, err)
		}
		secondState, err := analysisruntime.ParseEncryptedContainerAnalysisState(secondRaw, containerAnalyzerStateKey(), containerStateIdentity(second, request))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Merge(secondState); err != nil {
			t.Fatal(err)
		}
		cleanupContainerBundle(t, kubeContext, second)
	})
	t.Run("five-task-load-wave", func(t *testing.T) {
		const taskCount = 5
		stateDir := t.TempDir()
		store, err := analysisruntime.NewContainerStateStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		resources := make([]orka.ContainerAnalysisResources, 0, taskCount)
		requests := make([]ai.FailureAnalysisRequest, 0, taskCount)
		names := make([]string, 0, taskCount)
		start := time.Now()
		for i := 0; i < taskCount; i++ {
			loadRequest := request
			loadRequest.TestCase = request.TestCase
			loadRequest.TestCase.Name = fmt.Sprintf("%s load-%d", request.TestCase.Name, i)
			loadRequest.TestCase.FailureMessage = fmt.Sprintf("%s load-%d", request.TestCase.FailureMessage, i)
			r := buildKindContainerTask(t, namespace, image, fmt.Sprintf("%s-load-%d", id, i), endpoint, fmt.Sprintf("script-load-%d", i), secretName, loadRequest, labels, nil, "2m", false)
			resources = append(resources, r)
			requests = append(requests, loadRequest)
			names = append(names, applyContainerResources(t, kubeContext, r))
		}
		applyElapsed := time.Since(start)
		states := make([]analysisruntime.ContainerAnalysisState, 0, taskCount)
		for i, name := range names {
			status := waitContainerTask(t, kubeContext, namespace, name, 5*time.Minute)
			if status.Phase != "Succeeded" || status.Attempts != 1 {
				t.Fatalf("load Task %s status = %+v", name, status)
			}
			assertContainerJobPlacement(t, kubeContext, namespace, status.JobName, resources[i])
			raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
			if _, err := orka.ParseContainerAnalysisResult(raw); err != nil {
				t.Fatalf("load Task %s result: %v", name, err)
			}
			state, err := analysisruntime.ParseEncryptedContainerAnalysisState(raw, containerAnalyzerStateKey(), containerStateIdentity(resources[i], requests[i]))
			if err != nil {
				t.Fatalf("load Task %s state: %v", name, err)
			}
			states = append(states, state)
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(states))
		for _, state := range states {
			state := state
			wg.Add(1)
			go func() { defer wg.Done(); errs <- store.Merge(state) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		reloaded, err := analysisruntime.NewContainerStateStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			if got := len(reloaded.CacheSeed(requests[i])); got != 1 {
				t.Fatalf("persisted cache entries for request %d = %d, want 1", i, got)
			}
		}
		traceStore, err := ai.LoadTraceStore(filepath.Join(stateDir, output.AITraceFilename))
		if err != nil {
			t.Fatal(err)
		}
		traceTasks := map[string]bool{}
		for _, trace := range traceStore.Snapshot().Traces {
			traceTasks[trace.TaskName] = true
		}
		for i, name := range names {
			if !traceTasks[name] {
				t.Fatalf("persisted trace for load Task %d %s is missing", i, name)
			}
		}
		for i := range resources {
			cleanupContainerBundle(t, kubeContext, resources[i])
		}
		t.Logf("load wave: tasks=%d apply=%s total=%s cache_entries=%d traces=%d", taskCount, applyElapsed.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), taskCount, len(traceTasks))
	})

	if liveEndpoint != "" || liveModel != "" {
		if liveEndpoint == "" || liveModel == "" {
			t.Fatal("ORKA_CONTAINER_LIVE_ENDPOINT and ORKA_CONTAINER_LIVE_MODEL must be set together")
		}
		t.Run("live-kimi-flatcar", func(t *testing.T) {
			resources := buildKindContainerTask(t, namespace, image, id+"-live-kimi", liveEndpoint, liveModel, secretName, request, labels, nil, "25m", true)
			start := time.Now()
			name := applyContainerResources(t, kubeContext, resources)
			status := waitContainerTask(t, kubeContext, namespace, name, 30*time.Minute)
			elapsed := time.Since(start).Round(time.Second)
			if status.Phase != "Succeeded" {
				raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
				t.Fatalf("live Kimi Task status = %+v\n%s", status, raw)
			}
			assertContainerJobPlacement(t, kubeContext, namespace, status.JobName, resources)
			raw := fetchContainerTaskResult(t, kubeContext, namespace, name)
			result, err := orka.ParseContainerAnalysisResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc := models.TestCase{Name: request.TestCase.Name, Status: "failed", AISummary: result.Summary, AIAnalysis: result.Analysis}
			scoreBenchCase(t, bc, &tc, elapsed, "Orka container Kimi")
			state, err := analysisruntime.ParseEncryptedContainerAnalysisState(raw, containerAnalyzerStateKey(), containerStateIdentity(resources, request))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("container state: cache_entries=%d traces=%d task_attempts=%d", len(state.CacheEntries), len(state.Traces), status.Attempts)
			cleanupContainerBundle(t, kubeContext, resources)
		})
	}

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

func containerStateIdentity(resources orka.ContainerAnalysisResources, request ai.FailureAnalysisRequest) analysisruntime.ContainerStateIdentity {
	name := resources.Task["metadata"].(map[string]any)["name"].(string)
	return analysisruntime.NewContainerStateIdentity("orka-system", name, request)
}

func buildKindContainerTask(t *testing.T, namespace, image, prefix, endpoint, model, secretName string, request ai.FailureAnalysisRequest, labels map[string]string, cacheSeed map[string]ai.CacheEntry, taskTimeout string, benchmarkProject bool) orka.ContainerAnalysisResources {
	t.Helper()
	resources, err := orka.BuildContainerAnalysisResources(orka.ContainerAnalysisTaskSpec{
		Namespace: namespace, NamePrefix: prefix, Image: image,
		Command: []string{"/app"}, Args: []string{"-data-dir=/tmp/analyzer"},
		Timeout: taskTimeout, MaxRetries: 1, ProjectDir: containerAnalyzerProject(t, benchmarkProject), Request: request, CacheSeed: cacheSeed, Labels: labels,
		Environment: map[string]string{"AI_API": "chat_completions", "AI_ENDPOINT": endpoint, "AI_MODEL": model},
		SecretEnv: []orka.SecretEnvVar{
			{Name: "AI_TOKEN", SecretName: secretName, SecretKey: "token"},
			{Name: analysisruntime.ContainerStateKeyEnv, SecretName: secretName, SecretKey: "state-key"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resources
}

func containerAnalyzerProject(t *testing.T, benchmark bool) string {
	t.Helper()
	dir := t.TempDir()
	config := `id: container-analyzer-spike
name: Orka Container Analyzer Spike
testgrid:
  dashboard: container-analyzer-spike
storage:
  provider: local
  bucket: kubernetes-ci-logs
  base: /fixtures
branding:
  title: Orka Container Analyzer Spike
  base_path: /container-analyzer-spike
  site_url: https://example.invalid/container-analyzer-spike
  source_repo:
    owner: kubernetes-sigs
    name: cluster-api-provider-azure
ai:
  tools: [filesystem]
  min_tool_calls: 2
`
	prompt := `You are debugging Kubernetes Cluster API Provider Azure E2E failures.
Use the build artifacts to distinguish transient bootstrap failures from persistent product defects.
`
	if benchmark {
		config = strings.Replace(config, "  tools: [filesystem]\n  min_tool_calls: 2", "  tools: [filesystem, k8s]\n  max_iters: 15\n  timeout: 20m\n  min_tool_calls: 5\n  min_gcs_bytes: 500000\n  critique:\n    max_retries: 2", 1)
		prompt = benchPromptAddendum
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pruneContainerBundles(t *testing.T, kubeContext, namespace string) {
	t.Helper()
	client := containerKubeClient(t, kubeContext)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := orka.PruneContainerAnalysisBundles(ctx, client, namespace, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func applyContainerResources(t *testing.T, kubeContext string, resources orka.ContainerAnalysisResources) string {
	t.Helper()
	client := containerKubeClient(t, kubeContext)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := orka.ReconcileContainerAnalysisResources(ctx, client, resources); err != nil {
		t.Fatal(err)
	}
	return resources.Task["metadata"].(map[string]any)["name"].(string)
}

func cleanupContainerBundle(t *testing.T, kubeContext string, resources orka.ContainerAnalysisResources) {
	t.Helper()
	client := containerKubeClient(t, kubeContext)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	taskName := resources.Task["metadata"].(map[string]any)["name"].(string)
	state, err := client.TaskState(ctx, "orka-system", taskName)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || !orka.TerminalPhase(state.Phase) || state.UID == "" {
		t.Fatalf("terminal Task identity = %+v", state)
	}
	if err := orka.CleanupContainerAnalysisBundle(ctx, client, resources, state.UID); err != nil {
		t.Fatal(err)
	}
	assertContainerBundleMissing(t, kubeContext, resources)
}

func assertTerminalReconcileDoesNotRecreateBundle(t *testing.T, kubeContext string, resources orka.ContainerAnalysisResources) {
	t.Helper()
	applyContainerResources(t, kubeContext, resources)
	assertContainerBundleMissing(t, kubeContext, resources)
}

func assertContainerBundleMissing(t *testing.T, kubeContext string, resources orka.ContainerAnalysisResources) {
	t.Helper()
	name := resources.BundleConfigMap["metadata"].(map[string]any)["name"].(string)
	cmd := exec.Command("kubectl", "--context", kubeContext, "get", "configmap", name, "-n", "orka-system", "-o", "name")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "NotFound") {
		t.Fatalf("bundle ConfigMap %s still exists or cleanup check failed: %v\n%s", name, err, out)
	}
}

func containerKubeClient(t *testing.T, kubeContext string) *orka.KubeClient {
	t.Helper()
	base, err := orka.RESTConfig(kubeContext)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(containerKubectl(t, kubeContext, nil, "create", "token", "orka-pipeline", "-n", "orka-system", "--duration=10m"))
	if token == "" {
		t.Fatal("empty Orka pipeline token")
	}
	config := rest.CopyConfig(base)
	config.BearerToken = token
	config.BearerTokenFile = ""
	config.Username = ""
	config.Password = ""
	config.ExecProvider = nil
	config.AuthProvider = nil
	config.Impersonate = rest.ImpersonationConfig{}
	config.TLSClientConfig.CertFile = ""
	config.TLSClientConfig.KeyFile = ""
	config.TLSClientConfig.CertData = nil
	config.TLSClientConfig.KeyData = nil
	client, err := orka.NewKubeClient(config)
	if err != nil {
		t.Fatal(err)
	}
	client.Manager = "container-analyzer-smoke"
	return client
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

func assertContainerJobPlacement(t *testing.T, kubeContext, namespace, jobName string, resources orka.ContainerAnalysisResources) {
	t.Helper()
	out := containerKubectl(t, kubeContext, nil, "get", "job", jobName, "-n", namespace, "-o", "json")
	var job struct {
		Spec struct {
			BackoffLimit *int `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					NodeSelector                 map[string]string `json:"nodeSelector"`
					AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken"`
					Containers                   []struct {
						Env []struct {
							Name      string `json:"name"`
							Value     string `json:"value"`
							ValueFrom struct {
								ConfigMapKeyRef *struct {
									Name string `json:"name"`
									Key  string `json:"key"`
								} `json:"configMapKeyRef"`
							} `json:"valueFrom"`
						} `json:"env"`
					} `json:"containers"`
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
	expectedBundle := resources.BundleConfigMap["metadata"].(map[string]any)["name"].(string)
	expectedDigest := resources.Task["metadata"].(map[string]any)["annotations"].(map[string]any)["prow-ai-dashboard/bundle-digest"].(string)
	bundleRefFound := false
	digestFound := false
	if len(job.Spec.Template.Spec.Containers) == 1 {
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			switch env.Name {
			case analysisruntime.ProjectBundleEnv:
				ref := env.ValueFrom.ConfigMapKeyRef
				bundleRefFound = ref != nil && ref.Name == expectedBundle && ref.Key == analysisruntime.ProjectBundleConfigMapKey
			case analysisruntime.ProjectBundleDigestEnv:
				digestFound = env.Value == expectedDigest
			}
		}
	}
	if !bundleRefFound || !digestFound {
		t.Fatalf("Job did not preserve the immutable bundle reference")
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

func applyContainerSecret(t *testing.T, kubeContext, namespace, name, id, token string) {
	t.Helper()
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"prow-ai-dashboard/smoke": id}},
		"stringData": map[string]any{"token": token, "state-key": containerAnalyzerStateKeyText()},
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
