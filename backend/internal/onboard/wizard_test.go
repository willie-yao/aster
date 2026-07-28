package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

type wizardFakeRepositoryClient struct {
	metadata RepositoryMetadata
	err      error
	calls    int
}

func (f *wizardFakeRepositoryClient) Repository(_ context.Context, repo Repo, _ string) (RepositoryMetadata, error) {
	f.calls++
	if f.err != nil {
		return RepositoryMetadata{}, f.err
	}
	out := f.metadata
	if out.Repo.FullName == "" {
		out.Repo = repo
		out.Repo.Branch = "main"
		out.Repo.Visibility = "public"
	}
	return out, nil
}

type wizardFakeCatalogClient struct {
	catalog *jobconfig.Catalog
	err     error
	calls   int
}

func (f *wizardFakeCatalogClient) ForRepo(_ context.Context, _ string) (*jobconfig.Catalog, error) {
	f.calls++
	return f.catalog, f.err
}

type fakeSweeper struct {
	jobs  []models.ProwJob
	err   error
	calls int
}

func (f *fakeSweeper) Discover(_ context.Context, _ *project.Config, _ bool) ([]models.ProwJob, error) {
	f.calls++
	return append([]models.ProwJob(nil), f.jobs...), f.err
}

type fakeRemoteDetector struct {
	remote string
	err    error
}

func (f fakeRemoteDetector) Origin(context.Context) (string, error) {
	return f.remote, f.err
}

type fakePromptBuilder struct {
	content string
	drafted bool
	err     error
	calls   int
	gotOpts Options
}

func (f *fakePromptBuilder) Build(_ context.Context, opts Options, _ scaffoldData) (string, bool, error) {
	f.calls++
	f.gotOpts = opts
	if f.content == "" {
		f.content = "# Prompt\n\nReview this prompt.\n"
	}
	return f.content, f.drafted, f.err
}

type fakeScaffoldWriter struct {
	validateErr error
	writeErr    error
	validates   int
	writes      int
	outDir      string
	files       map[string]string
}

func (f *fakeScaffoldWriter) Validate(_ string, _ map[string]string) error {
	f.validates++
	return f.validateErr
}

func (f *fakeScaffoldWriter) Write(outDir string, files map[string]string) error {
	f.writes++
	f.outDir = outDir
	f.files = cloneFiles(files)
	return f.writeErr
}

type fakePullRequestWriter struct {
	calls int
}

func (f *fakePullRequestWriter) Open(context.Context, Repo, map[string]string, string, string, string, string) (string, error) {
	f.calls++
	return "https://github.com/example/dashboard/pull/1", nil
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("stdin was read")
}

func cloneFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for path, content := range files {
		out[path] = content
	}
	return out
}

