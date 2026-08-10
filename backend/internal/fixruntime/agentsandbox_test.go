package fixruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type fakeAgentSandboxAPI struct {
	mu                       sync.Mutex
	object                   map[string]any
	state                    sandboxState
	logs                     string
	createErr                error
	logsErr                  error
	deleteErr                error
	deleted                  bool
	keepStateIdentity        bool
	returnStateOnCreateError bool
	deleteUID                string
	executionPods            []string
}

func (f *fakeAgentSandboxAPI) Create(_ context.Context, _ string, object map[string]any) (sandboxState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.object = object
	desired := sandboxStateFromObject(&unstructured.Unstructured{Object: object})
	if !f.keepStateIdentity {
		f.state.ContractHash = desired.ContractHash
		f.state.ExecutionID = desired.ExecutionID
		f.state.ShapeHash = desired.ShapeHash
		f.state.ShutdownTime = desired.ShutdownTime
	}
	if f.createErr != nil {
		if f.returnStateOnCreateError {
			return f.state, f.createErr
		}
		return sandboxState{}, f.createErr
	}
	if f.state.UID == "" {
		f.state.Exists = true
		f.state.UID = "uid-1"
		f.state.PodName = object["metadata"].(map[string]any)["name"].(string)
	}
	return f.state, nil
}

func (f *fakeAgentSandboxAPI) State(_ context.Context, _, _ string) (sandboxState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted {
		return sandboxState{}, nil
	}
	return f.state, nil
}

func (f *fakeAgentSandboxAPI) Delete(_ context.Context, _, _, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteUID = uid
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = true
	return nil
}

func (f *fakeAgentSandboxAPI) PodLogs(_ context.Context, _, _ string, _ int64) (string, error) {
	return f.logs, f.logsErr
}

func (f *fakeAgentSandboxAPI) PodExists(_ context.Context, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.deleted, nil
}

func (f *fakeAgentSandboxAPI) ExecutionPods(_ context.Context, _, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted {
		return nil, nil
	}
	if f.executionPods != nil {
		return append([]string(nil), f.executionPods...), nil
	}
	name := f.state.PodName
	if name == "" {
		name = "orphan-pod"
	}
	return []string{name}, nil
}

func agentSandboxSpec() engineruntime.GenerateSpec {
	return engineruntime.GenerateSpec{
		Repo: engineruntime.RepoRef{
			Owner: "octocat", Name: "Hello-World",
			Ref: "0123456789abcdef0123456789abcdef01234567",
		},
		Instruction:     "create one deterministic file",
		MaxSteps:        2,
		MaxFiles:        1,
		ModelGateway:    engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.internal.example/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout:         time.Minute,
		ExpectedBaseSHA: "0123456789abcdef0123456789abcdef01234567",
		CommandPolicy: engineruntime.CommandPolicy{Commands: []engineruntime.ExecutionCommand{{
			Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30,
		}}},
		ExecutionID: "request-1",
	}
}

