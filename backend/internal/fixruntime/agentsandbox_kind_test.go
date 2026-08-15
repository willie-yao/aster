package fixruntime

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func newAgentSandboxRuntimeForKindTest(api agentSandboxAPI, opts AgentSandboxOptions) *AgentSandboxRuntime {
	opts.testOnly = true
	opts.appArmorCapability = appArmorUnavailableForKindTest
	opts = normalizeAgentSandboxOptions(opts)
	return &AgentSandboxRuntime{api: api, opts: opts, now: time.Now}
}

func newAgentSandboxRuntimeFromEnvForKindTest(expectedProvider modelprovider.Config, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	cfg, err := agentSandboxRESTConfig(strings.TrimSpace(os.Getenv("AGENT_SANDBOX_KUBE_CONTEXT")))
	if err != nil {
		return nil, err
	}
	api, err := newKubeAgentSandboxAPI(cfg)
	if err != nil {
		return nil, err
	}
	opts := AgentSandboxOptions{
		Namespace: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_NAMESPACE")), Image: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_IMAGE")),
		ServiceAccountName: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_SERVICE_ACCOUNT")), RuntimeClassName: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_RUNTIME_CLASS")),
		ModelProvider: expectedProvider, Timeout: expectedTimeout, OutputLimitBytes: expectedOutputLimit,
		PollEvery: defaultSandboxPollEvery, testOnly: true, appArmorCapability: appArmorUnavailableForKindTest,
	}
	caBundle, err := kindTestCABundleFromEnv()
	if err != nil {
		return nil, err
	}
	opts.CABundle = caBundle
	if value := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_POLL_INTERVAL")); value != "" {
		opts.PollEvery, err = time.ParseDuration(value)
		if err != nil {
			return nil, err
		}
	}
	return newAgentSandboxRuntimeForKindTest(api, opts), nil
}

func kindTestCABundleFromEnv() (modelprovider.CABundleConfig, error) {
	config := modelprovider.CABundleConfig{
		ExistingConfigMap: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_CONFIG_MAP")),
		Key:               strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_KEY")),
		SHA256:            strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_SHA256")),
	}
	if err := modelprovider.ValidateCABundleConfig(config); err != nil {
		return modelprovider.CABundleConfig{}, err
	}
	return config, nil
}

func TestKindTestCABundleFromEnv(t *testing.T) {
	for _, testCase := range []struct {
		name, configMap, key, hash, want string
	}{
		{name: "disabled"},
		{name: "valid", configMap: "model-provider-ca", key: "ca-bundle.pem", hash: strings.Repeat("a", 64)},
		{name: "partial", configMap: "model-provider-ca", want: "requires ConfigMap name, key, and SHA-256"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_CONFIG_MAP", testCase.configMap)
			t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_KEY", testCase.key)
			t.Setenv("AGENT_SANDBOX_MODEL_PROVIDER_CA_SHA256", testCase.hash)
			config, err := kindTestCABundleFromEnv()
			if testCase.want == "" && err != nil {
				t.Fatal(err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
			if testCase.name == "valid" && (!config.Enabled() || config.ExistingConfigMap != testCase.configMap || config.Key != testCase.key || config.SHA256 != testCase.hash) {
				t.Fatalf("config = %+v", config)
			}
		})
	}
}
