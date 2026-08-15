package onboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConsumerSetupAgentSkill(t *testing.T) {
	root := onboardingRepoRoot(t)
	skillPath := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "SKILL.md")
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
	if metadata.Name != "setup-aster-consumer" || strings.TrimSpace(metadata.Description) == "" {
		t.Fatalf("skill metadata = %+v", metadata)
	}
	for _, anchor := range []string{
		"go -C backend run ./cmd/aster", "onboard discover", "-json", "-dry-run", "-non-interactive",
		"-prompt-mode handoff", "-plan-out", "-apply-plan", "-plan-digest",
		"-result-out", "-handoff-out", "-artifact-smoke-builds", "-deployment-reason", "-artifact-access",
		"-replace-consumer-owned", "existing `prompts/system.md` and every existing skill file", "Existing `skills/*.yaml`",
		"source-only baseline", "$author-aster-diagnostics", "setup-handoff.json", "setup-handoff.schema.json",
		"onboard doctor", "After the user confirms the reviewed plan",
		"Never delete stale or unrelated files", "Use values already supplied anywhere in the user's request", "Never turn literal template placeholders",
		"Run discovery as soon as the source is known", "Do not ask for a slug",
		"separate workspaces, plans, handoffs", "fetch `origin`, compare", "stale local engine merely", "hard scope boundary",
		"manifest/locations.json", "manifest/consumer-files.sha256", "reports/setup-summary.md",
		"@v0.9.0-rc.2", "<engine-ref>", "-engine-ref <engine-ref>",
		"exact release tag or full commit SHA", "configured GitHub",
		"commit is local-only", "before a Pages plan", "local-only checkout can be deployed",
		"Pages workflow ends in `@<engine-ref>`", "never the mutable name `main`",
	} {
		if !strings.Contains(text, anchor) {
			t.Errorf("skill missing %q", anchor)
		}
	}
	for _, forbidden := range []string{"api-experimental", "rm -rf", "--no-verify", "backend/cmd/aster@latest", "-engine-ref v0.9.0-rc.2"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("skill contains forbidden text %q", forbidden)
		}
	}
	for _, name := range []string{"SKILL.md", "agents/openai.yaml", "references/decisions.md", "references/setup-handoff.schema.json", "scripts/validate_setup_handoff.py"} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(skillPath), name))
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range []string{"CAPZ", "cluster-api-provider-azure", "AzureManaged", "AKS", "GCP PD CSI", "Kueue"} {
			if strings.Contains(string(raw), term) {
				t.Errorf("%s contains provider-specific term %q", name, term)
			}
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
	for _, anchor := range []string{"$setup-aster-consumer", "source repository named or linked", "infer safe local defaults", "read-only discovery", "template placeholders"} {
		if !strings.Contains(openAI.Interface.DefaultPrompt, anchor) {
			t.Fatalf("default prompt missing %q: %q", anchor, openAI.Interface.DefaultPrompt)
		}
	}

	referencePath := filepath.Join(filepath.Dir(skillPath), "references", "decisions.md")
	reference, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{"## Input resolution", "## Placement", "## Deployment", "## Discovery", "## Prompt ownership and updates", "## Artifact usability", "## Reproducibility and handoff", "## Write boundaries"} {
		if !strings.Contains(string(reference), anchor) {
			t.Errorf("decision reference missing %q", anchor)
		}
	}
}

