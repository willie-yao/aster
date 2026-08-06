package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type benchmarkRunIdentity struct {
	Arm                    string
	EngineCommit           string
	BaselineConsumerCommit string
	FixtureSHA256          string
	ProjectSHA256          string
	EffectivePromptSHA256  string
	SkillSetHash           string
	EffectiveInputSHA256   string
	APIMode                string
}

type benchmarkInputs struct {
	systemPrompt    string
	agentic         project.Agentic
	cacheGeneration string
	projectSkills   *skills.Set
	identity        benchmarkRunIdentity
}

func loadBenchmarkInputs(t *testing.T, cases []benchCase, apiMode string) benchmarkInputs {
	t.Helper()
	variantDir := strings.TrimSpace(os.Getenv("BENCH_VARIANT_DIR"))
	arm, err := benchmarkArm(variantDir != "")
	if err != nil {
		t.Fatal(err)
	}

	out := benchmarkInputs{
		systemPrompt: ComposeBenchPrompt(),
		agentic:      defaultBenchAgentic(),
		identity: benchmarkRunIdentity{
			Arm:          arm,
			EngineCommit: benchmarkEngineCommit(t),
			APIMode:      apiMode,
		},
	}
	skillProjectDir := t.TempDir()
	baseDir := strings.TrimSpace(os.Getenv("BENCH_PROJECT_DIR"))
	if variantDir != "" && baseDir == "" {
		t.Fatal("BENCH_VARIANT_DIR requires BENCH_PROJECT_DIR")
	}
	if baseDir != "" {
		if len(cases) != 1 {
			for _, bc := range cases {
				if bc.consumerCommit != "" {
					t.Fatal("BENCH_PROJECT_DIR with pinned external consumers requires BENCH_CASE to select exactly one case")
				}
			}
		}
		if len(cases) == 1 && cases[0].consumerCommit != "" {
			if err := validateBenchmarkProjectDir(baseDir, cases[0]); err != nil {
				t.Fatalf("BENCH_PROJECT_DIR=%s: %v", baseDir, err)
			}
			out.identity.BaselineConsumerCommit = cases[0].consumerCommit
			out.identity.FixtureSHA256 = cases[0].fixtureSHA256
		}
		effectiveDir := baseDir
		if variantDir != "" {
			if err := validateBenchmarkVariantDir(baseDir, variantDir); err != nil {
				t.Fatalf("BENCH_VARIANT_DIR=%s: %v", variantDir, err)
			}
			effectiveDir = variantDir
		}
		cfg, prompt, err := project.LoadDir(effectiveDir)
		if err != nil {
			t.Fatalf("load benchmark consumer %s: %v", effectiveDir, err)
		}
		out.systemPrompt = ai.ComposeSystemPrompt(prompt)
		out.agentic = cfg.AI.EffectiveAgentic()
		configuredCacheGeneration := cfg.AI.CacheGeneration
		out.cacheGeneration, err = benchmarkCacheGenerationFingerprint(configuredCacheGeneration)
		if err != nil {
			t.Fatalf("benchmark cache generation: %v", err)
		}
		skillProjectDir = effectiveDir
		projectData, err := os.ReadFile(filepath.Join(effectiveDir, "project.yaml"))
		if err != nil {
			t.Fatalf("read benchmark project identity: %v", err)
		}
		out.identity.ProjectSHA256 = sha256Hex(projectData)
	} else {
		for _, bc := range cases {
			if bc.consumerCommit != "" {
				t.Fatal("pinned external benchmark cases require BENCH_CASE and BENCH_PROJECT_DIR")
			}
		}
		if len(cases) == 1 {
			out.identity.FixtureSHA256 = cases[0].fixtureSHA256
		}
	}

	if out.cacheGeneration == "" {
		out.cacheGeneration, err = benchmarkCacheGenerationFingerprint("")
		if err != nil {
			t.Fatalf("benchmark cache generation: %v", err)
		}
	}
	projectSkills, _, err := skills.LoadForTools(skillProjectDir, out.agentic.Tools)
	if err != nil {
		t.Fatalf("load benchmark skills: %v", err)
	}
	out.projectSkills = projectSkills
	out.identity.EffectivePromptSHA256 = sha256Hex([]byte(out.systemPrompt))
	out.identity.SkillSetHash = projectSkills.Hash()
	out.identity.EffectiveInputSHA256 = benchmarkEffectiveInputSHA256(out.identity, out.agentic, out.cacheGeneration)
	if err := validateBenchmarkRunIdentity(out.identity); err != nil {
		t.Fatal(err)
	}
	return out
}

