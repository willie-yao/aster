package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"golang.org/x/term"
)

var ErrCancelled = errors.New("onboarding cancelled")

type dependencies struct {
	repositories repositoryClient
	catalogs     catalogClient
	sweeper      jobSweeper
	remotes      remoteDetector
	prompts      promptBuilder
	files        scaffoldWriter
	pullRequests pullRequestWriter
	terminal     Terminal
	wizard       wizardUI
}

type defaultSweeper struct{}

func (defaultSweeper) Discover(ctx context.Context, cfg *project.Config, includePresubmits bool) ([]models.ProwJob, error) {
	return discover(ctx, cfg, includePresubmits)
}

type localScaffoldWriter struct{}

func (localScaffoldWriter) Validate(outDir string, files map[string]string) error {
	return validateFileDestination(outDir, files)
}

func (localScaffoldWriter) Write(outDir string, files map[string]string) error {
	return writeFiles(outDir, files)
}

type githubPullRequestWriter struct {
	client *http.Client
	token  string
}

func (w githubPullRequestWriter) Open(ctx context.Context, repo Repo, files map[string]string, branchPrefix, title, body, token string) (string, error) {
	if token == "" {
		token = w.token
	}
	return ghpr.NewClient(w.client, token).OpenPR(ctx, ghpr.Request{
		Owner: repo.Owner, Repo: repo.Name, Files: files, BranchPrefix: branchPrefix,
		Title: title, Body: body,
	})
}

func defaultDependencies(opts Options, terminal Terminal) dependencies {
	client := defaultDiscoveryHTTPClient()
	return dependencies{
		repositories: githubRepositoryClient{client: client},
		catalogs:     prowCatalogClient{client: client},
		sweeper:      defaultSweeper{},
		remotes:      gitRemoteDetector{},
		prompts:      defaultPromptBuilder{out: terminal.Out, err: terminal.Err},
		files:        localScaffoldWriter{},
		pullRequests: githubPullRequestWriter{client: &http.Client{Timeout: 30 * time.Second}, token: opts.GitHubToken},
		terminal:     terminal,
		wizard:       newWizardUI(terminal),
	}
}

// Run executes onboarding using the process terminal. Complete flag-based
// invocations remain non-interactive.
func Run(ctx context.Context, opts Options) error {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	return RunWithTerminal(ctx, opts, Terminal{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: interactive})
}

// RunWithTerminal executes onboarding with injected terminal streams.
func RunWithTerminal(ctx context.Context, opts Options, terminal Terminal) error {
	if terminal.In == nil {
		terminal.In = strings.NewReader("")
	}
	if terminal.Out == nil {
		terminal.Out = io.Discard
	}
	if terminal.Err == nil {
		terminal.Err = terminal.Out
	}
	return run(ctx, opts, defaultDependencies(opts, terminal))
}

// BuildPlan creates a validated, credential-free onboarding plan without applying it.
func BuildPlan(ctx context.Context, opts Options) (*Plan, error) {
	if err := normalizeRepositories(&opts); err != nil {
		return nil, err
	}
	terminal := Terminal{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}
	return buildPlan(ctx, opts, planningContext{}, defaultDependencies(opts, terminal))
}

// Apply revalidates and applies a plan using the provided GitHub write token.
func Apply(ctx context.Context, plan *Plan, githubToken string) error {
	terminal := Terminal{In: strings.NewReader(""), Out: os.Stdout, Err: os.Stderr}
	opts := Options{GitHubToken: githubToken}
	return applyPlan(ctx, plan, githubToken, defaultDependencies(opts, terminal))
}

