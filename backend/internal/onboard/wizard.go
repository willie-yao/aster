package onboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type prompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func newPrompter(terminal Terminal) *prompter {
	return &prompter{reader: bufio.NewReader(terminal.In), out: terminal.Out}
}

func (p *prompter) line(label, defaultValue string, required bool) (string, error) {
	for {
		fmt.Fprint(p.out, label)
		if defaultValue != "" {
			fmt.Fprintf(p.out, " [%s]", defaultValue)
		}
		fmt.Fprint(p.out, ": ")
		line, err := p.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = strings.TrimSpace(line)
		if isCancel(line) || (errors.Is(err, io.EOF) && line == "") {
			return "", ErrCancelled
		}
		if line == "" {
			line = defaultValue
		}
		if line == "" && required {
			fmt.Fprintln(p.out, "A value is required. Enter q to cancel.")
			if errors.Is(err, io.EOF) {
				return "", ErrCancelled
			}
			continue
		}
		return line, nil
	}
}

func (p *prompter) confirm(label string, defaultYes bool) (bool, error) {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	for {
		fmt.Fprint(p.out, label+suffix)
		line, err := p.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if isCancel(line) || (errors.Is(err, io.EOF) && line == "") {
			return false, ErrCancelled
		}
		switch line {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "Enter y, n, or q to cancel.")
		}
	}
}

func (p *prompter) selectOne(label string, options []string, defaultIndex int) (int, error) {
	fmt.Fprintln(p.out, label)
	for i, option := range options {
		fmt.Fprintf(p.out, "  %d. %s\n", i+1, option)
	}
	for {
		fmt.Fprintf(p.out, "Select [%d]: ", defaultIndex+1)
		line, err := p.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if isCancel(line) || (errors.Is(err, io.EOF) && line == "") {
			return 0, ErrCancelled
		}
		if line == "" {
			return defaultIndex, nil
		}
		selected, convErr := strconv.Atoi(line)
		if convErr == nil && selected >= 1 && selected <= len(options) {
			return selected - 1, nil
		}
		fmt.Fprintf(p.out, "Enter a number from 1 to %d, or q to cancel.\n", len(options))
	}
}

func isCancel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "q", "quit", "cancel":
		return true
	default:
		return false
	}
}