func TestConsumerSetupHandoffValidator(t *testing.T) {
	root := onboardingRepoRoot(t)
	skillDir := filepath.Join(root, ".agents", "skills", "setup-aster-consumer")
	script := filepath.Join(skillDir, "scripts", "validate_setup_handoff.py")
	output, err := exec.Command("python3", script, "--self-test").CombinedOutput()
	if err != nil {
		t.Fatalf("setup handoff validator self-test: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "setup handoff validator self-test passed") {
		t.Fatalf("unexpected validator output: %s", output)
	}
	schema := filepath.Join(skillDir, "references", "setup-handoff.schema.json")
	raw, err := os.ReadFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{`"plan_digest"`, `"source_only_candidate"`, `"artifact_smoke"`, `"artifact_location"`, `"test_infra"`, `"matches_reviewed_plan"`, `"preserve"`, `"next_phase"`} {
		if !strings.Contains(string(raw), anchor) {
			t.Errorf("setup handoff schema missing %q", anchor)
		}
	}
}

func TestDiagnosticAuthoringAgentSkill(t *testing.T) {
	root := onboardingRepoRoot(t)
	skillDir := filepath.Join(root, ".agents", "skills", "author-aster-diagnostics")
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
	if metadata.Name != "author-aster-diagnostics" || strings.TrimSpace(metadata.Description) == "" {
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
		"reports/benchmark-results.json", "reports/validation/*.log", "schema_version: 2", "report-schema.json", "validate_reports.py", "--evidence-root", "source_revision_status", "competing hypotheses", "open handoff", "same_author_review", "identity manifest", "Git blob ID", "write_validation_file_manifest.py", "locked lexical score", "manual semantic score", "fresh isolated LLM CLI", "private source or artifacts to a provider", "repeated prompt-only misses", "pre_freeze_holdout_kind", "post_reveal_causal_kind", "holdout_event_scope", "aggregate the holdout as `mixed`", "pre-freeze denylist", "blind_access.py", "schema-only benchmark fixture", "scoring overlay", "scoring_protocol", "prompt_regression", "baseline_provenance", "informed the baseline prompt cannot be an independent final holdout", "Prow pod actually started", "VolumeBinding", "companion test", "same volume, PVC, pod, node, and time", "untrusted inputs", "recurrence", "generalization", "validate_setup_handoff.py", "git worktree add --detach", "exclude `.git` itself", "consumer.commit: null", "Remote GCS or HTTP reads", "call it `<authoring-root>`", "<authoring-root>/reports/validation",
		"recommended", "experimental", "rejected", "unresolved",
		"Never write them to the authoring consumer's active `skills/`", "without a later explicit approval",
	} {
		if !strings.Contains(text, anchor) {
			t.Errorf("skill missing %q", anchor)
		}
	}
	if strings.Contains(text, "<consumer>/reports/") {
		t.Error("diagnostic skill writes authoring reports through the deployed-consumer placeholder")
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
	if !strings.Contains(openAI.Interface.DefaultPrompt, "$author-aster-diagnostics") || !strings.Contains(openAI.Interface.DefaultPrompt, "representative failure corpus") || !strings.Contains(openAI.Interface.DefaultPrompt, "prompt-only misses") || !strings.Contains(openAI.Interface.DefaultPrompt, "without activating") {
		t.Fatalf("default prompt does not preserve the skill boundary: %q", openAI.Interface.DefaultPrompt)
	}

	references := map[string][]string{
		"failure-corpus.md": {"## Build a representative corpus", "At least six diagnosed failures", "authorized Prow artifact indexes", "same-wrapper, different-cause counterexample", "## Diagnose each failure", "reports/failure-corpus.json", `"pre_freeze_holdout_kind": "not_applicable"`, `"holdout_event_scope": "not_applicable"`, `"actor": "scheduler"`, "storage_identity_correlation", "cross_run_evidence_ids", "recurrence_signatures", "prompt_regression", "baseline_provenance", "different test name in the same build remains", "Prow pod started", "VolumeBinding", "aggregate the holdout", "trusted quality exemplars", "## Validate the prompt with fresh sessions", "two bounded revision rounds", "## Apply the prompt quality rubric"},
		"decisions.md":      {"## Choose the corpus and splits", "prompt-only validation cases", "event-specific holdout", "aggregates to `mixed`", "prompt_regression", "pre-freeze denylist", "schema-only identity fixture", "untrusted candidates", "Deterministic or same-author behavior checks only", "post-reveal generalization holdout", "## Promotion boundary"},
		"recipe-authoring.md": {
			"second-line intervention", "separate validation-set causal events", "fresh sessions for the same evidence relationship", "## Design triggers", "## Build the applicability matrix", "## Run deterministic engine validation",
			"bounded failure signal", "final-draft-only trigger", "positive successful-operation evidence", "competing initiating cause", "slash-normalizes and lowercases", "Doctor does not validate recipe YAML", "ParseAndValidate",
		},
		"benchmarking.md": {"## Protect the blind boundary", "blind_access.py", "remote GCS or HTTP reads", "wrapper_enforced", "benchmark-manifest.schema-only.json", "prompt_regression", "baseline_provenance", "same build is still excluded", "companion anchor-test", "## Record prompt-authoring validation", "authoring_validation", "fresh_holdout_trials", "dashboard_trials", "freeze_manifest", "## Validate derived manifests without a provider", "identity-only A/B/C manifest", "post-reveal scoring-overlay", "scoring_protocol: same_evaluator_post_hoc", "## Run the benchmark matrix", "every final holdout", "aggregate recurrence plus", "## Write deterministic results", "BENCH_REPETITIONS=3", "Git blob IDs", "commit_status: not_applicable", "<authoring-root>/reports/failure-corpus.json", "quality floors failed"},
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
		if strings.Contains(string(raw), "<consumer>/reports/") {
			t.Errorf("%s writes authoring reports through the deployed-consumer placeholder", name)
		}
	}

	providerSpecificTerms := []string{
		"PodGroup", "cluster-api-provider-azure", "CAPZ", "Azure", "AzureManaged", "AKS", "ASO",
		"gcp-compute-persistent-disk-csi-driver", "GCP PD CSI", "Kueue", "Secrets Store CSI",
	}
	providerAgnosticFiles := []string{
		"SKILL.md", "agents/openai.yaml",
		"references/benchmarking.md", "references/decisions.md", "references/failure-corpus.md",
		"references/recipe-authoring.md", "references/report-schema.json",
		"references/benchmark-manifest.schema-only.json",
		"scripts/blind_access.py", "scripts/validate_reports.py", "scripts/write_validation_file_manifest.py",
	}
	for _, name := range providerAgnosticFiles {
		raw, err := os.ReadFile(filepath.Join(skillDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range providerSpecificTerms {
			if strings.Contains(string(raw), term) {
				t.Errorf("%s contains provider-specific term %q", name, term)
			}
		}
	}
}

func TestDiagnosticAuthoringReportValidator(t *testing.T) {
	root := onboardingRepoRoot(t)
	scripts := map[string]string{
		"validate_reports.py":               "report validator self-test passed",
		"write_validation_file_manifest.py": "validation file manifest self-test passed",
		"blind_access.py":                   "blind access self-test passed",
	}
	for name, expected := range scripts {
		script := filepath.Join(root, ".agents", "skills", "author-aster-diagnostics", "scripts", name)
		output, err := exec.Command("python3", script, "--self-test").CombinedOutput()
		if err != nil {
			t.Fatalf("%s self-test: %v\n%s", name, err, output)
		}
		if !strings.Contains(string(output), expected) {
			t.Fatalf("unexpected %s output: %s", name, output)
		}
	}

	schema := filepath.Join(root, ".agents", "skills", "author-aster-diagnostics", "references", "report-schema.json")
	if raw, err := os.ReadFile(schema); err != nil {
		t.Fatal(err)
	} else {
		for _, anchor := range []string{"report-schema-v2.json", `"schema_version"`, `"same_author_review"`, `"source_revision_status"`, `"competing_hypotheses"`, `"assignment_strength"`, `"storage_identity_correlation"`, `"transient_assessment"`, `"pre_freeze_holdout_kind"`, `"post_reveal_causal_kind"`, `"holdout_event_scope"`, `"post_reveal_event"`, `"prompt_regression"`, `"baseline_provenance_item"`, `"baseline_provenance"`, `"scoring_protocol"`, `"evaluation_snapshot"`, `"fresh_holdout_diagnosis"`, `"blind_access_control"`, `"validation_file_manifest"`, `"benchmark_identity_manifest"`, `"benchmark_scoring_overlay"`, `"condition_manifests"`, `"scope"`, `"fresh_holdout_trials"`, `"dashboard_trials"`, `"prompt_only_misses"`, `"commit_status"`, `"locked_score"`, `"semantic_score"`} {
			if !strings.Contains(string(raw), anchor) {
				t.Errorf("report schema missing %q", anchor)
			}
		}
	}

	fixture := filepath.Join(root, ".agents", "skills", "author-aster-diagnostics", "references", "benchmark-manifest.schema-only.json")
	if raw, err := os.ReadFile(fixture); err != nil {
		t.Fatal(err)
	} else {
		for _, anchor := range []string{`"document_type": "benchmark_identity_manifest"`, `"identity_only": true`, `"case_id": "schema-only-case"`, `"test_name": "schema-only-test-event"`, `"consumer_commit": null`} {
			if !strings.Contains(string(raw), anchor) {
				t.Errorf("schema-only benchmark fixture missing %q", anchor)
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
			"$setup-aster-consumer",
			"docs/agent-onboarding.md",
		},
		"docs/onboarding-a-new-project.md": {
			"## Choose an onboarding method",
			"Interactive wizard", "#interactive-wizard",
			"Coding agent-assisted", "#coding-agent-assisted-onboarding",
			"Non-interactive CLI", "#non-interactive-cli-onboarding",
			"Manual setup", "#manual-setup",
			"npx --yes skills@latest add willie-yao/aster",
			"https://github.com/kubernetes-sigs/kueue", "template placeholders",
			"$author-aster-diagnostics",
		},
		"docs/agent-onboarding.md": {
			"--skill setup-aster-consumer author-aster-diagnostics",
			"--agent codex",
			"--global",
			"Use $setup-aster-consumer", "The agent should not ask again", "-exact-job",
			"manifest/consumer-files.sha256", "reports/setup-summary.md",
			"Use $author-aster-diagnostics",
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
