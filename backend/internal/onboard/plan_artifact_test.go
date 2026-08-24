package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var reviewedPlanDigestPattern = regexp.MustCompile(`Reviewed plan digest: (sha256:[0-9a-f]{64})`)

func testReviewedPlan(t *testing.T) (*Plan, dependencies, *fakeScaffoldWriter) {
	t.Helper()
	deps, _, writer, _ := wizardDependencies("")
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main",
		OutDir: filepath.Join(t.TempDir(), "consumer"), NoPrompt: true,
		AIEnabled: &disabled, GitHubToken: "fixture-github-token", DryRun: true,
		PlanOut: filepath.Join(t.TempDir(), "reviewed-plan.json"),
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightPlan(plan, deps); err != nil {
		t.Fatal(err)
	}
	return plan, deps, writer
}

func TestPlanArtifactRoundTripAndApply(t *testing.T) {
	plan, deps, writer := testReviewedPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("plan artifact mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-github-token") {
		t.Fatal("plan artifact retained the GitHub token")
	}
	loaded, err := ReadPlanArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, plan) {
		t.Fatalf("loaded plan differs\nloaded=%+v\nwant=%+v", loaded, plan)
	}
	if err := applyPlan(context.Background(), loaded, "", deps); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d", writer.writes)
	}
}

func TestRunDryRunWritesReviewedPlanArtifact(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	disabled := false
	planPath := filepath.Join(t.TempDir(), "reviewed-plan.json")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main",
		OutDir: filepath.Join(t.TempDir(), "consumer"), NoPrompt: true,
		AIEnabled: &disabled, DryRun: true, PlanOut: planPath,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 0 {
		t.Fatalf("writes = %d", writer.writes)
	}
	match := reviewedPlanDigestPattern.FindStringSubmatch(out.String())
	if len(match) != 2 {
		t.Fatalf("dry-run output omitted digest:\n%s", out.String())
	}
	if _, err := ReadPlanArtifact(planPath, match[1]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("No scaffold files were written")) {
		t.Fatalf("dry-run output = %s", out.String())
	}
}

func TestRunDryRunBindsRelativeDestinationToReviewedDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	deps, out, _, _ := wizardDependencies("")
	disabled := false
	planPath := filepath.Join(t.TempDir(), "reviewed-plan.json")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main",
		OutDir: ".", NoPrompt: true, AIEnabled: &disabled,
		DryRun: true, PlanOut: planPath,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatal(err)
	}
	match := reviewedPlanDigestPattern.FindStringSubmatch(out.String())
	if len(match) != 2 {
		t.Fatalf("dry-run output omitted digest:\n%s", out.String())
	}
	loaded, err := ReadPlanArtifact(planPath, match[1])
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonicalLocalDestination(work)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Destination.OutDir != want {
		t.Fatalf("destination = %q, want %q", loaded.Destination.OutDir, want)
	}
	if !strings.Contains(out.String(), "Dashboard consumer directory: "+want) {
		t.Fatalf("review did not show absolute destination:\n%s", out.String())
	}
}

