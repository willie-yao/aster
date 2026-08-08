package onboard

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard/promptauthor"
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
	sweep, err := deps.sweeper.Discover(discoveryCtx, sweepConfig(opts), includePresubmits(opts))
	cancelDiscovery()
	if err != nil {
		return nil, fmt.Errorf("job sweep: %w", err)
	}
	jobs := sweep.Jobs
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
	warnings := []string{}
	if planning.discovery != nil {
		warnings = append(warnings, planning.discovery.Warnings...)
	}
	sourceRevision := SourceRevisionPlan{Status: sourceRevisionUnresolved}
	if deps.sourceRevision != nil {
		resolved, resolveErr := deps.sourceRevision.Resolve(ctx, sourceRepo, opts.GitHubToken)
		sourceRevision = resolved
		if resolveErr != nil {
			warnings = append(warnings, "Source revision could not be pinned. Treat the source ref as unresolved until it is recorded explicitly.")
		}
	}
	if sourceRevision.Ref != "" {
		sourceRepo.Branch = sourceRevision.Ref
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
		ProjectName:          data.Name,
		SourceRepo:           sourceRepo,
		SourceRevision:       sourceRevision.Revision,
		SourceRevisionStatus: sourceRevision.Status,
		Jobs:                 buildPromptJobSummaries(jobs, definitions, sourceRepo, opts.TestGrid),
	}
	prompt, promptResult, err := deps.prompts.Build(ctx, opts, data, promptInput)
	if err != nil {
		return nil, fmt.Errorf("rendering prompts/system.md: %w", err)
	}
	if opts.RequirePromptDraft && promptResult.Status != promptStatusAgentDraft {
		failure := promptResult.Failure
		if failure == nil {
			failure = &promptPreparationFailure{Stage: promptStageFinalPromptValidation, Category: promptFailurePromptValidation}
		}
		return nil, &requiredPromptDraftError{failure: failure}
	}
	files["prompts/system.md"] = prompt
	if promptResult.Handoff != "" {
		files["PROMPT_HANDOFF.md"] = promptResult.Handoff
		files[".opencode/skills/system-prompt-generation/SKILL.md"] = promptauthor.SkillContent()
	}
	if err := validateRenderedFilesNoCredentials(opts, files); err != nil {
		return nil, err
	}

	catalogRevision := sweep.CatalogRevision
	var testGridProvenance *Inferred[string]
	if catalogRevision == "" && planning.discovery != nil {
		catalogRevision = planning.discovery.CatalogRevision
	}
	if planning.selected != nil {
		value := Inferred[string]{Value: planning.selected.Dashboard, Source: "ranked kubernetes/test-infra jobs for " + sourceRepo.FullName, Confidence: candidateConfidence(*planning.selected)}
		testGridProvenance = &value
	}
	deployment := DeploymentPlan{
		Mode: opts.Mode, Reasons: deploymentReasons(opts), ArtifactAccess: effectiveArtifactAccess(opts),
		AIEnabled: effectiveAIEnabled(opts),
	}
	if !opts.deferDeploymentAI {
		deployment.AIAPI = deploymentAIAPI(opts)
		deployment.Endpoint = deploymentAIEndpoint(opts)
		deployment.Model = deploymentAIModel(opts)
	}
	discoveryPlan := DiscoveryPlan{
		TestGrid: opts.TestGrid, Bucket: opts.Bucket, GCSWebBase: opts.GCSWebBase, ExactJobs: append([]string(nil), opts.ExactJobs...),
		CatalogRevision: catalogRevision, Jobs: append([]models.ProwJob(nil), jobs...),
		SelectedCandidate: copyCandidate(planning.selected), TestGridProvenance: testGridProvenance,
	}
	discoveryPlan.Digest, err = discoveryPlanDigest(discoveryPlan)
	if err != nil {
		return nil, fmt.Errorf("hashing discovery output: %w", err)
	}
	promptPlan := promptResult.promptPlan(opts)
	promptPlan.BaselineStatus = promptBaselineSourceOnly
	promptPlan.CandidateSHA256 = planArtifactDigest([]byte(prompt))
	engine := currentEnginePlan()
	if engine.Revision == "" {
		warnings = append(warnings, "Engine revision could not be resolved. Record the resolved module version or commit before treating this setup as reproducible.")
	}
	if engine.Modified {
		warnings = append(warnings, "The onboarding engine checkout has local modifications. The recorded commit does not fully reproduce the generated plan.")
	}
	if deployment.ArtifactAccess == artifactAccessUnknown {
		warnings = append(warnings, "Artifact access is unresolved. Confirm privacy, authentication, and runner reachability before deployment.")
	}
	if discoveryPlan.TestGrid != "" && discoveryPlan.CatalogRevision == "" {
		warnings = append(warnings, "The TestGrid discovery source did not provide a pinned test-infra catalog revision.")
	}
	plan := &Plan{
		Engine:         engine,
		SourceRepo:     sourceRepo,
		SourceRevision: sourceRevision,
		DashboardRepo:  dashboardRepo,
		Deployment:     deployment,
		Discovery:      discoveryPlan,
		Project:        *parsed,
		Prompt:         promptPlan,
		Destination: DestinationPlan{
			OutDir: opts.OutDir, OpenPR: opts.OpenPR, UpdateExisting: opts.UpdateExisting,
			ReplaceConsumerOwned: opts.ReplaceConsumerOwned,
		},
		Warnings: warnings,
		Files:    files,
		Provenance: map[string]Inferred[string]{
			"source_repo":    {Value: sourceRepo.FullName, Source: "explicit input", Confidence: ConfidenceHigh},
			"dashboard_repo": {Value: dashboardRepo.FullName, Source: "explicit or confirmed input", Confidence: ConfidenceHigh},
		},
	}
	if planning.discovery != nil {
		plan.Provenance["source_repo"] = Inferred[string]{Value: sourceRepo.FullName, Source: planning.discovery.MetadataSource, Confidence: ConfidenceHigh}
		plan.Provenance["project_id"] = confirmedInference(opts.ID, planning.discovery.Identity.ID, "interactive input")
		plan.Provenance["project_name"] = confirmedInference(opts.Name, planning.discovery.Identity.Name, "interactive input")
		plan.Provenance["dashboard_repo"] = confirmedInference(dashboardRepo.FullName, planning.discovery.DashboardRepo, "confirmed dashboard repository input")
	}
	if opts.PlanOut != "" {
		if err := bindPlanArtifactDestination(plan); err != nil {
			return nil, err
		}
	}
	if err := inspectPlanDestination(plan, deps); err != nil {
		return nil, fmt.Errorf("planning dashboard consumer directory: %w", err)
	}
	return plan, nil
}

