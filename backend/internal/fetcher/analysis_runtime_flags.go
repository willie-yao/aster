package fetcher

import (
	"flag"
	"time"
)

// BindAnalysisRuntimeFlags adds the analysis runtime flags used by fetcher and worker.
func BindAnalysisRuntimeFlags(fs *flag.FlagSet, opts *Options) {
	fs.StringVar(&opts.AnalysisRuntime.Type, "analysis-runtime", AnalysisRuntimeInProcess, "single-failure analysis runtime: inprocess")
	fs.IntVar(&opts.AIMaxOutputTokens, "ai-max-output-tokens", 0, "optional authoritative analysis output-token cap; zero uses the provider default")
	shadow := &opts.ShadowAnalysis
	fs.BoolVar(&shadow.Enabled, "agent-analysis-shadow", false, "run private experimental Agent analysis after authoritative publication")
	fs.StringVar(&shadow.LedgerPath, "agent-analysis-shadow-ledger", "", "private shadow comparison ledger path outside the public output directory")
	fs.StringVar(&shadow.InputRoot, "agent-analysis-shadow-input-root", "", "private content-addressed analyzer input root outside the public output directory")
	fs.IntVar(&shadow.MaxPerRun, "agent-analysis-shadow-max-per-run", 1, "maximum shadow analyses per refresh")
	fs.IntVar(&shadow.MaxSteps, "agent-analysis-shadow-max-steps", 20, "maximum OpenCode steps per shadow analysis")
	fs.DurationVar(&shadow.Timeout, "agent-analysis-shadow-timeout", 10*time.Minute, "total timeout for one shadow analysis")
	fs.Int64Var(&shadow.OutputLimitBytes, "agent-analysis-shadow-output-limit-bytes", 64<<10, "maximum shadow analysis result bytes")
	fs.IntVar(&shadow.ModelContextTokens, "agent-analysis-shadow-model-context-tokens", 0, "exact model context window used by the shadow analyzer")
	fs.IntVar(&shadow.ModelOutputTokens, "agent-analysis-shadow-model-output-tokens", 0, "exact model output limit used by the shadow analyzer")
	fs.BoolVar(&shadow.RequireSourceEvidence, "agent-analysis-shadow-require-source-evidence", true, "require source evidence before shadow finalization")
	fs.StringVar(&shadow.ModelProvider.CredentialMode, "agent-analysis-shadow-credential-mode", "", "shadow analysis model credential mode")
	fs.StringVar(&shadow.ModelProvider.API, "agent-analysis-shadow-provider-api", "", "shadow analysis model API protocol")
	fs.StringVar(&shadow.ModelProvider.Endpoint, "agent-analysis-shadow-provider-endpoint", "", "shadow analysis model endpoint")
	fs.StringVar(&shadow.ModelProvider.Model, "agent-analysis-shadow-provider-model", "", "shadow analysis model id")
	fs.StringVar((*string)(&shadow.ModelProvider.ReasoningEffort), "agent-analysis-shadow-provider-reasoning-effort", "", "shadow analysis model reasoning effort")
	fs.StringVar(&shadow.ModelProvider.Auth.Type, "agent-analysis-shadow-provider-auth-type", "", "shadow analysis model authentication type")
	critic := &opts.CausalCritic
	fs.BoolVar(&critic.Enabled, "causal-critic-shadow", false, "run private sampled Agent Sandbox causal review after authoritative publication")
	fs.StringVar(&critic.LedgerPath, "causal-critic-shadow-ledger", "", "absolute private causal critic ledger path")
	fs.IntVar(&critic.MaxPerRun, "causal-critic-shadow-max-per-run", 1, "maximum sampled causal critic reviews per pass")
	fs.DurationVar(&critic.Timeout, "causal-critic-shadow-timeout", 5*time.Minute, "per-review Agent Sandbox timeout")
	fs.Int64Var(&critic.OutputLimitBytes, "causal-critic-shadow-output-limit-bytes", 64<<10, "maximum critic executor result bytes")
	fs.StringVar(&critic.ModelGateway.Endpoint, "causal-critic-shadow-gateway-endpoint", "", "internal HTTPS critic model gateway")
	fs.StringVar(&critic.ModelGateway.Model, "causal-critic-shadow-gateway-model", "", "critic gateway model id")
	fs.StringVar(&critic.ModelGateway.ProtocolVersion, "causal-critic-shadow-gateway-protocol", "openai-chat-completions-v1", "critic gateway protocol version")
}
