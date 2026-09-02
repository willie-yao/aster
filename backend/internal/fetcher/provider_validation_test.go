package fetcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupPipelineRejectsGPT54ChatReasoningBeforeStorageSetup(t *testing.T) {
	projectDir := t.TempDir()
	storageDir := filepath.Join(t.TempDir(), "missing-storage")
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  testgrid_dashboard: test
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: repo
ai:
  tools: [filesystem]
`, storageDir)
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_TOKEN", "fixture")
	t.Setenv("AI_API", "chat_completions")
	t.Setenv("AI_ENDPOINT", "https://model.invalid/v1/chat/completions")
	t.Setenv("AI_MODEL", "openai/gpt-5.4")
	t.Setenv("AI_REASONING_EFFORT", "high")
	_, err := setupPipeline(Options{ProjectDir: projectDir, OutDir: t.TempDir(), EnableAI: true})
	if err == nil || !strings.Contains(err.Error(), "set reasoning effort to none or use responses") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(storageDir); !os.IsNotExist(err) {
		t.Fatalf("storage setup ran before validation: %v", err)
	}
}
