package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/onboard/promptauthor"
	"github.com/willie-yao/aster/backend/internal/project"
	"golang.org/x/term"
)

var ErrCancelled = errors.New("onboarding cancelled")

type dependencies struct {
	repositories   repositoryClient
	sourceRevision sourceRevisionResolver
	catalogs       catalogClient
	sweeper        jobSweeper
	remotes        remoteDetector
	prompts        promptBuilder
	files          scaffoldWriter
	pullRequests   pullRequestWriter
	terminal       Terminal
	wizard         wizardUI
}

type defaultSweeper struct{}

func (defaultSweeper) Discover(ctx context.Context, cfg *project.Config, includePresubmits bool) (JobSweep, error) {
	return discover(ctx, cfg, includePresubmits)
}

type localScaffoldWriter struct{}

func (localScaffoldWriter) Inspect(outDir string, files map[string]string, replaceConsumerOwned bool) ([]DestinationFilePlan, []string, error) {
	return inspectFileDestination(outDir, files, replaceConsumerOwned)
}

func (localScaffoldWriter) Write(outDir string, files map[string]string, updateExisting, replaceConsumerOwned bool, expected []DestinationFilePlan) error {
	return writeFiles(outDir, files, updateExisting, replaceConsumerOwned, expected)
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
		repositories:   githubRepositoryClient{client: client},
		sourceRevision: githubSourceRevisionResolver{},
		catalogs:       prowCatalogClient{client: client},
		sweeper:        defaultSweeper{},
		remotes:        gitRemoteDetector{},
		prompts:        defaultPromptBuilder{err: terminal.Err},
		files:          localScaffoldWriter{},
		pullRequests:   githubPullRequestWriter{client: &http.Client{Timeout: 30 * time.Second}, token: opts.GitHubToken},
		terminal:       terminal,
		wizard:         newWizardUI(terminal),
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
	deps := defaultDependencies(opts, terminal)
	plan, err := buildPlan(ctx, opts, planningContext{}, deps)
	if err != nil {
		return nil, err
	}
	if err := preflightPlan(plan, deps); err != nil {
		return nil, err
	}
	return plan, nil
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
			return finishDryRun(deps.terminal.Out, plan, opts.PlanOut)
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
		return finishDryRun(deps.terminal.Out, plan, opts.PlanOut)
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
	if err := inspectPlanDestination(plan, deps); err != nil {
		return err
	}
	if plan.Destination.OpenPR {
		return nil
	}
	if replacements := destinationReplacementPaths(plan.Destination.Files); len(replacements) > 0 && !plan.Destination.UpdateExisting {
		return &destinationConflictError{paths: replacements}
	}
	return nil
}

