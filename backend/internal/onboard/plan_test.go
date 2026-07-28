package onboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPlan_RendersAndValidatesWithoutWriting(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("planning wrote %d time(s)", writer.writes)
	}
	if plan.Project.ID != "project" {
		t.Fatalf("project id = %q", plan.Project.ID)
	}
	for _, path := range []string{"project.yaml", "prompts/system.md", ".github/workflows/deploy.yml", "CHECKLIST.md"} {
		if plan.Files[path] == "" {
			t.Errorf("planned file %q is empty", path)
		}
	}
}

func TestBuildPlan_DoesNotRetainCredentials(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
		AIToken: "fixture-ai-token", GitHubToken: "fixture-github-token",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	all := string(encoded)
	for _, content := range plan.Files {
		all += content
	}
	for _, credential := range []string{opts.AIToken, opts.GitHubToken} {
		if strings.Contains(all, credential) {
			t.Fatalf("plan retained credential %q", credential)
		}
	}
}

func TestBuildPlan_OpenPRDoesNotRequireWriteCredential(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", NoPrompt: true, OpenPR: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !plan.Destination.OpenPR {
		t.Fatal("plan lost the explicit open-PR request")
	}
	if err := applyPlan(context.Background(), plan, "", deps); err == nil || !strings.Contains(err.Error(), "needs a GitHub token") {
		t.Fatalf("applyPlan error = %v", err)
	}
}

func TestApply_RejectsModifiedPlanBeforeWriting(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	plan.Files["../outside"] = "unsafe"
	if err := applyPlan(context.Background(), plan, "", deps); err == nil || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("modified plan wrote %d time(s)", writer.writes)
	}
}

func TestApply_RejectsProjectMutationBeforeWriting(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	plan.Files["project.yaml"] = "id: invalid\n"
	if err := applyPlan(context.Background(), plan, "", deps); err == nil || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("modified plan wrote %d time(s)", writer.writes)
	}
}

func TestApply_RejectsMismatchedDashboardRepoFields(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	plan.DashboardRepo.Owner = "other"
	if err := applyPlan(context.Background(), plan, "", deps); err == nil || !strings.Contains(err.Error(), "do not match full_name") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("mismatched plan wrote %d time(s)", writer.writes)
	}
}

func TestApply_RejectsGitHubTokenInPlanFiles(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	token := "fixture-github-token"
	plan.Files["prompts/system.md"] = token
	if err := applyPlan(context.Background(), plan, token, deps); err == nil || !strings.Contains(err.Error(), "contain the supplied GitHub credential") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("credential-bearing plan wrote %d time(s)", writer.writes)
	}
}

func TestNormalizeRepositories_ChecksCredentialsBeforeParsing(t *testing.T) {
	opts := Options{SourceRepo: "fixture-github-token", DashboardRepo: "example/dashboard", GitHubToken: "fixture-github-token"}
	err := normalizeRepositories(&opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.GitHubToken) {
		t.Fatalf("credential leaked into error: %v", err)
	}
}