func agentSandboxResult(t *testing.T, state engineruntime.TerminalState, failure string) string {
	t.Helper()
	result := engineruntime.ExecutionResult{
		Version:        engineruntime.ExecutionContractVersion,
		BaseSHA:        "0123456789abcdef0123456789abcdef01234567",
		ChangedFiles:   []string{"agent-sandbox-spike.txt"},
		Files:          map[string]string{"agent-sandbox-spike.txt": "agent-sandbox-v0.5.3\n"},
		Diff:           "diff --git a/agent-sandbox-spike.txt b/agent-sandbox-spike.txt\n",
		CommandResults: []engineruntime.CommandResult{{Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 2}},
		StdoutSummary:  "validation passed",
		TerminalState:  state,
		DurationMs:     10,
		FailureReason:  failure,
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAgentSandboxPreflightAndSandboxWorkloadParity(t *testing.T) {
	request, err := executionRequest(agentSandboxSpec())
	if err != nil {
		t.Fatal(err)
	}
	requestJSON := mustJSON(t, request)
	production := newAgentSandboxRuntimeForTest(nil, testAgentSandboxOptions())
	localKind := newAgentSandboxRuntimeForKindTest(nil, testAgentSandboxOptions())

	contractHash := agentSandboxContractHash(requestJSON, localKind.opts)
	sandbox := localKind.sandboxObject("fix-parity-0123456789ab", requestJSON, contractHash[:], request, "parity")
	sandboxPod := sandbox["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
	preflight := localKind.preflightPodObject("fix-eval", "preflight", requestJSON, request)
	preflightPod := preflight["spec"].(map[string]any)
	if !reflect.DeepEqual(sandboxPod, preflightPod) {
		t.Fatalf("local preflight and Sandbox Pod specs differ:\nSandbox: %s\nPreflight: %s", mustJSON(t, sandboxPod), mustJSON(t, preflightPod))
	}
	assertAppArmorMode(t, sandboxPod, false)

	productionPod := production.workloadPodSpec(requestJSON, request)
	assertAppArmorMode(t, productionPod, true)
	withoutAppArmor := k8sruntime.DeepCopyJSONValue(productionPod).(map[string]any)
	delete(withoutAppArmor["securityContext"].(map[string]any), "appArmorProfile")
	container := withoutAppArmor["containers"].([]any)[0].(map[string]any)
	delete(container["securityContext"].(map[string]any), "appArmorProfile")
	if !reflect.DeepEqual(withoutAppArmor, sandboxPod) {
		t.Fatalf("AppArmor capability changed another workload field:\nProduction: %s\nLocal: %s", mustJSON(t, withoutAppArmor), mustJSON(t, sandboxPod))
	}
	productionHash := agentSandboxContractHash(requestJSON, production.opts)
	if productionHash == contractHash {
		t.Fatal("production and local-kind security capabilities share an identity hash")
	}
}

func TestAgentSandboxProductionConstructorPinsRuntimeDefaultAppArmor(t *testing.T) {
	opts := AgentSandboxOptions{
		Namespace: "fix-eval", Image: "registry.internal.example/fixer@sha256:" + strings.Repeat("a", 64),
		ServiceAccountName: "fix-workload", RuntimeClassName: "kata-vm-isolation",
		ModelGateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.fix-eval.svc.cluster.local/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout:      10 * time.Minute, OutputLimitBytes: 128 << 10,
	}
	runtime, err := NewAgentSandboxRuntime(&fakeAgentSandboxAPI{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	request, err := executionRequest(agentSandboxSpec())
	if err != nil {
		t.Fatal(err)
	}
	assertAppArmorMode(t, runtime.workloadPodSpec(mustJSON(t, request), request), true)
}

func TestAgentSandboxProductionRejectsAppArmorUnavailableCapability(t *testing.T) {
	opts := testAgentSandboxOptions()
	opts.testOnly = false
	opts.appArmorCapability = appArmorUnavailableForKindTest
	if _, err := NewAgentSandboxRuntime(&fakeAgentSandboxAPI{}, opts); err == nil || !strings.Contains(err.Error(), "AppArmor capability") {
		t.Fatalf("error = %v", err)
	}
}

func assertAppArmorMode(t *testing.T, podSpec map[string]any, required bool) {
	t.Helper()
	podSecurity := podSpec["securityContext"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	containerSecurity := container["securityContext"].(map[string]any)
	for name, security := range map[string]map[string]any{"pod": podSecurity, "container": containerSecurity} {
		profile, ok := security["appArmorProfile"]
		if ok != required {
			t.Fatalf("%s AppArmor present = %v, want %v", name, ok, required)
		}
		if required && profile.(map[string]any)["type"] != "RuntimeDefault" {
			t.Fatalf("%s AppArmor = %v", name, profile)
		}
	}
}

func TestAgentSandboxRunUsesPurposeBoundResultChannelWithoutWorkspace(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "critic-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{"review":"pass"}`,
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "critic", ExecutionID: "request-1", RequestEnv: "PROW_AI_CAUSAL_CRITIC_REQUEST_B64",
		Request: []byte(`{"version":1}`), Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != `{"review":"pass"}` || result.FinishedReason != "PodSucceeded" || !result.Telemetry.CleanupCompleted {
		t.Fatalf("result = %+v", result)
	}
	metadata := api.object["metadata"].(map[string]any)
	if name := metadata["name"].(string); !strings.HasPrefix(name, "critic-") {
		t.Fatalf("name = %q", name)
	}
	if labels := metadata["labels"].(map[string]any); labels["prow-ai-dashboard/purpose"] != "critic" || len(labels) != 4 {
		t.Fatalf("labels = %+v", labels)
	}
	podTemplate := api.object["spec"].(map[string]any)["podTemplate"].(map[string]any)
	if labels := podTemplate["metadata"].(map[string]any)["labels"].(map[string]any); labels["prow-ai-dashboard/purpose"] != "critic" || len(labels) != 2 {
		t.Fatalf("pod labels = %+v", labels)
	}
	pod := podTemplate["spec"].(map[string]any)
	if _, ok := pod["volumes"]; ok {
		t.Fatal("read-only critic workload received writable volumes")
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	if _, ok := container["volumeMounts"]; ok {
		t.Fatal("read-only critic workload received writable volume mounts")
	}
	env := container["env"].([]any)[0].(map[string]any)
	if env["name"] != "PROW_AI_CAUSAL_CRITIC_REQUEST_B64" {
		t.Fatalf("env = %+v", env)
	}
}

func TestAgentSandboxRuntimeDerivesStableExecutionIdentity(t *testing.T) {
	spec := agentSandboxSpec()
	spec.ExecutionID = ""
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  agentSandboxResult(t, engineruntime.TerminalSucceeded, ""),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	runtime.applyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		return map[string]string{"agent-sandbox-spike.txt": "agent-sandbox-v0.5.3\n"}, "diff --git a/agent-sandbox-spike.txt b/agent-sandbox-spike.txt\n", nil
	}
	if _, err := runtime.Generate(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	annotations := api.object["metadata"].(map[string]any)["annotations"].(map[string]any)
	executionID := annotations[agentSandboxIDAnnotation].(string)
	if !strings.HasPrefix(executionID, "contract-") || len(executionID) != len("contract-")+16 {
		t.Fatalf("execution ID = %q", executionID)
	}
}

func TestAgentSandboxBenchmarkConfigUsesExplicitKubeContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := []byte(`apiVersion: v1
kind: Config
clusters:
- name: benchmark-cluster
  cluster:
    server: https://benchmark.example.test
- name: other-cluster
  cluster:
    server: https://other.example.test
contexts:
- name: benchmark
  context:
    cluster: benchmark-cluster
    user: benchmark
- name: other
  context:
    cluster: other-cluster
    user: other
current-context: other
users:
- name: benchmark
  user:
    token: benchmark
- name: other
  user:
    token: other
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	cfg, err := agentSandboxKubeconfigContextConfig("benchmark")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://benchmark.example.test" {
		t.Fatalf("host=%q", cfg.Host)
	}
}

func TestAgentSandboxRuntimeSuccessAndSecurityContract(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", NodeName: "kind-control-plane", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  agentSandboxResult(t, engineruntime.TerminalSucceeded, ""),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	runtime.applyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		return map[string]string{"agent-sandbox-spike.txt": "agent-sandbox-v0.5.3\n"}, "canonical diff", nil
	}
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.BaseSHA != agentSandboxSpec().ExpectedBaseSHA || result.Diff != "canonical diff" || result.TerminalState != engineruntime.TerminalSucceeded {
		t.Fatalf("result = %+v", result)
	}
	if !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("cleanup = %+v deleted=%v", result.Telemetry, api.deleted)
	}
	assertSandboxSecurity(t, api.object)
}

func TestAgentSandboxRuntimeRejectsEmptySuccessfulLogs(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if !errors.Is(err, engineruntime.ErrMalformedResult) || result.TerminalState != engineruntime.TerminalFailed || !result.Telemetry.CleanupCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAgentSandboxRuntimeRejectsCredentialsBeforeCreate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*engineruntime.GenerateSpec)
		want string
	}{
		{name: "git token", edit: func(s *engineruntime.GenerateSpec) { s.Repo.Token = "secret" }, want: "repository token"},
		{name: "model token", edit: func(s *engineruntime.GenerateSpec) { s.Token = "secret" }, want: "model or provider"},
		{name: "endpoint", edit: func(s *engineruntime.GenerateSpec) { s.Endpoint = "https://model.example.test" }, want: "model or provider"},
		{name: "shell", edit: func(s *engineruntime.GenerateSpec) { s.AllowBash = true }, want: "shell execution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAgentSandboxAPI{}
			runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
			spec := agentSandboxSpec()
			tc.edit(&spec)
			if _, err := runtime.Generate(context.Background(), spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if api.object != nil {
				t.Fatal("Sandbox was created for rejected input")
			}
		})
	}
}

func TestAgentSandboxRuntimeTerminalFailureCleansUp(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodFailed"},
		logs:  agentSandboxResult(t, engineruntime.TerminalFailed, "validation failed"),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || result.TerminalState != engineruntime.TerminalFailed || result.FailureReason != "validation failed" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("failure cleanup = %+v deleted=%v", result.Telemetry, api.deleted)
	}
}

func TestAgentSandboxRuntimeTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func() (context.Context, context.CancelFunc)
		state engineruntime.TerminalState
		isErr error
	}{
		{name: "timeout", setup: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 5*time.Millisecond)
		}, state: engineruntime.TerminalTimedOut, isErr: context.DeadlineExceeded},
		{name: "cancelled", setup: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}, state: engineruntime.TerminalCancelled, isErr: engineruntime.ErrCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAgentSandboxAPI{state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1"}}
			runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
			ctx, cancel := tc.setup()
			defer cancel()
			spec := agentSandboxSpec()
			result, err := runtime.Generate(ctx, spec)
			if result.TerminalState != tc.state || !errors.Is(err, tc.isErr) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if !result.Telemetry.CleanupCompleted || !api.deleted {
				t.Fatalf("cleanup = %+v deleted=%v", result.Telemetry, api.deleted)
			}
		})
	}
}

func TestAgentSandboxRuntimeRejectsUnreconstructableResult(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  agentSandboxResult(t, engineruntime.TerminalSucceeded, ""),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	runtime.applyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		return map[string]string{"other.txt": "wrong\n"}, "canonical diff", nil
	}
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || !errors.Is(err, engineruntime.ErrResultExtraFile) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if !api.deleted {
		t.Fatal("Sandbox was not cleaned after result rejection")
	}
}

func testAgentSandboxOptions() AgentSandboxOptions {
	return AgentSandboxOptions{
		Namespace: "fix-eval", Image: "fixer:test", ServiceAccountName: "fix-workload",
		ModelGateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.internal.example/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout:      time.Minute, OutputLimitBytes: 512 << 10, PollEvery: time.Millisecond,
	}
}

func assertSandboxSecurity(t *testing.T, object map[string]any) {
	t.Helper()
	if object["apiVersion"] != "agents.x-k8s.io/v1beta1" || object["kind"] != "Sandbox" {
		t.Fatalf("wrong resource: %v %v", object["apiVersion"], object["kind"])
	}
	spec := object["spec"].(map[string]any)
	if spec["service"] != false || spec["operatingMode"] != "Running" || spec["shutdownPolicy"] != "Delete" || spec["shutdownTime"] == "" {
		t.Fatalf("lifecycle or service not pinned: %v", spec)
	}
	pod := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false || pod["serviceAccountName"] != "fix-workload" || pod["restartPolicy"] != "Never" {
		t.Fatalf("pod identity not pinned: %v", pod)
	}
	assertAppArmorMode(t, pod, true)
	container := pod["containers"].([]any)[0].(map[string]any)
	security := container["securityContext"].(map[string]any)
	if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true || security["runAsNonRoot"] != true {
		t.Fatalf("container security context = %v", security)
	}
	if strings.Contains(strings.ToLower(string(mustJSON(t, object))), "hostpath") || strings.Contains(strings.ToLower(string(mustJSON(t, object))), "privileged") {
		t.Fatalf("unsafe pod field in object: %s", mustJSON(t, object))
	}
	env := container["env"].([]any)[0].(map[string]any)
	encoded := env["value"].(string)
	requestJSON, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var request engineruntime.ExecutionRequest
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestJSON), "secret") || request.ExpectedBaseSHA == "" || request.CommandPolicy.AllowShell {
		t.Fatalf("unsafe request: %s", requestJSON)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAgentSandboxUnavailableLogsReturnTerminalFailure(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state:   sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodFailed"},
		logsErr: errors.New("executor container never created after Pod scheduling: token=secret-value"),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || !errors.Is(err, engineruntime.ErrMalformedResult) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if result.Version != engineruntime.ExecutionContractVersion || result.BaseSHA != agentSandboxSpec().ExpectedBaseSHA || result.TerminalState != engineruntime.TerminalFailed {
		t.Fatalf("terminal result = %+v", result)
	}
	if !strings.Contains(result.FailureReason, "never created") || strings.Contains(result.FailureReason, "secret-value") {
		t.Fatalf("failure reason = %q", result.FailureReason)
	}
	if result.Resources.Backend != agentSandboxBackend || !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("resources=%+v telemetry=%+v deleted=%v", result.Resources, result.Telemetry, api.deleted)
	}
}

func TestAgentSandboxRuntimeRejectsMalformedResultAndCleansUp(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{"version":`,
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || !errors.Is(err, engineruntime.ErrMalformedResult) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("cleanup = %+v deleted=%v", result.Telemetry, api.deleted)
	}
}

func TestAgentSandboxCleanupRejectsUIDChange(t *testing.T) {
	api := &fakeAgentSandboxAPI{state: sandboxState{Exists: true, UID: "new-uid", PodName: "fix-request-1"}}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	err := runtime.Cleanup(context.Background(), engineruntime.WorkRef{Backend: agentSandboxBackend, Namespace: "fix-eval", Name: "fix-request-1", UID: "old-uid"})
	if !errors.Is(err, engineruntime.ErrWorkIdentityChanged) || api.deleted {
		t.Fatalf("error=%v deleted=%v", err, api.deleted)
	}
}

func TestAgentSandboxCompatibilityRequiresExactWorkloadShape(t *testing.T) {
	runtime := newAgentSandboxRuntimeForTest(nil, testAgentSandboxOptions())
	request, err := executionRequest(agentSandboxSpec())
	if err != nil {
		t.Fatal(err)
	}
	requestJSON := mustJSON(t, request)
	contractHash := agentSandboxContractHash(requestJSON, runtime.opts)
	object := runtime.sandboxObject("fix-request-1", requestJSON, contractHash[:], request, "request-1")
	desired := sandboxStateFromObject(&unstructured.Unstructured{Object: object})
	existing := desired
	existing.UID = "uid-1"
	if !compatibleSandboxState(existing, desired) {
		t.Fatal("identical workload shape was rejected")
	}
	existing.ShapeHash = strings.Repeat("f", 64)
	if compatibleSandboxState(existing, desired) {
		t.Fatal("changed workload shape was accepted")
	}
	existing = desired
	later, _ := time.Parse(time.RFC3339, desired.ShutdownTime)
	existing.ShutdownTime = later.Add(time.Minute).Format(time.RFC3339)
	if compatibleSandboxState(existing, desired) {
		t.Fatal("extended shutdown time was accepted")
	}
}

func TestAgentSandboxProductionOptionsFailClosed(t *testing.T) {
	base := AgentSandboxOptions{
		Namespace:          "fix-eval",
		Image:              "registry.internal.example/fixer@sha256:" + strings.Repeat("a", 64),
		ServiceAccountName: "fix-workload",
		RuntimeClassName:   "kata-vm-isolation",
		ModelGateway: engineruntime.ModelGatewayConfig{
			Endpoint: "https://gateway.fix-eval.svc.cluster.local/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1",
		},
		Timeout: 10 * time.Minute, OutputLimitBytes: 128 << 10,
	}
	for _, tc := range []struct {
		name string
		edit func(*AgentSandboxOptions)
		want string
	}{
		{name: "mutable image", edit: func(o *AgentSandboxOptions) { o.Image = "registry.internal.example/fixer:latest" }, want: "immutable sha256"},
		{name: "runtime class", edit: func(o *AgentSandboxOptions) { o.RuntimeClassName = "" }, want: "runtime class"},
		{name: "insecure gateway", edit: func(o *AgentSandboxOptions) { o.ModelGateway.Endpoint = "http://gateway.fix-eval.svc/v1" }, want: "absolute https"},
		{name: "public gateway", edit: func(o *AgentSandboxOptions) { o.ModelGateway.Endpoint = "https://api.openai.com/v1" }, want: "public CA private DNS"},
		{name: "unacknowledged private DNS", edit: func(o *AgentSandboxOptions) {
			o.ModelGateway.Endpoint = "https://model-gateway.platform.example.com/v1"
		}, want: "public CA private DNS"},
		{name: "direct provider with acknowledgement", edit: func(o *AgentSandboxOptions) {
			o.ModelGateway.Endpoint = "https://api.anthropic.com/v1"
			o.PublicCAPrivateDNS = true
		}, want: "non-provider"},
		{name: "NVIDIA provider with acknowledgement", edit: func(o *AgentSandboxOptions) {
			o.ModelGateway.Endpoint = "https://integrate.api.nvidia.com/v1/chat/completions"
			o.PublicCAPrivateDNS = true
		}, want: "non-provider"},
		{name: "internal with acknowledgement", edit: func(o *AgentSandboxOptions) { o.PublicCAPrivateDNS = true }, want: "applies only"},
		{name: "resource", edit: func(o *AgentSandboxOptions) { o.Resources.MemoryLimit = "not-a-quantity" }, want: "memory limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.edit(&opts)
			if _, err := NewAgentSandboxRuntime(&fakeAgentSandboxAPI{}, opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := NewAgentSandboxRuntime(&fakeAgentSandboxAPI{}, base); err != nil {
		t.Fatalf("valid production options rejected: %v", err)
	}
	base.ModelGateway.Endpoint = "https://model-gateway.platform.example.com/v1"
	base.PublicCAPrivateDNS = true
	if _, err := NewAgentSandboxRuntime(&fakeAgentSandboxAPI{}, base); err != nil {
		t.Fatalf("valid public CA private DNS options rejected: %v", err)
	}
}

func TestAgentSandboxCreateMismatchCleansNewUID(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		createErr: fmt.Errorf("%w: defaulted shape changed", engineruntime.ErrWorkIdentityChanged), returnStateOnCreateError: true,
		state: sandboxState{Exists: true, UID: "uid-created", PodName: "fix-request-1"},
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	_, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if !errors.Is(err, engineruntime.ErrWorkIdentityChanged) || !api.deleted || api.deleteUID != "uid-created" {
		t.Fatalf("error=%v deleted=%v deleteUID=%q", err, api.deleted, api.deleteUID)
	}
}

func TestAgentSandboxCreateAmbiguityCleansAcceptedWork(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		createErr: errors.New("connection reset after create"),
		state:     sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1"},
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	_, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || !errors.Is(err, engineruntime.ErrUnavailable) || !api.deleted || api.deleteUID != "uid-1" {
		t.Fatalf("error=%v deleted=%v deleteUID=%q", err, api.deleted, api.deleteUID)
	}
}

func TestAgentSandboxCreateAmbiguityReturnsObservedWorkWhenCleanupIsPending(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		createErr: errors.New("connection reset after create"), deleteErr: engineruntime.ErrCleanupPending,
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "critic-request-1"},
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	var observed engineruntime.WorkRef
	result, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "critic", ExecutionID: "request-1", RequestEnv: "PROW_AI_CAUSAL_CRITIC_REQUEST_B64",
		Request: []byte(`{"version":1}`), Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		WorkObserver: func(_ context.Context, work engineruntime.WorkRef) error {
			if work.UID != "" {
				observed = work
			}
			return nil
		},
	})
	if !errors.Is(err, engineruntime.ErrCleanupPending) || result.Work == nil || result.Work.UID != "uid-1" || observed.UID != "uid-1" {
		t.Fatalf("result=%+v observed=%+v err=%v", result, observed, err)
	}
}

func TestAgentSandboxCreateAmbiguityPreservesIncompatibleWork(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		createErr: errors.New("connection reset after create"), keepStateIdentity: true,
		state: sandboxState{Exists: true, UID: "uid-other", PodName: "fix-request-1", ContractHash: strings.Repeat("f", 64), ExecutionID: "other", ShapeHash: strings.Repeat("e", 64), ShutdownTime: time.Now().UTC().Format(time.RFC3339)},
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	_, err := runtime.Generate(context.Background(), agentSandboxSpec())
	if err == nil || !errors.Is(err, engineruntime.ErrWorkIdentityChanged) || api.deleted {
		t.Fatalf("error=%v deleted=%v", err, api.deleted)
	}
}

func TestAgentSandboxCleanupRequiresObservedUID(t *testing.T) {
	api := &fakeAgentSandboxAPI{state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1"}}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	err := runtime.Cleanup(context.Background(), engineruntime.WorkRef{Backend: agentSandboxBackend, Namespace: "fix-eval", Name: "fix-request-1"})
	if !errors.Is(err, engineruntime.ErrWorkIdentityChanged) || api.deleted {
		t.Fatalf("error=%v deleted=%v", err, api.deleted)
	}
}

func TestAgentSandboxCleanupDetectsOrphanedPod(t *testing.T) {
	api := &fakeAgentSandboxAPI{deleted: false, executionPods: []string{"controller-assigned-pod"}}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := runtime.Cleanup(ctx, engineruntime.WorkRef{Backend: agentSandboxBackend, Namespace: "fix-eval", Name: "orphan"})
	if !errors.Is(err, engineruntime.ErrCleanupPending) {
		t.Fatalf("error = %v", err)
	}
}
