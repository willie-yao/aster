package main

import (
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/fetcher"
)

func TestParseOptionsDefaultsToInProcess(t *testing.T) {
	opts, _, _, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.AnalysisRuntime.Type != fetcher.AnalysisRuntimeInProcess {
		t.Fatalf("analysis runtime = %q", opts.AnalysisRuntime.Type)
	}
}

func TestParseOptionsAgentAnalysisShadow(t *testing.T) {
	opts, _, _, err := parseOptions([]string{
		"-ai", "-analysis-runtime=inprocess", "-agent-analysis-shadow",
		"-agent-analysis-shadow-agent-version=v2",
		"-agent-analysis-shadow-ledger=/private/analysis-shadow.json",
		"-agent-analysis-shadow-max-per-run=2",
		"-agent-analysis-shadow-max-turns=20",
		"-agent-analysis-shadow-timeout=8m",
		"-agent-analysis-shadow-retries=1",
		"-agent-analysis-shadow-output-limit-bytes=32768",
		"-agent-analysis-shadow-provider-endpoint=https://models.example.invalid/v1/chat/completions",
		"-agent-analysis-shadow-provider-model=shadow-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.ShadowAnalysis
	if !cfg.Enabled || cfg.AgentVersion != "v2" || cfg.LedgerPath != "/private/analysis-shadow.json" ||
		cfg.MaxPerRun != 2 || cfg.MaxTurns != 20 || cfg.Timeout != 8*time.Minute || cfg.Retries != 1 ||
		cfg.OutputLimitBytes != 32768 || cfg.ModelProvider.Endpoint != "https://models.example.invalid/v1/chat/completions" ||
		cfg.ModelProvider.Model != "shadow-model" {
		t.Fatalf("shadow options = %+v", cfg)
	}
}

func TestParseOptionsCausalCriticShadow(t *testing.T) {
	opts, _, _, err := parseOptions([]string{
		"-ai", "-analysis-runtime=inprocess", "-causal-critic-shadow",
		"-causal-critic-shadow-ledger=/private/critic.json",
		"-causal-critic-shadow-max-per-run=2",
		"-causal-critic-shadow-timeout=4m",
		"-causal-critic-shadow-output-limit-bytes=32768",
		"-causal-critic-shadow-gateway-endpoint=https://gateway.models.svc.cluster.local/v1",
		"-causal-critic-shadow-gateway-model=critic-model",
		"-causal-critic-shadow-gateway-protocol=openai-chat-completions-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.CausalCritic
	if !cfg.Enabled || cfg.LedgerPath != "/private/critic.json" || cfg.MaxPerRun != 2 || cfg.Timeout != 4*time.Minute ||
		cfg.OutputLimitBytes != 32768 || cfg.ModelGateway.Endpoint != "https://gateway.models.svc.cluster.local/v1" ||
		cfg.ModelGateway.Model != "critic-model" || cfg.ModelGateway.ProtocolVersion != "openai-chat-completions-v1" {
		t.Fatalf("critic options = %+v", cfg)
	}
}
