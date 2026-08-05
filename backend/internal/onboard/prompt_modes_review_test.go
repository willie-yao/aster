package onboard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testPromptModeOptions(mode string) Options {
	disabled := false
	return Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out",
		PromptMode: mode, AIEnabled: &disabled,
	}
}

func TestDefaultPromptBuilderHonorsExplicitTemplateWithProviderCredentials(t *testing.T) {
	opts := testPromptModeOptions(promptModeTemplate)
	opts.AIToken = "fixture-token"
	opts.AIEndpoint = "https://provider.example/v1/responses"
	opts.AIModel = "fixture-model"
	prompt, result, err := (defaultPromptBuilder{}).Build(context.Background(), opts, buildScaffoldData(opts, nil), promptDraftInput{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requested != promptRequestTemplate || result.Status != promptStatusTemplate || !strings.Contains(prompt, "## Architecture") {
		t.Fatalf("result=%+v prompt=%q", result, prompt)
	}
}

func TestBuildPlanAcceptsHandoffFiles(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	deps.prompts = &fakePromptBuilder{result: promptPreparationResult{
		Requested: promptRequestHandoff,
		Status:    promptStatusHandoff,
		Output:    promptOutputTemplate,
		Handoff:   "# handoff\n",
	}}
	plan, err := buildPlan(context.Background(), testPromptModeOptions(promptModeHandoff), planningContext{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePlan(plan); err != nil {
		t.Fatalf("validatePlan: %v", err)
	}
	for _, path := range []string{"PROMPT_HANDOFF.md", ".opencode/skills/system-prompt-generation/SKILL.md"} {
		if plan.Files[path] == "" {
			t.Fatalf("missing %s", path)
		}
	}
}

func TestRunAppliesHandoffFiles(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	deps.prompts = &fakePromptBuilder{result: promptPreparationResult{
		Requested: promptRequestHandoff,
		Status:    promptStatusHandoff,
		Output:    promptOutputTemplate,
		Handoff:   "# handoff\n",
	}}
	if err := run(context.Background(), testPromptModeOptions(promptModeHandoff), deps); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 || writer.files["PROMPT_HANDOFF.md"] == "" || writer.files[".opencode/skills/system-prompt-generation/SKILL.md"] == "" {
		t.Fatalf("writes=%d files=%v", writer.writes, sortedFilePaths(writer.files))
	}
}

func TestBuildPlanAgentDraftHasNoHandoffFiles(t *testing.T) {
	deps, _, _, _ := wizardDependencies("")
	deps.prompts = &fakePromptBuilder{result: promptPreparationResult{
		Requested: promptRequestAgent,
		Status:    promptStatusAgentDraft,
		Output:    promptOutputAgentDraft,
	}}
	plan, err := buildPlan(context.Background(), testPromptModeOptions(promptModeAgent), planningContext{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePlan(plan); err != nil {
		t.Fatalf("validatePlan: %v", err)
	}
	for _, path := range []string{"PROMPT_HANDOFF.md", ".opencode/skills/system-prompt-generation/SKILL.md"} {
		if _, ok := plan.Files[path]; ok {
			t.Fatalf("agent draft unexpectedly included %s", path)
		}
	}
}

func TestValidatePromptPlanBindsAgentFallbackDiagnostics(t *testing.T) {
	failure := &promptPreparationFailure{Stage: promptStageFinalPromptValidation, Category: promptFailureUnknown}
	plan := (promptPreparationResult{
		Requested: promptRequestAgent,
		Status:    promptStatusAgentFallback,
		Output:    promptOutputTemplate,
		Failure:   failure,
	}).promptPlan(Options{})
	if err := validatePromptPlan(plan); err != nil {
		t.Fatalf("validatePromptPlan: %v", err)
	}
	plan.RequestedMode = string(promptRequestHandoff)
	if err := validatePromptPlan(plan); err == nil {
		t.Fatal("expected mismatched agent fallback request to fail")
	}
}

func TestValidateOptionsRejectsNoPromptConflict(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.NoPrompt = true
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequirePromptDraftSupportsAgentWithoutAPICredentials(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.RequirePromptDraft = true
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("validateOptions: %v", err)
	}

	deps, _, _, _ := wizardDependencies("")
	failure := &promptPreparationFailure{Stage: promptStageFinalPromptValidation, Category: promptFailureUnknown}
	deps.prompts = &fakePromptBuilder{result: promptPreparationResult{
		Requested: promptRequestAgent,
		Status:    promptStatusAgentFallback,
		Output:    promptOutputTemplate,
		Handoff:   "# handoff\n",
		Failure:   failure,
	}}
	_, err := buildPlan(context.Background(), opts, planningContext{}, deps)
	var strictErr *requiredPromptDraftError
	if !errors.As(err, &strictErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestWizardPromptAuthoringKeepsSelectedMode(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		inputs          []string
		confirms        []bool
		wantNoPrompt    bool
		wantModel       string
		wantPromptAPI   string
		wantPromptModel string
	}{
		{name: "agent", mode: promptModeAgent, inputs: []string{usePromptDefault}, wantModel: defaultPromptAgentModel},
		{name: "handoff", mode: promptModeHandoff},
		{name: "template", mode: promptModeTemplate, wantNoPrompt: true},
		{name: "api", mode: promptModeAPI, confirms: []bool{true}, wantPromptAPI: "responses", wantPromptModel: "deployed-model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled := true
			opts := Options{
				AIEnabled: &enabled, DeploymentAIAPI: "responses",
				DeploymentAIEndpoint: "https://provider.example/v1/responses", DeploymentAIModel: "deployed-model",
			}
			ui := &queuedWizardUI{selects: []string{tt.mode}, inputs: tt.inputs, confirms: tt.confirms}
			if err := wizardPromptAuthoring(context.Background(), ui, &opts); err != nil {
				t.Fatal(err)
			}
			if opts.PromptMode != tt.mode || opts.NoPrompt != tt.wantNoPrompt || opts.PromptAgentModel != tt.wantModel {
				t.Fatalf("opts = %+v", opts)
			}
			if tt.wantPromptAPI != "" && (opts.AIAPI != tt.wantPromptAPI || opts.AIModel != tt.wantPromptModel) {
				t.Fatalf("prompt provider = %s %s", opts.AIAPI, opts.AIModel)
			}
		})
	}
}

func TestWizardPromptAuthoringDeclinedAPIUsesTemplate(t *testing.T) {
	enabled := true
	opts := Options{
		AIEnabled: &enabled, DeploymentAIAPI: "responses",
		DeploymentAIEndpoint: "https://provider.example/v1/responses", DeploymentAIModel: "deployed-model",
	}
	ui := &queuedWizardUI{selects: []string{promptModeAPI}, confirms: []bool{false}}
	if err := wizardPromptAuthoring(context.Background(), ui, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.PromptMode != promptModeTemplate || !opts.NoPrompt {
		t.Fatalf("opts = %+v", opts)
	}
	if len(ui.confirmPrompts) != 1 || ui.confirmPrompts[0].Value || !strings.Contains(ui.confirmPrompts[0].Description, "bounded repository") {
		t.Fatalf("confirmation = %+v", ui.confirmPrompts)
	}
}

func TestWizardPromptAuthoringReplacesPartialAPIProvider(t *testing.T) {
	enabled := true
	opts := Options{
		AIEnabled: &enabled, AIEndpoint: "https://partial.example/v1/responses",
		DeploymentAIAPI: "responses", DeploymentAIEndpoint: "https://provider.example/v1/responses", DeploymentAIModel: "deployed-model",
	}
	ui := &queuedWizardUI{selects: []string{promptModeAPI}, confirms: []bool{true}}
	if err := wizardPromptAuthoring(context.Background(), ui, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.AIAPI != "responses" || opts.AIEndpoint != opts.DeploymentAIEndpoint || opts.AIModel != opts.DeploymentAIModel {
		t.Fatalf("prompt provider = %s %s %s", opts.AIAPI, opts.AIEndpoint, opts.AIModel)
	}
}