func runWizard(ctx context.Context, opts Options, deps dependencies) (*Plan, Options, error) {
	prompt := newPrompter(deps.terminal)
	fmt.Fprintln(deps.terminal.Out, "Guided prow-ai-dashboard onboarding")
	fmt.Fprintln(deps.terminal.Out, "Enter q at any prompt to cancel. No files are written before final confirmation.")
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
			useUpstream, confirmErr := prompt.confirm("Use the upstream repository for Prow discovery?", true)
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
	fmt.Fprintf(deps.terminal.Out, "Found %d Prow job definition(s) that test this repository.\n", len(report.MatchingJobs))

	selected, err := wizardDiscovery(prompt, &opts, report)
	if err != nil {
		return nil, opts, err
	}
	if selected != nil && !opts.IncludePresubmitsExplicit && selected.PresubmitJobs > 0 {
		defaultInclude := selected.PeriodicJobs == 0
		opts.IncludePresubmits, err = prompt.confirm("Include presubmit jobs in the dashboard?", defaultInclude)
		if err != nil {
			return nil, opts, err
		}
	}

	if !opts.ModeExplicit {
		choice, err := prompt.selectOne("\nDeployment profile", []string{
			"GitHub Pages, for public artifacts and a provider reachable from GitHub Actions",
			"Kubernetes with Helm, for cluster-local providers, persistent state, or authenticated actions",
		}, 0)
		if err != nil {
			return nil, opts, err
		}
		if choice == 1 {
			opts.Mode = modeK8s
		} else {
			opts.Mode = modePages
		}
	}

	if opts.DashboardRepo == "" {
		opts.DashboardRepo, err = prompt.line("Dashboard repository", report.DashboardRepo.Value, true)
		if err != nil {
			return nil, opts, err
		}
	}
	dashboardRepo, err := NormalizeGitHubRepo(opts.DashboardRepo)
	if err != nil {
		return nil, opts, fmt.Errorf("dashboard repository: %w", err)
	}
	opts.DashboardRepo = dashboardRepo.FullName

	if opts.ID == "" {
		opts.ID, err = prompt.line("Project id", report.Identity.ID.Value, true)
		if err != nil {
			return nil, opts, err
		}
	}
	if opts.Name == "" {
		opts.Name, err = prompt.line("Project display name", report.Identity.Name.Value, true)
		if err != nil {
			return nil, opts, err
		}
	}
	if opts.ShortName == "" {
		opts.ShortName, err = prompt.line("Short name (optional)", report.Identity.ShortName.Value, false)
		if err != nil {
			return nil, opts, err
		}
	}

	if opts.AIEnabled == nil {
		enabled, err := prompt.confirm("\nEnable AI failure analysis in the deployed dashboard?", true)
		if err != nil {
			return nil, opts, err
		}
		opts.AIEnabled = &enabled
	}
	if effectiveAIEnabled(opts) {
		apiDefault := deploymentAIAPI(opts)
		if apiDefault == "" {
			apiDefault = project.AIAPIChatCompletions
		}
		apiChoice := 0
		if apiDefault == project.AIAPIResponses {
			apiChoice = 1
		}
		choice, err := prompt.selectOne("Deployed AI API", []string{"chat_completions", "responses"}, apiChoice)
		if err != nil {
			return nil, opts, err
		}
		if choice == 1 {
			opts.DeploymentAIAPI = project.AIAPIResponses
		} else {
			opts.DeploymentAIAPI = project.AIAPIChatCompletions
		}
		opts.DeploymentAIEndpoint, err = prompt.line("Deployed AI endpoint", deploymentAIEndpoint(opts), true)
		if err != nil {
			return nil, opts, err
		}
		opts.DeploymentAIModel, err = prompt.line("Deployed AI model", deploymentAIModel(opts), true)
		if err != nil {
			return nil, opts, err
		}
	}

	if opts.NoPrompt || opts.AIToken == "" {
		opts.NoPrompt = true
	} else if opts.AIEndpoint != "" && opts.AIModel != "" {
		draft, err := prompt.confirm("Use the AI_ENDPOINT and AI_MODEL provider now to draft prompts/system.md from bounded repository documentation?", false)
		if err != nil {
			return nil, opts, err
		}
		opts.NoPrompt = !draft
	} else if effectiveAIEnabled(opts) {
		draft, err := prompt.confirm("Also use the deployed provider to draft prompts/system.md from bounded repository documentation?", false)
		if err != nil {
			return nil, opts, err
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
		opts.OutDir, err = prompt.line("Output directory", dashboardRepo.Name, true)
		if err != nil {
			return nil, opts, err
		}
	}

	planning := planningContext{discovery: &report, selected: selected}
	fmt.Fprintln(deps.terminal.Out, "\nRunning the real job sweep and validating the scaffold...")
	plan, err := buildPlan(ctx, opts, planning, deps)
	if err != nil {
		return nil, opts, err
	}
	if len(plan.Project.Categories) > 0 {
		categoryTokens := make([]string, 0, len(plan.Project.Categories))
		for _, category := range plan.Project.Categories {
			categoryTokens = append(categoryTokens, category.ID)
		}
		value, err := prompt.line("Category tokens, comma separated (empty for none)", strings.Join(categoryTokens, ","), false)
		if err != nil {
			return nil, opts, err
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
	confirmed, err := prompt.confirm("Create this scaffold?", false)
	if err != nil {
		return nil, opts, err
	}
	if !confirmed {
		return nil, opts, ErrCancelled
	}
	return plan, opts, nil
}

func setPlanCategoryTokens(plan *Plan, opts Options, value string) error {
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
	plan.Project = *parsed
	plan.Files["project.yaml"] = yamlText
	return nil
}

func wizardSourceRepo(ctx context.Context, prompt *prompter, opts Options, deps dependencies) (Repo, bool, error) {
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
			use, confirmErr := prompt.confirm("Use this repository?", true)
			if confirmErr != nil {
				return Repo{}, false, confirmErr
			}
			if use {
				return repo, true, nil
			}
		}
	}
	value, err := prompt.line("Source GitHub repository (owner/name or URL)", "", true)
	if err != nil {
		return Repo{}, false, err
	}
	repo, err := NormalizeGitHubRepo(value)
	if err != nil {
		return Repo{}, false, fmt.Errorf("source repository: %w", err)
	}
	return repo, false, nil
}

func wizardDiscovery(prompt *prompter, opts *Options, report DiscoveryReport) (*DashboardCandidate, error) {
	if opts.TestGrid != "" || opts.Bucket != "" {
		return nil, nil
	}
	fmt.Fprintln(prompt.out, "\nCandidate TestGrid dashboards")
	options := make([]string, 0, len(report.Candidates)+2)
	for _, candidate := range report.Candidates {
		options = append(options, fmt.Sprintf("%s (%d periodic, %d presubmit)", safeTerminal(candidate.Dashboard), candidate.PeriodicJobs, candidate.PresubmitJobs))
	}
	options = append(options, "Enter a TestGrid dashboard manually", "Use an artifact bucket")
	defaultIndex := 0
	if len(report.Candidates) == 0 {
		defaultIndex = len(options) - 2
	}
	choice, err := prompt.selectOne("Choose the discovery source", options, defaultIndex)
	if err != nil {
		return nil, err
	}
	if choice < len(report.Candidates) {
		selected := report.Candidates[choice]
		opts.TestGrid = selected.Dashboard
		return &selected, nil
	}
	if choice == len(report.Candidates) {
		opts.TestGrid, err = prompt.line("TestGrid dashboard", "", true)
		return nil, err
	}
	opts.Bucket, err = prompt.line("Artifact bucket", "", true)
	if err != nil {
		return nil, err
	}
	opts.GCSWebBase, err = prompt.line("gcsweb base URL (optional)", opts.GCSWebBase, false)
	return nil, err
}