func validateBenchmarkRunIdentity(identity benchmarkRunIdentity) error {
	if !benchmarkCaseIDRE.MatchString(identity.Arm) {
		return fmt.Errorf("benchmark arm is invalid")
	}
	if !benchmarkCommitRE.MatchString(identity.EngineCommit) {
		return fmt.Errorf("benchmark engine commit is invalid")
	}
	for name, value := range map[string]string{
		"fixture SHA-256":          identity.FixtureSHA256,
		"project SHA-256":          identity.ProjectSHA256,
		"effective prompt SHA-256": identity.EffectivePromptSHA256,
		"skill-set hash":           identity.SkillSetHash,
		"effective input SHA-256":  identity.EffectiveInputSHA256,
	} {
		if value != "" && !benchmarkSHA256RE.MatchString(value) {
			return fmt.Errorf("benchmark %s is invalid", name)
		}
	}
	if !benchmarkSHA256RE.MatchString(identity.EffectivePromptSHA256) || !benchmarkSHA256RE.MatchString(identity.SkillSetHash) || !benchmarkSHA256RE.MatchString(identity.EffectiveInputSHA256) {
		return fmt.Errorf("benchmark effective input identity is incomplete")
	}
	if identity.BaselineConsumerCommit != "" && !benchmarkCommitRE.MatchString(identity.BaselineConsumerCommit) {
		return fmt.Errorf("benchmark baseline consumer commit is invalid")
	}
	if identity.APIMode != ai.APIChatCompletions && identity.APIMode != ai.APIResponses {
		return fmt.Errorf("benchmark API mode is invalid")
	}
	return nil
}

func benchmarkArm(variant bool) (string, error) {
	arm := strings.TrimSpace(os.Getenv("BENCH_ARM"))
	if arm == "" {
		if variant {
			return "", fmt.Errorf("BENCH_ARM is required when BENCH_VARIANT_DIR is set")
		}
		return "baseline", nil
	}
	if !benchmarkCaseIDRE.MatchString(arm) {
		return "", fmt.Errorf("BENCH_ARM must match %s", benchmarkCaseIDRE.String())
	}
	return arm, nil
}

