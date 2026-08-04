package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

const defaultTestDashboardRepo = "example/project-prow-ai-dashboard"

type wizardFakeRepositoryClient struct {
	metadata      RepositoryMetadata
	err           error
	calls         int
	authLogin     string
	authErr       error
	authCalls     int
	authTokenSeen string
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

func (f *wizardFakeRepositoryClient) AuthenticatedLogin(_ context.Context, token string) (string, error) {
	f.authCalls++
	f.authTokenSeen = token
	return f.authLogin, f.authErr
}

type wizardFakeCatalogClient struct {
	catalog *jobconfig.Catalog
	err     error
	calls   int
}

func (f *wizardFakeCatalogClient) Catalog(_ context.Context) (*jobconfig.Catalog, error) {
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
	remote  string
	err     error
	root    string
	rootErr error
}

func (f fakeRemoteDetector) Origin(context.Context) (string, error) {
	return f.remote, f.err
}

func (f fakeRemoteDetector) Root(context.Context) (string, error) {
	if f.rootErr != nil {
		return "", f.rootErr
	}
	if f.root != "" {
		return f.root, nil
	}
	return os.Getwd()
}

type fakePromptBuilder struct {
	content  string
	drafted  bool
	result   promptPreparationResult
	results  []promptPreparationResult
	err      error
	calls    int
	gotOpts  Options
	gotInput promptDraftInput
}

func (f *fakePromptBuilder) Build(_ context.Context, opts Options, _ scaffoldData, input promptDraftInput) (string, promptPreparationResult, error) {
	f.calls++
	f.gotOpts = opts
	f.gotInput = input
	if f.content == "" {
		f.content = "# Prompt\n\nReview this prompt.\n"
	}
	result := f.result
	if len(f.results) >= f.calls {
		result = f.results[f.calls-1]
	}
	if result.Requested == "" {
		if f.drafted {
			result = newAPIPromptResult()
		} else {
			result = newTemplatePromptResult()
		}
	}
	return f.content, result, f.err
}

type fakeScaffoldWriter struct {
	validateErr    error
	writeErr       error
	validates      int
	writes         int
	outDir         string
	files          map[string]string
	inspection     []DestinationFilePlan
	inspections    map[string][]DestinationFilePlan
	staleFiles     []string
	inspectOutDirs []string
	updateExisting bool
}

func (f *fakeScaffoldWriter) Inspect(outDir string, files map[string]string) ([]DestinationFilePlan, []string, error) {
	f.validates++
	f.inspectOutDirs = append(f.inspectOutDirs, outDir)
	if f.validateErr != nil {
		return nil, nil, f.validateErr
	}
	if inspection, ok := f.inspections[outDir]; ok {
		return append([]DestinationFilePlan(nil), inspection...), append([]string(nil), f.staleFiles...), nil
	}
	if f.inspection != nil {
		return append([]DestinationFilePlan(nil), f.inspection...), append([]string(nil), f.staleFiles...), nil
	}
	actions := make([]DestinationFilePlan, 0, len(files))
	for _, path := range sortedFilePaths(files) {
		actions = append(actions, DestinationFilePlan{Path: path, Action: destinationActionCreate})
	}
	return actions, append([]string(nil), f.staleFiles...), nil
}

func (f *fakeScaffoldWriter) Write(outDir string, files map[string]string, updateExisting bool, _ []DestinationFilePlan) error {
	f.writes++
	f.outDir = outDir
	f.files = cloneFiles(files)
	f.updateExisting = updateExisting
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
	deps.wizard = newLineWizardUI(deps.terminal)
	return deps, out, writer, sweeper
}

func TestWizard_DefaultsAccepted(t *testing.T) {
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "y"}, "\n") + "\n"
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

