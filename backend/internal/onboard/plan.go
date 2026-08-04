package onboard

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

const onboardingDiscoveryTimeout = 5 * time.Minute

type planningContext struct {
	discovery *DiscoveryReport
	selected  *DashboardCandidate
}

func buildPlan(ctx context.Context, opts Options, planning planningContext, deps dependencies) (*Plan, error) {
	if err := validateOptions(&opts); err != nil {
		return nil, err
	}

	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, onboardingDiscoveryTimeout)
	jobs, err := deps.sweeper.Discover(discoveryCtx, sweepConfig(opts), includePresubmits(opts))
	cancelDiscovery()
	if err != nil {
		return nil, fmt.Errorf("job sweep: %w", err)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("discovery found 0 jobs for the given input; check the TestGrid dashboard name or bucket before scaffolding")
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Name != jobs[j].Name {
			return jobs[i].Name < jobs[j].Name
		}
		if jobs[i].JobType != jobs[j].JobType {
			return jobs[i].JobType < jobs[j].JobType
		}
		return jobs[i].Repo < jobs[j].Repo
	})
	jobNames := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobNames = append(jobNames, job.Name)
	}
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
	sourceRepo, err := NormalizeGitHubRepo(opts.SourceRepo)
	if err != nil {
		return nil, fmt.Errorf("--source-repo: %w", err)
	}
	if planning.discovery != nil {
		sourceRepo = planning.discovery.SourceRepo
	}
	dashboardRepo, err := NormalizeGitHubRepo(opts.DashboardRepo)
	if err != nil {
		return nil, fmt.Errorf("--dashboard-repo: %w", err)
	}
	switch opts.Mode {
	case modeK8s:
		if files["deploy/values.yaml"], err = render(k8sValuesTmpl, data); err != nil {
			return nil, fmt.Errorf("rendering deploy/values.yaml: %w", err)
		}
		if files["deploy/README.md"], err = render(k8sDeployReadmeTmpl, data); err != nil {
			return nil, fmt.Errorf("rendering deploy/README.md: %w", err)
		}
	default:
		if files[".github/workflows/deploy.yml"], err = render(deployYAMLTmpl, data); err != nil {
			return nil, fmt.Errorf("rendering deploy workflow: %w", err)
		}
		if files["CHECKLIST.md"], err = render(checklistTmpl, checklistData{
			Name: data.Name, DashboardOwner: dashboardRepo.Owner, DashboardName: dashboardRepo.Name,
			EngineRef: data.EngineRef, AIEnabled: data.AIEnabled, AIAPI: data.AIAPI,
		}); err != nil {
			return nil, fmt.Errorf("rendering CHECKLIST.md: %w", err)
		}
	}
	var definitions []jobconfig.JobDefinition
	if planning.discovery != nil {
		definitions = planning.discovery.MatchingJobs
	}
	promptInput := promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  sourceRepo,
		Jobs:        buildPromptJobSummaries(jobs, definitions, sourceRepo, opts.TestGrid),
	}
	prompt, promptResult, err := deps.prompts.Build(ctx, opts, data, promptInput)
	if err != nil {
		return nil, fmt.Errorf("rendering prompts/system.md: %w", err)
	}
	if opts.RequirePromptDraft && promptResult.Status != promptStatusAPIDraft {
		failure := promptResult.Failure
		if failure == nil {
			failure = &promptPreparationFailure{Stage: promptStageFinalPromptValidation, Category: promptFailurePromptValidation}
		}
		return nil, &requiredPromptDraftError{failure: failure}
	}
	files["prompts/system.md"] = prompt
	if err := validateRenderedFilesNoCredentials(opts, files); err != nil {
		return nil, err
	}

	catalogRevision := ""
	var testGridProvenance *Inferred[string]
	if planning.discovery != nil {
		catalogRevision = planning.discovery.CatalogRevision
	}
	if planning.selected != nil {
		value := Inferred[string]{Value: planning.selected.Dashboard, Source: "ranked kubernetes/test-infra jobs for " + sourceRepo.FullName, Confidence: candidateConfidence(*planning.selected)}
		testGridProvenance = &value
	}
	deployment := DeploymentPlan{Mode: opts.Mode, AIEnabled: effectiveAIEnabled(opts)}
	if !opts.deferDeploymentAI {
		deployment.AIAPI = deploymentAIAPI(opts)
		deployment.Endpoint = deploymentAIEndpoint(opts)
		deployment.Model = deploymentAIModel(opts)
	}
	plan := &Plan{
		SourceRepo:    sourceRepo,
		DashboardRepo: dashboardRepo,
		Deployment:    deployment,
		Discovery: DiscoveryPlan{
			TestGrid: opts.TestGrid, Bucket: opts.Bucket, GCSWebBase: opts.GCSWebBase,
			CatalogRevision: catalogRevision, Jobs: append([]models.ProwJob(nil), jobs...),
			SelectedCandidate: copyCandidate(planning.selected), TestGridProvenance: testGridProvenance,
		},
		Project:     *parsed,
		Prompt:      promptResult.promptPlan(opts),
		Destination: DestinationPlan{OutDir: opts.OutDir, OpenPR: opts.OpenPR},
		Files:       files,
		Provenance: map[string]Inferred[string]{
			"source_repo":    {Value: sourceRepo.FullName, Source: "explicit input", Confidence: ConfidenceHigh},
			"dashboard_repo": {Value: dashboardRepo.FullName, Source: "explicit or confirmed input", Confidence: ConfidenceHigh},
		},
	}
	if planning.discovery != nil {
		plan.Provenance["source_repo"] = Inferred[string]{Value: sourceRepo.FullName, Source: planning.discovery.MetadataSource, Confidence: ConfidenceHigh}
		plan.Provenance["project_id"] = confirmedInference(opts.ID, planning.discovery.Identity.ID, "interactive input")
		plan.Provenance["project_name"] = confirmedInference(opts.Name, planning.discovery.Identity.Name, "interactive input")
		plan.Provenance["dashboard_repo"] = confirmedInference(dashboardRepo.FullName, planning.discovery.DashboardRepo, "interactive input")
	}
	return plan, nil
}

