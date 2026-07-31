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
