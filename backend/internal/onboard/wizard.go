package onboard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const promptDraftDisclosure = "AI_TOKEN authenticates prompt drafting. Bounded repository documentation, source excerpts, and matched Prow job metadata will be sent only to that reviewed provider."

func runWizard(ctx context.Context, opts Options, deps dependencies) (*Plan, Options, error) {
	if err := validateCredentialSeparation(opts); err != nil {
		return nil, opts, err
	}
	if err := validateAIEndpoint(opts.AIEndpoint); err != nil {
		return nil, opts, err
	}
	if err := validateAIEndpoint(opts.DeploymentAIEndpoint); err != nil {
		return nil, opts, fmt.Errorf("deployed %w", err)
	}
	if deps.wizard == nil {
		return nil, opts, fmt.Errorf("interactive onboarding UI is unavailable")
	}
	prompt := deps.wizard
	if closer, ok := prompt.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	fmt.Fprintln(deps.terminal.Out, "Guided prow-ai-dashboard onboarding")
	fmt.Fprintln(deps.terminal.Out, "Use Ctrl+C to cancel. No files are written before final confirmation.")
	fmt.Fprintln(deps.terminal.Out)

	repo, detectedFromGit, err := wizardSourceRepo(ctx, prompt, opts, deps)
	if err != nil {
		return nil, opts, err
	}
	if detectedFromGit {
		metadata, metadataErr := deps.repositories.Repository(ctx, repo, opts.GitHubToken)
		if metadataErr != nil {
			return nil, opts, metadataErr
		}
		if metadata.Upstream != nil && metadata.Upstream.FullName != repo.FullName {
			fmt.Fprintf(deps.terminal.Out, "Detected GitHub fork upstream: %s\n", metadata.Upstream.FullName)
			useUpstream, confirmErr := prompt.Confirm(ctx, confirmPrompt{
				Title: "Use the upstream repository for Prow discovery?",
				Value: true,
			})
			if confirmErr != nil {
				return nil, opts, confirmErr
			}
			if useUpstream {
				repo = *metadata.Upstream
			}
		}
	}
	opts.SourceRepo = repo.FullName
	fmt.Fprintf(deps.terminal.Out, "\nInspecting GitHub metadata and kubernetes/test-infra for %s...\n", repo.FullName)
	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, onboardingDiscoveryTimeout)
	report, err := discoverRepository(discoveryCtx, repo, opts.GitHubToken, deps.repositories, deps.catalogs)
	cancelDiscovery()
	if err != nil {
		return nil, opts, err
	}
	opts.SourceRepo = report.SourceRepo.FullName
	fmt.Fprintf(deps.terminal.Out, "Found %d Prow job definition(s) that test this repository.\n", len(report.MatchingJobs))

	selected, err := wizardDiscovery(ctx, prompt, &opts, report)
	if err != nil {
		return nil, opts, err
	}
	if selected != nil && opts.IncludePresubmits == nil && selected.DashboardPresubmitJobs > 0 {
		defaultInclude := selected.DashboardPeriodicJobs == 0
		include, confirmErr := prompt.Confirm(ctx, confirmPrompt{
			Title:       "Include presubmit jobs in the dashboard?",
			Description: "Presubmit history increases coverage and fetch time.",
			Value:       defaultInclude,
		})
		if confirmErr != nil {
			return nil, opts, confirmErr
		}
		opts.IncludePresubmits = &include
	}

	if opts.Mode == "" {
		choice, selectErr := prompt.Select(ctx, selectPrompt{
			Title: "Deployment profile",
			Options: []selectOption{
				{
					Value:       modePages,
					Label:       "GitHub Pages",
					Description: "Public artifacts with an AI provider reachable from GitHub Actions.",
				},
				{
					Value:       modeK8s,
					Label:       "Kubernetes with Helm",
					Description: "Cluster-local providers, persistent state, or authenticated actions.",
				},
			},
			Value: modePages,
		})
		if selectErr != nil {
			return nil, opts, selectErr
		}
		opts.Mode = choice
	}

	if opts.DashboardRepo == "" {
		opts.DashboardRepo, err = prompt.Input(ctx, inputPrompt{
			Title:       "Dashboard repository",
			Description: "Existing owner/name repository that will publish the dashboard.",
			Value:       report.DashboardRepo.Value,
			Required:    true,
			Validate: func(value string) error {
				candidateOpts := opts
				candidateOpts.DashboardRepo = value
				if err := validateCredentialSeparation(candidateOpts); err != nil {
					return err
				}
				if _, err := NormalizeGitHubRepo(value); err != nil {
					return err
				}
				return nil
			},
		})
		if err != nil {
			return nil, opts, err
		}
	}
	if err := validateCredentialSeparation(opts); err != nil {
		return nil, opts, err
	}
	dashboardRepo, err := NormalizeGitHubRepo(opts.DashboardRepo)
	if err != nil {
		return nil, opts, fmt.Errorf("dashboard repository: %w", err)
	}
	opts.DashboardRepo = dashboardRepo.FullName

	if opts.ID == "" {
		opts.ID, err = prompt.Input(ctx, inputPrompt{
			Title:       "Project ID",
			Description: "Stable lowercase identifier inferred from repository metadata.",
			Value:       report.Identity.ID.Value,
			Required:    true,
		})
		if err != nil {
			return nil, opts, err
		}
	}
	if opts.Name == "" {
		opts.Name, err = prompt.Input(ctx, inputPrompt{
			Title:       "Project display name",
			Description: "Human-readable name shown throughout the dashboard.",
			Value:       report.Identity.Name.Value,
			Required:    true,
		})
		if err != nil {
			return nil, opts, err
		}
	}
	if opts.ShortName == "" {
		opts.ShortName, err = prompt.Input(ctx, inputPrompt{
			Title:       "Short name",
			Description: "Optional compact label. Enter none or - to omit it.",
			Value:       report.Identity.ShortName.Value,
		})
		if err != nil {
			return nil, opts, err
		}
		opts.ShortName = clearableValue(opts.ShortName)
	}

	if err := wizardDeploymentAI(ctx, prompt, &opts, deps.terminal.Out); err != nil {
		return nil, opts, err
	}

	if opts.NoPrompt || opts.AIToken == "" {
		opts.NoPrompt = true
	} else if opts.AIEndpoint != "" && opts.AIModel != "" {
		draft, confirmErr := prompt.Confirm(ctx, confirmPrompt{
			Title:       "Use AI_ENDPOINT and AI_MODEL now to draft prompts/system.md?",
			Description: promptDraftDisclosure,
			Value:       false,
		})
		if confirmErr != nil {
			return nil, opts, confirmErr
		}
		opts.NoPrompt = !draft
	} else if effectiveAIEnabled(opts) {
		draft, confirmErr := prompt.Confirm(ctx, confirmPrompt{
			Title:       "Also use the deployed provider to draft prompts/system.md?",
			Description: promptDraftDisclosure,
			Value:       false,
		})
		if confirmErr != nil {
			return nil, opts, confirmErr
		}
		opts.NoPrompt = !draft
		if draft {
			opts.AIAPI = opts.DeploymentAIAPI
			opts.AIEndpoint = opts.DeploymentAIEndpoint
			opts.AIModel = opts.DeploymentAIModel
		}
	} else {
		opts.NoPrompt = true
	}

	if !opts.OpenPR && opts.OutDir == "" {
		opts.OutDir, err = prompt.Input(ctx, inputPrompt{
			Title:       "Output directory",
			Description: "New directory where the validated scaffold will be written.",
			Value:       dashboardRepo.Name,
			Required:    true,
		})
		if err != nil {
			return nil, opts, err
		}
	}

	planning := planningContext{discovery: &report, selected: selected}
	fmt.Fprintln(deps.terminal.Out, "\nRunning the real job sweep and validating the scaffold...")
	plan, err := buildPlanWithPromptRecovery(ctx, opts, planning, deps, prompt)
	if err != nil {
		return nil, opts, err
	}
	if len(plan.Project.Categories) > 0 {
		categoryTokens := make([]string, 0, len(plan.Project.Categories))
		for _, category := range plan.Project.Categories {
			categoryTokens = append(categoryTokens, category.ID)
		}
		value, inputErr := prompt.Input(ctx, inputPrompt{
			Title:       "Category tokens",
			Description: "Comma-separated job-name tokens. Enter none or - to clear them.",
			Value:       strings.Join(categoryTokens, ","),
		})
		if inputErr != nil {
			return nil, opts, inputErr
		}
		if err := setPlanCategoryTokens(plan, opts, value); err != nil {
			return nil, opts, err
		}
	}
	if err := preflightPlan(plan, deps); err != nil {
		return nil, opts, err
	}
	printReview(deps.terminal.Out, plan)
	if opts.DryRun {
		return plan, opts, nil
	}
	confirmed, err := prompt.Confirm(ctx, confirmPrompt{
		Title:       "Create this scaffold?",
		Description: "This is the first prompt that permits a filesystem or GitHub write.",
		Value:       false,
	})
	if err != nil {
		return nil, opts, err
	}
	if !confirmed {
		return nil, opts, ErrCancelled
	}
	return plan, opts, nil
}