func wizardDependencies(input string) (dependencies, *bytes.Buffer, *fakeScaffoldWriter, *fakeSweeper) {
	out := &bytes.Buffer{}
	writer := &fakeScaffoldWriter{}
	sweeper := &fakeSweeper{jobs: []models.ProwJob{
		{Name: "periodic-project-main", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-release-1", JobType: models.JobTypePeriodic},
	}}
	catalog := &jobconfig.Catalog{Revision: "0123456789abcdef", Jobs: map[string]jobconfig.JobDefinition{
		"periodic/example": {
			Name: "periodic-project-main", JobType: models.JobTypePeriodic,
			Refs:        []jobconfig.RepoRef{{Org: "example", Repo: "project", BaseRef: "main"}},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
	}}
	deps := dependencies{
		repositories: &wizardFakeRepositoryClient{metadata: RepositoryMetadata{Repo: Repo{
			Owner: "example", Name: "project", FullName: "example/project",
			Branch: "main", Visibility: "public",
		}}},
		catalogs:     &wizardFakeCatalogClient{catalog: catalog},
		sweeper:      sweeper,
		remotes:      fakeRemoteDetector{err: errNoGitOrigin},
		prompts:      &fakePromptBuilder{},
		files:        writer,
		pullRequests: &fakePullRequestWriter{},
		terminal: Terminal{
			In: strings.NewReader(input), Out: out, Err: out, Interactive: true,
		},
	}
	return deps, out, writer, sweeper
}

func TestWizard_DefaultsAccepted(t *testing.T) {
	input := "\n\n\n\n\n\nn\n\ny\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 1 || writer.outDir != "project-prow-ai-dashboard" {
		t.Fatalf("writes=%d out=%q", writer.writes, writer.outDir)
	}
	projectYAML := writer.files["project.yaml"]
	for _, want := range []string{`id: "project"`, `name: "Project"`, `dashboard: "dashboard-a"`} {
		if !strings.Contains(projectYAML, want) {
			t.Errorf("project.yaml missing %q:\n%s", want, projectYAML)
		}
	}
	if !strings.Contains(writer.files[".github/workflows/deploy.yml"], "ai: false") {
		t.Fatalf("AI-disabled workflow did not carry ai: false:\n%s", writer.files[".github/workflows/deploy.yml"])
	}
	if !strings.Contains(out.String(), "No files or external resources have been changed") {
		t.Fatalf("review did not state the no-write boundary:\n%s", out.String())
	}
}

func TestWizard_InferredValuesCanBeChanged(t *testing.T) {
	input := strings.Join([]string{
		"",                    // strongest dashboard
		"2",                   // Kubernetes
		"example/custom-dash", // dashboard repo
		"custom",              // id
		"Custom Name",         // name
		"CUS",                 // short name
		"n",                   // deployed AI
		"custom-output",       // output
		"y",                   // confirm
	}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "https://github.com/example/project.git", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.outDir != "custom-output" {
		t.Fatalf("out dir = %q", writer.outDir)
	}
	projectYAML := writer.files["project.yaml"]
	for _, want := range []string{`id: "custom"`, `name: "Custom Name"`, `short_name: "CUS"`, `base_path: "/"`} {
		if !strings.Contains(projectYAML, want) {
			t.Errorf("project.yaml missing %q:\n%s", want, projectYAML)
		}
	}
	if _, ok := writer.files["deploy/values.yaml"]; !ok {
		t.Fatalf("Kubernetes values were not generated: %v", writer.files)
	}
}

func TestWizard_CancellationAtEachPromptLeavesNoFiles(t *testing.T) {
	stages := map[string][]string{
		"dashboard candidate": {"q"},
		"deployment":          {"", "q"},
		"dashboard repo":      {"", "", "q"},
		"project id":          {"", "", "", "q"},
		"project name":        {"", "", "", "", "q"},
		"short name":          {"", "", "", "", "", "q"},
		"AI enabled":          {"", "", "", "", "", "", "q"},
		"output":              {"", "", "", "", "", "", "n", "q"},
		"final confirmation":  {"", "", "", "", "", "", "n", "", "q"},
	}
	for name, answers := range stages {
		t.Run(name, func(t *testing.T) {
			deps, out, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
			opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
			if err := run(context.Background(), opts, deps); err != nil {
				t.Fatalf("run: %v\n%s", err, out.String())
			}
			if writer.writes != 0 {
				t.Fatalf("cancelled wizard wrote %d time(s)", writer.writes)
			}
			if !strings.Contains(out.String(), "No files were written") {
				t.Fatalf("cancellation message missing:\n%s", out.String())
			}
		})
	}
}

func TestWizard_EOFCancelsCleanly(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	if writer.writes != 0 || !strings.Contains(out.String(), "No files were written") {
		t.Fatalf("EOF behavior: writes=%d output=%q", writer.writes, out.String())
	}
}

func TestWizard_FinalConfirmationDefaultsToNo(t *testing.T) {
	input := "\n\n\n\n\n\nn\n\n\n"
	deps, _, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("default no wrote %d time(s)", writer.writes)
	}
}

func TestWizard_DryRunPerformsNoWrites(t *testing.T) {
	input := "\n\n\n\n\n\nn\n\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true, DryRun: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 0 || writer.validates != 1 {
		t.Fatalf("dry run writes=%d validates=%d", writer.writes, writer.validates)
	}
	if !strings.Contains(out.String(), "Dry run complete") {
		t.Fatalf("dry-run result missing:\n%s", out.String())
	}
}

func TestWizard_ExistingOutputIsPreserved(t *testing.T) {
	input := "\n\n\n\n\n\nn\n\n"
	deps, _, writer, _ := wizardDependencies(input)
	writer.validateErr = errors.New("refusing to overwrite existing project.yaml")
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	err := run(context.Background(), opts, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("preflight failure wrote %d time(s)", writer.writes)
	}
}

