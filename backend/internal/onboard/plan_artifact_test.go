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
		AIEnabled: &disabled, GitHubToken: "fixture-github-token",
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
	if loaded.Destination.OutDir != work {
		t.Fatalf("destination = %q, want %q", loaded.Destination.OutDir, work)
	}
	if !strings.Contains(out.String(), "Dashboard consumer directory: "+work) {
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
