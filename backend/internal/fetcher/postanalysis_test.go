package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

func TestRunFinalizedSideEffectsLoadsFinalizedOutput(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	storageDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + storageDir + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: repo
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "jobs", models.JobDataFilename("job")), models.JobDetail{JobID: "job", Name: "Job"}); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	if err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFinalizedDataRejectsMalformedJob(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "jobs", "bad.json"), []byte(`{"job_id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadFinalizedData(dataDir)
	if err == nil || !strings.Contains(err.Error(), "parse finalized job") {
		t.Fatalf("error = %v, want malformed job error", err)
	}
}

type finalizedFakePR struct{}

func (finalizedFakePR) OpenPR(context.Context, ghpr.Request) (string, error) {
	return "", nil
}
func (finalizedFakePR) SearchOpenPR(context.Context, string, string, string, string) (int, string, bool, error) {
	return 0, "", false, nil
}
func (finalizedFakePR) ResolveBase(context.Context, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "base-sha", TreeSHA: "tree-sha"}, nil
}

type finalizedFakeAgent struct{}

func (finalizedFakeAgent) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{
		Files: map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff:  "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n",
	}, nil
}

func TestRunFinalizedSideEffectsProducesFixPreview(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	storageDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + storageDir + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo: {owner: example, name: repo}
ai:
  fix_prs:
    enabled: true
    author_name: Test
    author_email: test@example.com
    dry_run: true
    critique_retries: 0
    agent_runtime:
      type: orka
      agent_ref: test-fixer
      api: http://orka.test
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{
		RecurringPatterns: []models.PatternAnalysis{{
			ID: "pattern", Subject: "job", Systemic: true, Confidence: "high",
			SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml", Summary: "recurring",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIX_TOKEN", "test-token")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return finalizedFakeAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() {
		newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager
	})

	if err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}
	var previews []fixpr.Preview
	data, err := os.ReadFile(filepath.Join(dataDir, "fix_previews.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &previews); err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || !strings.Contains(previews[0].Diff, "fixed: true") {
		t.Fatalf("previews = %+v", previews)
	}
}

type failingFinalizedAgent struct{}

func (failingFinalizedAgent) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{}, errors.New("generation failed")
}

func TestProcessFixPRsPropagatesPatternFailure(t *testing.T) {
	projectDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + t.TempDir() + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo: {owner: example, name: repo}
ai:
  fix_prs:
    enabled: true
    author_name: Test
    author_email: test@example.com
    dry_run: true
    critique_retries: 0
    agent_runtime:
      type: orka
      agent_ref: test-fixer
      api: http://orka.test
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIX_TOKEN", "test-token")
	oldRuntime, oldManager := newBatchFixRuntime, newBatchFixManager
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) { return failingFinalizedAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(finalizedFakePR{}, stateFile, opts)
	}
	t.Cleanup(func() {
		newBatchFixRuntime, newBatchFixManager = oldRuntime, oldManager
	})

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{
		RecurringPatterns: []models.PatternAnalysis{{
			ID: "pattern", Subject: "job", Systemic: true, Confidence: "high",
			SharedRootCause: "configuration is stale", SuggestedFix: "update config/fix.yaml",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "generation failed") {
		t.Fatalf("error = %v, want per-pattern generation failure", err)
	}
}