func TestRun_NonInteractiveMissingInputsNeverReadsStdin(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	deps.terminal.In = panicReader{}
	deps.terminal.Interactive = true
	opts := Options{NonInteractive: true}
	err := run(context.Background(), opts, deps)
	if err == nil || !strings.Contains(err.Error(), "non-interactive onboarding requires") {
		t.Fatalf("error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("non-interactive failure wrote %d time(s)", writer.writes)
	}
}

func TestRun_NonTTYMissingInputsFails(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	deps.terminal.Interactive = false
	err := run(context.Background(), Options{}, deps)
	if err == nil || !strings.Contains(err.Error(), "stdin is not an interactive terminal") {
		t.Fatalf("error = %v", err)
	}
}

func TestRun_CompleteFlagsRemainNonInteractive(t *testing.T) {
	deps, _, writer, sweeper := wizardDependencies("")
	deps.terminal.In = panicReader{}
	deps.terminal.Interactive = false
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "git@github.com:example/project.git", Mode: modePages,
		EngineRef: "main", OutDir: "out", NoPrompt: true, AIEnabled: &disabled,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	if writer.writes != 1 || sweeper.calls != 1 {
		t.Fatalf("writes=%d sweeps=%d", writer.writes, sweeper.calls)
	}
}

func TestRun_InteractiveAndFlaggedInputsGenerateSameFiles(t *testing.T) {
	wizardInput := "\n\n\n\n\n\nn\n\ny\n"
	wizardDeps, _, wizardWriter, _ := wizardDependencies(wizardInput)
	if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, wizardDeps); err != nil {
		t.Fatalf("wizard run: %v", err)
	}

	flaggedDeps, _, flaggedWriter, _ := wizardDependencies("")
	flaggedDeps.terminal.Interactive = false
	disabled := false
	flagged := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, ID: "project", Name: "Project",
		EngineRef: "main", OutDir: "project-prow-ai-dashboard", NoPrompt: true, AIEnabled: &disabled,
	}
	if err := run(context.Background(), flagged, flaggedDeps); err != nil {
		t.Fatalf("flagged run: %v", err)
	}
	if !reflect.DeepEqual(wizardWriter.files, flaggedWriter.files) {
		t.Fatalf("wizard and flagged files differ\nwizard=%v\nflagged=%v", wizardWriter.files, flaggedWriter.files)
	}
}

func TestBuildPlan_DoesNotContainTokens(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out",
		NoPrompt: true, AIToken: "fixture-ai-token", GitHubToken: "fixture-github-token",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	all := string(encoded)
	for _, content := range plan.Files {
		all += content
	}
	for _, secret := range []string{"fixture-ai-token", "fixture-github-token"} {
		if strings.Contains(all, secret) {
			t.Fatalf("plan or generated files contain %q", secret)
		}
	}
}

var _ io.Reader = panicReader{}

