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
		"-agent-analysis-shadow-ledger=/private/analysis-shadow.json",
		"-agent-analysis-shadow-input-root=/private/input",
		"-agent-analysis-shadow-max-per-run=2",
		"-agent-analysis-shadow-max-steps=20",
		"-agent-analysis-shadow-timeout=8m",
		"-agent-analysis-shadow-output-limit-bytes=32768",
		"-agent-analysis-shadow-model-context-tokens=200000",
		"-agent-analysis-shadow-model-output-tokens=8192",
		"-agent-analysis-shadow-credential-mode=direct",
		"-agent-analysis-shadow-provider-api=chat_completions",
		"-agent-analysis-shadow-provider-endpoint=https://models.example.invalid/v1/chat/completions",
		"-agent-analysis-shadow-provider-model=shadow-model",
		"-agent-analysis-shadow-provider-reasoning-effort=high",
		"-agent-analysis-shadow-provider-auth-type=bearer",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.ShadowAnalysis
	if !cfg.Enabled || cfg.LedgerPath != "/private/analysis-shadow.json" || cfg.InputRoot != "/private/input" ||
		cfg.MaxPerRun != 2 || cfg.MaxSteps != 20 || cfg.Timeout != 8*time.Minute ||
		cfg.OutputLimitBytes != 32768 || cfg.ModelProvider.Endpoint != "https://models.example.invalid/v1/chat/completions" ||
		cfg.ModelProvider.Model != "shadow-model" || cfg.ModelContextTokens != 200000 || cfg.ModelOutputTokens != 8192 ||
		cfg.ModelProvider.ReasoningEffort != "high" || cfg.ModelProvider.Auth.Type != "bearer" {
		t.Fatalf("shadow options = %+v", cfg)
	}
}
