package fixruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestNewDefaultsToLocalAgent(t *testing.T) {
	got, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*runtime.LocalAgentRuntime); !ok {
		t.Fatalf("runtime = %T, want LocalAgentRuntime", got)
	}
}

func TestNewOrkaRequiresConfiguration(t *testing.T) {
	if _, err := New(&project.FixAgentRuntime{Type: "orka"}); err == nil {
		t.Fatal("incomplete Orka runtime config was accepted")
	}
}

func TestNewOrkaRejectsPartialDelegatedIdentity(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:65535
    insecure-skip-tls-verify: true
users:
- name: test
  user:
    token: test
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("ORKA_FIX_SERVICE_ACCOUNT_NAME", "dashboard-fix")
	_, err := New(&project.FixAgentRuntime{
		Type: "orka", OrkaAgentRef: "fixer", OrkaAPI: "http://orka.invalid", OrkaNamespace: "orka-system",
	})
	if err == nil || !strings.Contains(err.Error(), "delegated ServiceAccount namespace is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewLocalRuntimeUsesSRTSandbox(t *testing.T) {
	got, err := New(&project.FixAgentRuntime{Type: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	local, ok := got.(*runtime.LocalAgentRuntime)
	if !ok {
		t.Fatalf("runtime = %T, want LocalAgentRuntime", got)
	}
	if _, ok := local.Sandbox.(*runtime.SRTSandbox); !ok {
		t.Fatalf("sandbox = %T, want SRTSandbox", local.Sandbox)
	}
}

func TestNewOrkaSelectionUnchanged(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:65535
    insecure-skip-tls-verify: true
users:
- name: test
  user:
    token: test
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	got, err := New(&project.FixAgentRuntime{
		Type: "orka", OrkaAgentRef: "fixer", OrkaAPI: "http://orka.invalid", OrkaNamespace: "orka-system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*orka.AgentRuntime); !ok {
		t.Fatalf("runtime = %T, want Orka AgentRuntime", got)
	}
}

func TestNewAgentSandboxSelectionRequiresIsolatedConfiguration(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:65535
users:
- name: test
  user:
    token: test
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) {
		return &rest.Config{Host: "https://127.0.0.1:65535"}, nil
	}
	gateway := project.FixModelGateway{Endpoint: "https://fixture-gateway.fixture.svc.cluster.local/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"}
	t.Setenv("AGENT_SANDBOX_NAMESPACE", "fix-eval")
	t.Setenv("AGENT_SANDBOX_IMAGE", "registry.example.test/fixer@sha256:"+strings.Repeat("a", 64))
	t.Setenv("AGENT_SANDBOX_SERVICE_ACCOUNT", "fix-workload")
	t.Setenv("AGENT_SANDBOX_RUNTIME_CLASS", "kata-vm-isolation")
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_ENDPOINT", gateway.Endpoint)
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_MODEL", gateway.Model)
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_PROTOCOL", gateway.ProtocolVersion)
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS", "false")
	t.Setenv("AGENT_SANDBOX_TIMEOUT", "10m")
	t.Setenv("AGENT_SANDBOX_OUTPUT_LIMIT_BYTES", "131072")
	t.Setenv("AGENT_SANDBOX_CPU_REQUEST", "100m")
	t.Setenv("AGENT_SANDBOX_CPU_LIMIT", "1")
	t.Setenv("AGENT_SANDBOX_MEMORY_REQUEST", "128Mi")
	t.Setenv("AGENT_SANDBOX_MEMORY_LIMIT", "512Mi")
	t.Setenv("AGENT_SANDBOX_EPHEMERAL_STORAGE_LIMIT", "256Mi")
	got, err := New(&project.FixAgentRuntime{Type: "agent-sandbox", Timeout: "10m", OutputLimitBytes: 131072, ModelGateway: gateway})
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := got.(*AgentSandboxRuntime)
	if !ok {
		t.Fatalf("runtime = %T, want AgentSandboxRuntime", got)
	}
	if runtime.opts.appArmorCapability != appArmorRuntimeDefault {
		t.Fatalf("production AppArmor capability = %s", runtime.opts.appArmorCapability)
	}
	gateway.Endpoint = "https://model-gateway.platform.example.com/v1"
	gateway.PublicCAPrivateDNS = true
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_ENDPOINT", gateway.Endpoint)
	t.Setenv("AGENT_SANDBOX_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS", "true")
	got, err = New(&project.FixAgentRuntime{Type: "agent-sandbox", Timeout: "10m", OutputLimitBytes: 131072, ModelGateway: gateway})
	if err != nil {
		t.Fatalf("public CA private DNS runtime: %v", err)
	}
	if !got.(*AgentSandboxRuntime).opts.PublicCAPrivateDNS {
		t.Fatal("public CA private DNS setting was not retained")
	}
}

func TestAgentSandboxRunnerFromEnvIncludesStagerConfiguration(t *testing.T) {
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) {
		return &rest.Config{Host: "https://127.0.0.1:65535"}, nil
	}
	prefix := "AGENT_SANDBOX_ANALYSIS_"
	gateway := runtime.ModelGatewayConfig{
		Endpoint: "https://fixture-gateway.fixture.svc.cluster.local/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1",
	}
	for name, value := range map[string]string{
		"NAMESPACE": "analysis-eval", "IMAGE": "registry.example.test/analyzer@sha256:" + strings.Repeat("a", 64),
		"STAGER_IMAGE": "registry.example.test/stager@sha256:" + strings.Repeat("b", 64), "STAGER_INPUT_CLAIM": "analysis-input",
		"SERVICE_ACCOUNT": "analysis-workload", "RUNTIME_CLASS": "kata-vm-isolation",
		"MODEL_GATEWAY_ENDPOINT": gateway.Endpoint, "MODEL_GATEWAY_MODEL": gateway.Model, "MODEL_GATEWAY_PROTOCOL": gateway.ProtocolVersion,
		"MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS": "false", "TIMEOUT": "1m", "OUTPUT_LIMIT_BYTES": "131072",
		"CPU_REQUEST": "100m", "CPU_LIMIT": "1", "MEMORY_REQUEST": "128Mi", "MEMORY_LIMIT": "512Mi", "EPHEMERAL_STORAGE_LIMIT": "256Mi",
	} {
		t.Setenv(prefix+name, value)
	}
	runtime, err := NewAgentSandboxRunnerFromEnv(prefix, gateway, false, time.Minute, 131072)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.opts.StagerImage != "registry.example.test/stager@sha256:"+strings.Repeat("b", 64) || runtime.opts.StagerInputClaim != "analysis-input" {
		t.Fatalf("stager options = %q %q", runtime.opts.StagerImage, runtime.opts.StagerInputClaim)
	}
}

func TestNewAgentSandboxSelectionDoesNotUseAmbientKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "ambient-kubeconfig"))
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) { return nil, errors.New("not in cluster") }
	gateway := project.FixModelGateway{Endpoint: "https://fixture-gateway.fixture.svc.cluster.local/v1", Model: "fixture-model", ProtocolVersion: "openai-chat-completions-v1"}
	for name, value := range map[string]string{
		"AGENT_SANDBOX_NAMESPACE": "fix-eval", "AGENT_SANDBOX_IMAGE": "registry.example.test/fixer@sha256:" + strings.Repeat("a", 64),
		"AGENT_SANDBOX_SERVICE_ACCOUNT": "fix-workload", "AGENT_SANDBOX_RUNTIME_CLASS": "kata-vm-isolation",
		"AGENT_SANDBOX_MODEL_GATEWAY_ENDPOINT": gateway.Endpoint, "AGENT_SANDBOX_MODEL_GATEWAY_MODEL": gateway.Model,
		"AGENT_SANDBOX_MODEL_GATEWAY_PROTOCOL": gateway.ProtocolVersion, "AGENT_SANDBOX_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS": "false", "AGENT_SANDBOX_TIMEOUT": "10m",
		"AGENT_SANDBOX_OUTPUT_LIMIT_BYTES": "131072", "AGENT_SANDBOX_CPU_REQUEST": "100m", "AGENT_SANDBOX_CPU_LIMIT": "1",
		"AGENT_SANDBOX_MEMORY_REQUEST": "128Mi", "AGENT_SANDBOX_MEMORY_LIMIT": "512Mi", "AGENT_SANDBOX_EPHEMERAL_STORAGE_LIMIT": "256Mi",
	} {
		t.Setenv(name, value)
	}
	_, err := New(&project.FixAgentRuntime{Type: "agent-sandbox", Timeout: "10m", OutputLimitBytes: 131072, ModelGateway: gateway})
	if err == nil || !strings.Contains(err.Error(), "requires in-cluster Kubernetes configuration") {
		t.Fatalf("error = %v", err)
	}
}
