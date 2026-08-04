package onboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
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
	if len(plan.Destination.Files) != len(plan.Files) {
		t.Fatalf("destination files = %+v", plan.Destination.Files)
	}
	for _, file := range plan.Destination.Files {
		if file.Action != destinationActionCreate {
			t.Fatalf("destination file = %+v", file)
		}
	}
}

func TestBuildPlan_DeferredAIDropsProviderCoordinates(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main", OutDir: "out", NoPrompt: true,
		AIEnabled: &disabled, AIAPI: "responses", AIEndpoint: "https://private.example/v1/responses", AIModel: "private-model",
		deferDeploymentAI: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if plan.Deployment.AIEnabled || plan.Deployment.AIAPI != "" || plan.Deployment.Endpoint != "" || plan.Deployment.Model != "" {
		t.Fatalf("disabled deployment retained provider coordinates: %+v", plan.Deployment)
	}
	all := plan.Files["deploy/values.yaml"] + plan.Files["deploy/README.md"]
	for _, private := range []string{opts.AIEndpoint, opts.AIModel} {
		if strings.Contains(all, private) {
			t.Fatalf("disabled scaffold retained %q:\n%s", private, all)
		}
	}
}

func TestBuildPlan_FlagDisabledAIPreservesProviderSeed(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main", OutDir: "out", NoPrompt: true,
		AIEnabled: &disabled, AIAPI: "responses", AIEndpoint: "https://provider.example/v1/responses", AIModel: "seed-model",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if plan.Deployment.AIEnabled || plan.Deployment.AIAPI != "responses" || plan.Deployment.Endpoint != opts.AIEndpoint || plan.Deployment.Model != opts.AIModel {
		t.Fatalf("flag-disabled deployment lost provider seed: %+v", plan.Deployment)
	}
	values := plan.Files["deploy/values.yaml"]
	for _, want := range []string{opts.AIEndpoint, opts.AIModel} {
		if !strings.Contains(values, want) {
			t.Fatalf("flag-disabled values missing %q:\n%s", want, values)
		}
	}
}

func TestBuildPlan_DeferredDeploymentKeepsSeparatePromptProvider(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	deps.prompts = &fakePromptBuilder{drafted: true}
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main", OutDir: "out",
		AIEnabled: &disabled, deferDeploymentAI: true,
		AIToken: "fixture-ai-token", AIAPI: "responses",
		AIEndpoint: "https://draft.example/v1/responses", AIModel: "draft-model",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if plan.Deployment.AIEnabled || plan.Deployment.Endpoint != "" || plan.Deployment.Model != "" {
		t.Fatalf("deferred deployment retained provider: %+v", plan.Deployment)
	}
	if plan.Prompt.Endpoint != opts.AIEndpoint || plan.Prompt.Model != opts.AIModel {
		t.Fatalf("prompt provider = %+v", plan.Prompt)
	}
	if strings.Contains(plan.Files["deploy/values.yaml"], opts.AIEndpoint) {
		t.Fatalf("draft provider leaked into deployment values:\n%s", plan.Files["deploy/values.yaml"])
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
	if err := applyPlan(context.Background(), plan, token, deps); err == nil || !strings.Contains(err.Error(), "contains the supplied GitHub credential") {
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

func TestApply_RejectsGitHubTokenInPlanMetadataBeforeValidation(t *testing.T) {
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
	plan.Destination.OutDir = token
	plan.Deployment.Mode = token
	err = applyPlan(context.Background(), plan, token, deps)
	if err == nil || !strings.Contains(err.Error(), "contains the supplied GitHub credential") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("credential leaked into error: %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("credential-bearing plan wrote %d time(s)", writer.writes)
	}
}

func TestBuildPlanPassesExistingJobEvidenceToPromptBuilder(t *testing.T) {
	for name, planning := range map[string]planningContext{
		"flagged": {},
		"interactive": {discovery: &DiscoveryReport{SourceRepo: Repo{Owner: "example", Name: "project", FullName: "example/project"}, MatchingJobs: []jobconfig.JobDefinition{{
			Name: "periodic-project-main", JobType: models.JobTypePeriodic, ConfigFile: "config/jobs.yaml",
			Refs:        []jobconfig.RepoRef{{Org: "example", Repo: "project", BaseRef: "main"}},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-b, dashboard-a"},
		}}}},
	} {
		t.Run(name, func(t *testing.T) {
			deps, _, _, sweeper := wizardDependencies("")
			sweeper.jobs = []models.ProwJob{{
				Name: "periodic-project-main", JobType: models.JobTypePeriodic,
				ConfigFile: "config/jobs.yaml", Branch: "main",
			}}
			prompts := &fakePromptBuilder{}
			deps.prompts = prompts
			opts := Options{
				TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
				SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
			}
			if _, err := buildPlan(context.Background(), opts, planning, deps); err != nil {
				t.Fatalf("buildPlan: %v", err)
			}
			if prompts.calls != 1 || prompts.gotInput.ProjectName != "Project" || prompts.gotInput.SourceRepo.FullName != "example/project" {
				t.Fatalf("prompt input = %+v, calls=%d", prompts.gotInput, prompts.calls)
			}
			if len(prompts.gotInput.Jobs) != 1 || prompts.gotInput.Jobs[0].Name != "periodic-project-main" {
				t.Fatalf("prompt jobs = %+v", prompts.gotInput.Jobs)
			}
			if name == "interactive" && !reflect.DeepEqual(prompts.gotInput.Jobs[0].Dashboards, []string{"dashboard-a", "dashboard-b"}) {
				t.Fatalf("interactive dashboards = %v", prompts.gotInput.Jobs[0].Dashboards)
			}
		})
	}
}

func TestApply_RejectsDestinationChangesAfterReview(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	dir := t.TempDir()
	deps.files = localScaffoldWriter{}
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: dir, NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte("unexpected"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyPlan(context.Background(), plan, "", deps); err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("applyPlan error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("fake writer writes = %d", writer.writes)
	}
	content, err := os.ReadFile(filepath.Join(dir, "project.yaml"))
	if err != nil || string(content) != "unexpected" {
		t.Fatalf("destination changed: %q %v", content, err)
	}
}

func TestBuildPlanDoesNotRenderMachineSpecificOutputPath(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main",
		OutDir: "/private/machine-specific/dashboard", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	for path, content := range plan.Files {
		if strings.Contains(content, opts.OutDir) {
			t.Fatalf("generated file %s contains output path %q", path, opts.OutDir)
		}
	}
}

func TestValidatePlanRejectsUnnormalizedDestination(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true,
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	plan.Destination.OutDir = " out "
	if err := validatePlan(plan); err == nil || !strings.Contains(err.Error(), "not normalized") {
		t.Fatalf("validatePlan error = %v", err)
	}
}