func TestPlanArtifactRejectsDigestMismatch(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, digest); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsInvalidPlanWithMatchingDigest(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	plan.Deployment.Mode = "invalid"
	planCopy := *plan
	planCopy.Files = nil
	artifact := planArtifact{SchemaVersion: planArtifactSchemaVersion, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsRelativeLocalDestination(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	plan.Destination.OutDir = "relative-consumer"
	planCopy := *plan
	planCopy.Files = nil
	artifact := planArtifact{SchemaVersion: planArtifactSchemaVersion, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactCanonicalizesSymlinkAncestor(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	base := t.TempDir()
	target := filepath.Join(base, "repo-a")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "current")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	plan.Destination.OutDir = filepath.Join(link, "consumer")
	path := filepath.Join(base, "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadPlanArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonicalLocalDestination(filepath.Join(target, "consumer"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Destination.OutDir != want {
		t.Fatalf("destination = %q, want %q", loaded.Destination.OutDir, want)
	}
}

func TestPlanArtifactRejectsRetargetedSymlinkAncestor(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	base := t.TempDir()
	anchor := filepath.Join(base, "reviewed")
	if err := os.Mkdir(anchor, 0o755); err != nil {
		t.Fatal(err)
	}
	plan.Destination.OutDir = filepath.Join(anchor, "consumer")
	path := filepath.Join(base, "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(base, "reviewed-original")
	if err := os.Rename(anchor, original); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "repo-b")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, digest); err == nil || !strings.Contains(err.Error(), "no longer resolves") {
		t.Fatalf("error = %v", err)
	}
}

func TestWritePlanArtifactRejectsSymlinkDestination(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "consumer")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	plan.Destination.OutDir = link
	if _, err := WritePlanArtifact(filepath.Join(base, "plan.json"), plan); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsSymlink(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	digest, err := WritePlanArtifact(target, plan)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(link, digest); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsExistingOutput(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePlanArtifact(path, plan); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing" {
		t.Fatalf("existing plan changed: %q %v", data, err)
	}
}

func TestPlanArtifactRejectsOutputInsideDestination(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	path := filepath.Join(plan.Destination.OutDir, "onboard-plan.json")
	if _, err := WritePlanArtifact(path, plan); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsOpenPRPlan(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	plan.Destination.OpenPR = true
	plan.Destination.Files = nil
	plan.Destination.StaleFiles = nil
	if _, err := WritePlanArtifact(filepath.Join(t.TempDir(), "plan.json"), plan); err == nil || !strings.Contains(err.Error(), "open-PR") {
		t.Fatalf("error = %v", err)
	}

	planCopy := *plan
	planCopy.Files = nil
	artifact := planArtifact{SchemaVersion: planArtifactSchemaVersion, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "open-pr-plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil || !strings.Contains(err.Error(), "open-PR") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	planCopy := *plan
	planCopy.Files = nil
	base := planArtifact{SchemaVersion: planArtifactSchemaVersion, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"unknown field":  append(valid[:len(valid)-1], []byte(`,"unknown":true}`)...),
		"trailing value": append(append([]byte(nil), valid...), []byte(` {}`)...),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil {
				t.Fatal("invalid plan artifact was accepted")
			}
		})
	}
}

func TestApplyReviewedPlanRejectsDestinationDrift(t *testing.T) {
	plan, deps, writer := testReviewedPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	digest, err := WritePlanArtifact(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadPlanArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	writer.inspection = testPagesDestinationActions("project.yaml")
	if err := applyPlan(context.Background(), loaded, "", deps); err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("writes = %d", writer.writes)
	}
}

func TestPlanArtifactRejectsPreviousSchema(t *testing.T) {
	plan, _, _ := testReviewedPlan(t)
	planCopy := *plan
	planCopy.Files = nil
	artifact := planArtifact{SchemaVersion: planArtifactSchemaVersion - 1, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "old-plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsK8sStorageMetadataMismatch(t *testing.T) {
	_, deps, _ := testReviewedPlan(t)
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main",
		OutDir: filepath.Join(t.TempDir(), "consumer"), NoPrompt: true,
		AIEnabled: &disabled, K8sStorageClass: "shared-rwx", DryRun: true,
		PlanOut: filepath.Join(t.TempDir(), "reviewed-plan.json"),
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	plan.Deployment.K8sStorageClass = "other-rwx"
	if _, err := WritePlanArtifact(filepath.Join(t.TempDir(), "plan.json"), plan); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanArtifactRejectsInvalidStorageMetadata(t *testing.T) {
	t.Run("Pages storage metadata", func(t *testing.T) {
		plan, _, _ := testReviewedPlan(t)
		plan.Deployment.K8sStorageClass = "shared-rwx"
		writeAndReadInvalidPlanArtifact(t, plan, "Pages deployment contains Kubernetes storage metadata")
	})

	t.Run("invalid Kubernetes name", func(t *testing.T) {
		_, deps, _ := testReviewedPlan(t)
		disabled := false
		opts := Options{
			TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
			SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main",
			OutDir: filepath.Join(t.TempDir(), "consumer"), NoPrompt: true,
			AIEnabled: &disabled, K8sStorageClass: "shared-rwx",
		}
		plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
		if err != nil {
			t.Fatal(err)
		}
		plan.Deployment.K8sStorageClass = "INVALID_CLASS"
		plan.Files["deploy/values.yaml"] = strings.ReplaceAll(plan.Files["deploy/values.yaml"], "shared-rwx", "INVALID_CLASS")
		writeAndReadInvalidPlanArtifact(t, plan, "is invalid")
	})
}

func writeAndReadInvalidPlanArtifact(t *testing.T, plan *Plan, want string) {
	t.Helper()
	if err := bindPlanArtifactDestination(plan); err != nil {
		t.Fatal(err)
	}
	planCopy := *plan
	planCopy.Files = nil
	artifact := planArtifact{SchemaVersion: planArtifactSchemaVersion, Plan: planCopy, Files: copyPlanFiles(plan.Files)}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "invalid-plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlanArtifact(path, planArtifactDigest(data)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}
