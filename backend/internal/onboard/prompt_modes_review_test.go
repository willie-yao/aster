package onboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testPromptModeOptions(mode string) Options {
	disabled := false
	return Options{
		TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
		SourceRepo: "example/project", Mode: modePages, EngineRef: "main", OutDir: "out",
		PromptMode: mode, AIEnabled: &disabled,
	}
}

func TestDefaultPromptModeIsHandoff(t *testing.T) {
	if got := effectivePromptMode(Options{}); got != promptModeHandoff {
		t.Fatalf("mode = %q", got)
	}
}

func TestDefaultPromptBuilderHonorsExplicitTemplate(t *testing.T) {
	opts := testPromptModeOptions(promptModeTemplate)
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

func TestValidateOptionsRejectsNoPromptConflict(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.NoPrompt = true
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateOptionsRejectsMalformedAgentModel(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentModel = "claude"
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequirePromptDraftSupportsOnlyAgent(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.RequirePromptDraft = true
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("validateOptions: %v", err)
	}

	deps, _, _, _ := wizardDependencies("")
	failure := &promptPreparationFailure{Stage: promptStageAgentExecution, Category: promptFailureAgentExecution}
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

	for _, mode := range []string{promptModeHandoff, promptModeTemplate} {
		invalid := testPromptModeOptions(mode)
		invalid.RequirePromptDraft = true
		if err := validateOptions(&invalid); err == nil {
			t.Fatalf("strict mode %q was accepted", mode)
		}
	}
}

func TestWizardPromptAuthoringKeepsSelectedMode(t *testing.T) {
	tests := []struct {
		mode         string
		inputs       []string
		wantNoPrompt bool
		wantModel    string
	}{
		{mode: promptModeAgent, inputs: []string{usePromptDefault}, wantModel: defaultPromptAgentModel},
		{mode: promptModeHandoff},
		{mode: promptModeTemplate, wantNoPrompt: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			opts := Options{}
			selects := []string{tt.mode}
			if tt.mode == promptModeAgent {
				selects = append(selects, promptRuntimeOpenCode)
			}
			ui := &queuedWizardUI{selects: selects, inputs: tt.inputs}
			if err := wizardPromptAuthoring(context.Background(), ui, &opts); err != nil {
				t.Fatal(err)
			}
			if opts.PromptMode != tt.mode || opts.NoPrompt != tt.wantNoPrompt || opts.PromptAgentModel != tt.wantModel {
				t.Fatalf("opts = %+v", opts)
			}
			if len(ui.confirmPrompts) != 0 {
				t.Fatalf("unexpected confirmation = %+v", ui.confirmPrompts)
			}
		})
	}
}

func TestValidateOptionsRejectsInvalidPromptNetworkDomain(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentModel = defaultPromptAgentModel
	opts.PromptNetworkDomains = []string{"https://user:secret@example.com"}
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "prompt-network-domain") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateOptionsNormalizesPromptNetworkDomains(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentModel = "other/model"
	opts.PromptNetworkDomains = []string{"Provider.Example.COM:443", "provider.example.com:443"}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.PromptNetworkDomains) != 1 || opts.PromptNetworkDomains[0] != "provider.example.com:443" {
		t.Fatalf("network domains = %v", opts.PromptNetworkDomains)
	}
}

func TestValidateOptionsAcceptsOrkaPromptRuntime(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentRuntime = promptRuntimeOrka
	opts.PromptOrkaAPI = "http://orka.example.test:8080"
	opts.PromptOrkaAgentRef = "prompt-author"
	opts.PromptOrkaNamespace = "orka-system"
	opts.PromptOrkaGitSecret = "source-read"
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}
	if effectivePromptAgentRuntime(opts) != promptRuntimeOrka {
		t.Fatalf("runtime = %q", effectivePromptAgentRuntime(opts))
	}
}

func TestValidateOptionsRejectsLocalPolicyForOrkaPromptRuntime(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentRuntime = promptRuntimeOrka
	opts.PromptOrkaAPI = "http://orka.example.test:8080"
	opts.PromptOrkaAgentRef = "prompt-author"
	opts.PromptAgentModel = defaultPromptAgentModel
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "apply only") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateOptionsBoundsOrkaPromptTimeout(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentRuntime = promptRuntimeOrka
	opts.PromptOrkaAPI = "http://orka.example.test:8080"
	opts.PromptOrkaAgentRef = "prompt-author"
	opts.PromptTimeout = 31 * time.Minute
	if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "at most 30m") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateOptionsUsesOrkaAPITerminology(t *testing.T) {
	opts := testPromptModeOptions(promptModeAgent)
	opts.PromptAgentRuntime = promptRuntimeOrka
	opts.PromptOrkaAPI = "https://user:secret@orka.example.test"
	opts.PromptOrkaAgentRef = "prompt-author"
	err := validateOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "--prompt-orka-api") || !strings.Contains(err.Error(), "ORKA_API_TOKEN") || strings.Contains(err.Error(), "AI_ENDPOINT") {
		t.Fatalf("error = %v", err)
	}
}