func run(ctx context.Context, opts Options, deps dependencies) error {
	if opts.TestGrid != "" && opts.Bucket != "" {
		return fmt.Errorf("provide exactly one of --testgrid or --bucket")
	}
	if isComplete(opts) {
		if err := normalizeRepositories(&opts); err != nil {
			return err
		}
		plan, err := buildPlan(ctx, opts, planningContext{}, deps)
		if err != nil {
			return err
		}
		if err := preflightPlan(plan, deps); err != nil {
			return err
		}
		printReview(deps.terminal.Out, plan)
		if opts.DryRun {
			printDryRun(deps.terminal.Out, plan)
			return nil
		}
		return applyPlan(ctx, plan, opts.GitHubToken, deps)
	}
	if opts.NonInteractive {
		return fmt.Errorf("non-interactive onboarding requires %s", strings.Join(missingInputs(opts), ", "))
	}
	if !deps.terminal.Interactive {
		return fmt.Errorf("onboarding needs %s, but stdin is not an interactive terminal; provide the missing flags or pass --non-interactive for an immediate validation error", strings.Join(missingInputs(opts), ", "))
	}
	plan, opts, err := runWizard(ctx, opts, deps)
	if errors.Is(err, ErrCancelled) {
		fmt.Fprintln(deps.terminal.Out, "Onboarding cancelled. No files were written.")
		return nil
	}
	if err != nil {
		return err
	}
	if opts.DryRun {
		printDryRun(deps.terminal.Out, plan)
		return nil
	}
	return applyPlan(ctx, plan, opts.GitHubToken, deps)
}

func isComplete(opts Options) bool {
	return strings.TrimSpace(opts.SourceRepo) != "" && strings.TrimSpace(opts.DashboardRepo) != "" && ((opts.TestGrid == "") != (opts.Bucket == ""))
}

func missingInputs(opts Options) []string {
	var missing []string
	if strings.TrimSpace(opts.SourceRepo) == "" {
		missing = append(missing, "--source-repo")
	}
	if strings.TrimSpace(opts.DashboardRepo) == "" {
		missing = append(missing, "--dashboard-repo")
	}
	if opts.TestGrid == "" && opts.Bucket == "" {
		missing = append(missing, "one of --testgrid or --bucket")
	}
	return missing
}

func normalizeRepositories(opts *Options) error {
	if err := validateCredentialSeparation(*opts); err != nil {
		return err
	}
	source, err := NormalizeGitHubRepo(opts.SourceRepo)
	if err != nil {
		return fmt.Errorf("--source-repo: %w", err)
	}
	dashboard, err := NormalizeGitHubRepo(opts.DashboardRepo)
	if err != nil {
		return fmt.Errorf("--dashboard-repo: %w", err)
	}
	opts.SourceRepo = source.FullName
	opts.DashboardRepo = dashboard.FullName
	return nil
}

func preflightPlan(plan *Plan, deps dependencies) error {
	if plan == nil {
		return fmt.Errorf("onboarding plan is empty")
	}
	if plan.Destination.OpenPR {
		return nil
	}
	return deps.files.Validate(plan.Destination.OutDir, plan.Files)
}

func applyPlan(ctx context.Context, plan *Plan, githubToken string, deps dependencies) error {
	if planContainsCredential(plan, githubToken) {
		return fmt.Errorf("onboarding plan contains the supplied GitHub credential; no output was applied")
	}
	if err := validatePlan(plan); err != nil {
		return err
	}
	if plan.Destination.OpenPR {
		if githubToken == "" {
			return fmt.Errorf("applying an open-PR onboarding plan needs a GitHub token with write access to the dashboard repo")
		}
		title := fmt.Sprintf("Add %s prow-ai-dashboard scaffold", plan.Project.Name)
		fmt.Fprintf(deps.terminal.Out, "Opening a scaffold pull request against %s...\n", plan.DashboardRepo.FullName)
		url, err := deps.pullRequests.Open(ctx, plan.DashboardRepo, plan.Files, "onboard/scaffold", title, scaffoldPRBody(plan.Project.Name, plan.Deployment.Mode, plan.Deployment.AIEnabled), githubToken)
		if err != nil {
			return fmt.Errorf("opening scaffold pull request: %w", err)
		}
		fmt.Fprintf(deps.terminal.Out, "Scaffold pull request opened: %s\n", url)
		return nil
	}
	if err := deps.files.Write(plan.Destination.OutDir, plan.Files); err != nil {
		return err
	}
	fmt.Fprintf(deps.terminal.Out, "Scaffold written to %s/\n", plan.Destination.OutDir)
	fmt.Fprintf(deps.terminal.Out, "Next: review prompts/system.md and project.yaml, then follow %s.\n", scaffoldGuide(plan.Deployment.Mode))
	return nil
}

