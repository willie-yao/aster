package fixruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestAgentSandboxProductionKindFixture(t *testing.T) {
	if os.Getenv("AGENT_SANDBOX_LIVE") != "1" {
		t.Skip("set AGENT_SANDBOX_LIVE=1 for the disposable-kind evaluation")
	}
	gatewayEndpoint := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_TEST_GATEWAY_ENDPOINT"))
	if gatewayEndpoint == "" {
		t.Fatal("AGENT_SANDBOX_TEST_GATEWAY_ENDPOINT is required")
	}
	gateway := testGatewayProvider(gatewayEndpoint, "fixture-model")
	runtime, err := newAgentSandboxRuntimeFromEnvForKindTest(gateway, 5*time.Minute, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	const baseSHA = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	spec := agentSandboxEvaluationSpec(gateway)
	result, err := runtime.Generate(context.Background(), spec)
	if err != nil {
		t.Fatalf("Generate: %v\nresult: %+v", err, result)
	}
	if result.BaseSHA != baseSHA || result.TerminalState != engineruntime.TerminalSucceeded {
		t.Fatalf("terminal result = %+v", result)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "README" {
		t.Fatalf("changed files = %v", result.ChangedFiles)
	}
	if result.Files["README"] != "Hello Agent Sandbox!\n" {
		t.Fatalf("changed content = %q", result.Files["README"])
	}
	if len(result.CommandResults) != 1 || !equalStrings(result.CommandResults[0].Argv, []string{"git", "diff", "--cached", "--check"}) || result.CommandResults[0].ExitCode != 0 || result.CommandResults[0].TimedOut {
		t.Fatalf("command results = %+v", result.CommandResults)
	}
	if !strings.Contains(result.StdoutSummary, "credential-free fixture gateway") {
		t.Fatalf("executor summary = %q", result.StdoutSummary)
	}
	if !result.Telemetry.TaskFinalized || !result.Telemetry.ResultAvailable || !result.Telemetry.FinalizationValid || !result.Telemetry.CleanupCompleted {
		t.Fatalf("telemetry = %+v", result.Telemetry)
	}
	if result.Resources.Backend != agentSandboxBackend || result.Resources.Namespace == "" || result.Resources.PodName == "" || result.Resources.MeasuredUsage {
		t.Fatalf("resource metadata = %+v", result.Resources)
	}
	if credentialLike.MatchString(result.Diff + result.StdoutSummary + result.StderrSummary + result.FailureReason + result.Output) {
		t.Fatal("credential-like text found in the execution result")
	}
	state, err := runtime.api.State(context.Background(), runtime.opts.Namespace, result.Resources.Name)
	if err != nil || state.Exists {
		t.Fatalf("Sandbox remains after success: state=%+v err=%v", state, err)
	}
	podExists, err := runtime.api.PodExists(context.Background(), runtime.opts.Namespace, result.Resources.PodName)
	if err != nil || podExists {
		t.Fatalf("Sandbox Pod remains after success: exists=%v err=%v", podExists, err)
	}
	if evidenceDir := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_EVIDENCE_DIR")); evidenceDir != "" {
		if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidenceDir, "primary-result.json"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

var credentialLike = regexp.MustCompile(`(?i)(github_pat_|ghp_|bearer[[:space:]]+[a-z0-9._-]+|api[_-]?key|access[_-]?token|client[_-]?secret)`)

func TestWriteAgentSandboxEvaluationFixtures(t *testing.T) {
	if os.Getenv("AGENT_SANDBOX_EVALUATION_FIXTURES") != "1" {
		t.Skip("set AGENT_SANDBOX_EVALUATION_FIXTURES=1 to render evaluation fixtures")
	}
	outputDir := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_EVALUATION_FIXTURE_DIR"))
	namespace := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_NAMESPACE"))
	image := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_IMAGE"))
	runtimeClass := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_RUNTIME_CLASS"))
	gatewayEndpoint := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_TEST_GATEWAY_ENDPOINT"))
	if outputDir == "" || namespace == "" || image == "" || runtimeClass == "" || gatewayEndpoint == "" {
		t.Fatal("evaluation fixture environment is incomplete")
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gateway := testGatewayProvider(gatewayEndpoint, "fixture-model")
	opts := AgentSandboxOptions{
		Namespace: namespace, Image: image, ServiceAccountName: "fix-workload", RuntimeClassName: runtimeClass,
		ModelProvider: gateway, Timeout: 5 * time.Minute, OutputLimitBytes: 256 << 10,
	}
	if configMap := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_CONFIG_MAP")); configMap != "" {
		opts.CABundle = modelprovider.CABundleConfig{
			ExistingConfigMap: configMap,
			Key:               strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_KEY")),
			SHA256:            strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_SHA256")),
		}
		if err := modelprovider.ValidateCABundleConfig(opts.CABundle); err != nil {
			t.Fatal(err)
		}
	}
	production := newAgentSandboxRuntimeForTest(nil, opts)
	localKind := newAgentSandboxRuntimeForKindTest(nil, opts)
	spec := agentSandboxEvaluationSpec(gateway)
	request, err := executionRequest(spec)
	if err != nil {
		t.Fatal(err)
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	writeSandbox := func(filename string, runtime *AgentSandboxRuntime) {
		t.Helper()
		contractHash := agentSandboxContractHash(requestJSON, runtime.opts)
		name := agentSandboxName(spec.ExecutionID, contractHash[:])
		object := runtime.sandboxObject(name, requestJSON, contractHash[:], request, spec.ExecutionID)
		object["metadata"].(map[string]any)["namespace"] = namespace
		writeEvaluationJSON(t, filepath.Join(outputDir, filename), object)
	}
	writeSandbox("production-sandbox.json", production)
	writeSandbox("local-sandbox.json", localKind)
	writeEvaluationJSON(t, filepath.Join(outputDir, "local-preflight-pod.json"),
		localKind.preflightPodObject(namespace, "immutable-image-preflight", requestJSON, request))
	if err := os.WriteFile(filepath.Join(outputDir, "security-capability.txt"), []byte("production_apparmor=RuntimeDefault\nlocal_kind_apparmor=unavailable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func agentSandboxEvaluationSpec(gateway modelprovider.Config) engineruntime.GenerateSpec {
	const baseSHA = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	return engineruntime.GenerateSpec{
		Repo:        engineruntime.RepoRef{Owner: "octocat", Name: "Hello-World", Ref: baseSHA},
		Instruction: "Read README and replace Hello World with Hello Agent Sandbox. Do not run shell commands.",
		MaxSteps:    5, MaxFiles: 1, ModelProvider: gateway,
		Timeout: 5 * time.Minute, ExpectedBaseSHA: baseSHA, OutputLimitBytes: 256 << 10,
		CommandPolicy: engineruntime.CommandPolicy{Commands: []engineruntime.ExecutionCommand{{
			Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30,
		}}},
		ExecutionID: "kind-v053-production-primary",
	}
}

func writeEvaluationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