func buildPlanWithPromptRecovery(ctx context.Context, opts Options, planning planningContext, deps dependencies, prompt wizardUI) (*Plan, error) {
	for {
		plan, err := buildPlan(ctx, opts, planning, deps)
		if err != nil {
			return nil, err
		}
		if plan.Prompt.RequestedMode != string(promptRequestAPIExperimental) || plan.Prompt.FinalStatus != string(promptStatusFallback) {
			return plan, nil
		}
		choice, err := prompt.Select(ctx, selectPrompt{
			Title:       "Experimental API prompt drafting failed",
			Description: "Retry the same reviewed provider, continue safely, or cancel onboarding.",
			Options: []selectOption{
				{Value: "retry", Label: "Retry bounded draft", Description: "Use the same reviewed API, endpoint, and model."},
				{Value: "template", Label: "Continue with TODO template", Description: "Keep the reviewable template and proceed."},
				{Value: "cancel", Label: "Cancel onboarding", Description: "Stop without writing files or opening a pull request."},
			},
			Value: "template",
		})
		if err != nil {
			return nil, err
		}
		switch choice {
		case "retry":
			continue
		case "template":
			return plan, nil
		default:
			return nil, ErrCancelled
		}
	}
}

func clearableValue(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "-":
		return ""
	default:
		return value
	}
}