func planContainsCredential(planValue *Plan, credential string) bool {
	if planValue == nil || credential == "" {
		return false
	}
	metadata, err := json.Marshal(planValue)
	if err != nil || strings.Contains(string(metadata), credential) {
		return true
	}
	for path, content := range planValue.Files {
		if strings.Contains(path, credential) || strings.Contains(content, credential) {
			return true
		}
	}
	return false
}

func validatePlan(planValue *Plan) error {
	if planValue == nil {
		return fmt.Errorf("onboarding plan is nil")
	}
	normalizedRepo, err := NormalizeGitHubRepo(planValue.DashboardRepo.FullName)
	if err != nil {
		return fmt.Errorf("onboarding plan dashboard repo: %w", err)
	}
	if normalizedRepo.Owner != planValue.DashboardRepo.Owner || normalizedRepo.Name != planValue.DashboardRepo.Name {
		return fmt.Errorf("onboarding plan dashboard repo fields do not match full_name")
	}
	if !planValue.Destination.OpenPR && strings.TrimSpace(planValue.Destination.OutDir) == "" {
		return fmt.Errorf("onboarding plan output directory is required")
	}
	if err := validatePromptPlan(planValue.Prompt); err != nil {
		return err
	}
	expected := map[string]struct{}{
		"project.yaml": {}, "prompts/system.md": {},
	}
	switch planValue.Deployment.Mode {
	case modePages:
		expected[".github/workflows/deploy.yml"] = struct{}{}
		expected["CHECKLIST.md"] = struct{}{}
	case modeK8s:
		expected["deploy/values.yaml"] = struct{}{}
		expected["deploy/README.md"] = struct{}{}
	default:
		return fmt.Errorf("onboarding plan mode %q is invalid", planValue.Deployment.Mode)
	}
	if len(planValue.Files) != len(expected) {
		return fmt.Errorf("onboarding plan contains an unexpected file set")
	}
	for file := range planValue.Files {
		if _, ok := expected[file]; !ok {
			return fmt.Errorf("onboarding plan contains unexpected file %q", file)
		}
		if path.IsAbs(file) || path.Clean(file) != file || strings.Contains(file, "\\") {
			return fmt.Errorf("onboarding plan file path %q is not a safe repo-relative path", file)
		}
	}
	if strings.TrimSpace(planValue.Files["prompts/system.md"]) == "" {
		return fmt.Errorf("onboarding plan prompt is empty")
	}
	parsed, err := project.Parse([]byte(planValue.Files["project.yaml"]))
	if err != nil {
		return fmt.Errorf("onboarding plan project.yaml failed validation: %w", err)
	}
	if !reflect.DeepEqual(*parsed, planValue.Project) {
		return fmt.Errorf("onboarding plan project metadata does not match project.yaml")
	}
	return nil
}

func validateFileDestination(outDir string, files map[string]string) error {
	for _, rel := range sortedFilePaths(files) {
		full := filepath.Join(outDir, rel)
		if _, err := os.Stat(full); err == nil {
			return fmt.Errorf("refusing to overwrite existing %s; choose an empty --out directory", full)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", full, err)
		}
	}
	return nil
}