func TestWizard_ConfigureLaterProducesDisabledScaffold(t *testing.T) {
	input := strings.Join([]string{
		"",                       // strongest dashboard
		"",                       // deployment
		defaultTestDashboardRepo, // dashboard repo
		"",                       // id
		"",                       // name
		"",                       // short name
		"",                       // enable AI
		"9",                      // configure later
		"",                       // output
		"y",                      // confirm
	}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d", writer.writes)
	}
	workflow := writer.files[".github/workflows/deploy.yml"]
	if !strings.Contains(workflow, "ai: false") {
		t.Fatalf("configure later did not disable AI:\n%s", workflow)
	}
	if !strings.Contains(writer.files["CHECKLIST.md"], "AI is disabled in the initial workflow") {
		t.Fatalf("configure-later checklist guidance missing:\n%s", writer.files["CHECKLIST.md"])
	}
	if !strings.Contains(out.String(), "AI analysis:          disabled in initial scaffold") {
		t.Fatalf("review did not show initial AI state:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Deployed AI endpoint") || strings.Contains(out.String(), "Deployed AI model") {
		t.Fatalf("configure later requested provider coordinates:\n%s", out.String())
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
		"project id":          {"", "", defaultTestDashboardRepo, "q"},
		"project name":        {"", "", defaultTestDashboardRepo, "", "q"},
		"short name":          {"", "", defaultTestDashboardRepo, "", "", "q"},
		"AI enabled":          {"", "", defaultTestDashboardRepo, "", "", "", "q"},
		"output":              {"", "", defaultTestDashboardRepo, "", "", "", "n", "q"},
		"final confirmation":  {"", "", defaultTestDashboardRepo, "", "", "", "n", "", "q"},
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
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", ""}, "\n") + "\n"
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
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", ""}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true, DryRun: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 0 || writer.validates != 2 {
		t.Fatalf("dry run writes=%d validates=%d", writer.writes, writer.validates)
	}
	if !strings.Contains(out.String(), "Dry run complete") {
		t.Fatalf("dry-run result missing:\n%s", out.String())
	}
}

func TestWizard_ExistingOutputIsPreserved(t *testing.T) {
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", ""}, "\n") + "\n"
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
	deps.wizard = panicWizardUI{}
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
	wizardInput := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "y"}, "\n") + "\n"
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

func TestRun_K8sInteractiveAndFlaggedInputsGenerateSameFiles(t *testing.T) {
	wizardInput := strings.Join([]string{"", "2", defaultTestDashboardRepo, "", "", "", "n", "", "y"}, "\n") + "\n"
	wizardDeps, _, wizardWriter, _ := wizardDependencies(wizardInput)
	if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, wizardDeps); err != nil {
		t.Fatalf("wizard run: %v", err)
	}

	flaggedDeps, _, flaggedWriter, _ := wizardDependencies("")
	flaggedDeps.terminal.Interactive = false
	disabled := false
	flagged := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modeK8s, ID: "project", Name: "Project",
		EngineRef: "main", OutDir: "project-prow-ai-dashboard", NoPrompt: true, AIEnabled: &disabled,
	}
	if err := run(context.Background(), flagged, flaggedDeps); err != nil {
		t.Fatalf("flagged run: %v", err)
	}
	if !reflect.DeepEqual(wizardWriter.files, flaggedWriter.files) {
		t.Fatalf("wizard and flagged Kubernetes files differ\nwizard=%v\nflagged=%v", wizardWriter.files, flaggedWriter.files)
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
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "custom", "y"}, "\n") + "\n"
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

