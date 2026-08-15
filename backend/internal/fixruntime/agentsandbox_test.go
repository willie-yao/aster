package fixruntime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
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
	caValidationErr          error
	caValidationCalls        int
}

func (f *fakeAgentSandboxAPI) ValidateCABundle(_ context.Context, _ string, _ modelprovider.CABundleConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caValidationCalls++
	return f.caValidationErr
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
		ModelProvider:   testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture-model"),
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
		ModelProvider: testGatewayProvider("https://gateway.fix-eval.svc.cluster.local/v1/chat/completions", "fixture-model"),
		Timeout:       10 * time.Minute, OutputLimitBytes: 128 << 10,
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
	if result.Output != `{"review":"pass"}` || result.FinishedReason != "PodSucceeded" || !result.Telemetry.CleanupCompleted || result.Telemetry.UsageStatus != "unavailable_from_model_gateway" || result.Telemetry.ProviderCredentialMode != modelprovider.CredentialModeGateway || result.Telemetry.ProviderAPI != modelprovider.APIChatCompletions {
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
	if _, ok := pod["initContainers"]; ok {
		t.Fatal("read-only critic workload received a stager")
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

func TestAgentSandboxRunUsesStagedReadOnlyWorkspace(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "analysis-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{"terminal_state":"succeeded"}`,
	}
	opts := testAgentSandboxOptions()
	opts.StagerImage = "stager:test"
	opts.StagerInputClaim = "analysis-input"
	runtime := newAgentSandboxRuntimeForTest(api, opts)
	result, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", ExecutionID: "request-1", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64",
		Request: []byte(`{"version":1}`), Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		StagedWorkspace: &agentsandbox.StagedWorkspace{RequestEnv: "PROW_AI_ANALYSIS_STAGE_REQUEST_B64", Request: []byte(`{"manifest":"abc"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != `{"terminal_state":"succeeded"}` || !result.Telemetry.CleanupCompleted {
		t.Fatalf("result=%+v", result)
	}
	pod := api.object["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("containers=%+v", containers)
	}
	container := containers[0].(map[string]any)
	mounts := container["volumeMounts"].([]any)
	wantMounts := map[string]bool{
		agentsandbox.StagedWorkspaceRoot:       true,
		agentsandbox.StagedWorkspaceResultPath: false,
		"/tmp":                                 false,
	}
	if len(mounts) != len(wantMounts) {
		t.Fatalf("mounts=%+v", mounts)
	}
	for _, value := range mounts {
		mount := value.(map[string]any)
		path := mount["mountPath"].(string)
		wantReadOnly, ok := wantMounts[path]
		if !ok || (mount["readOnly"] == true) != wantReadOnly {
			t.Fatalf("mount=%+v", mount)
		}
		if _, ok := mount["subPath"]; ok {
			t.Fatalf("mount=%+v", mount)
		}
	}
	initContainers := pod["initContainers"].([]any)
	if len(initContainers) != 1 {
		t.Fatalf("init containers=%+v", initContainers)
	}
	stager := initContainers[0].(map[string]any)
	if stager["name"] != agentSandboxStagerName || stager["image"] != "stager:test" {
		t.Fatalf("stager=%+v", stager)
	}
	stageEnv := stager["env"].([]any)[0].(map[string]any)
	if stageEnv["name"] != "PROW_AI_ANALYSIS_STAGE_REQUEST_B64" {
		t.Fatalf("stage env=%+v", stageEnv)
	}
	decoded, err := base64.StdEncoding.DecodeString(stageEnv["value"].(string))
	if err != nil || string(decoded) != `{"manifest":"abc"}` {
		t.Fatalf("stage request=%q err=%v", decoded, err)
	}
	if pod["automountServiceAccountToken"] != false || len(pod["volumes"].([]any)) != 5 {
		t.Fatalf("pod=%+v", pod)
	}
	inputVolume := pod["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if inputVolume["claimName"] != "analysis-input" || inputVolume["readOnly"] != true {
		t.Fatalf("input volume=%+v", inputVolume)
	}
	stagerMounts := stager["volumeMounts"].([]any)
	volumes := pod["volumes"].([]any)
	if mounts[0].(map[string]any)["name"] != "workspace" || mounts[1].(map[string]any)["name"] != "result" || mounts[2].(map[string]any)["name"] != "executor-tmp" || volumes[2].(map[string]any)["name"] != "result" || volumes[2].(map[string]any)["emptyDir"].(map[string]any)["sizeLimit"] != agentSandboxResultVolumeLimit || stagerMounts[0].(map[string]any)["name"] != "input" || stagerMounts[0].(map[string]any)["readOnly"] != true || stagerMounts[2].(map[string]any)["name"] != "stager-tmp" {
		t.Fatalf("staged mounts are invalid: executor=%+v stager=%+v", mounts, stagerMounts)
	}
}

func TestAgentSandboxStagedWorkspaceRequiresStagerImage(t *testing.T) {
	runtime := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, testAgentSandboxOptions())
	_, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		StagedWorkspace: &agentsandbox.StagedWorkspace{RequestEnv: "PROW_AI_ANALYSIS_STAGE_REQUEST_B64", Request: []byte(`{}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "stager image") {
		t.Fatalf("error=%v", err)
	}
}

func TestAgentSandboxWorkloadIdentityIncludesStageRequest(t *testing.T) {
	opts := testAgentSandboxOptions()
	opts.StagerImage = "stager:test"
	opts.StagerInputClaim = "analysis-input"
	left := agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{"request":1}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		StagedWorkspace: &agentsandbox.StagedWorkspace{RequestEnv: "PROW_AI_ANALYSIS_STAGE_REQUEST_B64", Request: []byte(`{"stage":1}`)},
	}
	right := left
	right.StagedWorkspace = &agentsandbox.StagedWorkspace{RequestEnv: left.StagedWorkspace.RequestEnv, Request: []byte(`{"stage":2}`)}
	if agentSandboxWorkloadHash(left, opts) == agentSandboxWorkloadHash(right, opts) {
		t.Fatal("stage request did not affect workload identity")
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

func TestAgentSandboxRuntimeIdentityIncludesWorkloadConfiguration(t *testing.T) {
	base := testAgentSandboxOptions()
	baseIdentity := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, base).RuntimeIdentity()
	if legacy := legacyAgentSandboxRuntimeIdentity(base); baseIdentity != legacy {
		t.Fatalf("disabled CA runtime identity changed: got %s want %s", baseIdentity, legacy)
	}
	for _, mutate := range []func(*AgentSandboxOptions){
		func(opts *AgentSandboxOptions) { opts.Namespace = "other-exec" },
		func(opts *AgentSandboxOptions) { opts.ServiceAccountName = "other-workload" },
		func(opts *AgentSandboxOptions) { opts.RuntimeClassName = "other-runtime" },
		func(opts *AgentSandboxOptions) { opts.Resources.MemoryLimit = "1Gi" },
		func(opts *AgentSandboxOptions) {
			opts.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
		},
		func(opts *AgentSandboxOptions) {
			opts.ModelProvider = testResponsesProvider("https://api.openai.com/v1/responses", "fixture-model")
			opts.ProviderSecretRef = ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}
		},
		func(opts *AgentSandboxOptions) {
			opts.StagerImage = "stager:test"
			opts.StagerInputClaim = "analysis-input"
		},
		func(opts *AgentSandboxOptions) { opts.CABundle = testCABundleConfig() },
		func(opts *AgentSandboxOptions) {
			opts.CABundle = testCABundleConfig()
			opts.CABundle.Key = "rotated.pem"
		},
		func(opts *AgentSandboxOptions) {
			opts.CABundle = testCABundleConfig()
			opts.CABundle.SHA256 = strings.Repeat("b", 64)
		},
	} {
		changed := base
		mutate(&changed)
		if got := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, changed).RuntimeIdentity(); got == baseIdentity {
			t.Fatalf("runtime identity did not change for %+v", changed)
		}
	}
}

func legacyAgentSandboxRuntimeIdentity(opts AgentSandboxOptions) string {
	opts = normalizeAgentSandboxOptions(opts)
	payload, _ := json.Marshal(struct {
		Backend            string                           `json:"backend"`
		Namespace          string                           `json:"namespace"`
		Image              string                           `json:"image"`
		ServiceAccountName string                           `json:"service_account_name"`
		RuntimeClassName   string                           `json:"runtime_class_name"`
		ModelProvider      modelprovider.Config             `json:"model_provider,omitempty"`
		ProviderSecretRef  ProviderSecretRef                `json:"provider_secret_ref,omitempty"`
		ModelGateway       engineruntime.ModelGatewayConfig `json:"model_gateway,omitempty"`
		PublicCAPrivateDNS bool                             `json:"public_ca_private_dns,omitempty"`
		Timeout            string                           `json:"timeout"`
		OutputLimitBytes   int64                            `json:"output_limit_bytes"`
		PollEvery          string                           `json:"poll_every"`
		Resources          AgentSandboxResources            `json:"resources"`
		AppArmorCapability string                           `json:"app_armor_capability"`
		StagerImage        string                           `json:"stager_image,omitempty"`
		StagerInputClaim   string                           `json:"stager_input_claim,omitempty"`
	}{
		Backend: agentSandboxBackend, Namespace: opts.Namespace, Image: opts.Image,
		ServiceAccountName: opts.ServiceAccountName, RuntimeClassName: opts.RuntimeClassName,
		ModelProvider: opts.ModelProvider, ProviderSecretRef: opts.ProviderSecretRef,
		ModelGateway: opts.ModelGateway, PublicCAPrivateDNS: opts.PublicCAPrivateDNS,
		Timeout: opts.Timeout.String(), OutputLimitBytes: opts.OutputLimitBytes, PollEvery: opts.PollEvery.String(),
		Resources: opts.Resources, AppArmorCapability: opts.appArmorCapability.String(), StagerImage: opts.StagerImage, StagerInputClaim: opts.StagerInputClaim,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func TestAgentSandboxCABundleIsValidatedAndMountedForFixOnly(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{}`,
	}
	opts := testAgentSandboxOptions()
	opts.CABundle = testCABundleConfig()
	runtime := newAgentSandboxRuntimeForTest(api, opts)
	_, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "fix", ExecutionID: "request-1", RequestEnv: agentSandboxRequestEnv, Request: []byte(`{"version":2}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit, WritableWorkspace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.caValidationCalls != 1 {
		t.Fatalf("CA validation calls = %d", api.caValidationCalls)
	}
	metadata := api.object["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if annotations[agentSandboxCABundleAnnotation] != opts.CABundle.SHA256 {
		t.Fatalf("CA annotation = %v", annotations[agentSandboxCABundleAnnotation])
	}
	pod := api.object["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
	container := pod["containers"].([]any)[0].(map[string]any)
	assertEnvValue(t, container["env"].([]any), "NODE_EXTRA_CA_CERTS", modelprovider.CABundleMountPath)
	assertEnvValue(t, container["env"].([]any), modelprovider.CABundleHashEnv, opts.CABundle.SHA256)
	mounts := container["volumeMounts"].([]any)
	if len(mounts) != 3 || mounts[2].(map[string]any)["name"] != modelprovider.CABundleVolumeName || mounts[2].(map[string]any)["mountPath"] != modelprovider.CABundleMountDir || mounts[2].(map[string]any)["readOnly"] != true {
		t.Fatalf("CA mounts = %v", mounts)
	}
	volumes := pod["volumes"].([]any)
	if len(volumes) != 3 {
		t.Fatalf("volumes = %v", volumes)
	}
	configMap := volumes[2].(map[string]any)["configMap"].(map[string]any)
	if configMap["name"] != opts.CABundle.ExistingConfigMap || configMap["optional"] != false || configMap["defaultMode"] != int64(0o444) {
		t.Fatalf("CA ConfigMap volume = %v", configMap)
	}
	items := configMap["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != opts.CABundle.Key || items[0].(map[string]any)["path"] != "ca-bundle.pem" {
		t.Fatalf("CA ConfigMap items = %v", items)
	}

	_, err = runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", ExecutionID: "request-2", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{"version":3}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit, WritableWorkspace: true,
	})
	if err == nil || !strings.Contains(err.Error(), "Fix-runtime only") {
		t.Fatalf("analysis error = %v", err)
	}
}

func TestAgentSandboxCABundleValidationFailsBeforeCreate(t *testing.T) {
	api := &fakeAgentSandboxAPI{caValidationErr: errors.New("configured hash mismatch")}
	opts := testAgentSandboxOptions()
	opts.CABundle = testCABundleConfig()
	runtime := newAgentSandboxRuntimeForTest(api, opts)
	_, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "fix", ExecutionID: "request-1", RequestEnv: agentSandboxRequestEnv, Request: []byte(`{"version":2}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit, WritableWorkspace: true,
	})
	if err == nil || !strings.Contains(err.Error(), "validate model provider CA bundle") {
		t.Fatalf("error = %v", err)
	}
	if api.object != nil {
		t.Fatal("Sandbox was created after CA validation failure")
	}
}

func assertEnvValue(t *testing.T, env []any, name, want string) {
	t.Helper()
	for _, raw := range env {
		entry := raw.(map[string]any)
		if entry["name"] == name {
			if entry["value"] != want {
				t.Fatalf("%s = %v, want %q", name, entry["value"], want)
			}
			return
		}
	}
	t.Fatalf("missing environment %s", name)
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

func TestAgentSandboxRuntimeReconstructsCompletedResultWithFailedCommand(t *testing.T) {
	spec := agentSandboxSpec()
	spec.MaxSteps = 3
	spec.CommandPolicy.Commands = []engineruntime.ExecutionCommand{
		{Argv: []string{"go", "test", "./..."}, TimeoutSeconds: 30},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30},
	}
	executorResult := engineruntime.ExecutionResult{
		Version:      engineruntime.ExecutionContractVersion,
		BaseSHA:      spec.ExpectedBaseSHA,
		ChangedFiles: []string{"agent-sandbox-spike.txt"},
		Files:        map[string]string{"agent-sandbox-spike.txt": "reconstructed content\n"},
		Diff:         "diff --git a/agent-sandbox-spike.txt b/agent-sandbox-spike.txt\n",
		CommandResults: []engineruntime.CommandResult{
			{Argv: []string{"go", "test", "./..."}, ExitCode: 1, DurationMs: 20},
			{Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 2},
		},
		TerminalState: engineruntime.TerminalSucceeded,
		DurationMs:    30,
	}
	encoded, err := json.Marshal(executorResult)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  string(encoded),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	reconstructions := 0
	runtime.applyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		reconstructions++
		return map[string]string{"agent-sandbox-spike.txt": "reconstructed content\n"}, "canonical diff", nil
	}
	result, err := runtime.Generate(context.Background(), spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if reconstructions != 1 || result.TerminalState != engineruntime.TerminalSucceeded || len(result.CommandResults) != 2 || result.CommandResults[0].ExitCode != 1 {
		t.Fatalf("reconstructions=%d result=%+v", reconstructions, result)
	}
	if result.Files["agent-sandbox-spike.txt"] != "reconstructed content\n" || result.Diff != "canonical diff" {
		t.Fatalf("reconstructed result = %+v", result)
	}
	if !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("cleanup = %+v deleted=%v", result.Telemetry, api.deleted)
	}
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
		ModelProvider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture-model"),
		Timeout:       time.Minute, OutputLimitBytes: 512 << 10, PollEvery: time.Millisecond,
	}
}

func testCABundleConfig() modelprovider.CABundleConfig {
	return modelprovider.CABundleConfig{ExistingConfigMap: "model-provider-ca", Key: "ca-bundle.pem", SHA256: strings.Repeat("a", 64)}
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
		ModelProvider:      testGatewayProvider("https://gateway.fix-eval.svc.cluster.local/v1/chat/completions", "fixture-model"),
		Timeout:            10 * time.Minute, OutputLimitBytes: 128 << 10,
	}
	for _, tc := range []struct {
		name string
		edit func(*AgentSandboxOptions)
		want string
	}{
		{name: "mutable image", edit: func(o *AgentSandboxOptions) { o.Image = "registry.internal.example/fixer:latest" }, want: "immutable sha256"},
		{name: "mutable stager image", edit: func(o *AgentSandboxOptions) { o.StagerImage = "registry.internal.example/stager:latest" }, want: "stager image"},
		{name: "stager without claim", edit: func(o *AgentSandboxOptions) {
			o.StagerImage = "registry.internal.example/stager@sha256:" + strings.Repeat("b", 64)
		}, want: "requires an input claim"},
		{name: "invalid stager claim", edit: func(o *AgentSandboxOptions) {
			o.StagerImage = "registry.internal.example/stager@sha256:" + strings.Repeat("b", 64)
			o.StagerInputClaim = "Invalid_Claim"
		}, want: "input claim"},
		{name: "runtime class", edit: func(o *AgentSandboxOptions) { o.RuntimeClassName = "" }, want: "runtime class"},
		{name: "insecure gateway", edit: func(o *AgentSandboxOptions) {
			o.ModelProvider.Endpoint = "http://gateway.fix-eval.svc/v1/chat/completions"
		}, want: "absolute HTTPS"},
		{name: "public gateway", edit: func(o *AgentSandboxOptions) { o.ModelProvider.Endpoint = "https://api.openai.com/v1/chat/completions" }, want: "public CA private DNS"},
		{name: "unacknowledged private DNS", edit: func(o *AgentSandboxOptions) {
			o.ModelProvider.Endpoint = "https://model-gateway.platform.example.com/v1/chat/completions"
		}, want: "public CA private DNS"},
		{name: "direct provider with acknowledgement", edit: func(o *AgentSandboxOptions) {
			o.ModelProvider.Endpoint = "https://api.anthropic.com/v1/chat/completions"
			o.ModelProvider.PublicCAPrivateDNS = true
		}, want: "non-provider"},
		{name: "NVIDIA provider with acknowledgement", edit: func(o *AgentSandboxOptions) {
			o.ModelProvider.Endpoint = "https://integrate.api.nvidia.com/v1/chat/completions"
			o.ModelProvider.PublicCAPrivateDNS = true
		}, want: "non-provider"},
		{name: "internal with acknowledgement", edit: func(o *AgentSandboxOptions) { o.ModelProvider.PublicCAPrivateDNS = true }, want: "applies only"},
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
	base.ModelProvider.Endpoint = "https://model-gateway.platform.example.com/v1/chat/completions"
	base.ModelProvider.PublicCAPrivateDNS = true
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

func TestAgentSandboxRunTimeoutUsesPurposeSpecificGrace(t *testing.T) {
	base := agentsandbox.Spec{Timeout: time.Minute}
	analysis := base
	analysis.Purpose = "analysis"
	if got := agentSandboxRunTimeout(analysis); got != time.Minute+agentanalysis.WorkspacePostModelGrace+5*time.Second {
		t.Fatalf("analysis run timeout = %s", got)
	}
	fix := base
	fix.Purpose = "fix"
	if got := agentSandboxRunTimeout(fix); got != time.Minute+agentSandboxResultGrace+5*time.Second {
		t.Fatalf("fix run timeout = %s", got)
	}
}

func TestAgentSandboxPreparedWorkspaceUsesImmutableInputMounts(t *testing.T) {
	runtime := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, testAgentSandboxOptions())
	runtime.opts.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	runtime.opts.StagerImage = "stager:test"
	runtime.opts.StagerInputClaim = "analysis-input"
	manifestHash := strings.Repeat("a", 64)
	identityHash := strings.Repeat("b", 64)
	pod := runtime.sandboxWorkloadPodSpec(agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: manifestHash, IdentityHash: identityHash},
	})
	object := runtime.sandboxObjectForSpec("analysis-test", agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: manifestHash, IdentityHash: identityHash},
	}, []byte(strings.Repeat("b", 32)), "execution-1")
	annotations := object["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations[agentSandboxPreparedAnnotation] != manifestHash || annotations[agentSandboxPreparedIdentityAnnotation] != identityHash || annotations[agentSandboxReasoningEffortAnnotation] != "high" {
		t.Fatalf("annotations=%+v", annotations)
	}
	if got := pod["activeDeadlineSeconds"]; got != int64(time.Minute/time.Second)+int64(agentanalysis.WorkspacePostModelGrace/time.Second) {
		t.Fatalf("analysis active deadline = %v", got)
	}
	if _, ok := pod["initContainers"]; ok {
		t.Fatalf("prepared workspace unexpectedly has init containers: %+v", pod)
	}
	mounts := pod["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
	if len(mounts) != 4 {
		t.Fatalf("mounts=%+v", mounts)
	}
	want := map[string]string{
		agentsandbox.StagedWorkspaceSourcePath:    manifestHash + "/source",
		agentsandbox.StagedWorkspaceArtifactsPath: manifestHash + "/artifacts",
	}
	for _, raw := range mounts {
		mount := raw.(map[string]any)
		if subPath, ok := want[mount["mountPath"].(string)]; ok {
			if mount["name"] != "input" || mount["readOnly"] != true || mount["subPath"] != subPath {
				t.Fatalf("input mount=%+v", mount)
			}
		}
	}
	volumes := pod["volumes"].([]any)
	if len(volumes) != 3 || volumes[0].(map[string]any)["name"] != "input" {
		t.Fatalf("volumes=%+v", volumes)
	}
}

func TestAgentSandboxWorkloadIdentityIncludesPreparedManifest(t *testing.T) {
	opts := testAgentSandboxOptions()
	opts.StagerImage = "stager:test"
	opts.StagerInputClaim = "analysis-input"
	left := agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{"request":1}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("c", 64)},
	}
	right := left
	right.PreparedWorkspace = &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("b", 64), IdentityHash: strings.Repeat("c", 64)}
	if agentSandboxWorkloadHash(left, opts) == agentSandboxWorkloadHash(right, opts) {
		t.Fatal("prepared manifest did not affect workload identity")
	}
}

func TestAgentSandboxWorkloadIdentityIncludesPreparedWorkspaceIdentity(t *testing.T) {
	opts := testAgentSandboxOptions()
	left := agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{"request":1}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)},
	}
	right := left
	right.PreparedWorkspace = &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("c", 64)}
	if agentSandboxWorkloadHash(left, opts) == agentSandboxWorkloadHash(right, opts) {
		t.Fatal("prepared workspace identity did not affect workload identity")
	}
}

func TestEnrichSandboxStateWithPodTiming(t *testing.T) {
	state := sandboxState{}
	pod := map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-08-11T10:00:00Z"},
		"status": map[string]any{
			"conditions":            []any{map[string]any{"type": "PodScheduled", "status": "True", "lastTransitionTime": "2026-08-11T10:00:02Z"}},
			"initContainerStatuses": []any{map[string]any{"name": agentSandboxStagerName, "state": map[string]any{"terminated": map[string]any{"startedAt": "2026-08-11T10:00:03Z", "finishedAt": "2026-08-11T10:00:08Z"}}}},
			"containerStatuses":     []any{map[string]any{"name": agentSandboxContainerName, "state": map[string]any{"terminated": map[string]any{"startedAt": "2026-08-11T10:00:09Z", "finishedAt": "2026-08-11T10:00:19Z"}}}},
		},
	}
	enrichSandboxStateWithPod(&state, pod)
	if durationMilliseconds(state.PodCreatedAt, state.ScheduledAt) != 2000 || durationMilliseconds(state.StageStartedAt, state.StageFinishedAt) != 5000 || durationMilliseconds(state.ExecutionStartedAt, state.ExecutionFinishedAt) != 10000 {
		t.Fatalf("state=%+v", state)
	}
}

func TestAgentSandboxPreparedWorkspaceRequiresOnlyInputClaim(t *testing.T) {
	opts := testAgentSandboxOptions()
	opts.testOnly = true
	opts.StagerInputClaim = "analysis-input"
	opts = normalizeAgentSandboxOptions(opts)
	if err := validateAgentSandboxOptions(opts); err != nil {
		t.Fatal(err)
	}
	runtime := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, opts)
	runtime.api.(*fakeAgentSandboxAPI).state = sandboxState{Exists: true, UID: "uid-1", PodName: "analysis-1", Finished: true, FinishedReason: "PodSucceeded"}
	runtime.api.(*fakeAgentSandboxAPI).logs = agentSandboxResult(t, engineruntime.TerminalSucceeded, "")
	_, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("c", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentSandboxPreparedWorkspaceRejectsMissingInputClaim(t *testing.T) {
	runtime := newAgentSandboxRuntimeForTest(&fakeAgentSandboxAPI{}, testAgentSandboxOptions())
	_, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", Request: []byte(`{}`),
		Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
		PreparedWorkspace: &agentsandbox.PreparedWorkspace{ManifestHash: strings.Repeat("a", 64), IdentityHash: strings.Repeat("c", 64)},
	})
	if err == nil || !strings.Contains(err.Error(), "prepared workspace requires an input claim") {
		t.Fatalf("error=%v", err)
	}
}

func TestAgentSandboxProviderCredentialEnvironmentShape(t *testing.T) {
	base := testAgentSandboxOptions()
	spec := agentsandbox.Spec{
		Purpose: "fix", RequestEnv: agentSandboxRequestEnv, Request: []byte(`{"version":2}`),
		Timeout: base.Timeout, OutputLimitBytes: base.OutputLimitBytes, WritableWorkspace: true,
	}
	for _, tc := range []struct {
		name       string
		provider   modelprovider.Config
		secret     ProviderSecretRef
		wantEnv    int
		wantSecret bool
	}{
		{name: "direct bearer", provider: testDirectBearerProvider("https://api.githubcopilot.com/chat/completions", "fixture"), secret: ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}, wantEnv: 2, wantSecret: true},
		{name: "direct none", provider: testDirectUnauthenticatedProvider("https://provider.example/v1/chat/completions", "fixture"), wantEnv: 1},
		{name: "gateway", provider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture"), wantEnv: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.ModelProvider = tc.provider
			opts.ProviderSecretRef = tc.secret
			runtime := newAgentSandboxRuntimeForTest(nil, opts)
			pod := runtime.sandboxWorkloadPodSpec(spec)
			container := pod["containers"].([]any)[0].(map[string]any)
			env := container["env"].([]any)
			if len(env) != tc.wantEnv {
				t.Fatalf("environment entries = %d", len(env))
			}
			secretRefs := 0
			for _, raw := range env {
				entry := raw.(map[string]any)
				valueFrom, ok := entry["valueFrom"].(map[string]any)
				if !ok {
					continue
				}
				secretRef, ok := valueFrom["secretKeyRef"].(map[string]any)
				if !ok {
					t.Fatal("credential environment did not use secretKeyRef")
				}
				secretRefs++
				if entry["name"] != modelprovider.TokenEnv || secretRef["name"] != tc.secret.Name || secretRef["key"] != tc.secret.Key {
					t.Fatal("credential environment reference did not match the configured Secret")
				}
			}
			if secretRefs != btoi(tc.wantSecret) {
				t.Fatalf("Secret references = %d", secretRefs)
			}
			for _, raw := range pod["volumes"].([]any) {
				if _, ok := raw.(map[string]any)["secret"]; ok {
					t.Fatal("provider credential was mounted as a volume")
				}
			}
		})
	}
}

func TestAgentSandboxRuntimeIdentityDistinguishesCredentialModeWithoutCredentialValue(t *testing.T) {
	base := testAgentSandboxOptions()
	direct := base
	direct.ModelProvider = testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture")
	direct.ProviderSecretRef = ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}
	gateway := base
	gateway.ModelProvider = testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture")
	t.Setenv(modelprovider.TokenEnv, strings.Repeat("credential-one-", 3))
	directIdentity := newAgentSandboxRuntimeForTest(nil, direct).RuntimeIdentity()
	t.Setenv(modelprovider.TokenEnv, strings.Repeat("credential-two-", 3))
	if rotated := newAgentSandboxRuntimeForTest(nil, direct).RuntimeIdentity(); rotated != directIdentity {
		t.Fatal("runtime identity changed after only the Secret value changed")
	}
	if gatewayIdentity := newAgentSandboxRuntimeForTest(nil, gateway).RuntimeIdentity(); gatewayIdentity == directIdentity {
		t.Fatal("direct and gateway modes shared a runtime identity")
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestAgentSandboxDirectTelemetryIdentifiesCredentialMode(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "analysis-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{"terminal_state":"succeeded"}`,
	}
	opts := testAgentSandboxOptions()
	opts.ModelProvider = testDirectUnauthenticatedProvider("https://provider.example/v1/chat/completions", "fixture")
	opts.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	runtime := newAgentSandboxRuntimeForTest(api, opts)
	result, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", ExecutionID: "request-1", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64",
		Request: []byte(`{"version":3}`), Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Telemetry.ProviderCredentialMode != modelprovider.CredentialModeDirect || result.Telemetry.ProviderAPI != modelprovider.APIChatCompletions || result.Telemetry.ProviderReasoningEffort != "high" || result.Telemetry.UsageStatus != "unavailable_from_direct_provider" {
		t.Fatalf("telemetry = %+v", result.Telemetry)
	}
}

func TestAgentSandboxProviderSecretReferenceValidation(t *testing.T) {
	base := testAgentSandboxOptions()
	base.testOnly = true
	for _, tc := range []struct {
		name     string
		provider modelprovider.Config
		secret   ProviderSecretRef
		want     string
	}{
		{name: "bearer missing Secret", provider: testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture"), want: "Secret name and key"},
		{name: "bearer missing key", provider: testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture"), secret: ProviderSecretRef{Name: "agent-sandbox-model"}, want: "Secret name and key"},
		{name: "none with Secret", provider: testDirectUnauthenticatedProvider("https://provider.example/v1/chat/completions", "fixture"), secret: ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}, want: "require direct bearer"},
		{name: "gateway with Secret", provider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture"), secret: ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}, want: "require direct bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.ModelProvider = tc.provider
			opts.ProviderSecretRef = tc.secret
			if err := validateAgentSandboxOptions(opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAgentSandboxResponsesTelemetryIdentifiesAPI(t *testing.T) {
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "analysis-request-1", Finished: true, FinishedReason: "PodSucceeded"},
		logs:  `{"terminal_state":"succeeded"}`,
	}
	opts := testAgentSandboxOptions()
	opts.ModelProvider = testResponsesProvider("https://api.openai.com/v1/responses", "fixture")
	opts.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	opts.ProviderSecretRef = ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}
	runtime := newAgentSandboxRuntimeForTest(api, opts)
	result, err := runtime.Run(t.Context(), agentsandbox.Spec{
		Purpose: "analysis", ExecutionID: "request-1", RequestEnv: "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64",
		Request: []byte(`{"version":3}`), Timeout: time.Minute, OutputLimitBytes: defaultSandboxOutputLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Telemetry.ProviderCredentialMode != modelprovider.CredentialModeDirect || result.Telemetry.ProviderAPI != modelprovider.APIResponses || result.Telemetry.ProviderReasoningEffort != "high" {
		t.Fatalf("telemetry = %+v", result.Telemetry)
	}
}

func TestAgentSandboxRuntimeRetainsReviewScopeClassification(t *testing.T) {
	executorResult := engineruntime.ExecutionResult{
		Version: engineruntime.ExecutionContractVersion, BaseSHA: agentSandboxSpec().ExpectedBaseSHA,
		Files: map[string]string{}, TerminalState: engineruntime.TerminalFailed,
		FailureCode: engineruntime.ExecutionFailureReviewScope, FailureReason: "generated change exceeded review scope",
		CommandResults: []engineruntime.CommandResult{{
			Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 2,
		}},
		DurationMs: 10,
	}
	encoded, err := json.Marshal(executorResult)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAgentSandboxAPI{
		state: sandboxState{Exists: true, UID: "uid-1", PodName: "fix-request-1", Finished: true, FinishedReason: "PodFailed"},
		logs:  string(encoded),
	}
	runtime := newAgentSandboxRuntimeForTest(api, testAgentSandboxOptions())
	result, err := runtime.Generate(t.Context(), agentSandboxSpec())
	if err == nil || result.TerminalState != engineruntime.TerminalFailed || result.FailureCode != engineruntime.ExecutionFailureReviewScope {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(result.CommandResults) != 1 || result.CommandResults[0].Argv[0] != "git" || !result.Telemetry.CleanupCompleted || !api.deleted {
		t.Fatalf("result=%+v deleted=%v", result, api.deleted)
	}
}
