package benchmarks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/project"
)

type benchmarkRunIdentity struct {
	Arm                     string
	EngineCommit            string
	BenchmarkManifestSHA256 string
	BaselineConsumerCommit  string
	BaselinePromptSHA256    string
	FixtureSHA256           string
	ProjectSHA256           string
	EffectivePromptSHA256   string
	SkillSetHash            string
	EffectiveInputSHA256    string
	ComparisonInputSHA256   string
	EvidenceCondition       string
	FrozenEvidenceSHA256    string
	EvidenceStageSHA256     string
	APIMode                 string
	ReasoningEffort         ai.ReasoningEffort
	ProviderPath            string
	ProviderConfigSHA256    string
	TransportID             string
	ModelContextTokens      int
	ModelOutputTokens       int
	Pricing                 benchmarkPricingIdentity
}

type benchmarkPricingIdentity struct {
	Currency              string `json:"currency"`
	InputPerMillion       string `json:"input_per_million"`
	CachedInputPerMillion string `json:"cached_input_per_million"`
	OutputPerMillion      string `json:"output_per_million"`
	SHA256                string `json:"sha256"`
}

type benchmarkInputs struct {
	systemPrompt    string
	agentic         project.Agentic
	cacheGeneration string
	projectSkills   *skills.Set
	identity        benchmarkRunIdentity
}

