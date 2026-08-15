package fixruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/orka"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/runtime"
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

func setAgentSandboxProviderEnv(t *testing.T, prefix string, provider modelprovider.Config, secret ProviderSecretRef, timeout string) {
	t.Helper()
	for name, value := range map[string]string{
		"NAMESPACE": "fix-eval", "IMAGE": "registry.example.test/fixer@sha256:" + strings.Repeat("a", 64),
		"SERVICE_ACCOUNT": "fix-workload", "RUNTIME_CLASS": "kata-vm-isolation",
		"MODEL_PROVIDER_CREDENTIAL_MODE": provider.CredentialMode, "MODEL_PROVIDER_API": provider.API,
		"MODEL_PROVIDER_ENDPOINT": provider.Endpoint, "MODEL_PROVIDER_MODEL": provider.Model, "MODEL_PROVIDER_REASONING_EFFORT": string(provider.ReasoningEffort),
		"MODEL_PROVIDER_AUTH_TYPE": provider.Auth.Type, "MODEL_PROVIDER_AUTH_SECRET_NAME": secret.Name,
		"MODEL_PROVIDER_AUTH_SECRET_KEY": secret.Key, "MODEL_PROVIDER_PUBLIC_CA_PRIVATE_DNS": "false",
		"TIMEOUT": timeout, "OUTPUT_LIMIT_BYTES": "131072", "CPU_REQUEST": "100m", "CPU_LIMIT": "1",
		"MEMORY_REQUEST": "128Mi", "MEMORY_LIMIT": "512Mi", "EPHEMERAL_STORAGE_LIMIT": "256Mi",
	} {
		t.Setenv(prefix+name, value)
	}
}

func TestNewAgentSandboxSelectionRequiresIsolatedConfiguration(t *testing.T) {
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) {
		return &rest.Config{Host: "https://127.0.0.1:65535"}, nil
	}
	provider := testDirectBearerProvider("https://api.githubcopilot.com/chat/completions", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	secret := ProviderSecretRef{Name: "agent-sandbox-model", Key: "AI_TOKEN"}
	setAgentSandboxProviderEnv(t, "AGENT_SANDBOX_", provider, secret, "10m")
	caBundle := testCABundleConfig()
	t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_CONFIG_MAP", caBundle.ExistingConfigMap)
	t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_KEY", caBundle.Key)
	t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_SHA256", caBundle.SHA256)
	got, err := New(&project.FixAgentRuntime{
		Type: "agent-sandbox", Timeout: "10m", OutputLimitBytes: 131072,
		ModelProvider: project.FixModelProvider{
			CredentialMode: provider.CredentialMode, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model, ReasoningEffort: provider.ReasoningEffort,
			Auth: project.FixModelProviderAuth{Type: provider.Auth.Type},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := got.(*AgentSandboxRuntime)
	if !ok {
		t.Fatalf("runtime = %T, want AgentSandboxRuntime", got)
	}
	if runtime.opts.appArmorCapability != appArmorRuntimeDefault || runtime.opts.ProviderSecretRef != secret || runtime.opts.CABundle != caBundle {
		t.Fatalf("production options = %+v", runtime.opts)
	}
}

func TestAgentSandboxProviderRunnerFromEnvIncludesStagerConfiguration(t *testing.T) {
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) {
		return &rest.Config{Host: "https://127.0.0.1:65535"}, nil
	}
	prefix := "AGENT_SANDBOX_ANALYSIS_"
	provider := testGatewayProvider("https://fixture-gateway.fixture.svc.cluster.local/v1/chat/completions", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	setAgentSandboxProviderEnv(t, prefix, provider, ProviderSecretRef{}, "1m")
	t.Setenv(prefix+"NAMESPACE", "analysis-eval")
	t.Setenv(prefix+"IMAGE", "registry.example.test/analyzer@sha256:"+strings.Repeat("a", 64))
	t.Setenv(prefix+"STAGER_IMAGE", "registry.example.test/stager@sha256:"+strings.Repeat("b", 64))
	t.Setenv(prefix+"STAGER_INPUT_CLAIM", "analysis-input")
	t.Setenv(prefix+"SERVICE_ACCOUNT", "analysis-workload")
	t.Setenv(prefix+"MODEL_PROVIDER_CA_CONFIG_MAP", "must-be-ignored")
	t.Setenv(prefix+"MODEL_PROVIDER_CA_KEY", "ca.pem")
	t.Setenv(prefix+"MODEL_PROVIDER_CA_SHA256", strings.Repeat("a", 64))
	runtime, err := NewAgentSandboxProviderRunnerFromEnv(prefix, provider, time.Minute, 131072)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.opts.StagerImage != "registry.example.test/stager@sha256:"+strings.Repeat("b", 64) || runtime.opts.StagerInputClaim != "analysis-input" || runtime.opts.ModelProvider.ReasoningEffort != modelprovider.ReasoningEffortHigh {
		t.Fatalf("stager options = %q %q", runtime.opts.StagerImage, runtime.opts.StagerInputClaim)
	}
	if runtime.opts.CABundle != (modelprovider.CABundleConfig{}) {
		t.Fatalf("analyzer unexpectedly inherited Fix CA bundle = %+v", runtime.opts.CABundle)
	}
}

func TestNewAgentSandboxSelectionDoesNotUseAmbientKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "ambient-kubeconfig"))
	originalInClusterConfig := agentSandboxInClusterConfig
	t.Cleanup(func() { agentSandboxInClusterConfig = originalInClusterConfig })
	agentSandboxInClusterConfig = func() (*rest.Config, error) { return nil, errors.New("not in cluster") }
	provider := testDirectUnauthenticatedProvider("https://provider.example/v1/chat/completions", "fixture-model")
	setAgentSandboxProviderEnv(t, "AGENT_SANDBOX_", provider, ProviderSecretRef{}, "10m")
	_, err := New(&project.FixAgentRuntime{
		Type: "agent-sandbox", Timeout: "10m", OutputLimitBytes: 131072,
		ModelProvider: project.FixModelProvider{
			CredentialMode: provider.CredentialMode, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model,
			Auth: project.FixModelProviderAuth{Type: provider.Auth.Type},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires in-cluster Kubernetes configuration") {
		t.Fatalf("error = %v", err)
	}
}
