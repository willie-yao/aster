package onboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestBuildPlan_RendersAndValidatesWithoutWriting(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "scaffold")
	opts := testOpts()
	opts.OutDir = outDir

	plan, err := buildPlan(context.Background(), opts, plannerDependencies{
		discover: func(context.Context, *project.Config, bool) ([]models.ProwJob, error) {
			return []models.ProwJob{
				{Name: "periodic-project-aks-main", JobType: models.JobTypePeriodic},
				{Name: "periodic-project-conformance-main", JobType: models.JobTypePeriodic},
			}, nil
		},
		prompt: func(context.Context, Options, scaffoldData) (string, error) {
			return "# Prompt\n\nReview me.\n", nil
		},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("planning created output path %s", outDir)
	}
	if plan.Project.ID != "my-proj" {
		t.Fatalf("project id = %q", plan.Project.ID)
	}
	for _, path := range []string{"project.yaml", "prompts/system.md", ".github/workflows/deploy.yml", "CHECKLIST.md"} {
		if plan.Files[path] == "" {
			t.Errorf("planned file %q is empty", path)
		}
	}
	if _, err := project.Parse([]byte(plan.Files["project.yaml"])); err != nil {
		t.Fatalf("planned project.yaml: %v", err)
	}
}

func TestBuildPlan_DoesNotRetainCredentials(t *testing.T) {
	opts := testOpts()
	opts.OutDir = "out"
	opts.AIToken = "fixture-ai-token"
	opts.AIEndpoint = "https://provider.example/v1/chat/completions"
	opts.AIModel = "model"
	opts.GitHubToken = "fixture-github-token"

	plan, err := buildPlan(context.Background(), opts, plannerDependencies{
		discover: func(context.Context, *project.Config, bool) ([]models.ProwJob, error) {
			return []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic}}, nil
		},
		prompt: func(context.Context, Options, scaffoldData) (string, error) {
			return "# Prompt\n", nil
		},
	})
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
	opts := testOpts()
	opts.OpenPR = true
	opts.GitHubToken = ""

	plan, err := buildPlan(context.Background(), opts, plannerDependencies{
		discover: func(context.Context, *project.Config, bool) ([]models.ProwJob, error) {
			return []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic}}, nil
		},
		prompt: func(context.Context, Options, scaffoldData) (string, error) {
			return "# Prompt\n", nil
		},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !plan.OpenPR {
		t.Fatal("plan lost the explicit open-PR request")
	}
	if err := Apply(context.Background(), plan, ""); err == nil || !strings.Contains(err.Error(), "needs a GitHub token") {
		t.Fatalf("Apply error = %v", err)
	}
}