const (
	promptBaselineSourceOnly    = "source-only-unvalidated"
	artifactAccessPublic        = "public"
	artifactAccessAuthenticated = "authenticated"
	artifactAccessPrivate       = "private"
	artifactAccessUnknown       = "unknown"
)

func effectiveArtifactAccess(opts Options) string {
	value := strings.ToLower(strings.TrimSpace(opts.ArtifactAccess))
	if value == "" {
		return artifactAccessUnknown
	}
	return value
}

func deploymentReasons(opts Options) []string {
	seen := map[string]struct{}{}
	var reasons []string
	for _, reason := range opts.ModeReasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		reasons = append(reasons, reason)
	}
	if len(reasons) > 0 {
		return reasons
	}
	if opts.Mode == modeK8s {
		return []string{"Kubernetes selected. Confirm this is required by private artifacts, cluster-local provider reachability, persistent state, authenticated admin actions, or cluster-local endpoints."}
	}
	return []string{"GitHub Pages selected. Confirm artifacts and the AI provider are reachable from GitHub Actions and that persistent authenticated admin actions are not required."}
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
	for _, credential := range onboardingCredentialValues(opts) {
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
	err    io.Writer
	author promptauthor.Runtime
}

func (b defaultPromptBuilder) Build(ctx context.Context, opts Options, data scaffoldData, input promptDraftInput) (string, promptPreparationResult, error) {
	switch effectivePromptMode(opts) {
	case promptModeAgent:
		a := b.author
		if a == nil {
			a = newPromptAuthor(opts)
		}
		return buildAgentPrompt(ctx, opts, data, input, a, b.err)
	case promptModeHandoff:
		parentCtx := ctx
		ctx, cancel := context.WithTimeout(ctx, effectivePromptDraftTimeout(opts))
		defer cancel()
		branch, revision, resolveErr := resolveAgentSourceRevision(ctx, input, opts.GitHubToken)
		if resolveErr != nil && parentCtx.Err() != nil {
			return "", promptPreparationResult{}, parentCtx.Err()
		}
		ref, refKind := revision, "commit"
		if resolveErr != nil {
			ref, refKind = branch, "default-branch"
			if branch == "" {
				refKind = "unresolved"
			}
		}
		handoff, err := buildPromptHandoff(input, ref, refKind)
		if err != nil {
			return "", promptPreparationResult{}, err
		}
		p, err := render(systemPromptTmpl, data)
		return p, promptPreparationResult{Requested: promptRequestHandoff, Status: promptStatusHandoff, Output: promptOutputTemplate, Handoff: handoff}, err
	case promptModeTemplate:
		p, err := render(systemPromptTmpl, data)
		return p, newTemplatePromptResult(), err
	default:
		return "", promptPreparationResult{}, fmt.Errorf("unsupported prompt mode %q", effectivePromptMode(opts))
	}
}