func candidateConfidence(candidate DashboardCandidate) Confidence {
	if candidate.MatchingJobs >= 3 {
		return ConfidenceHigh
	}
	if candidate.MatchingJobs > 0 {
		return ConfidenceMedium
	}
	return ConfidenceLow
}

func validateRenderedFilesNoCredentials(opts Options, files map[string]string) error {
	for _, credential := range []string{opts.AIToken, opts.GitHubToken} {
		if credential == "" {
			continue
		}
		for _, content := range files {
			if strings.Contains(content, credential) {
				return fmt.Errorf("rendered onboarding files contained a credential; no output was applied")
			}
		}
	}
	return nil
}

func confirmedInference(value string, suggestion Inferred[string], editedSource string) Inferred[string] {
	if value == suggestion.Value {
		return suggestion
	}
	return Inferred[string]{Value: value, Source: editedSource, Confidence: ConfidenceHigh}
}

func copyCandidate(candidate *DashboardCandidate) *DashboardCandidate {
	if candidate == nil {
		return nil
	}
	copy := *candidate
	copy.JobNames = append([]string(nil), candidate.JobNames...)
	return &copy
}

func effectiveAIEnabled(opts Options) bool {
	return opts.AIEnabled == nil || *opts.AIEnabled
}

type defaultPromptBuilder struct {
	out io.Writer
	err io.Writer
}

func (b defaultPromptBuilder) Build(ctx context.Context, opts Options, data scaffoldData, input promptDraftInput) (string, promptPreparationResult, error) {
	return buildSystemPrompt(ctx, opts, data, input, b.out, b.err)
}
