package fixruntime

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/project"
)

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

func TestNewRejectsUnsupportedRuntime(t *testing.T) {
	if _, err := New(&project.FixAgentRuntime{Type: "opencode"}); err == nil {
		t.Fatal("unsupported fix runtime was accepted")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("missing agent_runtime configuration was accepted")
	}
}