func setPlanCategoryTokens(plan *Plan, opts Options, value string) error {
	value = clearableValue(value)
	var categories []project.CategoryRule
	seen := map[string]struct{}{}
	for _, token := range strings.Split(value, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		categories = append(categories, project.CategoryRule{Match: token, ID: token, Label: labelFor(token)})
	}
	data := buildScaffoldData(opts, categories)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		return fmt.Errorf("rendering edited project.yaml: %w", err)
	}
	parsed, err := project.Parse([]byte(yamlText))
	if err != nil {
		return fmt.Errorf("edited project.yaml failed validation: %w", err)
	}
	if err := validateRenderedFilesNoCredentials(opts, map[string]string{"project.yaml": yamlText}); err != nil {
		return err
	}
	plan.Project = *parsed
	plan.Files["project.yaml"] = yamlText
	return nil
}

func wizardSourceRepo(ctx context.Context, prompt wizardUI, opts Options, deps dependencies) (Repo, bool, error) {
	if opts.SourceRepo != "" {
		repo, err := NormalizeGitHubRepo(opts.SourceRepo)
		if err != nil {
			return Repo{}, false, fmt.Errorf("source repository: %w", err)
		}
		fmt.Fprintf(deps.terminal.Out, "Source repository: %s (explicit input)\n", repo.FullName)
		return repo, false, nil
	}
	if remote, err := deps.remotes.Origin(ctx); err == nil {
		repo, normalizeErr := NormalizeGitHubRepo(remote)
		if normalizeErr == nil {
			fmt.Fprintf(deps.terminal.Out, "Source repository detected from git remote origin: %s\n", repo.FullName)
			use, confirmErr := prompt.Confirm(ctx, confirmPrompt{
				Title: "Use this repository?",
				Value: true,
			})
			if confirmErr != nil {
				return Repo{}, false, confirmErr
			}
			if use {
				return repo, true, nil
			}
		}
	}
	value, err := prompt.Input(ctx, inputPrompt{
		Title:       "Source GitHub repository",
		Description: "Repository tested by Prow, as owner/name or a GitHub URL.",
		Required:    true,
		Validate: func(value string) error {
			candidateOpts := opts
			candidateOpts.SourceRepo = value
			if err := validateCredentialSeparation(candidateOpts); err != nil {
				return err
			}
			_, err := NormalizeGitHubRepo(value)
			return err
		},
	})
	if err != nil {
		return Repo{}, false, err
	}
	candidateOpts := opts
	candidateOpts.SourceRepo = value
	if err := validateCredentialSeparation(candidateOpts); err != nil {
		return Repo{}, false, err
	}
	repo, err := NormalizeGitHubRepo(value)
	if err != nil {
		return Repo{}, false, fmt.Errorf("source repository: %w", err)
	}
	return repo, false, nil
}

