package fetcher

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"
)

// AnalysisRuntimeFlags binds the shared Orka analysis CLI options.
type AnalysisRuntimeFlags struct {
	nodeSelectorJSON string
	tolerationsJSON  string
	affinityJSON     string
}

// BindAnalysisRuntimeFlags adds the analysis runtime flags used by fetcher and worker.
func BindAnalysisRuntimeFlags(fs *flag.FlagSet, opts *Options) *AnalysisRuntimeFlags {
	values := &AnalysisRuntimeFlags{}
	fs.StringVar(&opts.AnalysisRuntime.Type, "analysis-runtime", AnalysisRuntimeInProcess, "single-failure analysis runtime: inprocess or orka-container")
	container := &opts.AnalysisRuntime.OrkaContainer
	fs.StringVar(&container.Namespace, "orka-analysis-namespace", "", "Orka namespace for container analysis Tasks")
	fs.StringVar(&container.ResultAPI, "orka-analysis-api", "", "Orka result API base URL")
	fs.StringVar(&container.Image, "orka-analysis-image", "", "analyzer image for Orka container Tasks")
	fs.StringVar(&container.ModelSecretName, "orka-analysis-model-secret", "", "model token Secret in the Orka namespace")
	fs.StringVar(&container.ModelTokenKey, "orka-analysis-model-token-key", "token", "model token Secret key")
	fs.StringVar(&container.GitHubSecretName, "orka-analysis-github-secret", "", "read-only GitHub token Secret in the Orka namespace")
	fs.StringVar(&container.GitHubTokenKey, "orka-analysis-github-token-key", "", "read-only GitHub token Secret key")
	fs.StringVar(&container.StateSecretName, "orka-analysis-state-secret", "", "state key Secret name in the dashboard and Orka namespaces")
	fs.StringVar(&container.StateSecretKey, "orka-analysis-state-key", "state-key", "state Secret key")
	fs.IntVar(&container.MaxConcurrent, "orka-analysis-max-concurrent-tasks", 2, "maximum concurrent Orka analysis Tasks")
	fs.DurationVar(&container.PollInterval, "orka-analysis-poll-interval", 2*time.Second, "Orka Task poll interval")
	fs.DurationVar(&container.TaskTimeout, "orka-analysis-task-timeout", 20*time.Minute, "per-Task timeout")
	fs.IntVar(&container.Retries, "orka-analysis-retries", 1, "Orka Task retries")
	fs.StringVar(&values.nodeSelectorJSON, "orka-analysis-node-selector-json", "", "JSON node selector for analyzer Tasks")
	fs.StringVar(&values.tolerationsJSON, "orka-analysis-tolerations-json", "", "JSON tolerations for analyzer Tasks")
	fs.StringVar(&values.affinityJSON, "orka-analysis-affinity-json", "", "JSON affinity for analyzer Tasks")
	shadow := &opts.ShadowAnalysis
	fs.BoolVar(&shadow.Enabled, "agent-analysis-shadow", false, "run private experimental Agent analysis after authoritative publication")
	fs.StringVar(&shadow.Namespace, "agent-analysis-shadow-namespace", "", "Orka namespace for shadow analysis Tasks")
	fs.StringVar(&shadow.ResultAPI, "agent-analysis-shadow-api", "", "Orka result API base URL for shadow analysis")
	fs.StringVar(&shadow.AgentRef, "agent-analysis-shadow-agent-ref", "", "operator-owned Orka Agent name for shadow analysis")
	fs.StringVar(&shadow.AgentVersion, "agent-analysis-shadow-agent-version", "v1", "declared shadow analysis Agent version")
	fs.StringVar(&shadow.GitSecret, "agent-analysis-shadow-git-secret", "", "optional read-only Orka git Secret for shadow analysis")
	fs.StringVar(&shadow.KubeContext, "agent-analysis-shadow-kube-context", "", "optional kubeconfig context for shadow analysis")
	fs.StringVar(&shadow.LedgerPath, "agent-analysis-shadow-ledger", "", "private shadow comparison ledger path outside the public output directory")
	fs.IntVar(&shadow.MaxPerRun, "agent-analysis-shadow-max-per-run", 1, "maximum shadow analyses per refresh")
	fs.IntVar(&shadow.MaxTurns, "agent-analysis-shadow-max-turns", 12, "maximum Agent turns per shadow analysis")
	fs.DurationVar(&shadow.Timeout, "agent-analysis-shadow-timeout", 10*time.Minute, "total timeout for one shadow analysis")
	fs.IntVar(&shadow.Retries, "agent-analysis-shadow-retries", 0, "Orka Task retries for shadow analysis")
	critic := &opts.CausalCritic
	fs.BoolVar(&critic.Enabled, "causal-critic-shadow", false, "run private sampled Agent Sandbox causal review after authoritative publication")
	fs.StringVar(&critic.LedgerPath, "causal-critic-shadow-ledger", "", "absolute private causal critic ledger path")
	fs.IntVar(&critic.MaxPerRun, "causal-critic-shadow-max-per-run", 1, "maximum sampled causal critic reviews per pass")
	fs.DurationVar(&critic.Timeout, "causal-critic-shadow-timeout", 5*time.Minute, "per-review Agent Sandbox timeout")
	fs.Int64Var(&critic.OutputLimitBytes, "causal-critic-shadow-output-limit-bytes", 64<<10, "maximum critic executor result bytes")
	fs.StringVar(&critic.ModelGateway.Endpoint, "causal-critic-shadow-gateway-endpoint", "", "internal HTTPS critic model gateway")
	fs.StringVar(&critic.ModelGateway.Model, "causal-critic-shadow-gateway-model", "", "critic gateway model id")
	fs.StringVar(&critic.ModelGateway.ProtocolVersion, "causal-critic-shadow-gateway-protocol", "openai-chat-completions-v1", "critic gateway protocol version")
	return values
}

// DecodePlacement decodes the JSON placement flags after parsing.
func (f *AnalysisRuntimeFlags) DecodePlacement(opts *Options) error {
	container := &opts.AnalysisRuntime.OrkaContainer
	if f.nodeSelectorJSON != "" {
		if err := json.Unmarshal([]byte(f.nodeSelectorJSON), &container.NodeSelector); err != nil {
			return fmt.Errorf("parse Orka analysis node selector: %w", err)
		}
	}
	if f.tolerationsJSON != "" {
		if err := json.Unmarshal([]byte(f.tolerationsJSON), &container.Tolerations); err != nil {
			return fmt.Errorf("parse Orka analysis tolerations: %w", err)
		}
	}
	if f.affinityJSON != "" {
		if err := json.Unmarshal([]byte(f.affinityJSON), &container.Affinity); err != nil {
			return fmt.Errorf("parse Orka analysis affinity: %w", err)
		}
	}
	return nil
}
