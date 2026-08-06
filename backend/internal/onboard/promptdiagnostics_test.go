package onboard

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPromptFailureWarningIsSafeAndActionable(t *testing.T) {
	failure := &promptPreparationFailure{
		Stage:    promptStageAgentExecution,
		Category: promptFailureAgentExecution,
		cause:    errors.New("raw OpenCode output"),
	}
	var out bytes.Buffer
	writePromptFailure(&out, "OpenCode prompt authoring failed", failure, "agent handoff bundle with TODO template")
	text := out.String()
	for _, want := range []string{"agent execution", "OpenCode prompt authoring could not run to completion", "agent handoff bundle", "Verify pinned srt and OpenCode availability"} {
		if !strings.Contains(text, want) {
			t.Fatalf("warning missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, failure.cause.Error()) {
		t.Fatalf("warning exposed raw failure: %s", text)
	}
}

func TestPromptPlanAgentFallbackIsValidated(t *testing.T) {
	failure := &promptPreparationFailure{Stage: promptStageAgentExecution, Category: promptFailureAgentExecution}
	plan := (promptPreparationResult{
		Requested: promptRequestAgent,
		Status:    promptStatusAgentFallback,
		Output:    promptOutputTemplate,
		Failure:   failure,
	}).promptPlan(Options{PromptAgentModel: defaultPromptAgentModel, PromptTimeout: 30 * time.Minute})
	if err := validatePromptPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.FailureStage = string(promptStageSourceRevision)
	if err := validatePromptPlan(plan); err == nil {
		t.Fatal("expected mismatched failure diagnostics to fail")
	}
}

func TestPromptPreparationStageLabels(t *testing.T) {
	stages := map[promptPreparationStage]string{
		promptStageSourceRevision:        "source revision resolution",
		promptStageAgentExecution:        "agent execution",
		promptStageFinalPromptValidation: "final rendering and prompt validation",
	}
	for stage, want := range stages {
		if got := stage.label(); got != want {
			t.Errorf("stage %q label = %q, want %q", stage, got, want)
		}
	}
}

func TestRequiredPromptDraftErrorHidesCause(t *testing.T) {
	failure := &promptPreparationFailure{Stage: promptStageAgentExecution, Category: promptFailureAgentExecution, cause: errors.New("private output")}
	err := (&requiredPromptDraftError{failure: failure}).Error()
	if !strings.Contains(err, "required agent prompt draft") || strings.Contains(err, "private output") {
		t.Fatalf("error = %q", err)
	}
}