func loadBenchmarkInputs(t *testing.T, cases []benchCase, apiMode, endpoint, model string) benchmarkInputs {
	t.Helper()
	variantDir := strings.TrimSpace(os.Getenv("BENCH_VARIANT_DIR"))
	arm, err := benchmarkArm(variantDir != "")
	if err != nil {
		t.Fatal(err)
	}

	condition, err := benchmarkEvidenceCondition()
	if err != nil {
		t.Fatal(err)
	}
	resultsEnabled := strings.TrimSpace(os.Getenv("BENCH_RESULTS_JSONL")) != ""
	providerPath := strings.TrimSpace(os.Getenv("BENCH_PROVIDER_PATH"))
	transportID := strings.TrimSpace(os.Getenv("BENCH_TRANSPORT_ID"))
	reasoningEffort := benchmarkReasoningEffort(t)
	if resultsEnabled && (providerPath == "" || transportID == "") {
		t.Fatal("BENCH_PROVIDER_PATH and BENCH_TRANSPORT_ID are required when BENCH_RESULTS_JSONL is set")
	}
	if err := validateBenchmarkProviderPath(providerPath, model); err != nil {
		t.Fatal(err)
	}
	out := benchmarkInputs{
		systemPrompt: ComposeBenchPrompt(),
		agentic:      defaultBenchAgentic(),
		identity: benchmarkRunIdentity{
			Arm:               arm,
			EngineCommit:      benchmarkEngineCommit(t, resultsEnabled),
			EvidenceCondition: condition,
			APIMode:           apiMode,
			ReasoningEffort:   reasoningEffort,
			ProviderPath:      providerPath,
			ProviderConfigSHA256: benchmarkProviderConfigSHA256(
				apiMode, endpoint, model, reasoningEffort,
			),
			TransportID: transportID,
		},
	}
	if resultsEnabled {
		out.identity.BenchmarkManifestSHA256 = benchmarkManifestIdentity(t)
		out.identity.ModelContextTokens = benchmarkRequiredInt(t, "BENCH_MODEL_CONTEXT_TOKENS", 8192, 2_000_000)
		out.identity.ModelOutputTokens = benchmarkRequiredInt(t, "BENCH_MODEL_OUTPUT_TOKENS", 1024, 131072)
		if out.identity.ModelOutputTokens > out.identity.ModelContextTokens {
			t.Fatal("BENCH_MODEL_OUTPUT_TOKENS must not exceed BENCH_MODEL_CONTEXT_TOKENS")
		}
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
			out.identity.BaselinePromptSHA256 = cases[0].promptSHA256
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
		out.identity.Pricing, err = benchmarkPricingIdentityFromEnv(cfg.AI.EffectiveUsage().Pricing, os.Getenv)
		if err != nil {
			t.Fatalf("load benchmark pricing identity: %v", err)
		}
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
	if resultsEnabled && out.identity.Pricing.SHA256 == "" {
		t.Fatal("BENCH_RESULTS_JSONL requires configured ai.usage.pricing")
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

func benchmarkReasoningEffort(t *testing.T) ai.ReasoningEffort {
	t.Helper()
	effort, err := ai.NormalizeReasoningEffort(os.Getenv("AI_REASONING_EFFORT"))
	if err != nil {
		t.Fatal(err)
	}
	return effort
}

func validateBenchmarkProviderPath(providerPath, model string) error {
	providerPath = strings.TrimSpace(providerPath)
	model = strings.TrimSpace(model)
	if providerPath != "" && !strings.HasSuffix(providerPath, "/"+model) {
		return fmt.Errorf("BENCH_PROVIDER_PATH %q does not match AI_MODEL %q", providerPath, model)
	}
	return nil
}

func validateBenchmarkRunIdentity(identity benchmarkRunIdentity) error {
	if !benchmarkCaseIDRE.MatchString(identity.Arm) {
		return fmt.Errorf("benchmark arm is invalid")
	}
	if !benchmarkCommitRE.MatchString(identity.EngineCommit) {
		return fmt.Errorf("benchmark engine commit is invalid")
	}
	for name, value := range map[string]string{
		"fixture SHA-256":            identity.FixtureSHA256,
		"benchmark manifest SHA-256": identity.BenchmarkManifestSHA256,
		"baseline prompt SHA-256":    identity.BaselinePromptSHA256,
		"project SHA-256":            identity.ProjectSHA256,
		"effective prompt SHA-256":   identity.EffectivePromptSHA256,
		"skill-set hash":             identity.SkillSetHash,
		"effective input SHA-256":    identity.EffectiveInputSHA256,
		"comparison input SHA-256":   identity.ComparisonInputSHA256,
		"provider config SHA-256":    identity.ProviderConfigSHA256,
		"pricing SHA-256":            identity.Pricing.SHA256,
		"frozen evidence SHA-256":    identity.FrozenEvidenceSHA256,
		"evidence stage SHA-256":     identity.EvidenceStageSHA256,
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
	if identity.EvidenceCondition != benchmarkEvidenceConditionFixture && identity.EvidenceCondition != benchmarkEvidenceConditionOracle {
		return fmt.Errorf("benchmark evidence condition is invalid")
	}
	if identity.EvidenceCondition == benchmarkEvidenceConditionOracle && identity.FrozenEvidenceSHA256 != "" && !benchmarkSHA256RE.MatchString(identity.FrozenEvidenceSHA256) {
		return fmt.Errorf("benchmark frozen evidence identity is invalid")
	}
	if identity.APIMode != ai.APIChatCompletions && identity.APIMode != ai.APIResponses {
		return fmt.Errorf("benchmark API mode is invalid")
	}
	if effort, err := ai.NormalizeReasoningEffort(string(identity.ReasoningEffort)); err != nil || effort != identity.ReasoningEffort {
		return fmt.Errorf("benchmark reasoning effort is invalid or not normalized")
	}
	if identity.ProviderPath != "" && (len(identity.ProviderPath) > 160 || strings.ContainsAny(identity.ProviderPath, " \t\r\n")) {
		return fmt.Errorf("benchmark provider path is invalid")
	}
	if identity.ProviderConfigSHA256 != "" && !benchmarkSHA256RE.MatchString(identity.ProviderConfigSHA256) {
		return fmt.Errorf("benchmark provider config identity is invalid")
	}
	if identity.Pricing.SHA256 != "" {
		if err := validateBenchmarkPricingIdentity(identity.Pricing); err != nil {
			return err
		}
	}
	if identity.TransportID != "" && (len(identity.TransportID) > 80 || strings.ContainsAny(identity.TransportID, " \t\r\n")) {
		return fmt.Errorf("benchmark transport id is invalid")
	}
	if identity.ModelContextTokens != 0 && (identity.ModelContextTokens < 8192 || identity.ModelContextTokens > 2_000_000 || identity.ModelOutputTokens < 1024 || identity.ModelOutputTokens > identity.ModelContextTokens || identity.ModelOutputTokens > 131072) {
		return fmt.Errorf("benchmark model limits are invalid")
	}
	return nil
}

func benchmarkRequiredInt(t *testing.T, name string, minimum, maximum int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		t.Fatalf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value
}

func benchmarkManifestIdentity(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("BENCH_MANIFEST"))
	if path == "" {
		t.Fatal("BENCH_MANIFEST is required for benchmark identity")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read BENCH_MANIFEST: %v", err)
	}
	return sha256Hex(data)
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
	if variant && arm == "baseline" {
		return "", fmt.Errorf("BENCH_ARM=baseline is reserved for runs without BENCH_VARIANT_DIR")
	}
	if !variant && arm != "baseline" {
		return "", fmt.Errorf("non-baseline BENCH_ARM requires BENCH_VARIANT_DIR")
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

func benchmarkProviderConfigSHA256(apiMode, endpoint, model string, reasoningEffort ai.ReasoningEffort) string {
	data, err := json.Marshal(struct {
		API             string             `json:"api"`
		Endpoint        string             `json:"endpoint"`
		Model           string             `json:"model"`
		ReasoningEffort ai.ReasoningEffort `json:"reasoning_effort,omitempty"`
	}{
		API: strings.TrimSpace(apiMode), Endpoint: strings.TrimSpace(endpoint), Model: strings.TrimSpace(model), ReasoningEffort: reasoningEffort,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal benchmark provider identity: %v", err))
	}
	return sha256Hex(data)
}

func benchmarkPricingIdentityFromEnv(pricing project.AIUsagePricing, getenv func(string) string) (benchmarkPricingIdentity, error) {
	identity, err := newBenchmarkPricingIdentity(pricing)
	if err != nil || identity.SHA256 != "" {
		return identity, err
	}
	return newBenchmarkPricingIdentity(project.AIUsagePricing{
		Currency: getenv("BENCH_PRICING_CURRENCY"), InputPerMillion: getenv("BENCH_PRICING_INPUT_PER_MILLION"),
		CachedInputPerMillion: getenv("BENCH_PRICING_CACHED_INPUT_PER_MILLION"), OutputPerMillion: getenv("BENCH_PRICING_OUTPUT_PER_MILLION"),
	})
}

func newBenchmarkPricingIdentity(pricing project.AIUsagePricing) (benchmarkPricingIdentity, error) {
	identity := benchmarkPricingIdentity{
		Currency: strings.TrimSpace(pricing.Currency), InputPerMillion: strings.TrimSpace(pricing.InputPerMillion),
		CachedInputPerMillion: strings.TrimSpace(pricing.CachedInputPerMillion), OutputPerMillion: strings.TrimSpace(pricing.OutputPerMillion),
	}
	if identity.Currency == "" && identity.InputPerMillion == "" && identity.CachedInputPerMillion == "" && identity.OutputPerMillion == "" {
		return benchmarkPricingIdentity{}, nil
	}
	if identity.CachedInputPerMillion == "" {
		identity.CachedInputPerMillion = identity.InputPerMillion
	}
	data, err := json.Marshal(map[string]string{
		"currency": identity.Currency, "input_per_million": identity.InputPerMillion,
		"cached_input_per_million": identity.CachedInputPerMillion, "output_per_million": identity.OutputPerMillion,
	})
	if err != nil {
		return benchmarkPricingIdentity{}, err
	}
	identity.SHA256 = sha256Hex(data)
	if err := validateBenchmarkPricingIdentity(identity); err != nil {
		return benchmarkPricingIdentity{}, err
	}
	return identity, nil
}

func validateBenchmarkPricingIdentity(identity benchmarkPricingIdentity) error {
	if len(identity.Currency) != 3 || identity.Currency != strings.ToUpper(identity.Currency) || identity.InputPerMillion == "" || identity.CachedInputPerMillion == "" || identity.OutputPerMillion == "" || !benchmarkSHA256RE.MatchString(identity.SHA256) {
		return fmt.Errorf("benchmark pricing identity is invalid")
	}
	for _, value := range []string{identity.InputPerMillion, identity.CachedInputPerMillion, identity.OutputPerMillion} {
		if _, err := strconv.ParseFloat(value, 64); err != nil || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE+") {
			return fmt.Errorf("benchmark pricing rate is invalid")
		}
	}
	data, err := json.Marshal(map[string]string{
		"currency": identity.Currency, "input_per_million": identity.InputPerMillion,
		"cached_input_per_million": identity.CachedInputPerMillion, "output_per_million": identity.OutputPerMillion,
	})
	if err != nil || sha256Hex(data) != identity.SHA256 {
		return fmt.Errorf("benchmark pricing identity hash changed")
	}
	return nil
}

func benchmarkEffectiveInputSHA256(identity benchmarkRunIdentity, agentic project.Agentic, cacheGeneration string) string {
	var pricing *benchmarkPricingIdentity
	if identity.Pricing.SHA256 != "" {
		value := identity.Pricing
		pricing = &value
	}
	data, err := json.Marshal(struct {
		ProjectSHA256           string                    `json:"project_sha256,omitempty"`
		FixtureSHA256           string                    `json:"fixture_sha256,omitempty"`
		BenchmarkManifestSHA256 string                    `json:"benchmark_manifest_sha256,omitempty"`
		BaselinePromptSHA256    string                    `json:"baseline_prompt_sha256,omitempty"`
		EffectivePromptSHA256   string                    `json:"effective_prompt_sha256"`
		SkillSetHash            string                    `json:"skill_set_hash"`
		APIMode                 string                    `json:"api_mode"`
		ReasoningEffort         ai.ReasoningEffort        `json:"reasoning_effort,omitempty"`
		ProviderPath            string                    `json:"provider_path,omitempty"`
		ProviderConfigSHA256    string                    `json:"provider_config_sha256,omitempty"`
		TransportID             string                    `json:"transport_id,omitempty"`
		ModelContextTokens      int                       `json:"model_context_tokens,omitempty"`
		ModelOutputTokens       int                       `json:"model_output_tokens,omitempty"`
		EvidenceCondition       string                    `json:"evidence_condition"`
		FrozenEvidenceSHA256    string                    `json:"frozen_evidence_sha256,omitempty"`
		EvidenceStageSHA256     string                    `json:"evidence_stage_sha256,omitempty"`
		CacheGeneration         string                    `json:"cache_generation,omitempty"`
		Agentic                 project.Agentic           `json:"agentic"`
		Pricing                 *benchmarkPricingIdentity `json:"pricing,omitempty"`
	}{
		ProjectSHA256: identity.ProjectSHA256, FixtureSHA256: identity.FixtureSHA256, BenchmarkManifestSHA256: identity.BenchmarkManifestSHA256, BaselinePromptSHA256: identity.BaselinePromptSHA256,
		EffectivePromptSHA256: identity.EffectivePromptSHA256, SkillSetHash: identity.SkillSetHash,
		APIMode: identity.APIMode, ReasoningEffort: identity.ReasoningEffort, ProviderPath: identity.ProviderPath, ProviderConfigSHA256: identity.ProviderConfigSHA256, TransportID: identity.TransportID,
		ModelContextTokens: identity.ModelContextTokens, ModelOutputTokens: identity.ModelOutputTokens,
		EvidenceCondition: identity.EvidenceCondition, FrozenEvidenceSHA256: identity.FrozenEvidenceSHA256, EvidenceStageSHA256: identity.EvidenceStageSHA256,
		CacheGeneration: cacheGeneration, Agentic: agentic, Pricing: pricing,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal benchmark input identity: %v", err))
	}
	return sha256Hex(data)
}

func benchmarkComparisonInputSHA256(bc benchCase, identity benchmarkRunIdentity) string {
	sourceRefs := append([]benchmarkSourceRef(nil), bc.sourceRefs...)
	sort.Slice(sourceRefs, func(i, j int) bool { return sourceRefs[i].ID < sourceRefs[j].ID })
	data, err := json.Marshal(struct {
		StableID                string                   `json:"stable_id"`
		FixtureSHA256           string                   `json:"fixture_sha256"`
		BenchmarkManifestSHA256 string                   `json:"benchmark_manifest_sha256"`
		ConsumerCommit          string                   `json:"consumer_commit"`
		ProjectSHA256           string                   `json:"project_sha256"`
		EffectivePromptSHA256   string                   `json:"effective_prompt_sha256"`
		SkillSetHash            string                   `json:"skill_set_hash"`
		SourceRefs              []benchmarkSourceRef     `json:"source_refs"`
		PrimarySourceID         string                   `json:"primary_source_id"`
		JobName                 string                   `json:"job_name"`
		BuildID                 string                   `json:"build_id"`
		TestName                string                   `json:"test_name"`
		TestSource              string                   `json:"test_source"`
		JUnitFile               string                   `json:"junit_file"`
		FailureMessageSHA256    string                   `json:"failure_message_sha256"`
		ConsecutiveFailures     int                      `json:"consecutive_failures"`
		APIMode                 string                   `json:"api_mode"`
		ReasoningEffort         string                   `json:"reasoning_effort"`
		ProviderPath            string                   `json:"provider_path"`
		ProviderConfigSHA256    string                   `json:"provider_config_sha256"`
		TransportID             string                   `json:"transport_id"`
		ModelContextTokens      int                      `json:"model_context_tokens"`
		ModelOutputTokens       int                      `json:"model_output_tokens"`
		Pricing                 benchmarkPricingIdentity `json:"pricing"`
	}{
		StableID: bc.stableID, FixtureSHA256: bc.fixtureSHA256, BenchmarkManifestSHA256: identity.BenchmarkManifestSHA256, ConsumerCommit: bc.consumerCommit, ProjectSHA256: identity.ProjectSHA256,
		EffectivePromptSHA256: identity.EffectivePromptSHA256, SkillSetHash: identity.SkillSetHash, SourceRefs: sourceRefs, PrimarySourceID: bc.primarySourceID,
		JobName: bc.jobName, BuildID: bc.buildID, TestName: bc.testName, TestSource: bc.testSource, JUnitFile: bc.junitFile,
		FailureMessageSHA256: sha256Hex([]byte(bc.failureMsg)), ConsecutiveFailures: bc.consecutiveFailures,
		APIMode: identity.APIMode, ReasoningEffort: string(identity.ReasoningEffort), ProviderPath: identity.ProviderPath, ProviderConfigSHA256: identity.ProviderConfigSHA256, TransportID: identity.TransportID,
		ModelContextTokens: identity.ModelContextTokens, ModelOutputTokens: identity.ModelOutputTokens,
		Pricing: identity.Pricing,
	})
	if err != nil {
		panic(err)
	}
	return sha256Hex(data)
}

func benchmarkEngineCommit(t *testing.T, requireClean bool) string {
	t.Helper()
	if requireClean {
		status, err := exec.Command("git", "status", "--porcelain", "--untracked-files=all").Output()
		if err != nil {
			t.Fatalf("inspect benchmark engine worktree: %v", err)
		}
		if err := validateBenchmarkWorktreeStatus(status); err != nil {
			t.Fatal(err)
		}
	}
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

func validateBenchmarkWorktreeStatus(status []byte) error {
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("BENCH_RESULTS_JSONL requires a clean engine worktree")
	}
	return nil
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
discovery:
  testgrid_dashboard: benchmark
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

	got := loadBenchmarkInputs(t, []benchCase{{}}, ai.APIChatCompletions, "https://provider.example.test/v1/chat/completions", "model")
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

	got := loadBenchmarkInputs(t, []benchCase{{fixtureSHA256: strings.Repeat("a", 64)}}, ai.APIChatCompletions, "https://provider.example.test/v1/chat/completions", "model")
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

func TestValidateBenchmarkProviderPath(t *testing.T) {
	if err := validateBenchmarkProviderPath("github-copilot/claude-sonnet-4.6", "claude-sonnet-4.6"); err != nil {
		t.Fatal(err)
	}
	if err := validateBenchmarkProviderPath("github-copilot/claude-sonnet-4.6", "different-model"); err == nil {
		t.Fatal("mismatched provider and model were accepted")
	}
}

func TestValidateBenchmarkWorktreeStatus(t *testing.T) {
	if err := validateBenchmarkWorktreeStatus(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateBenchmarkWorktreeStatus([]byte(" M file.go\n?? new.go\n")); err == nil {
		t.Fatal("dirty worktree was accepted")
	}
}

func TestBenchmarkArmRejectsVariantAsBaseline(t *testing.T) {
	t.Setenv("BENCH_ARM", "baseline")
	if _, err := benchmarkArm(true); err == nil {
		t.Fatal("variant was allowed to identify as baseline")
	}
	t.Setenv("BENCH_ARM", "variant-a")
	if _, err := benchmarkArm(false); err == nil {
		t.Fatal("non-baseline arm without variant inputs was accepted")
	}
}

func TestValidateBenchmarkRunIdentity(t *testing.T) {
	valid := benchmarkRunIdentity{
		Arm: "baseline", EngineCommit: strings.Repeat("a", 40), FixtureSHA256: strings.Repeat("b", 64),
		BaselineConsumerCommit: strings.Repeat("c", 40), BaselinePromptSHA256: strings.Repeat("2", 64),
		ProjectSHA256: strings.Repeat("d", 64), EffectivePromptSHA256: strings.Repeat("e", 64), SkillSetHash: strings.Repeat("f", 64),
		EffectiveInputSHA256: strings.Repeat("1", 64), EvidenceCondition: benchmarkEvidenceConditionFixture, APIMode: ai.APIChatCompletions, ProviderPath: "github-copilot/claude-sonnet-4.6", TransportID: "copilot-structural-proxy-v1",
	}
	if err := validateBenchmarkRunIdentity(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.EffectiveInputSHA256 = "short"
	if err := validateBenchmarkRunIdentity(invalid); err == nil {
		t.Fatal("invalid input identity was accepted")
	}
	invalid = valid
	invalid.ProviderPath = "provider path"
	if err := validateBenchmarkRunIdentity(invalid); err == nil {
		t.Fatal("invalid provider path was accepted")
	}
	invalid = valid
	invalid.ReasoningEffort = " HIGH "
	if err := validateBenchmarkRunIdentity(invalid); err == nil {
		t.Fatal("non-normalized reasoning effort was accepted")
	}
}

func TestBenchmarkEffectiveInputIncludesEvidenceCondition(t *testing.T) {
	identity := benchmarkRunIdentity{
		ProjectSHA256: strings.Repeat("a", 64), EffectivePromptSHA256: strings.Repeat("b", 64),
		SkillSetHash: strings.Repeat("c", 64), APIMode: ai.APIChatCompletions,
		EvidenceCondition: benchmarkEvidenceConditionFixture,
	}
	fixture := benchmarkEffectiveInputSHA256(identity, project.Agentic{}, "generation")
	identity.EvidenceCondition = benchmarkEvidenceConditionOracle
	identity.FrozenEvidenceSHA256 = strings.Repeat("d", 64)
	oracle := benchmarkEffectiveInputSHA256(identity, project.Agentic{}, "generation")
	if fixture == oracle {
		t.Fatal("evidence conditions shared an effective input identity")
	}
	identity.EvidenceStageSHA256 = strings.Repeat("e", 64)
	if withStages := benchmarkEffectiveInputSHA256(identity, project.Agentic{}, "generation"); withStages == oracle {
		t.Fatal("evidence stage identities shared an effective input identity")
	}
}

func TestBenchmarkReasoningEffortIdentity(t *testing.T) {
	identity := benchmarkRunIdentity{
		ProjectSHA256: strings.Repeat("a", 64), EffectivePromptSHA256: strings.Repeat("b", 64),
		SkillSetHash: strings.Repeat("c", 64), APIMode: ai.APIResponses,
		EvidenceCondition: benchmarkEvidenceConditionFixture,
	}
	empty := benchmarkEffectiveInputSHA256(identity, project.Agentic{}, "generation")
	legacy, err := json.Marshal(struct {
		ProjectSHA256         string          `json:"project_sha256,omitempty"`
		BaselinePromptSHA256  string          `json:"baseline_prompt_sha256,omitempty"`
		EffectivePromptSHA256 string          `json:"effective_prompt_sha256"`
		SkillSetHash          string          `json:"skill_set_hash"`
		APIMode               string          `json:"api_mode"`
		ProviderPath          string          `json:"provider_path,omitempty"`
		TransportID           string          `json:"transport_id,omitempty"`
		EvidenceCondition     string          `json:"evidence_condition"`
		FrozenEvidenceSHA256  string          `json:"frozen_evidence_sha256,omitempty"`
		EvidenceStageSHA256   string          `json:"evidence_stage_sha256,omitempty"`
		CacheGeneration       string          `json:"cache_generation,omitempty"`
		Agentic               project.Agentic `json:"agentic"`
	}{
		ProjectSHA256: identity.ProjectSHA256, EffectivePromptSHA256: identity.EffectivePromptSHA256,
		SkillSetHash: identity.SkillSetHash, APIMode: identity.APIMode,
		EvidenceCondition: identity.EvidenceCondition, CacheGeneration: "generation", Agentic: project.Agentic{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if empty != sha256Hex(legacy) {
		t.Fatalf("empty effort changed historical benchmark identity: got %s want %s", empty, sha256Hex(legacy))
	}
	identity.ReasoningEffort = ai.ReasoningEffortHigh
	if high := benchmarkEffectiveInputSHA256(identity, project.Agentic{}, "generation"); high == empty {
		t.Fatal("non-empty effort reused empty benchmark identity")
	}
}

func TestBenchmarkReasoningEffortFromEnvironment(t *testing.T) {
	t.Setenv("AI_REASONING_EFFORT", " HIGH ")
	if got := benchmarkReasoningEffort(t); got != ai.ReasoningEffortHigh {
		t.Fatalf("reasoning effort = %q, want high", got)
	}
}

func TestBenchmarkComparisonInputSHA256IncludesCanonicalSourceCatalog(t *testing.T) {
	first := benchmarkSourceRef{ID: "client", Repository: "kubernetes/kubernetes", Revision: strings.Repeat("a", 40)}
	second := benchmarkSourceRef{ID: "server", Repository: "kubernetes/kubernetes", Revision: strings.Repeat("b", 40)}
	base := benchCase{
		stableID:        "0123456789abcdef0123",
		primarySourceID: "client",
		sourceRefs:      []benchmarkSourceRef{first, second},
	}
	identity := benchmarkRunIdentity{}
	want := benchmarkComparisonInputSHA256(base, identity)
	reordered := base
	reordered.sourceRefs = []benchmarkSourceRef{second, first}
	if got := benchmarkComparisonInputSHA256(reordered, identity); got != want {
		t.Fatalf("comparison hash changed with source catalog order: %s != %s", got, want)
	}
	changedRevision := base
	changedRevision.sourceRefs = []benchmarkSourceRef{first, {ID: "server", Repository: "kubernetes/kubernetes", Revision: strings.Repeat("c", 40)}}
	if got := benchmarkComparisonInputSHA256(changedRevision, identity); got == want {
		t.Fatal("comparison hash ignored source revision change")
	}
	changedPrimary := base
	changedPrimary.primarySourceID = "server"
	if got := benchmarkComparisonInputSHA256(changedPrimary, identity); got == want {
		t.Fatal("comparison hash ignored primary source change")
	}
}
