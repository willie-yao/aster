package onboard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"gopkg.in/yaml.v3"
)

func TestBuildApplyResultAndSetupHandoffValidate(t *testing.T) {
	plan, deps, _ := testReviewedPlan(t)
	deps.files = localScaffoldWriter{}
	if err := applyPlan(context.Background(), plan, "", deps); err != nil {
		t.Fatal(err)
	}
	planDigest := "sha256:" + strings.Repeat("1", 64)
	result, err := buildApplyResult(plan, planDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.MatchesReviewedPlan || result.Prompt.Status != "created-source-only-baseline" {
		t.Fatalf("apply result = %+v", result)
	}
	for _, file := range result.Files {
		if file.Mode != "0644" || !file.MatchesReviewedPlan {
			t.Fatalf("applied file = %+v", file)
		}
	}
	doctor := DoctorReport{ProjectDir: plan.Destination.OutDir, Checks: []DoctorCheck{{Name: "project.yaml", Status: DoctorPass, Detail: "ok"}}}
	handoff := buildSetupHandoff(plan, planDigest, result, doctor, ArtifactSmokeReport{ReadOnly: true, BuildsPerJob: 0, Jobs: []ArtifactJobSmoke{}})
	if handoff.ArtifactLocation.Provider != "gcs" || handoff.ArtifactLocation.Bucket == "" {
		t.Fatalf("artifact location = %+v", handoff.ArtifactLocation)
	}
	if handoff.TestInfra.Status != sourceRevisionResolved || handoff.TestInfra.Repository == nil || handoff.TestInfra.Repository.FullName != "kubernetes/test-infra" || !validGitRevision(handoff.TestInfra.Revision) {
		t.Fatalf("test-infra handoff = %+v", handoff.TestInfra)
	}
	path := filepath.Join(t.TempDir(), "setup-handoff.json")
	if err := writePrivateJSON(path, handoff); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("handoff mode = %o", info.Mode().Perm())
	}
	root := onboardingRepoRoot(t)
	schemaPath := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "references", "setup-handoff.schema.json")
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile setup handoff schema: %v", err)
	}
	schemaError := func(path string) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		instance, err := jsonschema.UnmarshalJSON(file)
		if err != nil {
			return err
		}
		return schema.Validate(instance)
	}
	validateSchema := func(path string) {
		t.Helper()
		if err := schemaError(path); err != nil {
			t.Fatalf("validate handoff schema: %v", err)
		}
	}
	writeSchemaVersionVariant := func(source, from, to, name string) string {
		t.Helper()
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Replace(string(raw), from, to, 1)
		if text == string(raw) {
			t.Fatalf("schema version token %q not found", from)
		}
		variant := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(variant, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return variant
	}
	validateSchema(path)
	script := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "scripts", "validate_setup_handoff.py")
	output, err := exec.Command("python3", script, path).CombinedOutput()
	if err != nil {
		t.Fatalf("validate handoff: %v\n%s", err, output)
	}
	legacy := handoff
	legacy.SchemaVersion = 1
	legacyPath := filepath.Join(t.TempDir(), "setup-handoff-v1-pages.json")
	if err := writePrivateJSON(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	validateSchema(legacyPath)
	output, err = exec.Command("python3", script, legacyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("validate release-compatible Pages handoff: %v\n%s", err, output)
	}
	for _, variant := range []string{
		writeSchemaVersionVariant(legacyPath, `"schema_version": 1`, `"schema_version": 1.0`, "setup-handoff-v1-float.json"),
		writeSchemaVersionVariant(path, `"schema_version": 2`, `"schema_version": 2.0`, "setup-handoff-v2-float.json"),
	} {
		validateSchema(variant)
		output, err = exec.Command("python3", script, variant).CombinedOutput()
		if err != nil {
			t.Fatalf("validate integral numeric schema version: %v\n%s", err, output)
		}
	}
	booleanVersion := writeSchemaVersionVariant(path, `"schema_version": 2`, `"schema_version": true`, "setup-handoff-boolean-version.json")
	assertBothReject := func(variant, label string) {
		t.Helper()
		if err := schemaError(variant); err == nil {
			t.Fatalf("JSON Schema accepted %s", label)
		}
		if output, err = exec.Command("python3", script, variant).CombinedOutput(); err == nil {
			t.Fatalf("Python validator accepted %s: %s", label, output)
		}
	}
	assertBothAccept := func(variant, label string) {
		t.Helper()
		validateSchema(variant)
		if output, err = exec.Command("python3", script, variant).CombinedOutput(); err != nil {
			t.Fatalf("Python validator rejected %s: %v\n%s", label, err, output)
		}
	}
	assertBothReject(booleanVersion, "a boolean root schema version")
	assertBothAccept(
		writeSchemaVersionVariant(path, `    "schema_version": 1`, `    "schema_version": 1.0`, "setup-handoff-apply-version-float.json"),
		"an integral apply-result schema version",
	)
	assertBothReject(
		writeSchemaVersionVariant(path, `    "schema_version": 1`, `    "schema_version": true`, "setup-handoff-apply-version-boolean.json"),
		"a boolean apply-result schema version",
	)
	assertBothAccept(
		writeSchemaVersionVariant(path, `"builds_per_job": 0`, `"builds_per_job": 0.0`, "setup-handoff-smoke-builds-float.json"),
		"an integral artifact-smoke build count",
	)
	assertBothReject(
		writeSchemaVersionVariant(path, `"builds_per_job": 0`, `"builds_per_job": true`, "setup-handoff-smoke-builds-boolean.json"),
		"a boolean artifact-smoke build count",
	)
	assertBothReject(
		writeSchemaVersionVariant(path, `"engine": {`, `"engine": {\n    "modified": "false",`, "setup-handoff-engine-modified-string.json"),
		"a non-boolean engine modified value",
	)
	assertBothReject(
		writeSchemaVersionVariant(path, `"ai_enabled": false`, `"ai_enabled": "false"`, "setup-handoff-ai-enabled-string.json"),
		"a non-boolean AI enabled value",
	)
	handoff.Deployment.Mode = modeK8s
	handoff.Deployment.K8sStorageClass = "shared-rwx"
	k8sPath := filepath.Join(t.TempDir(), "setup-handoff-k8s.json")
	if err := writePrivateJSON(k8sPath, handoff); err != nil {
		t.Fatal(err)
	}
	validateSchema(k8sPath)
	output, err = exec.Command("python3", script, k8sPath).CombinedOutput()
	if err != nil {
		t.Fatalf("validate Kubernetes handoff: %v\n%s", err, output)
	}
}

