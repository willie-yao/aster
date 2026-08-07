package onboard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConsumerSetupAgentSkill(t *testing.T) {
	root := onboardingRepoRoot(t)
	skillPath := filepath.Join(root, ".agents", "skills", "setup-prow-ai-consumer", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "TODO") {
		t.Fatalf("skill contains a TODO: %s", skillPath)
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[0]) != "" {
		t.Fatalf("skill frontmatter is malformed: %s", skillPath)
	}
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil {
		t.Fatalf("skill frontmatter: %v", err)
	}
	if metadata.Name != "setup-prow-ai-consumer" || strings.TrimSpace(metadata.Description) == "" {
		t.Fatalf("skill metadata = %+v", metadata)
	}
	for _, anchor := range []string{
		"go -C backend run ./cmd/fetcher", "onboard discover", "-json", "-dry-run", "-non-interactive",
		"-prompt-mode handoff", "-plan-out", "-apply-plan", "-plan-digest",
		"PROMPT_HANDOFF.md", "onboard doctor", "After the user confirms the reviewed plan",
		"Never delete stale or unrelated files",
	} {
		if !strings.Contains(text, anchor) {
			t.Errorf("skill missing %q", anchor)
		}
	}
	for _, forbidden := range []string{"api-experimental", "rm -rf", "--no-verify"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("skill contains forbidden text %q", forbidden)
		}
	}

	openAIPath := filepath.Join(filepath.Dir(skillPath), "agents", "openai.yaml")
	openAIRaw, err := os.ReadFile(openAIPath)
	if err != nil {
		t.Fatal(err)
	}
	var openAI struct {
		Interface struct {
			DisplayName      string `yaml:"display_name"`
			ShortDescription string `yaml:"short_description"`
			DefaultPrompt    string `yaml:"default_prompt"`
		} `yaml:"interface"`
	}
	if err := yaml.Unmarshal(openAIRaw, &openAI); err != nil {
		t.Fatalf("openai.yaml: %v", err)
	}
	if openAI.Interface.DisplayName == "" || len(openAI.Interface.ShortDescription) < 25 || len(openAI.Interface.ShortDescription) > 64 {
		t.Fatalf("openai interface metadata = %+v", openAI.Interface)
	}
	if !strings.Contains(openAI.Interface.DefaultPrompt, "$setup-prow-ai-consumer") {
		t.Fatalf("default prompt does not invoke the skill: %q", openAI.Interface.DefaultPrompt)
	}

	referencePath := filepath.Join(filepath.Dir(skillPath), "references", "decisions.md")
	reference, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{"## Placement", "## Deployment", "## Discovery", "## Write boundaries"} {
		if !strings.Contains(string(reference), anchor) {
			t.Errorf("decision reference missing %q", anchor)
		}
	}
}