func TestPromptDraftDisclosureCoversSourceAndJobs(t *testing.T) {
	for _, want := range []string{"repository documentation", "source excerpts", "matched Prow job metadata"} {
		if !strings.Contains(promptDraftDisclosure, want) {
			t.Fatalf("prompt drafting disclosure missing %q: %s", want, promptDraftDisclosure)
		}
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
		"AI provider":   {"", "", defaultTestDashboardRepo, "", "", "", "", "q"},
		"AI custom API": {"", "", defaultTestDashboardRepo, "", "", "", "", "8", "q"},
		"AI endpoint":   {"", "", defaultTestDashboardRepo, "", "", "", "", "8", "", "q"},
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
		answers := []string{"", "", defaultTestDashboardRepo, "", "", "", "", "8", "", "https://provider.example/v1/chat/completions", "q"}
		deps, _, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
		if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	t.Run("Pages endpoint warning", func(t *testing.T) {
		answers := []string{"", "", defaultTestDashboardRepo, "", "", "", "", "8", "", "http://localhost:8000/v1/chat/completions", "model", "q"}
		deps, _, writer, _ := wizardDependencies(strings.Join(answers, "\n") + "\n")
		if err := run(context.Background(), Options{SourceRepo: "example/project", EngineRef: "main", NoPrompt: true}, deps); err != nil {
			t.Fatalf("run: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes=%d", writer.writes)
		}
	})

	t.Run("prompt drafting", func(t *testing.T) {
		answers := []string{"", "", defaultTestDashboardRepo, "", "", "", "", "", "", "q"}
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
		answers := []string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "q"}
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

func (forkRepositoryClient) AuthenticatedLogin(context.Context, string) (string, error) {
	return "", nil
}

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

func TestWizard_DetectedForkUsesUpstreamSourceAndForkDashboardOwner(t *testing.T) {
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
	projectYAML := writer.files["project.yaml"]
	if !strings.Contains(projectYAML, `owner: "upstream-owner"`) {
		t.Fatalf("upstream source repo not used:\n%s", projectYAML)
	}
	if !strings.Contains(projectYAML, `site_url: "https://fork-owner.github.io/project-prow-ai-dashboard"`) {
		t.Fatalf("detected fork owner was not preserved for the dashboard destination:\n%s", projectYAML)
	}
	if !strings.Contains(out.String(), "original Git remote owner and source repository name (high confidence)") {
		t.Fatalf("dashboard inference source missing:\n%s", out.String())
	}
	if writer.outDir != filepath.Join("..", "project-prow-ai-dashboard") {
		t.Fatalf("dashboard consumer directory = %q", writer.outDir)
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
	if pullRequests.calls != 0 || writer.writes != 0 || writer.validates != 0 {
		t.Fatalf("pull requests=%d writes=%d local inspections=%d", pullRequests.calls, writer.writes, writer.validates)
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

func TestWizard_ExplicitTestGridPromptsForRequiredPresubmits(t *testing.T) {
	input := strings.Join([]string{
		"",                       // include presubmits, default yes because the dashboard has no periodics
		"",                       // deployment
		defaultTestDashboardRepo, // dashboard repo
		"",                       // id
		"",                       // name
		"",                       // short name
		"n",                      // AI
		"",                       // output
		"y",                      // confirm
	}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	catalog := deps.catalogs.(*wizardFakeCatalogClient).catalog
	catalog.Jobs = map[string]jobconfig.JobDefinition{
		"presubmit/example": {
			Name: "pull-project", JobType: models.JobTypePresubmit, Repo: "example/project",
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
	}
	opts := Options{
		SourceRepo: "example/project", TestGrid: "dashboard-a", EngineRef: "main", NoPrompt: true,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 1 || !strings.Contains(writer.files["project.yaml"], "include_presubmits: true") {
		t.Fatalf("presubmit-only dashboard was not enabled:\n%s", writer.files["project.yaml"])
	}
}

func TestSetPlanCategoryTokens_RejectsCredentialBeforeMutation(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: "example/project-prow-ai-dashboard",
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out",
		NoPrompt: true, AIToken: "fixture-ai-token",
	}
	plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	before := plan.Files["project.yaml"]
	err = setPlanCategoryTokens(plan, opts, opts.AIToken)
	if err == nil || !strings.Contains(err.Error(), "contained a credential") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.AIToken) {
		t.Fatalf("credential leaked into error: %v", err)
	}
	if plan.Files["project.yaml"] != before {
		t.Fatal("plan mutated before the credential check")
	}
}

func TestWizard_ValidatesSeedCredentialsBeforeOutput(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	opts := Options{
		SourceRepo: "example/project", AIToken: "fixture-ai-token", AIModel: "fixture-ai-token",
	}
	err := run(context.Background(), opts, deps)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wizard emitted secret-bearing defaults before validation: %q", out.String())
	}
	if writer.writes != 0 {
		t.Fatalf("writes = %d", writer.writes)
	}
}

func TestWizard_APIModeValueIsExplicitWithoutBookkeeping(t *testing.T) {
	input := strings.Join([]string{
		"",                       // strongest dashboard
		defaultTestDashboardRepo, // dashboard repo
		"",                       // id
		"",                       // name
		"",                       // short name
		"n",                      // AI
		"",                       // output
		"y",                      // confirm
	}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	opts := Options{SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if _, ok := writer.files["deploy/values.yaml"]; !ok {
		t.Fatalf("API-supplied mode was overwritten: %v", writer.files)
	}
	if strings.Contains(out.String(), "Deployment profile") {
		t.Fatalf("API-supplied mode triggered a deployment prompt:\n%s", out.String())
	}
}

func TestWizard_ClearSentinelsRemoveOptionalSuggestions(t *testing.T) {
	input := strings.Join([]string{
		"",                       // strongest dashboard
		"",                       // deployment
		defaultTestDashboardRepo, // dashboard repo
		"",                       // id
		"",                       // name
		"none",                   // omit inferred short name
		"n",                      // AI
		"",                       // output
		"none",                   // clear categories
		"y",                      // confirm
	}, "\n") + "\n"
	deps, out, writer, sweeper := wizardDependencies(input)
	sweeper.jobs = []models.ProwJob{
		{Name: "periodic-project-aks-one", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-aks-two", JobType: models.JobTypePeriodic},
		{Name: "periodic-project-other-one", JobType: models.JobTypePeriodic},
	}
	opts := Options{SourceRepo: "example/my-project", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	projectYAML := writer.files["project.yaml"]
	if strings.Contains(projectYAML, "short_name:") || strings.Contains(projectYAML, "categories:") {
		t.Fatalf("clear sentinels were ignored:\n%s", projectYAML)
	}
}

func TestCredentialSeparationCoversShortTokens(t *testing.T) {
	opts := testOpts()
	opts.NoPrompt = true
	opts.AIToken = "short"
	opts.Name = "short"
	err := validateOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
}

func TestWizard_RejectsCredentialsEnteredAsRepositoriesWithoutLeaking(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		deps, _, writer, _ := wizardDependencies("fixture-ai-token\n")
		opts := Options{AIToken: "fixture-ai-token", EngineRef: "main", NoPrompt: true}
		err := run(context.Background(), opts, deps)
		if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), opts.AIToken) {
			t.Fatalf("credential leaked into error: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes = %d", writer.writes)
		}
	})

	t.Run("dashboard", func(t *testing.T) {
		input := strings.Join([]string{"", "", "fixture-ai-token"}, "\n") + "\n"
		deps, _, writer, _ := wizardDependencies(input)
		opts := Options{SourceRepo: "example/project", AIToken: "fixture-ai-token", EngineRef: "main", NoPrompt: true}
		err := run(context.Background(), opts, deps)
		if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), opts.AIToken) {
			t.Fatalf("credential leaked into error: %v", err)
		}
		if writer.writes != 0 {
			t.Fatalf("writes = %d", writer.writes)
		}
	})
}

func TestWizard_UsesCanonicalRepositoryFromGitHub(t *testing.T) {
	input := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "y"}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	deps.repositories = &wizardFakeRepositoryClient{metadata: RepositoryMetadata{Repo: Repo{
		Owner: "canonical", Name: "project", FullName: "canonical/project", Branch: "main", Visibility: "public",
	}}}
	catalog := deps.catalogs.(*wizardFakeCatalogClient).catalog
	for key, definition := range catalog.Jobs {
		definition.Refs = []jobconfig.RepoRef{{Org: "canonical", Repo: "project", BaseRef: "main"}}
		catalog.Jobs[key] = definition
	}
	opts := Options{SourceRepo: "old-owner/old-name", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	projectYAML := writer.files["project.yaml"]
	if !strings.Contains(projectYAML, `owner: "canonical"`) || !strings.Contains(projectYAML, `name: "project"`) {
		t.Fatalf("canonical source repo not rendered:\n%s", projectYAML)
	}
	if strings.Contains(projectYAML, "old-owner") || strings.Contains(projectYAML, "old-name") {
		t.Fatalf("stale source repo retained:\n%s", projectYAML)
	}
}

func TestWizard_DashboardWidePresubmitsControlPrompt(t *testing.T) {
	input := strings.Join([]string{
		"y",                      // include dashboard presubmits even though none test the source repo
		"",                       // deployment
		defaultTestDashboardRepo, // dashboard repo
		"",                       // id
		"",                       // name
		"",                       // short name
		"n",                      // AI
		"",                       // output
		"y",                      // confirm
	}, "\n") + "\n"
	deps, out, writer, _ := wizardDependencies(input)
	catalog := deps.catalogs.(*wizardFakeCatalogClient).catalog
	catalog.Jobs = map[string]jobconfig.JobDefinition{
		"source-periodic": {
			Name: "source-periodic", JobType: models.JobTypePeriodic,
			Refs:        []jobconfig.RepoRef{{Org: "example", Repo: "project"}},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
		"other-presubmit": {
			Name: "other-presubmit", JobType: models.JobTypePresubmit, Repo: "example/other",
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
	}
	opts := Options{SourceRepo: "example/project", TestGrid: "dashboard-a", EngineRef: "main", NoPrompt: true}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Include presubmit jobs in the dashboard?") {
		t.Fatalf("dashboard-wide presubmits did not trigger the prompt:\n%s", out.String())
	}
	if !strings.Contains(writer.files["project.yaml"], "include_presubmits: true") {
		t.Fatalf("presubmit choice was not rendered:\n%s", writer.files["project.yaml"])
	}
}

func TestWizard_AuthenticatedUserOwnsUpstreamDashboardSuggestion(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	repositories := deps.repositories.(*wizardFakeRepositoryClient)
	repositories.metadata.Repo = Repo{Owner: "upstream-org", Name: "project", FullName: "upstream-org/project", Branch: "main", Visibility: "public"}
	repositories.authLogin = "authenticated-owner"
	for key, definition := range deps.catalogs.(*wizardFakeCatalogClient).catalog.Jobs {
		definition.Refs = []jobconfig.RepoRef{{Org: "upstream-org", Repo: "project", BaseRef: "main"}}
		deps.catalogs.(*wizardFakeCatalogClient).catalog.Jobs[key] = definition
	}
	ui := &queuedWizardUI{inputs: []string{usePromptDefault}}
	deps.wizard = ui
	disabled := false
	plan, _, err := runWizard(context.Background(), Options{
		SourceRepo: "upstream-org/project", TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", ShortName: "P", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true, DryRun: true, GitHubToken: "fixture-token",
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if plan.DashboardRepo.FullName != "authenticated-owner/project-prow-ai-dashboard" {
		t.Fatalf("dashboard repo = %+v", plan.DashboardRepo)
	}
	if repositories.authCalls != 1 || repositories.authTokenSeen != "fixture-token" {
		t.Fatalf("auth calls=%d token=%q", repositories.authCalls, repositories.authTokenSeen)
	}
	if len(ui.inputPrompts) != 1 || ui.inputPrompts[0].Title != "Dashboard repository" || ui.inputPrompts[0].Value != plan.DashboardRepo.FullName {
		t.Fatalf("dashboard prompt = %+v", ui.inputPrompts)
	}
	if inferred := plan.Provenance["dashboard_repo"]; inferred.Source != "authenticated GitHub login and source repository name" || inferred.Confidence != ConfidenceHigh {
		t.Fatalf("dashboard provenance = %+v", inferred)
	}
}

func TestWizard_NoSafeDashboardOwnerRequiresInput(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	ui := &queuedWizardUI{inputs: []string{"chosen-owner/project-prow-ai-dashboard"}}
	deps.wizard = ui
	disabled := false
	plan, _, err := runWizard(context.Background(), Options{
		SourceRepo: "example/project", TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", ShortName: "P", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true, DryRun: true,
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if len(ui.inputPrompts) != 1 || ui.inputPrompts[0].Title != "Dashboard repository" || ui.inputPrompts[0].Value != "" || !ui.inputPrompts[0].Required {
		t.Fatalf("dashboard prompt = %+v", ui.inputPrompts)
	}
	if plan.DashboardRepo.FullName != "chosen-owner/project-prow-ai-dashboard" {
		t.Fatalf("dashboard repo = %+v", plan.DashboardRepo)
	}
	if inferred := plan.Provenance["dashboard_repo"]; inferred.Source != "confirmed dashboard repository input" || inferred.Confidence != ConfidenceHigh {
		t.Fatalf("dashboard provenance = %+v", inferred)
	}
}

func TestWizard_ExplicitDashboardRepositorySkipsOwnerLookup(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	repositories := deps.repositories.(*wizardFakeRepositoryClient)
	repositories.authLogin = "authenticated-owner"
	ui := &queuedWizardUI{}
	deps.wizard = ui
	disabled := false
	plan, _, err := runWizard(context.Background(), Options{
		SourceRepo: "example/project", DashboardRepo: "explicit-owner/dashboard", TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", ShortName: "P", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true, DryRun: true, GitHubToken: "fixture-token",
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if plan.DashboardRepo.FullName != "explicit-owner/dashboard" || repositories.authCalls != 0 || len(ui.inputPrompts) != 0 {
		t.Fatalf("plan=%+v authCalls=%d prompts=%+v", plan.DashboardRepo, repositories.authCalls, ui.inputPrompts)
	}
}

func TestWizard_ShortNameDefaultsEmpty(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	ui := &queuedWizardUI{inputs: []string{usePromptDefault}}
	deps.wizard = ui
	disabled := false
	plan, _, err := runWizard(context.Background(), Options{
		SourceRepo: "example/project", DashboardRepo: defaultTestDashboardRepo, TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true, DryRun: true,
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if len(ui.inputPrompts) != 1 || ui.inputPrompts[0].Title != "Short name" || ui.inputPrompts[0].Value != "" {
		t.Fatalf("short-name prompt = %+v", ui.inputPrompts)
	}
	if plan.Project.ShortName != "" || strings.Contains(plan.Files["project.yaml"], "short_name:") {
		t.Fatalf("project short name = %q\n%s", plan.Project.ShortName, plan.Files["project.yaml"])
	}
}

func TestWizard_ExplicitShortNameIsPreserved(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	ui := &queuedWizardUI{}
	deps.wizard = ui
	disabled := false
	plan, _, err := runWizard(context.Background(), Options{
		SourceRepo: "example/project", DashboardRepo: defaultTestDashboardRepo, TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", ShortName: "CAPZ", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true, DryRun: true,
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if plan.Project.ShortName != "CAPZ" || !strings.Contains(plan.Files["project.yaml"], `short_name: "CAPZ"`) || len(ui.inputPrompts) != 0 {
		t.Fatalf("short name=%q prompts=%+v\n%s", plan.Project.ShortName, ui.inputPrompts, plan.Files["project.yaml"])
	}
}

func testPagesDestinationActions(replace ...string) []DestinationFilePlan {
	replacements := map[string]struct{}{}
	for _, path := range replace {
		replacements[path] = struct{}{}
	}
	paths := []string{".github/workflows/deploy.yml", "CHECKLIST.md", "project.yaml", "prompts/system.md"}
	actions := make([]DestinationFilePlan, 0, len(paths))
	for _, path := range paths {
		action := destinationActionCreate
		if _, ok := replacements[path]; ok {
			action = destinationActionReplace
		}
		actions = append(actions, DestinationFilePlan{Path: path, Action: action})
	}
	return actions
}

func TestRun_NonInteractiveConflictsRequireUpdateExisting(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	writer.inspection = testPagesDestinationActions("project.yaml")
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo, SourceRepo: "example/project",
		Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true, AIEnabled: &disabled,
	}
	err := run(context.Background(), opts, deps)
	var conflict *destinationConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("writes = %d", writer.writes)
	}
}

func TestRun_UpdateExistingWritesOnlyReviewedFiles(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	writer.inspection = testPagesDestinationActions("project.yaml")
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo, SourceRepo: "example/project",
		Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true, AIEnabled: &disabled,
		UpdateExisting: true,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if writer.writes != 1 || !writer.updateExisting {
		t.Fatalf("writes=%d update=%t", writer.writes, writer.updateExisting)
	}
	if !strings.Contains(out.String(), "replace project.yaml") || !strings.Contains(out.String(), "create prompts/system.md") {
		t.Fatalf("review omitted create/replace plan:\n%s", out.String())
	}
}

func TestPrepareInteractiveDestinationChooseAnotherDirectory(t *testing.T) {
	writer := &fakeScaffoldWriter{inspections: map[string][]DestinationFilePlan{
		"old": {{Path: "project.yaml", Action: destinationActionReplace}},
		"new": {{Path: "project.yaml", Action: destinationActionCreate}},
	}}
	deps := dependencies{files: writer}
	ui := &queuedWizardUI{selects: []string{"another"}, inputs: []string{"new"}}
	opts := Options{OutDir: "old"}
	plan := &Plan{Destination: DestinationPlan{OutDir: "old"}, Files: map[string]string{"project.yaml": "content"}}
	if err := prepareInteractiveDestination(context.Background(), ui, &opts, plan, deps); err != nil {
		t.Fatal(err)
	}
	if opts.OutDir != "new" || plan.Destination.OutDir != "new" || plan.Destination.Files[0].Action != destinationActionCreate {
		t.Fatalf("opts=%+v destination=%+v", opts, plan.Destination)
	}
	if len(ui.selectPrompts) != 1 || ui.selectPrompts[0].Value != "another" || len(ui.inputPrompts) != 1 || ui.inputPrompts[0].Title != "Dashboard consumer directory" {
		t.Fatalf("selects=%+v inputs=%+v", ui.selectPrompts, ui.inputPrompts)
	}
}

func TestPrepareInteractiveDestinationCancellation(t *testing.T) {
	writer := &fakeScaffoldWriter{inspection: []DestinationFilePlan{{Path: "project.yaml", Action: destinationActionReplace}}}
	deps := dependencies{files: writer}
	ui := &queuedWizardUI{selects: []string{"cancel"}}
	opts := Options{OutDir: "old"}
	plan := &Plan{Destination: DestinationPlan{OutDir: "old"}, Files: map[string]string{"project.yaml": "content"}}
	if err := prepareInteractiveDestination(context.Background(), ui, &opts, plan, deps); !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v", err)
	}
}

func TestWizard_InteractiveUpdateRequiresFinalConfirmation(t *testing.T) {
	deps, out, _, _ := wizardDependencies("")
	writer := deps.files.(*fakeScaffoldWriter)
	writer.inspection = testPagesDestinationActions("project.yaml")
	ui := &queuedWizardUI{selects: []string{"update"}, confirms: []bool{true}}
	deps.wizard = ui
	disabled := false
	plan, opts, err := runWizard(context.Background(), Options{
		SourceRepo: "example/project", DashboardRepo: defaultTestDashboardRepo, TestGrid: "dashboard-a", Mode: modePages,
		ID: "project", Name: "Project", ShortName: "P", OutDir: "out", EngineRef: "main",
		AIEnabled: &disabled, NoPrompt: true,
	}, deps)
	if err != nil {
		t.Fatalf("runWizard: %v\n%s", err, out.String())
	}
	if !opts.UpdateExisting || !plan.Destination.UpdateExisting {
		t.Fatalf("opts=%+v destination=%+v", opts, plan.Destination)
	}
	if len(ui.confirmPrompts) != 1 || ui.confirmPrompts[0].Title != "Create and update these scaffold files?" || ui.confirmPrompts[0].Value {
		t.Fatalf("confirmation = %+v", ui.confirmPrompts)
	}
}

func TestRun_DryRunShowsCreateReplaceAndStaleFiles(t *testing.T) {
	deps, out, writer, _ := wizardDependencies("")
	writer.inspection = testPagesDestinationActions("project.yaml")
	writer.staleFiles = []string{"deploy/values.yaml"}
	disabled := false
	opts := Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo, SourceRepo: "example/project",
		Mode: modePages, EngineRef: "main", OutDir: "out", NoPrompt: true, AIEnabled: &disabled,
		UpdateExisting: true, DryRun: true,
	}
	if err := run(context.Background(), opts, deps); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Dashboard consumer directory: out", "replace project.yaml", "create prompts/system.md", "deploy/values.yaml", "left untouched", "create/replace plan was reviewed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
	if writer.writes != 0 {
		t.Fatalf("writes = %d", writer.writes)
	}
}

func TestSiblingDashboardConsumerDirUsesCheckoutRoot(t *testing.T) {
	got := siblingDashboardConsumerDir("/workspace/source", "/workspace/source/backend", "project-prow-ai-dashboard")
	want := filepath.Join("..", "..", "project-prow-ai-dashboard")
	if got != want {
		t.Fatalf("sibling directory = %q, want %q", got, want)
	}
}