func TestBuildApplyResultRecordsPreservedPrompt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# Existing prompt\n"
	promptPath := filepath.Join(dir, "prompts", "system.md")
	if err := os.WriteFile(promptPath, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(promptPath, 0o640); err != nil {
		t.Fatal(err)
	}
	digest := planArtifactDigest([]byte(original))
	plan := &Plan{
		Prompt: PromptPlan{BaselineStatus: promptBaselineSourceOnly, ExistingSHA256: digest, CandidateSHA256: planArtifactDigest([]byte("candidate"))},
		Destination: DestinationPlan{OutDir: dir, Files: []DestinationFilePlan{{
			Path: "prompts/system.md", Action: destinationActionPreserve, Ownership: destinationOwnershipConsumer, ReviewedDigest: digest,
		}}},
		Files: map[string]string{"prompts/system.md": "candidate"},
	}
	result, err := buildApplyResult(plan, "sha256:"+strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if result.Prompt.Status != "preserved-existing" || result.Prompt.ActiveSHA256 != digest || result.Files[0].Status != destinationActionPreserve || result.Files[0].Mode != "0640" {
		t.Fatalf("result = %+v", result)
	}
}

func TestPrepareReviewedOutputPathsRejectsConsumerPaths(t *testing.T) {
	dir := t.TempDir()
	_, _, err := prepareReviewedOutputPaths(dir, filepath.Join(dir, "result.json"), "")
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyReviewedWritesValidatedOutputs(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	artifactRoot := t.TempDir()
	jobName := "periodic-project-main"
	buildDir := filepath.Join(artifactRoot, "logs", jobName, "123", "artifacts")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{
		filepath.Join(artifactRoot, "logs", jobName, "123", "prowjob.json"):  `{"metadata":{"name":"periodic-project-main-123"},"spec":{"job":"periodic-project-main","type":"periodic"},"status":{"state":"success","build_id":"123"}}`,
		filepath.Join(artifactRoot, "logs", jobName, "123", "started.json"):  `{"timestamp":1}`,
		filepath.Join(artifactRoot, "logs", jobName, "123", "finished.json"): `{"timestamp":2,"passed":true,"result":"SUCCESS"}`,
		filepath.Join(artifactRoot, "logs", jobName, "123", "build-log.txt"): "build output",
		filepath.Join(buildDir, "junit_01.xml"):                              `<testsuite name="smoke"></testsuite>`,
	}
	for path, content := range artifacts {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan.Project.Storage = project.Storage{Provider: "local", Base: artifactRoot}
	plan.Project.Discovery = project.Discovery{Source: project.DiscoveryBucket, ExactJobs: []string{jobName}}
	projectYAML, err := yaml.Marshal(plan.Project)
	if err != nil {
		t.Fatal(err)
	}
	plan.Files["project.yaml"] = string(projectYAML)
	plan.Discovery = DiscoveryPlan{
		Bucket: "local-fixture", ExactJobs: []string{jobName},
		Jobs: []models.ProwJob{{Name: jobName, JobType: models.JobTypePeriodic, JobID: jobName}},
	}
	plan.Discovery.Digest, err = discoveryPlanDigest(plan.Discovery)
	if err != nil {
		t.Fatal(err)
	}
	plan.Destination.Files, plan.Destination.StaleFiles, err = inspectFileDestination(plan.Destination.OutDir, plan.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	digest, err := WritePlanArtifact(planPath, plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadPlanArtifact(planPath, digest)
	if err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	resultPath := filepath.Join(outputDir, "apply-result.json")
	handoffPath := filepath.Join(outputDir, "setup-handoff.json")
	result, handoff, err := ApplyReviewed(context.Background(), loaded, "", ReviewedApplyOptions{
		PlanDigest: digest, ResultOut: resultPath, HandoffOut: handoffPath, ArtifactSmokeBuilds: 1,
	})
	if err != nil {
		t.Fatalf("ApplyReviewed: %v", err)
	}
	if !result.MatchesReviewedPlan || handoff.Doctor.HasFailures() || len(handoff.ArtifactSmoke.Jobs) != 1 {
		t.Fatalf("result=%+v handoff=%+v", result, handoff)
	}
	if handoff.ArtifactLocation.Provider != "local" || handoff.ArtifactLocation.Base != artifactRoot || handoff.TestInfra.Status != "not_applicable" {
		t.Fatalf("artifact/test-infra handoff = %+v %+v", handoff.ArtifactLocation, handoff.TestInfra)
	}
	root := onboardingRepoRoot(t)
	schemaPath := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "references", "setup-handoff.schema.json")
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile setup handoff schema: %v", err)
	}
	validateSchema := func(path string) {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		instance, err := jsonschema.UnmarshalJSON(file)
		if err != nil {
			t.Fatalf("decode handoff for schema validation: %v", err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("validate handoff schema: %v", err)
		}
	}
	validateSchema(handoffPath)
	script := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "scripts", "validate_setup_handoff.py")
	output, err := exec.Command("python3", script, handoffPath).CombinedOutput()
	if err != nil {
		t.Fatalf("validate handoff: %v\n%s", err, output)
	}
}

func TestApplyReviewedRejectsDigestDifferentFromLoadedArtifact(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadPlanArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "sha256:" + strings.Repeat("f", 64)
	_, _, err = ApplyReviewed(context.Background(), loaded, "", ReviewedApplyOptions{PlanDigest: wrong})
	if err == nil || !strings.Contains(err.Error(), "loaded reviewed artifact") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(loaded.Destination.OutDir); !os.IsNotExist(statErr) {
		t.Fatalf("destination was written despite digest mismatch: %v", statErr)
	}
}