func validateBenchmarkVariantDir(baseDir, variantDir string) error {
	baseProject, err := os.ReadFile(filepath.Join(baseDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("read baseline project.yaml: %w", err)
	}
	variantProject, err := os.ReadFile(filepath.Join(variantDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("read variant project.yaml: %w", err)
	}
	if sha256Hex(baseProject) != sha256Hex(variantProject) {
		return fmt.Errorf("variant project.yaml must be byte-identical to the pinned baseline")
	}
	return nil
}

func benchmarkEffectiveInputSHA256(identity benchmarkRunIdentity, agentic project.Agentic, cacheGeneration string) string {
	data, err := json.Marshal(struct {
		ProjectSHA256         string          `json:"project_sha256,omitempty"`
		EffectivePromptSHA256 string          `json:"effective_prompt_sha256"`
		SkillSetHash          string          `json:"skill_set_hash"`
		APIMode               string          `json:"api_mode"`
		CacheGeneration       string          `json:"cache_generation,omitempty"`
		Agentic               project.Agentic `json:"agentic"`
	}{
		ProjectSHA256: identity.ProjectSHA256, EffectivePromptSHA256: identity.EffectivePromptSHA256,
		SkillSetHash: identity.SkillSetHash, APIMode: identity.APIMode,
		CacheGeneration: cacheGeneration, Agentic: agentic,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal benchmark input identity: %v", err))
	}
	return sha256Hex(data)
}

func benchmarkEngineCommit(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve benchmark engine commit: %v", err)
	}
	commit := strings.TrimSpace(string(output))
	if !benchmarkCommitRE.MatchString(commit) {
		t.Fatalf("benchmark engine commit is invalid: %q", commit)
	}
	return commit
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestBenchmarkArm(t *testing.T) {
	t.Run("baseline default", func(t *testing.T) {
		t.Setenv("BENCH_ARM", "")
		if got, err := benchmarkArm(false); err != nil || got != "baseline" {
			t.Fatalf("arm = %q, error = %v", got, err)
		}
	})
	t.Run("variant requires arm", func(t *testing.T) {
		t.Setenv("BENCH_ARM", "")
		if _, err := benchmarkArm(true); err == nil {
			t.Fatal("variant without arm was accepted")
		}
	})
	t.Run("configured", func(t *testing.T) {
		t.Setenv("BENCH_ARM", "proposed-recipes")
		if got, err := benchmarkArm(true); err != nil || got != "proposed-recipes" {
			t.Fatalf("arm = %q, error = %v", got, err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("BENCH_ARM", "bad arm")
		if _, err := benchmarkArm(false); err == nil {
			t.Fatal("invalid arm was accepted")
		}
	})
}

func TestValidateBenchmarkVariantDir(t *testing.T) {
	base := t.TempDir()
	variant := t.TempDir()
	projectData := []byte("id: same\n")
	for _, dir := range []string{base, variant} {
		if err := os.WriteFile(filepath.Join(dir, "project.yaml"), projectData, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateBenchmarkVariantDir(base, variant); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(variant, "project.yaml"), []byte("id: changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBenchmarkVariantDir(base, variant); err == nil {
		t.Fatal("changed variant project was accepted")
	}
}

func writeBenchmarkConsumer(t *testing.T, dir, prompt string, recipe bool) {
	t.Helper()
	projectData := `id: benchmark
name: Benchmark
testgrid:
  dashboard: benchmark
storage:
  provider: gcs
  bucket: kubernetes-ci-logs
branding:
  title: Benchmark
  base_path: /benchmark
  site_url: https://example.invalid/benchmark
  source_repo:
    owner: example
    name: project
ai:
  endpoint: https://example.invalid/v1/chat/completions
  model: benchmark-model
  tools: [filesystem]
  min_tool_calls: 2
  critique:
    cache_policy: hard
`
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(projectData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	if recipe {
		if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte("id: variant-evidence\ntriggers: ['(?i)failure']\n")
		if err := os.WriteFile(filepath.Join(dir, "skills", "variant.yaml"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadBenchmarkInputsDoesNotAlterBaselineAnalysisInputs(t *testing.T) {
	dir := t.TempDir()
	writeBenchmarkConsumer(t, dir, "baseline prompt\n", false)
	t.Setenv("BENCH_PROJECT_DIR", dir)
	t.Setenv("BENCH_VARIANT_DIR", "")
	t.Setenv("BENCH_ARM", "")

	got := loadBenchmarkInputs(t, []benchCase{{}}, ai.APIChatCompletions)
	cfg, prompt, err := project.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantSkills, _, err := skills.LoadForTools(dir, cfg.AI.EffectiveAgentic().Tools)
	if err != nil {
		t.Fatal(err)
	}
	if got.systemPrompt != ai.ComposeSystemPrompt(prompt) {
		t.Fatal("benchmark input loading changed the composed prompt")
	}
	gotAgentic, err := json.Marshal(got.agentic)
	if err != nil {
		t.Fatal(err)
	}
	wantAgentic, err := json.Marshal(cfg.AI.EffectiveAgentic())
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAgentic) != string(wantAgentic) {
		t.Fatalf("benchmark input loading changed agentic options: got %s want %s", gotAgentic, wantAgentic)
	}
	if got.projectSkills.Hash() != wantSkills.Hash() || got.identity.Arm != "baseline" {
		t.Fatalf("benchmark input loading changed skills or arm: %+v", got.identity)
	}
}

func TestLoadBenchmarkInputsAppliesOnlyPromptAndSkillVariant(t *testing.T) {
	base := t.TempDir()
	variant := t.TempDir()
	writeBenchmarkConsumer(t, base, "baseline prompt\n", false)
	writeBenchmarkConsumer(t, variant, "variant prompt\n", true)
	t.Setenv("BENCH_PROJECT_DIR", base)
	t.Setenv("BENCH_VARIANT_DIR", variant)
	t.Setenv("BENCH_ARM", "proposed-recipes")

	got := loadBenchmarkInputs(t, []benchCase{{fixtureSHA256: strings.Repeat("a", 64)}}, ai.APIChatCompletions)
	if got.identity.Arm != "proposed-recipes" || got.identity.ProjectSHA256 == "" || got.identity.EffectivePromptSHA256 == "" || got.identity.SkillSetHash == "" || got.identity.EffectiveInputSHA256 == "" {
		t.Fatalf("variant identity is incomplete: %+v", got.identity)
	}
	if !strings.Contains(got.systemPrompt, "variant prompt") || strings.Contains(got.systemPrompt, "baseline prompt") {
		t.Fatal("variant prompt was not selected")
	}
	if got.projectSkills.ConsumerCount() != 1 {
		t.Fatalf("consumer skill count = %d, want 1", got.projectSkills.ConsumerCount())
	}
}

func TestBenchmarkCacheDirIsolatesArmsAndInputs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BENCH_CACHE_DIR", root)
	bc := benchCase{name: "case", stableID: "0123456789abcdef0123"}
	baseline := benchmarkCacheDir(t, bc, 1, benchmarkRunIdentity{Arm: "baseline", EffectiveInputSHA256: strings.Repeat("a", 64)})
	variant := benchmarkCacheDir(t, bc, 1, benchmarkRunIdentity{Arm: "variant", EffectiveInputSHA256: strings.Repeat("b", 64)})
	if baseline == variant || !strings.Contains(baseline, "baseline-aaaaaaaaaaaa") || !strings.Contains(variant, "variant-bbbbbbbbbbbb") {
		t.Fatalf("cache dirs are not isolated: baseline=%s variant=%s", baseline, variant)
	}
}

func TestValidateBenchmarkRunIdentity(t *testing.T) {
	valid := benchmarkRunIdentity{
		Arm: "baseline", EngineCommit: strings.Repeat("a", 40), FixtureSHA256: strings.Repeat("b", 64),
		BaselineConsumerCommit: strings.Repeat("c", 40), ProjectSHA256: strings.Repeat("d", 64),
		EffectivePromptSHA256: strings.Repeat("e", 64), SkillSetHash: strings.Repeat("f", 64),
		EffectiveInputSHA256: strings.Repeat("1", 64), APIMode: ai.APIChatCompletions,
	}
	if err := validateBenchmarkRunIdentity(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.EffectiveInputSHA256 = "short"
	if err := validateBenchmarkRunIdentity(invalid); err == nil {
		t.Fatal("invalid input identity was accepted")
	}
}
