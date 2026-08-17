// Package fixruntime selects the configured coding-agent runtime for fix PRs.
package fixruntime

import (
	"fmt"

	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

// New returns the Agent Sandbox coding-agent runtime for fix PRs.
func New(cfg *project.FixAgentRuntime) (runtime.AgentRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("fix runtime requires agent_runtime configuration")
	}
	if cfg.Type != "" && cfg.Type != "agent-sandbox" {
		return nil, fmt.Errorf("unsupported fix runtime %q", cfg.Type)
	}
	rt, err := NewAgentSandboxRuntimeFromEnv(cfg.ModelProvider.RuntimeConfig(), cfg.ParsedTimeout(), cfg.OutputLimitBytes)
	if err != nil {
		return nil, fmt.Errorf("agent sandbox fix backend unavailable: %w", err)
	}
	return rt, nil
}