func wizardDiscovery(ctx context.Context, prompt wizardUI, opts *Options, report DiscoveryReport) (*DashboardCandidate, error) {
	if opts.Bucket != "" {
		return nil, nil
	}
	if opts.TestGrid != "" {
		for _, candidate := range report.Candidates {
			if candidate.Dashboard == opts.TestGrid {
				selected := candidate
				return &selected, nil
			}
		}
		return nil, nil
	}
	options := make([]selectOption, 0, len(report.Candidates)+2)
	for _, candidate := range report.Candidates {
		summary := fmt.Sprintf("%s (%s)", safeTerminal(candidate.Dashboard),
			directSourceMatchSummary(candidate.PeriodicJobs, candidate.PresubmitJobs))
		description := fmt.Sprintf("Dashboard contains %d periodic and %d presubmit jobs", candidate.DashboardPeriodicJobs, candidate.DashboardPresubmitJobs)
		if candidate.DashboardPostsubmitJobs > 0 {
			description += fmt.Sprintf("; %d postsubmit jobs are unsupported", candidate.DashboardPostsubmitJobs)
		}
		options = append(options, selectOption{
			Value:       "candidate:" + candidate.Dashboard,
			Label:       summary,
			Description: description + ".",
		})
	}
	options = append(options,
		selectOption{
			Value:       "manual_testgrid",
			Label:       "Enter a TestGrid dashboard manually",
			Description: "Use a dashboard name not inferred from repository metadata.",
		},
		selectOption{
			Value:       "artifact_bucket",
			Label:       "Use an artifact bucket",
			Description: "Discover jobs directly from a Prow artifact bucket.",
		},
	)
	defaultValue := "manual_testgrid"
	if len(report.Candidates) > 0 {
		defaultValue = "candidate:" + report.Candidates[0].Dashboard
	}
	choice, err := prompt.Select(ctx, selectPrompt{
		Title:       "Choose the discovery source",
		Description: "Select the TestGrid dashboard or artifact source for this project.",
		Options:     options,
		Value:       defaultValue,
	})
	if err != nil {
		return nil, err
	}
	for _, candidate := range report.Candidates {
		if choice == "candidate:"+candidate.Dashboard {
			selected := candidate
			opts.TestGrid = selected.Dashboard
			return &selected, nil
		}
	}
	if choice == "manual_testgrid" {
		opts.TestGrid, err = prompt.Input(ctx, inputPrompt{
			Title:    "TestGrid dashboard",
			Required: true,
		})
		return nil, err
	}
	opts.Bucket, err = prompt.Input(ctx, inputPrompt{
		Title:       "Artifact bucket",
		Description: "Bucket containing Prow logs or pr-logs indexes.",
		Required:    true,
	})
	if err != nil {
		return nil, err
	}
	opts.GCSWebBase, err = prompt.Input(ctx, inputPrompt{
		Title:       "gcsweb base URL",
		Description: "Optional gateway root for non-GCS object storage.",
		Value:       opts.GCSWebBase,
	})
	return nil, err
}
