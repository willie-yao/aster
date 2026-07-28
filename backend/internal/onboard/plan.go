package onboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Plan is a validated, credential-free scaffold ready to apply.
type Plan struct {
	Project       project.Config
	Files         map[string]string
	DashboardRepo string
	Mode          string
	OutDir        string
	OpenPR        bool
	Categories    []project.CategoryRule
}

type plannerDependencies struct {
	discover func(context.Context, *project.Config, bool) ([]models.ProwJob, error)
	prompt   func(context.Context, Options, scaffoldData) (string, error)
}

// Run plans and applies a scaffold. Planning performs no filesystem or GitHub
// writes; application starts only after the full scaffold validates.
func Run(ctx context.Context, opts Options) error {
	plan, err := BuildPlan(ctx, opts)
	if err != nil {
		return err
	}
	return Apply(ctx, plan, opts.GitHubToken)
}

// BuildPlan discovers jobs, renders every file, and validates project.yaml
// without writing files or opening a pull request.
func BuildPlan(ctx context.Context, opts Options) (*Plan, error) {
	return buildPlan(ctx, opts, plannerDependencies{
		discover: discover,
		prompt:   buildSystemPrompt,
	})
}

func buildPlan(ctx context.Context, opts Options, deps plannerDependencies) (*Plan, error) {
	if err := validateOptions(&opts); err != nil {
		return nil, err
	}

	jobs, err := deps.discover(ctx, sweepConfig(opts), opts.IncludePresubmits)
	if err != nil {
		return nil, fmt.Errorf("job sweep: %w", err)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("discovery found 0 jobs for the given input; check the testgrid dashboard name or bucket before scaffolding")
	}
	jobNames := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobNames = append(jobNames, job.Name)
	}
	sort.Strings(jobNames)
	fmt.Printf("✓ discovery found %d jobs\n", len(jobNames))

	categories := InferCategories(jobNames)
	data := buildScaffoldData(opts, categories)
	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		return nil, fmt.Errorf("rendering project.yaml: %w", err)
	}
	parsed, err := project.Parse([]byte(projectYAML))
	if err != nil {
		return nil, fmt.Errorf("generated project.yaml failed validation: %w", err)
	}

	files := map[string]string{"project.yaml": projectYAML}
	dashboardOwner, dashboardName := splitRepo(opts.DashboardRepo)
	switch opts.Mode {
	case modeK8s:
		if files["deploy/values.yaml"], err = render(k8sValuesTmpl, data); err != nil {
			return nil, err
		}
		if files["deploy/README.md"], err = render(k8sDeployReadmeTmpl, data); err != nil {
			return nil, err
		}
	default:
		if files[".github/workflows/deploy.yml"], err = render(deployYAMLTmpl, data); err != nil {
			return nil, err
		}
		if files["CHECKLIST.md"], err = render(checklistTmpl, checklistData{
			Name: data.Name, DashboardOwner: dashboardOwner, DashboardName: dashboardName, EngineRef: data.EngineRef,
		}); err != nil {
			return nil, err
		}
	}
	files["prompts/system.md"], err = deps.prompt(ctx, opts, data)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Project:       *parsed,
		Files:         files,
		DashboardRepo: opts.DashboardRepo,
		Mode:          opts.Mode,
		OutDir:        opts.OutDir,
		OpenPR:        opts.OpenPR,
		Categories:    categories,
	}, nil
}

// Apply writes a fully validated plan locally or opens the explicitly requested
// scaffold pull request.
func Apply(ctx context.Context, plan *Plan, githubToken string) error {
	if plan == nil {
		return fmt.Errorf("onboarding plan is nil")
	}
	dashboardOwner, dashboardName := splitRepo(plan.DashboardRepo)
	if plan.OpenPR {
		title := fmt.Sprintf("Add %s prow-ai-dashboard scaffold", plan.Project.Name)
		httpClient := &http.Client{Timeout: 30 * time.Second}
		fmt.Printf("⤴ opening a scaffold PR against %s…\n", plan.DashboardRepo)
		url, err := ghpr.NewClient(httpClient, githubToken).OpenPR(ctx, ghpr.Request{
			Owner:        dashboardOwner,
			Repo:         dashboardName,
			Files:        plan.Files,
			BranchPrefix: "onboard/scaffold",
			Title:        title,
			Body:         scaffoldPRBody(plan.Project.Name, plan.Mode),
		})
		if err != nil {
			return fmt.Errorf("opening scaffold PR: %w", err)
		}
		fmt.Printf("\n✓ opened scaffold PR: %s\n", url)
		fmt.Printf("  review it (especially prompts/system.md), then follow %s\n", scaffoldGuide(plan.Mode))
		return nil
	}

	if err := writeFiles(plan.OutDir, plan.Files); err != nil {
		return err
	}
	fmt.Printf("\n✓ scaffold written to %s/\n", plan.OutDir)
	fmt.Printf("  next: review prompts/system.md and project.yaml, then follow %s\n", scaffoldGuide(plan.Mode))
	if len(plan.Categories) > 0 {
		fmt.Printf("  inferred %d categor%s from job names (review/reorder them)\n", len(plan.Categories), plural(len(plan.Categories)))
	}
	return nil
}

func sortedFilePaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
