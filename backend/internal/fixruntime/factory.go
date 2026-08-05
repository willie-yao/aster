// Package fixruntime selects the configured coding-agent runtime for fix PRs.
package fixruntime

import (
	"fmt"
	"os"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// New returns the configured coding-agent runtime.
func New(cfg *project.FixAgentRuntime) (runtime.AgentRuntime, error) {
	if cfg == nil || cfg.Type == "" || cfg.Type == "opencode" {
		return runtime.NewLocalAgent(), nil
	}
	if cfg.Type != "orka" {
		return nil, fmt.Errorf("unsupported fix runtime %q", cfg.Type)
	}
	maxRetries := 1
	if cfg.OrkaRetries != nil {
		maxRetries = *cfg.OrkaRetries
	}
	rt, err := orka.NewAgentRuntimeFromEnv(orka.FromEnvConfig{
		Namespace:                        cfg.OrkaNamespace,
		AgentRef:                         cfg.OrkaAgentRef,
		API:                              cfg.OrkaAPI,
		Version:                          cfg.OrkaVersion,
		MaxRetries:                       maxRetries,
		KubeContext:                      os.Getenv("ORKA_KUBE_CONTEXT"),
		DelegatedServiceAccountName:      os.Getenv(orka.FixServiceAccountNameEnv),
		DelegatedServiceAccountNamespace: os.Getenv(orka.FixServiceAccountNamespaceEnv),
		PodName:                          os.Getenv(orka.PodNameEnv),
		PodUID:                           os.Getenv(orka.PodUIDEnv),
	})
	if err != nil {
		return nil, fmt.Errorf("orka fix backend unavailable: %w", err)
	}
	return rt, nil
}
