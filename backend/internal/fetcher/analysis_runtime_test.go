package fetcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func validContainerAnalysisOptions() Options {
	return Options{
		EnableAI: true,
		AnalysisRuntime: AnalysisRuntimeOptions{
			Type: AnalysisRuntimeOrkaContainer,
			OrkaContainer: OrkaContainerAnalysisOptions{
				Namespace: "orka-system", ResultAPI: "http://orka.orka-system.svc.cluster.local:8080", Image: "analyzer:sha-deadbeef",
				ModelSecretName: "model-secret", ModelTokenKey: "token",
				StateSecretName: "state-secret", StateSecretKey: "state-key",
				MaxConcurrent: 2, PollInterval: time.Second, TaskTimeout: time.Minute, Retries: 1,
				NodeSelector: map[string]string{"agentpool": "nodepool1"},
			},
		},
	}
}

func TestValidateAnalysisRuntimeOptions(t *testing.T) {
	t.Setenv("PROW_AI_STATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	if err := validateAnalysisRuntimeOptions(Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess}}); err != nil {
		t.Fatal(err)
	}
	if err := validateAnalysisRuntimeOptions(validContainerAnalysisOptions()); err != nil {
		t.Fatal(err)
	}
	unknown := Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: "remote"}}
	if err := validateAnalysisRuntimeOptions(unknown); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown runtime error = %v", err)
	}
	gpu := validContainerAnalysisOptions()
	gpu.AnalysisRuntime.OrkaContainer.NodeSelector = map[string]string{"agentpool": "h100"}
	if err := validateAnalysisRuntimeOptions(gpu); err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("GPU placement error = %v", err)
	}
	accelerator := validContainerAnalysisOptions()
	accelerator.AnalysisRuntime.OrkaContainer.NodeSelector["cloud.google.com/gke-accelerator"] = "nvidia-tesla-t4"
	if err := validateAnalysisRuntimeOptions(accelerator); err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("accelerator placement error = %v", err)
	}
	missing := validContainerAnalysisOptions()
	missing.AnalysisRuntime.OrkaContainer.ModelSecretName = ""
	if err := validateAnalysisRuntimeOptions(missing); err == nil || !strings.Contains(err.Error(), "model Secret") {
		t.Fatalf("missing model Secret error = %v", err)
	}
}

func TestValidateContainerAnalysisStateKey(t *testing.T) {
	t.Setenv("PROW_AI_STATE_KEY", "not-base64")
	err := validateAnalysisRuntimeOptions(validContainerAnalysisOptions())
	if err == nil || !strings.Contains(err.Error(), "state key") {
		t.Fatalf("state key error = %v", err)
	}
}

func TestRunWatchRejectsContainerAnalysis(t *testing.T) {
	err := RunWatch(t.Context(), validContainerAnalysisOptions(), time.Second, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "does not support watch mode") {
		t.Fatalf("RunWatch error = %v", err)
	}
}

func TestSetupPipelineContainerUsesHelmProviderCoordinates(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: project
ai:
  api: responses
  endpoint: https://project.invalid/v1/responses
  model: project-model
  tools: [filesystem]
`, dir)
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_TOKEN", "dashboard-token")
	t.Setenv("AI_API", "chat_completions")
	t.Setenv("AI_ENDPOINT", "https://helm.invalid/v1/chat/completions")
	t.Setenv("AI_MODEL", "helm-model")
	t.Setenv("PROW_AI_STATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	opts := validContainerAnalysisOptions()
	opts.ProjectDir = dir
	opts.OutDir = t.TempDir()
	opts.BuildsPerJob = 1
	opts.Workers = 1
	opts.Timeout = time.Minute
	pipeline, err := setupPipeline(opts)
	if err != nil {
		t.Fatal(err)
	}
	provider := pipeline.aiProject.Provider
	if provider.API != "chat_completions" || provider.Endpoint != "https://helm.invalid/v1/chat/completions" || provider.Model != "helm-model" {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestWarnOnAnalysisPersistenceIsBestEffort(t *testing.T) {
	called := 0
	warnOnAnalysisPersistence("test state", func() error {
		called++
		return errors.New("disk full")
	})
	if called != 1 {
		t.Fatalf("save calls = %d, want 1", called)
	}
}

type maintenanceOnlyAnalyzer struct {
	called bool
}

func (a *maintenanceOnlyAnalyzer) Maintain(context.Context) error {
	a.called = true
	return nil
}

func (a *maintenanceOnlyAnalyzer) AnalyzeFailure(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	return ai.FailureAnalysisResult{}, nil
}

func (a *maintenanceOnlyAnalyzer) StateStore() *analysisruntime.ContainerStateStore { return nil }

func TestAnalyzeFailuresNoWorkStillRunsContainerMaintenance(t *testing.T) {
	analyzer := &maintenanceOnlyAnalyzer{}
	pipeline := &pipeline{
		opts:              Options{AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeOrkaContainer}},
		containerAnalyzer: analyzer,
	}
	if err := pipeline.analyzeFailuresWithAI(t.Context(), nil, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}
	if !analyzer.called {
		t.Fatal("container maintenance was skipped")
	}
}

func TestPassExecutionContextsDetachDurableContainerWork(t *testing.T) {
	root := context.Background()
	bounded, cancel := context.WithTimeout(root, time.Second)
	defer cancel()
	analysis, sideEffects := passExecutionContexts(root, bounded, AnalysisRuntimeOrkaContainer)
	if _, ok := analysis.Deadline(); ok {
		t.Fatal("container analysis inherited the fetch deadline")
	}
	if _, ok := sideEffects.Deadline(); ok {
		t.Fatal("container side effects inherited the fetch deadline")
	}
	analysis, sideEffects = passExecutionContexts(root, bounded, AnalysisRuntimeInProcess)
	if _, ok := analysis.Deadline(); !ok {
		t.Fatal("in-process analysis lost the pass deadline")
	}
	if _, ok := sideEffects.Deadline(); !ok {
		t.Fatal("in-process side effects lost the pass deadline")
	}
}