func printReview(out io.Writer, plan *Plan) {
	fmt.Fprintln(out, "\nReview")
	fmt.Fprintf(out, "  Source repository:    %s\n", safeTerminal(plan.SourceRepo.FullName))
	if source, ok := plan.Provenance["source_repo"]; ok {
		fmt.Fprintf(out, "    Source: %s (%s confidence)\n", source.Source, source.Confidence)
	}
	if plan.Discovery.TestGrid != "" {
		fmt.Fprintf(out, "  TestGrid dashboard:   %s\n", safeTerminal(plan.Discovery.TestGrid))
		if inferred := plan.Discovery.TestGridProvenance; inferred != nil {
			fmt.Fprintf(out, "    Source: %s (%s confidence)\n", inferred.Source, inferred.Confidence)
		}
	} else {
		fmt.Fprintf(out, "  Artifact bucket:      %s\n", safeTerminal(plan.Discovery.Bucket))
	}
	fmt.Fprintf(out, "  Jobs discovered:      %d\n", len(plan.Discovery.Jobs))
	if plan.Discovery.CatalogRevision != "" {
		fmt.Fprintf(out, "  Prow catalog revision: %s\n", safeTerminal(plan.Discovery.CatalogRevision))
	}
	fmt.Fprintf(out, "  Deployment:           %s\n", deploymentLabel(plan.Deployment.Mode))
	fmt.Fprintf(out, "  Dashboard repository: %s\n", safeTerminal(plan.DashboardRepo.FullName))
	if inferred, ok := plan.Provenance["dashboard_repo"]; ok {
		fmt.Fprintf(out, "    Source: %s (%s confidence)\n", inferred.Source, inferred.Confidence)
	}
	fmt.Fprintf(out, "  Project:              %s (%s)\n", safeTerminal(plan.Project.Name), safeTerminal(plan.Project.ID))
	if id, ok := plan.Provenance["project_id"]; ok {
		name := plan.Provenance["project_name"]
		fmt.Fprintf(out, "    ID source: %s (%s); name source: %s (%s)\n", id.Source, id.Confidence, name.Source, name.Confidence)
	}
	if plan.Project.ShortName != "" {
		fmt.Fprintf(out, "  Short name:           %s\n", safeTerminal(plan.Project.ShortName))
	}
	fmt.Fprintf(out, "  Categories:           %d rule(s)\n", len(plan.Project.Categories))
	if plan.Deployment.AIEnabled {
		fmt.Fprintln(out, "  AI analysis:          enabled")
		fmt.Fprintf(out, "  AI provider:          %s\n", providerLabel(plan.Deployment.AIAPI, plan.Deployment.Endpoint))
		fmt.Fprintf(out, "  AI API:               %s\n", safeTerminal(plan.Deployment.AIAPI))
		fmt.Fprintf(out, "  AI endpoint:          %s\n", reviewValue(plan.Deployment.Endpoint))
		fmt.Fprintf(out, "  AI model:             %s\n", reviewValue(plan.Deployment.Model))
	} else {
		fmt.Fprintln(out, "  AI analysis:          disabled in initial scaffold")
	}
	fmt.Fprintf(out, "  Prompt:               %s\n", safeTerminal(plan.Prompt.Source))
	fmt.Fprintf(out, "  Prompt requested:     %s\n", safeTerminal(plan.Prompt.RequestedMode))
	if plan.Prompt.Output == string(promptOutputAPIDraft) {
		fmt.Fprintf(out, "  Prompt provider:      %s, %s, %s\n", safeTerminal(plan.Prompt.API), reviewValue(plan.Prompt.Endpoint), safeTerminal(plan.Prompt.Model))
	}
	if plan.Prompt.FailureStage != "" {
		fmt.Fprintf(out, "  Prompt failure:       %s (%s)\n", safeTerminal(promptPreparationStage(plan.Prompt.FailureStage).label()), safeTerminal(plan.Prompt.FailureCategory))
		if plan.Prompt.FailureAction != "" {
			fmt.Fprintf(out, "  Prompt action:        %s\n", safeTerminal(plan.Prompt.FailureAction))
		}
	}
	if plan.Prompt.RevisionFallback {
		fmt.Fprintln(out, "  Prompt revision:      first validated extraction retained")
	}
	if plan.Destination.OpenPR {
		fmt.Fprintln(out, "  Destination:          scaffold pull request")
	} else {
		fmt.Fprintf(out, "  Output:               %s\n", safeTerminal(plan.Destination.OutDir))
	}
	paths := sortedFilePaths(plan.Files)
	fmt.Fprintln(out, "  Files:")
	for _, path := range paths {
		fmt.Fprintf(out, "    - %s\n", path)
	}
	fmt.Fprintln(out, "\nNo files or external resources have been changed.")
}

func sortedFilePaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func reviewValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "configure after generation"
	}
	return safeTerminal(value)
}

func printDryRun(out io.Writer, plan *Plan) {
	fmt.Fprintln(out, "\nDry run complete. The scaffold rendered and project.yaml passed strict validation.")
	fmt.Fprintln(out, "No files were written and no pull request was opened.")
}

func deploymentLabel(mode string) string {
	if mode == modeK8s {
		return "Kubernetes with Helm"
	}
	return "GitHub Pages"
}
