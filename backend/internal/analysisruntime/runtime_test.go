package analysisruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestResolveContextBudgets(t *testing.T) {
	var modelCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s, want /v1/models", r.URL.Path)
		}
		modelCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"model","context_window":32768}]}`))
	}))
	defer srv.Close()
	client := ai.NewClientWithOptions(ai.Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "model", Token: "x"})

	t.Run("operator override wins without metadata request", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "128000")
		modelCalls.Store(0)
		budgets, source, err := resolveContextBudgets(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		if source != "operator" || budgets.ContextWindowTokens != 128000 {
			t.Fatalf("source=%q budgets=%+v", source, budgets)
		}
		if got := modelCalls.Load(); got != 0 {
			t.Fatalf("metadata calls=%d, want 0", got)
		}
	})

	t.Run("metadata used without override", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "")
		modelCalls.Store(0)
		budgets, source, err := resolveContextBudgets(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		if source != "detected" || budgets.ContextWindowTokens != 32768 {
			t.Fatalf("source=%q budgets=%+v", source, budgets)
		}
		if got := modelCalls.Load(); got != 1 {
			t.Fatalf("metadata calls=%d, want 1", got)
		}
	})

	t.Run("invalid override fails clearly", func(t *testing.T) {
		t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "not-a-number")
		if _, _, err := resolveContextBudgets(context.Background(), client); err == nil {
			t.Fatal("expected invalid override error")
		}
	})
}

func TestLoadProjectUsesCurrentToolSelectedSkills(t *testing.T) {
	dir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "project.yaml"), `id: test
name: Test
testgrid:
  dashboard: test
storage:
  provider: local
  base: /fixtures
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: project
ai:
  tools: [filesystem]
`)
	write(filepath.Join(dir, "prompts", "system.md"), "Investigate artifacts.\n")
	write(filepath.Join(dir, "skills", "consumer.yaml"), `id: consumer.recipe
triggers: ["boom"]
`)
	cfg, err := project.Load(filepath.Join(dir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProject(dir, cfg, ProviderFallbacks{
		API: "chat_completions", Endpoint: "https://model.invalid/v1/chat/completions", Model: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProfileSelection.Kubernetes {
		t.Fatal("filesystem-only project selected Kubernetes skills")
	}
	ids := map[string]bool{}
	for _, skill := range loaded.SkillSet.Skills() {
		ids[skill.ID] = true
	}
	if !ids["consumer.recipe"] || !ids["engine.prow.failure-evidence"] {
		t.Fatalf("loaded skill ids = %v", ids)
	}
	if ids["engine.kubernetes.machine-node-providerid"] {
		t.Fatal("filesystem-only project loaded Kubernetes skills")
	}
}

func TestAnalysisChatAgentTimeout(t *testing.T) {
	if got := analysisChatAgentTimeout(45*time.Minute, 10*time.Minute); got != 10*time.Minute {
		t.Fatalf("operator timeout = %v", got)
	}
	if got := analysisChatAgentTimeout(time.Minute, 0); got != time.Minute {
		t.Fatalf("short project timeout = %v", got)
	}
	if got := analysisChatAgentTimeout(45*time.Minute, 0); got != analysisChatDefaultTimeout {
		t.Fatalf("legacy default timeout = %v", got)
	}
}
