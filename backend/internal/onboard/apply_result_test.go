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

type handoffValidation struct {
	schemaErr, pythonErr error
	pythonOutput         []byte
}

func setupHandoffValidators(t *testing.T) func(path string) handoffValidation {
	t.Helper()
	root := onboardingRepoRoot(t)
	schemaPath := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "references", "setup-handoff.schema.json")
	schema, err := jsonschema.NewCompiler().Compile(schemaPath)
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
	script := filepath.Join(root, ".agents", "skills", "setup-aster-consumer", "scripts", "validate_setup_handoff.py")
	return func(path string) handoffValidation {
		schemaErr := schemaError(path)
		pythonOutput, pythonErr := exec.Command("python3", script, path).CombinedOutput()
		return handoffValidation{schemaErr: schemaErr, pythonErr: pythonErr, pythonOutput: pythonOutput}
	}
}

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
	validate := setupHandoffValidators(t)
	validation := validate(path)
	if validation.schemaErr != nil {
		t.Errorf("validate handoff schema: %v", validation.schemaErr)
	}
	if validation.pythonErr != nil {
		t.Errorf("validate handoff: %v\n%s", validation.pythonErr, validation.pythonOutput)
	}
	legacy := handoff
	legacy.SchemaVersion = 1
	legacyPath := filepath.Join(t.TempDir(), "setup-handoff-v1-pages.json")
	if err := writePrivateJSON(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	validation = validate(legacyPath)
	if validation.schemaErr == nil {
		t.Error("JSON Schema accepted a version 1 handoff")
	}
	if validation.pythonErr == nil {
		t.Errorf("Python validator accepted a version 1 handoff: %s", validation.pythonOutput)
	}
	engineToken, engineString := `"engine": {`, "\"engine\": {\n    \"modified\": \"false\","
	if handoff.Engine.Modified {
		engineToken, engineString = `"modified": true`, `"modified": "false"`
	}
	for _, tc := range []struct {
		name, source, from, to string
		wantErr                bool
	}{
		{"v1-float", legacyPath, `"schema_version": 1`, `"schema_version": 1.0`, true},
		{"v2-float", path, `"schema_version": 2`, `"schema_version": 2.0`, false},
		{"boolean-version", path, `"schema_version": 2`, `"schema_version": true`, true},
		{"apply-version-float", path, `    "schema_version": 1`, `    "schema_version": 1.0`, false},
		{"apply-version-boolean", path, `    "schema_version": 1`, `    "schema_version": true`, true},
		{"smoke-builds-float", path, `"builds_per_job": 0`, `"builds_per_job": 0.0`, false},
		{"smoke-builds-boolean", path, `"builds_per_job": 0`, `"builds_per_job": true`, true},
		{"engine-modified-string", path, engineToken, engineString, true},
		{"ai-enabled-string", path, `"ai_enabled": false`, `"ai_enabled": "false"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Replace(string(raw), tc.from, tc.to, 1)
			if text == string(raw) {
				t.Fatalf("variant token %q not found", tc.from)
			}
			if _, err := jsonschema.UnmarshalJSON(strings.NewReader(text)); err != nil {
				t.Fatalf("invalid JSON variant: %v", err)
			}
			variant := filepath.Join(t.TempDir(), "setup-handoff-"+tc.name+".json")
			if err := os.WriteFile(variant, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			validation := validate(variant)
			if (validation.schemaErr != nil) != tc.wantErr {
				t.Errorf("JSON Schema error = %v, want error %t", validation.schemaErr, tc.wantErr)
			}
			if (validation.pythonErr != nil) != tc.wantErr {
				t.Errorf("Python validator error = %v, want error %t\n%s", validation.pythonErr, tc.wantErr, validation.pythonOutput)
			}
		})
	}
	handoff.Deployment.Mode = modeK8s
	handoff.Deployment.K8sStorageClass = "shared-rwx"
	k8sPath := filepath.Join(t.TempDir(), "setup-handoff-k8s.json")
	if err := writePrivateJSON(k8sPath, handoff); err != nil {
		t.Fatal(err)
	}
	validation = validate(k8sPath)
	if validation.schemaErr != nil {
		t.Errorf("validate Kubernetes handoff schema: %v", validation.schemaErr)
	}
	if validation.pythonErr != nil {
		t.Errorf("validate Kubernetes handoff: %v\n%s", validation.pythonErr, validation.pythonOutput)
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
	validate := setupHandoffValidators(t)
	validation := validate(handoffPath)
	if validation.schemaErr != nil {
		t.Errorf("validate handoff schema: %v", validation.schemaErr)
	}
	if validation.pythonErr != nil {
		t.Errorf("validate handoff: %v\n%s", validation.pythonErr, validation.pythonOutput)
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
