package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseOptionsRejectsRemovedAnalysisRuntime(t *testing.T) {
	_, _, _, err := parseOptions([]string{"-analysis-runtime=inprocess"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("removed analysis runtime flag error = %v", err)
	}
}

func TestParseOptionsAgentAnalysisShadow(t *testing.T) {
	opts, _, _, err := parseOptions([]string{
		"-ai", "-agent-analysis-shadow",
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