func TestDiagnosticAuthoringAgentSkill(t *testing.T) {
	root := onboardingRepoRoot(t)
	skillDir := filepath.Join(root, ".agents", "skills", "author-prow-ai-diagnostics")
	skillPath := filepath.Join(skillDir, "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "TODO") {
		t.Fatalf("skill contains a TODO: %s", skillPath)
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[0]) != "" {
		t.Fatalf("skill frontmatter is malformed: %s", skillPath)
	}
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil {
		t.Fatalf("skill frontmatter: %v", err)
	}
	if metadata.Name != "author-prow-ai-diagnostics" || strings.TrimSpace(metadata.Description) == "" {
		t.Fatalf("skill metadata = %+v", metadata)
	}

	for _, heading := range []string{
		"## Architecture",
		"## Diagnostic lifecycle",
		"## Test and job flavors",
		"## Artifact layout",
		"## Common failure patterns",
		"## Transient classification",
		"## Triage order",
		"## Relevant source repositories",
		"## Unresolved details",
	} {
		if count := strings.Count(text, heading); count != 1 {
			t.Errorf("skill heading %q count = %d, want 1", heading, count)
		}
	}
	for _, anchor := range []string{
		"onboard doctor", "<validation-engine>", "pinned engine checkout", "proposals/skills/", "reports/failure-corpus.json", "reports/diagnostic-authoring.md",
		"reports/benchmark-results.json", "python3 -m json.tool", "fresh isolated LLM CLI", "private source or artifacts to a provider", "repeated prompt-only misses", "final holdout",
		"recommended", "experimental", "rejected", "unresolved",
		"Never write them to the authoring consumer's active `skills/`", "without a later explicit approval",
	} {
		if !strings.Contains(text, anchor) {
			t.Errorf("skill missing %q", anchor)
		}
	}
	for _, forbidden := range []string{"rm -rf", "az ", "kubectl ", "skills/auto"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("skill contains forbidden text %q", forbidden)
		}
	}

	openAIRaw, err := os.ReadFile(filepath.Join(skillDir, "agents", "openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var openAI struct {
		Interface struct {
			DisplayName      string `yaml:"display_name"`
			ShortDescription string `yaml:"short_description"`
			DefaultPrompt    string `yaml:"default_prompt"`
		} `yaml:"interface"`
	}
	if err := yaml.Unmarshal(openAIRaw, &openAI); err != nil {
		t.Fatalf("openai.yaml: %v", err)
	}
	if openAI.Interface.DisplayName == "" || len(openAI.Interface.ShortDescription) < 25 || len(openAI.Interface.ShortDescription) > 64 {
		t.Fatalf("openai interface metadata = %+v", openAI.Interface)
	}
	if !strings.Contains(openAI.Interface.DefaultPrompt, "$author-prow-ai-diagnostics") || !strings.Contains(openAI.Interface.DefaultPrompt, "representative failure corpus") || !strings.Contains(openAI.Interface.DefaultPrompt, "prompt-only misses") || !strings.Contains(openAI.Interface.DefaultPrompt, "without activating") {
		t.Fatalf("default prompt does not preserve the skill boundary: %q", openAI.Interface.DefaultPrompt)
	}

	references := map[string][]string{
		"failure-corpus.md": {"## Build a representative corpus", "At least six diagnosed failures", "authorized Prow artifact indexes", "same-wrapper, different-cause counterexample", "## Diagnose each failure", "reports/failure-corpus.json", `"split": "authoring"`, "## Validate the prompt with fresh sessions", "two bounded revision rounds", "## Apply the prompt quality rubric"},
		"decisions.md":      {"## Choose the corpus and splits", "prompt-only validation cases", "## Classify prompt and recipe outcomes", "## Promotion boundary"},
		"recipe-authoring.md": {
			"second-line intervention", "at least two independent prompt-only misses", "## Design triggers", "## Build the applicability matrix", "## Run deterministic engine validation",
			"bounded failure signal", "final-draft-only trigger", "positive successful-operation evidence", "competing initiating cause", "slash-normalizes and lowercases", "Doctor does not validate recipe YAML", "ParseAndValidate",
		},
		"benchmarking.md": {"## Protect the blind boundary", "## Record prompt-authoring validation", "authoring_validation", "## Validate derived manifests without a provider", "validateBenchmarkProjectDir", "## Run the benchmark matrix", "## Write deterministic results", "BENCH_REPETITIONS=3", "quality floors failed"},
	}
	for name, anchors := range references {
		raw, err := os.ReadFile(filepath.Join(skillDir, "references", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, anchor := range anchors {
			if !strings.Contains(string(raw), anchor) {
				t.Errorf("%s missing %q", name, anchor)
			}
		}
	}
}

func onboardingRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func TestAgentOnboardingDocsAdvertiseInstallableSkills(t *testing.T) {
	root := onboardingRepoRoot(t)
	checks := map[string][]string{
		"README.md": {
			"$setup-prow-ai-consumer",
			"docs/agent-onboarding.md",
		},
		"docs/onboarding-a-new-project.md": {
			"npx --yes skills@latest add willie-yao/prow-ai-dashboard",
			"Use $setup-prow-ai-consumer to set up a Pages consumer",
			"$author-prow-ai-diagnostics",
		},
		"docs/agent-onboarding.md": {
			"--skill setup-prow-ai-consumer author-prow-ai-diagnostics",
			"--agent codex",
			"--global",
			"Use $setup-prow-ai-consumer",
			"Use $author-prow-ai-diagnostics",
			"npx --yes skills@latest update",
		},
	}
	for name, anchors := range checks {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, anchor := range anchors {
			if !strings.Contains(string(raw), anchor) {
				t.Errorf("%s missing %q", name, anchor)
			}
		}
	}
}
