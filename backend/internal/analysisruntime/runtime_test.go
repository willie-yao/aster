package analysisruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/project"
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
discovery:
  testgrid_dashboard: test
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
  cache_generation: "1"
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
		API: "chat_completions", Endpoint: "https://model.invalid/v1/chat/completions", Model: "model", ReasoningEffort: string(ai.ReasoningEffortHigh), CacheGeneration: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CacheGeneration != "2" || loaded.CacheGenerationFingerprint != project.AICacheGenerationFingerprint("2") || loaded.Provider.ReasoningEffort != ai.ReasoningEffortHigh {
		t.Fatalf("cache generation = %q fingerprint=%q", loaded.CacheGeneration, loaded.CacheGenerationFingerprint)
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
	if _, err := LoadProject(dir, cfg, ProviderFallbacks{
		Endpoint: "https://model.invalid/v1/chat/completions", Model: "model", ReasoningEffort: "ultra",
	}); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("invalid reasoning effort error = %v", err)
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

func TestLoadProjectRequiresConsumerSkillCount(t *testing.T) {
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
discovery:
  testgrid_dashboard: test
storage:
  provider: local
  base: /fixtures
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo: {owner: branding, name: repo}
ai:
  source_repo: {owner: analysis, name: source}
  consumer_skills:
    required: true
    minimum_count: 2
  tools: [filesystem]
`)
	write(filepath.Join(dir, "prompts", "system.md"), "Investigate artifacts.\n")
	write(filepath.Join(dir, "skills", "one.yaml"), "id: consumer.one\ntriggers: [boom]\n")
	cfg, err := project.Load(filepath.Join(dir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(dir, cfg, ProviderFallbacks{Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"})
	if err == nil || !strings.Contains(err.Error(), "count 1") || !strings.Contains(err.Error(), "minimum 2") {
		t.Fatalf("consumer skill requirement error = %v", err)
	}
	write(filepath.Join(dir, "skills", "two.yaml"), "id: consumer.two\ntriggers: [bang]\n")
	loaded, err := LoadProject(dir, cfg, ProviderFallbacks{Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AnalysisSource.Owner != "analysis" || loaded.AnalysisSource.Name != "source" {
		t.Fatalf("analysis source = %+v", loaded.AnalysisSource)
	}
	manifest := loaded.Config.AI.SkillBundle
	if manifest == nil || manifest.EngineCount != 3 || manifest.ConsumerCount != 2 || !manifest.ConsumerBundlePresent || manifest.Hash == "" {
		t.Fatalf("public skill metadata = %+v", manifest)
	}
	if len(manifest.Profiles) != 1 || manifest.Profiles[0] != "prow" {
		t.Fatalf("profiles = %v", manifest.Profiles)
	}
}

func TestLoadProjectRequiredConsumerBundleMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `id: test
name: Test
discovery: {testgrid_dashboard: test}
storage: {provider: local, base: /fixtures}
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo: {owner: branding, name: repo}
ai:
  consumer_skills: {required: true}
  tools: [filesystem]
`
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := project.Load(filepath.Join(dir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(dir, cfg, ProviderFallbacks{Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"})
	if err == nil || !strings.Contains(err.Error(), "bundle is required") {
		t.Fatalf("missing bundle error = %v", err)
	}
}

func TestNewRejectsInvalidReasoningEffortBeforeProviderIO(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	analysisProject := &Project{
		Config: &project.Config{AI: &project.AI{}},
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: server.URL + "/v1/chat/completions", Model: "model", ReasoningEffort: "invalid",
		},
	}
	if _, err := New(t.Context(), Options{Project: analysisProject, Token: "token"}); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("provider requests = %d, want 0", requests.Load())
	}
}

// TestReusePlannerMatchesAnalyzerPromptIdentity keeps scheduling and publication
// on one prompt contract. The planner decides reuse by comparing prompt hashes,
// so a planner configured differently from the analyzing service would treat
// every published analysis as stale and reanalyze the whole corpus.
func TestReusePlannerMatchesAnalyzerPromptIdentity(t *testing.T) {
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
discovery:
  testgrid_dashboard: test
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

	planner := NewReusePlanner(loaded)
	if planner == nil {
		t.Fatal("planner was not created")
	}
	sourceRepo := loaded.AnalysisSource
	if sourceRepo.Owner == "" || sourceRepo.Name == "" {
		sourceRepo = cfg.EffectiveAnalysisSourceRepo()
	}
	if sourceRepo.Owner == "" || sourceRepo.Name == "" {
		t.Fatal("test project resolved no analysis source repository")
	}
	owner, name := planner.SourceRepo()
	if owner != sourceRepo.Owner || name != sourceRepo.Name {
		t.Fatalf("planner source repo = %s/%s, want %s/%s", owner, name, sourceRepo.Owner, sourceRepo.Name)
	}
}

func TestRuntimeAppliesOptionalComparisonOutputLimit(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "200000")
	configured := &Project{
		Config:   &project.Config{AI: &project.AI{}},
		Provider: project.AIProvider{API: ai.APIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "model"},
	}
	limited, err := New(t.Context(), Options{Project: configured, MaxOutputTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if limited.MaxOutputTokens != 8192 {
		t.Fatalf("max output tokens = %d", limited.MaxOutputTokens)
	}
	unlimited, err := New(t.Context(), Options{Project: configured})
	if err != nil {
		t.Fatal(err)
	}
	if unlimited.MaxOutputTokens != 0 {
		t.Fatalf("default max output tokens = %d", unlimited.MaxOutputTokens)
	}
}
