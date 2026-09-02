package onboard

import (
	"context"
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

func TestWizardPromptAuthoringKeepsSelectedMode(t *testing.T) {
	for _, mode := range []string{promptModeHandoff, promptModeTemplate} {
		t.Run(mode, func(t *testing.T) {
			opts := Options{}
			ui := &queuedWizardUI{selects: []string{mode}}
			if err := wizardPromptAuthoring(context.Background(), ui, &opts); err != nil {
				t.Fatal(err)
			}
			if opts.PromptMode != mode {
				t.Fatalf("opts = %+v", opts)
			}
			if len(ui.confirmPrompts) != 0 {
				t.Fatalf("unexpected confirmation = %+v", ui.confirmPrompts)
			}
		})
	}
}
