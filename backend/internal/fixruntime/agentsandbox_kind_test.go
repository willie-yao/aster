package fixruntime

import (
	"os"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
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
	if value := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_POLL_INTERVAL")); value != "" {
		opts.PollEvery, err = time.ParseDuration(value)
		if err != nil {
			return nil, err
		}
	}
	return newAgentSandboxRuntimeForKindTest(api, opts), nil
}
