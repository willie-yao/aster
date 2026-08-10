package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
)

func TestParseOptionsOrkaAnalysis(t *testing.T) {
	opts, watchInterval, reconcileInterval, err := parseOptions([]string{
		"-project-dir=/config", "-out=/data", "-builds=11", "-workers=4", "-timeout=42m",
		"-watch-interval=5m", "-reconcile-interval=1h", "-include-presubmits", "-ai",
		"-analysis-runtime=orka-container",
		"-orka-analysis-namespace=dashboard-analysis",
		"-orka-analysis-api=http://orka.orka-system.svc.cluster.local:8080",
		"-orka-analysis-image=analyzer:sha-deadbeef",
		"-orka-analysis-model-secret=model-secret",
		"-orka-analysis-model-token-key=model-token",
		"-orka-analysis-state-secret=state-secret",
		"-orka-analysis-state-key=state-token",
		"-orka-analysis-max-concurrent-tasks=3",
		"-orka-analysis-poll-interval=1500ms",
		"-orka-analysis-task-timeout=55m",
		"-orka-analysis-retries=2",
		`-orka-analysis-node-selector-json={"agentpool":"cpu"}`,
		`-orka-analysis-tolerations-json=[{"key":"workload","operator":"Equal","value":"analysis"}]`,
		`-orka-analysis-affinity-json={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ProjectDir != "/config" || opts.OutDir != "/data" || opts.BuildsPerJob != 11 || opts.Workers != 4 || opts.Timeout != 42*time.Minute {
		t.Fatalf("base options = %+v", opts)
	}
	if watchInterval != 5*time.Minute || reconcileInterval != time.Hour || !opts.IncludePresubmits || !opts.EnableAI {
		t.Fatalf("watch options: watch=%s reconcile=%s presubmits=%v ai=%v", watchInterval, reconcileInterval, opts.IncludePresubmits, opts.EnableAI)
	}
	cfg := opts.AnalysisRuntime.OrkaContainer
	if opts.AnalysisRuntime.Type != fetcher.AnalysisRuntimeOrkaContainer ||
		cfg.Namespace != "dashboard-analysis" || cfg.ResultAPI != "http://orka.orka-system.svc.cluster.local:8080" ||
		cfg.Image != "analyzer:sha-deadbeef" || cfg.ModelSecretName != "model-secret" || cfg.ModelTokenKey != "model-token" ||
		cfg.StateSecretName != "state-secret" || cfg.StateSecretKey != "state-token" || cfg.MaxConcurrent != 3 ||
		cfg.PollInterval != 1500*time.Millisecond || cfg.TaskTimeout != 55*time.Minute || cfg.Retries != 2 {
		t.Fatalf("Orka options = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.NodeSelector, map[string]string{"agentpool": "cpu"}) {
		t.Fatalf("node selector = %#v", cfg.NodeSelector)
	}
	if len(cfg.Tolerations) != 1 || cfg.Tolerations[0]["key"] != "workload" || cfg.Tolerations[0]["value"] != "analysis" {
		t.Fatalf("tolerations = %#v", cfg.Tolerations)
	}
	if _, ok := cfg.Affinity["nodeAffinity"]; !ok {
		t.Fatalf("affinity = %#v", cfg.Affinity)
	}
}

func TestParseOptionsDefaultsToInProcess(t *testing.T) {
	opts, _, _, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.AnalysisRuntime.Type != fetcher.AnalysisRuntimeInProcess {
		t.Fatalf("analysis runtime = %q", opts.AnalysisRuntime.Type)
	}
}

func TestParseOptionsRejectsInvalidPlacement(t *testing.T) {
	if _, _, _, err := parseOptions([]string{"-orka-analysis-node-selector-json={"}); err == nil {
		t.Fatal("invalid placement JSON was accepted")
	}
}

func TestParseOptionsAgentAnalysisShadow(t *testing.T) {
	opts, _, _, err := parseOptions([]string{
		"-ai", "-analysis-runtime=inprocess", "-agent-analysis-shadow",
		"-agent-analysis-shadow-namespace=orka-system",
		"-agent-analysis-shadow-api=https://orka.example.invalid",
		"-agent-analysis-shadow-agent-ref=analysis-agent",
		"-agent-analysis-shadow-agent-version=v2",
		"-agent-analysis-shadow-git-secret=source-readonly",
		"-agent-analysis-shadow-kube-context=kind-shadow",
		"-agent-analysis-shadow-ledger=/private/analysis-shadow.json",
		"-agent-analysis-shadow-max-per-run=2",
		"-agent-analysis-shadow-max-turns=20",
		"-agent-analysis-shadow-timeout=8m",
		"-agent-analysis-shadow-retries=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.ShadowAnalysis
	if !cfg.Enabled || cfg.Namespace != "orka-system" || cfg.ResultAPI != "https://orka.example.invalid" ||
		cfg.AgentRef != "analysis-agent" || cfg.AgentVersion != "v2" || cfg.GitSecret != "source-readonly" ||
		cfg.KubeContext != "kind-shadow" || cfg.LedgerPath != "/private/analysis-shadow.json" ||
		cfg.MaxPerRun != 2 || cfg.MaxTurns != 20 || cfg.Timeout != 8*time.Minute || cfg.Retries != 1 {
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