func TestWizard_CategoryTokensCanBeEdited(t *testing.T) {
	input := strings.Join([]string{"", "", "", "", "", "", "n", "", "custom", "y"}, "\n") + "\n"
	deps, out, writer, sweeper := wizardDependencies(input)
	sweeper.jobs = []models.ProwJob{
		{Name: "periodic-project-aks-one", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-aks-two", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-conformance-one", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-conformance-two", JobType: models.JobTypePeriodic},
	}
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	projectYAML := writer.files["project.yaml"]
	if !strings.Contains(projectYAML, `match: "custom"`) || strings.Contains(projectYAML, `match: "aks"`) {
		t.Fatalf("edited categories were not applied:\n%s", projectYAML)
	}
}

func TestWizard_AdditionalCancellationStages(t *testing.T) {
	t.Run("source input", func(t *testing.T) {
		deps, out, writer, _ := wizardDependencies("q\n")
		if err := run(context.Background(), Options{EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 || !strings.Contains(out.String(), "No files were written") {
			t.Fatalf("writes=%d output=%q", writer.writes, out.String())
		}
	})

	t.Run("detected source confirmation", func(t *testing.T) {
		deps, _, writer, _ := wizardDependencies("q\n")
		deps.remotes = fakeRemoteDetector{remote: "git@github.com:example/project.git"}
		if err := run(context.Background(), Options{EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	t.Run("presubmit choice", func(t *testing.T) {
		deps, _, writer, _ := wizardDependencies("\nq\n")
		catalog := deps.catalogs.(*wizardFakeCatalogClient).catalog
		catalog.Jobs["presubmit/example"] = jobconfig.JobDefinition{
			Name: "pull-project", JobType: models.JobTypePresubmit, Repo: "example/project",
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		}
		if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	stages := map[string][]string{
		"AI API":      {"", "", "", "", "", "", "", "q"},
		"AI endpoint": {"", "", "", "", "", "", "", "", "q"},
	}
	for name, answers := range stages {
		t.Run(name, func(t *testing.T) {
			deps, _, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
			opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
			if err := run(context.Background(), opts, deps); err != nil {
				t.Fatalf("run: %v", err)
			}
			if writer.writes != 0 {
				t.Fatalf("writes=%d", writer.writes)
			}
		})
	}

	t.Run("AI model", func(t *testing.T) {
		answers := []string{"", "", "", "", "", "", "", "", "https://provider.example/v1/chat/completions", "q"}
		deps, _, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
		if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	t.Run("prompt drafting", func(t *testing.T) {
		answers := []string{"", "", "", "", "", "", "", "", "", "q"}
		deps, _, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
		opts := Options{
			SourceRepo: "example/project", EngineRef: "main", AIToken: "fixture-token",
			AIEndpoint: "https://provider.example/v1/chat/completions", AIModel: "model",
		}
		if err := run(context.Background(), opts, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	t.Run("category editing", func(t *testing.T) {
		answers := []string{"", "", "", "", "", "", "n", "", "q"}
		deps, _, writer, sweeper := wizardDependencies(strings.Join(answers, "\n") + "\n")
		sweeper.jobs = []models.ProwJob{
			{Name: "periodic-project-aks-one", JobType: models.JobTypePeriodic},
			{Name: "periodic-project-aks-two", JobType: models.JobTypePeriodic},
			{Name: "periodic-project-other-one", JobType: models.JobTypePeriodic},
		}
		if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})
}

type forkRepositoryClient struct{}

func (forkRepositoryClient) Repository(_ context.Context, repo Repo, _ string) (RepositoryMetadata, error) {
	if repo.FullName == "fork-owner/project" {
		upstream := Repo{Owner: "upstream-owner", Name: "project", FullName: "upstream-owner/project"}
		repo.Branch = "main"
		repo.Visibility = "public"
		return RepositoryMetadata{Repo: repo, Upstream: &upstream}, nil
	}
	repo.Branch = "main"
	repo.Visibility = "public"
	return RepositoryMetadata{Repo: repo}, nil
}

func TestWizard_DetectedForkDefaultsToUpstream(t *testing.T) {
	input := strings.Join([]string{"", "", "", "", "", "", "", "", "n", "", "y"}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	deps.repositories = forkRepositoryClient{}
	deps.remotes = fakeRemoteDetector{remote: "git@github.com:fork-owner/project.git"}
	catalog := deps.catalogs.(*wizardFakeCatalogClient).catalog
	for key, definition := range catalog.Jobs {
		definition.Refs = []jobconfig.RepoRef{{Org: "upstream-owner", Repo: "project", BaseRef: "main"}}
		catalog.Jobs[key] = definition
	}
	if err := run(context.Background(), Options{EngineRef: "main", NoPrompt: true}, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Detected GitHub fork upstream: upstream-owner/project") {
		t.Fatalf("upstream prompt missing:\n%s", out.String())
	}
	if !strings.Contains(writer.files["project.yaml"], `owner: "upstream-owner"`) {
		t.Fatalf("upstream source repo not used:\n%s", writer.files["project.yaml"])
	}
}

func TestRun_OpenPRDryRunDoesNotCallGitHub(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	pullRequests := deps.pullRequests.(*fakePullRequestWriter)
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", NoPrompt: true,
		AIEnabled: &disabled, OpenPR: true, DryRun: true,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if pullRequests.calls != 0 || writer.writes != 0 {
		t.Fatalf("pull requests=%d writes=%d", pullRequests.calls, writer.writes)
	}
}

func TestValidateOptions_RejectsCredentialsInPlanFieldsWithoutLeaking(t *testing.T) {
	opts := testOpts()
	opts.NoPrompt = true
	opts.AIToken = "fixture-ai-token"
	opts.Name = "fixture-ai-token"
	err := validateOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.AIToken) {
		t.Fatalf("credential leaked into error: %v", err)
	}
}

func TestBuildPlan_RejectsCredentialInRenderedFilesWithoutLeaking(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	deps.prompts = &fakePromptBuilder{content: "fixture-ai-token"}
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out",
		NoPrompt: true, AIToken: "fixture-ai-token",
	}
	_, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err == nil || !strings.Contains(err.Error(), "contained a credential") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.AIToken) {
		t.Fatalf("credential leaked into error: %v", err)
	}
}

func TestBuildPlan_SeparatesDraftingAndDeploymentProviders(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	prompts := &fakePromptBuilder{drafted: true}
	deps.prompts = prompts
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main", OutDir: "out",
		AIToken: "fixture-ai-token", AIAPI: project.AIAPIChatCompletions,
		AIEndpoint: "https://draft.example/v1/chat/completions", AIModel: "draft-model",
		DeploymentAIAPI: project.AIAPIResponses, DeploymentAIEndpoint: "https://deploy.example/v1/responses",
		DeploymentAIModel: "deploy-model",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if prompts.gotOpts.AIEndpoint != "https://draft.example/v1/chat/completions" || prompts.gotOpts.AIModel != "draft-model" {
		t.Fatalf("draft provider = %+v", prompts.gotOpts)
	}
	if plan.Deployment.Endpoint != "https://deploy.example/v1/responses" || plan.Deployment.Model != "deploy-model" {
		t.Fatalf("deployment provider = %+v", plan.Deployment)
	}
	if plan.Prompt.Endpoint != "https://draft.example/v1/chat/completions" || plan.Prompt.Model != "draft-model" {
		t.Fatalf("prompt plan = %+v", plan.Prompt)
	}
	values := plan.Files["deploy/values.yaml"]
	if !strings.Contains(values, "https://deploy.example/v1/responses") || strings.Contains(values, "https://draft.example") {
		t.Fatalf("deployment values mixed providers:\n%s", values)
	}
}