func inspectPlanDestination(plan *Plan, deps dependencies) error {
	if plan == nil {
		return fmt.Errorf("onboarding plan is empty")
	}
	if plan.Destination.OpenPR {
		plan.Destination.Files = nil
		plan.Destination.StaleFiles = nil
		return nil
	}
	files, stale, err := deps.files.Inspect(plan.Destination.OutDir, plan.Files, plan.Destination.ReplaceConsumerOwned)
	if err != nil {
		return err
	}
	plan.Destination.Files = files
	plan.Destination.StaleFiles = stale
	plan.Prompt.ExistingSHA256 = ""
	for _, file := range files {
		if file.Path == "prompts/system.md" && file.Action != destinationActionCreate {
			plan.Prompt.ExistingSHA256 = file.ReviewedDigest
			break
		}
	}
	return nil
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
		title := fmt.Sprintf("Add %s Aster scaffold", plan.Project.Name)
		fmt.Fprintf(deps.terminal.Out, "Opening a scaffold pull request against %s...\n", plan.DashboardRepo.FullName)
		url, err := deps.pullRequests.Open(ctx, plan.DashboardRepo, plan.Files, "onboard/scaffold", title, scaffoldPRBody(plan.Project.Name, plan.Deployment.Mode, plan.Deployment.AIEnabled), githubToken)
		if err != nil {
			return fmt.Errorf("opening scaffold pull request: %w", err)
		}
		fmt.Fprintf(deps.terminal.Out, "Scaffold pull request opened: %s\n", url)
		return nil
	}
	files, _, err := deps.files.Inspect(plan.Destination.OutDir, plan.Files, plan.Destination.ReplaceConsumerOwned)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(files, plan.Destination.Files) {
		return fmt.Errorf("dashboard consumer directory changed after review; rerun onboarding before writing")
	}
	if replacements := destinationReplacementPaths(files); len(replacements) > 0 && !plan.Destination.UpdateExisting {
		return &destinationConflictError{paths: replacements}
	}
	if err := deps.files.Write(plan.Destination.OutDir, plan.Files, plan.Destination.UpdateExisting, plan.Destination.ReplaceConsumerOwned, plan.Destination.Files); err != nil {
		return err
	}
	fmt.Fprintf(deps.terminal.Out, "Scaffold written to %s/\n", plan.Destination.OutDir)
	fmt.Fprintf(deps.terminal.Out, "Next: review project.yaml and the source-only prompts/system.md baseline, follow %s, then run $author-aster-diagnostics.\n", scaffoldGuide(plan.Deployment.Mode))
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
	if strings.TrimSpace(planValue.Engine.Path) == "" || strings.TrimSpace(planValue.Engine.Version) == "" {
		return fmt.Errorf("onboarding plan engine identity is incomplete")
	}
	for label, repo := range map[string]Repo{"source": planValue.SourceRepo, "dashboard": planValue.DashboardRepo} {
		normalized, err := NormalizeGitHubRepo(repo.FullName)
		if err != nil {
			return fmt.Errorf("onboarding plan %s repo: %w", label, err)
		}
		if normalized.Owner != repo.Owner || normalized.Name != repo.Name {
			return fmt.Errorf("onboarding plan %s repo fields do not match full_name", label)
		}
	}
	switch planValue.SourceRevision.Status {
	case sourceRevisionResolved:
		if strings.TrimSpace(planValue.SourceRevision.Ref) == "" {
			return fmt.Errorf("onboarding plan resolved source revision has no ref")
		}
		if planValue.SourceRepo.Branch != "" && planValue.SourceRepo.Branch != planValue.SourceRevision.Ref {
			return fmt.Errorf("onboarding plan source branch does not match the resolved ref")
		}
		if !validGitRevision(planValue.SourceRevision.Revision) {
			return fmt.Errorf("onboarding plan source revision is invalid")
		}
	case sourceRevisionUnresolved:
		if planValue.SourceRevision.Revision != "" {
			return fmt.Errorf("onboarding plan unresolved source revision retained a commit")
		}
	default:
		return fmt.Errorf("onboarding plan source revision status %q is invalid", planValue.SourceRevision.Status)
	}
	if len(planValue.Deployment.Reasons) == 0 {
		return fmt.Errorf("onboarding plan deployment reasons are empty")
	}
	for _, reason := range planValue.Deployment.Reasons {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("onboarding plan deployment reason is empty")
		}
	}
	switch planValue.Deployment.ArtifactAccess {
	case artifactAccessPublic, artifactAccessAuthenticated, artifactAccessPrivate, artifactAccessUnknown:
	default:
		return fmt.Errorf("onboarding plan artifact access %q is invalid", planValue.Deployment.ArtifactAccess)
	}
	if planValue.Discovery.TestGrid != "" && !validGitRevision(planValue.Discovery.CatalogRevision) {
		return fmt.Errorf("onboarding plan TestGrid discovery is missing a valid test-infra revision")
	}
	if planValue.Discovery.TestGrid == "" && planValue.Discovery.CatalogRevision != "" {
		return fmt.Errorf("onboarding plan bucket discovery retained a test-infra revision")
	}
	discoveryDigest, err := discoveryPlanDigest(planValue.Discovery)
	if err != nil {
		return fmt.Errorf("hashing onboarding plan discovery output: %w", err)
	}
	if discoveryDigest != planValue.Discovery.Digest {
		return fmt.Errorf("onboarding plan discovery digest does not match the selected jobs")
	}
	if !planValue.Destination.OpenPR {
		normalizedOutDir, err := normalizeDashboardConsumerDir(planValue.Destination.OutDir)
		if err != nil {
			return err
		}
		if normalizedOutDir != planValue.Destination.OutDir {
			return fmt.Errorf("onboarding plan dashboard consumer directory is not normalized")
		}
	}
	if planValue.Destination.ReplaceConsumerOwned && !planValue.Destination.UpdateExisting {
		return fmt.Errorf("onboarding plan consumer-owned replacement requires update-existing mode")
	}
	if planValue.Destination.OpenPR && (planValue.Destination.UpdateExisting || planValue.Destination.ReplaceConsumerOwned) {
		return fmt.Errorf("onboarding plan cannot combine open-PR and local update modes")
	}
	if planValue.Destination.OpenPR && (len(planValue.Destination.Files) > 0 || len(planValue.Destination.StaleFiles) > 0) {
		return fmt.Errorf("onboarding plan open-PR destination contains local filesystem state")
	}
	if err := validatePromptPlan(planValue.Prompt); err != nil {
		return err
	}
	expected := map[string]struct{}{
		"project.yaml": {}, "prompts/system.md": {},
	}
	if promptPlanIncludesHandoff(planValue.Prompt) {
		expected["PROMPT_HANDOFF.md"] = struct{}{}
		expected[".opencode/skills/system-prompt-generation/SKILL.md"] = struct{}{}
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
		if err := validateDestinationFilePath(file); err != nil {
			return err
		}
	}
	if planValue.Prompt.CandidateSHA256 != planArtifactDigest([]byte(planValue.Files["prompts/system.md"])) {
		return fmt.Errorf("onboarding plan candidate prompt digest does not match prompts/system.md")
	}
	if !planValue.Destination.OpenPR {
		seen := make(map[string]struct{}, len(planValue.Destination.Files))
		for _, file := range planValue.Destination.Files {
			_, generated := expected[file.Path]
			if !generated && !isConsumerSkillPath(file.Path) {
				return fmt.Errorf("onboarding plan destination contains unexpected file %q", file.Path)
			}
			if _, duplicate := seen[file.Path]; duplicate {
				return fmt.Errorf("onboarding plan destination duplicates file %q", file.Path)
			}
			seen[file.Path] = struct{}{}
			if err := validateDestinationFilePath(file.Path); err != nil {
				return err
			}
			if file.Ownership != destinationFileOwnership(file.Path) {
				return fmt.Errorf("onboarding plan destination ownership for %q is invalid", file.Path)
			}
			switch file.Action {
			case destinationActionCreate:
				if file.ReviewedDigest != "" {
					return fmt.Errorf("onboarding plan create action for %q retained a reviewed digest", file.Path)
				}
				if !generated {
					return fmt.Errorf("onboarding plan cannot create ungenerated consumer file %q", file.Path)
				}
			case destinationActionReplace:
				if _, err := parseSHA256Digest(file.ReviewedDigest, "reviewed destination digest"); err != nil {
					return fmt.Errorf("onboarding plan replacement for %q has an invalid reviewed digest", file.Path)
				}
				if !planValue.Destination.UpdateExisting {
					return fmt.Errorf("onboarding plan replacement requires update-existing mode")
				}
				if file.Ownership == destinationOwnershipConsumer && !planValue.Destination.ReplaceConsumerOwned {
					return fmt.Errorf("onboarding plan consumer-owned replacement for %q lacks explicit approval", file.Path)
				}
				if isConsumerSkillPath(file.Path) {
					return fmt.Errorf("onboarding plan cannot replace consumer skill %q", file.Path)
				}
			case destinationActionPreserve:
				if file.Ownership != destinationOwnershipConsumer {
					return fmt.Errorf("onboarding plan preserve action for %q is not consumer-owned", file.Path)
				}
				if _, err := parseSHA256Digest(file.ReviewedDigest, "reviewed destination digest"); err != nil {
					return fmt.Errorf("onboarding plan preservation for %q has an invalid reviewed digest", file.Path)
				}
			default:
				return fmt.Errorf("onboarding plan destination action %q is invalid", file.Action)
			}
		}
		for file := range expected {
			if _, ok := seen[file]; !ok {
				return fmt.Errorf("onboarding plan destination file set is incomplete")
			}
		}
		promptDestination := destinationFileByPath(planValue.Destination.Files, "prompts/system.md")
		if promptDestination == nil {
			return fmt.Errorf("onboarding plan destination prompt is missing")
		}
		if promptDestination.Action == destinationActionCreate {
			if planValue.Prompt.ExistingSHA256 != "" {
				return fmt.Errorf("onboarding plan new prompt retained an existing digest")
			}
		} else if planValue.Prompt.ExistingSHA256 != promptDestination.ReviewedDigest {
			return fmt.Errorf("onboarding plan existing prompt digest does not match the reviewed destination")
		}
	}
	for _, stale := range planValue.Destination.StaleFiles {
		if err := validateDestinationFilePath(stale); err != nil {
			return err
		}
		known := false
		for _, candidate := range knownGeneratedFiles {
			if stale == candidate {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("onboarding plan stale file %q is not a known generated file", stale)
		}
	}
	if strings.TrimSpace(planValue.Files["prompts/system.md"]) == "" {
		return fmt.Errorf("onboarding plan prompt is empty")
	}
	if promptPlanIncludesHandoff(planValue.Prompt) {
		if strings.TrimSpace(planValue.Files["PROMPT_HANDOFF.md"]) == "" {
			return fmt.Errorf("onboarding plan prompt handoff is empty")
		}
		if planValue.Files[".opencode/skills/system-prompt-generation/SKILL.md"] != promptauthor.SkillContent() {
			return fmt.Errorf("onboarding plan prompt authoring skill is invalid")
		}
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

func validGitRevision(value string) bool {
	return len(value) == 40 && strings.IndexFunc(value, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF", r)
	}) < 0
}

func destinationFileByPath(files []DestinationFilePlan, wanted string) *DestinationFilePlan {
	for i := range files {
		if files[i].Path == wanted {
			return &files[i]
		}
	}
	return nil
}

func printReview(out io.Writer, plan *Plan) {
	fmt.Fprintln(out, "\nReview")
	fmt.Fprintf(out, "  Engine:               %s (%s)\n", safeTerminal(plan.Engine.Path), safeTerminal(plan.Engine.Version))
	if plan.Engine.Revision != "" {
		fmt.Fprintf(out, "  Engine revision:      %s\n", safeTerminal(plan.Engine.Revision))
	}
	fmt.Fprintf(out, "  Source repository:    %s\n", safeTerminal(plan.SourceRepo.FullName))
	if source, ok := plan.Provenance["source_repo"]; ok {
		fmt.Fprintf(out, "    Source: %s (%s confidence)\n", source.Source, source.Confidence)
	}
	if plan.SourceRevision.Revision != "" {
		fmt.Fprintf(out, "  Source revision:      %s (%s)\n", safeTerminal(plan.SourceRevision.Revision), safeTerminal(plan.SourceRevision.Ref))
	} else {
		fmt.Fprintf(out, "  Source revision:      unresolved (%s)\n", reviewValue(plan.SourceRevision.Ref))
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
	fmt.Fprintf(out, "  Discovery digest:     %s\n", safeTerminal(plan.Discovery.Digest))
	fmt.Fprintf(out, "  Deployment:           %s\n", deploymentLabel(plan.Deployment.Mode))
	fmt.Fprintf(out, "  Artifact access:      %s\n", safeTerminal(plan.Deployment.ArtifactAccess))
	for _, reason := range plan.Deployment.Reasons {
		fmt.Fprintf(out, "    Reason: %s\n", safeTerminal(reason))
	}
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
	fmt.Fprintf(out, "  Prompt baseline:      %s\n", safeTerminal(plan.Prompt.BaselineStatus))
	fmt.Fprintf(out, "  Candidate prompt:     %s\n", safeTerminal(plan.Prompt.CandidateSHA256))
	if plan.Prompt.ExistingSHA256 != "" {
		fmt.Fprintf(out, "  Existing prompt:      %s\n", safeTerminal(plan.Prompt.ExistingSHA256))
	}
	fmt.Fprintf(out, "  Prompt requested:     %s\n", safeTerminal(plan.Prompt.RequestedMode))
	if plan.Prompt.RequestedMode == string(promptRequestAgent) {
		fmt.Fprintf(out, "  Prompt timeout:       %s\n", safeTerminal(plan.Prompt.Timeout))
		coordinate := plan.Prompt.Model
		if plan.Prompt.AgentRef != "" {
			coordinate = plan.Prompt.AgentRef
		}
		fmt.Fprintf(out, "  Prompt agent:         %s, %s\n", safeTerminal(plan.Prompt.Runtime), safeTerminal(coordinate))
	}
	if plan.Prompt.FailureStage != "" {
		fmt.Fprintf(out, "  Prompt failure:       %s (%s)\n", safeTerminal(promptPreparationStage(plan.Prompt.FailureStage).label()), safeTerminal(plan.Prompt.FailureCategory))
		if plan.Prompt.FailureAction != "" {
			fmt.Fprintf(out, "  Prompt action:        %s\n", safeTerminal(plan.Prompt.FailureAction))
		}
	}
	if plan.Destination.OpenPR {
		fmt.Fprintln(out, "  Destination:          scaffold pull request")
	} else {
		fmt.Fprintf(out, "  Dashboard consumer directory: %s\n", safeTerminal(filepath.Clean(plan.Destination.OutDir)))
		if plan.Destination.UpdateExisting {
			fmt.Fprintln(out, "  Existing scaffold:    update known engine-generated files")
		}
		if plan.Destination.ReplaceConsumerOwned {
			fmt.Fprintln(out, "  Consumer prompt:      explicitly approved for replacement")
		}
	}
	fmt.Fprintln(out, "  Files:")
	if len(plan.Destination.Files) > 0 {
		for _, file := range plan.Destination.Files {
			fmt.Fprintf(out, "    - %s %s\n", file.Action, file.Path)
		}
	} else {
		for _, path := range sortedFilePaths(plan.Files) {
			fmt.Fprintf(out, "    - %s\n", path)
		}
	}
	for _, stale := range plan.Destination.StaleFiles {
		fmt.Fprintf(out, "  Warning: existing generated file %s is stale for the selected modes and will be left untouched.\n", stale)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(out, "  Warning: %s\n", safeTerminal(warning))
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
	fmt.Fprintln(out, "\nDry run complete. The scaffold rendered, project.yaml passed strict validation, and the create/replace plan was reviewed, including explicit preserves.")
	fmt.Fprintln(out, "No scaffold files were written and no pull request was opened.")
}

func finishDryRun(out io.Writer, plan *Plan, planOut string) error {
	printDryRun(out, plan)
	if planOut == "" {
		return nil
	}
	canonicalPlanOut, err := canonicalPlanArtifactPath(planOut)
	if err != nil {
		return err
	}
	digest, err := WritePlanArtifact(canonicalPlanOut, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Reviewed plan artifact: %s\n", safeTerminal(canonicalPlanOut))
	fmt.Fprintf(out, "Reviewed plan digest: %s\n", digest)
	return nil
}

func deploymentLabel(mode string) string {
	if mode == modeK8s {
		return "Kubernetes with Helm"
	}
	return "GitHub Pages"
}
