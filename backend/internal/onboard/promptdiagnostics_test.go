package onboard

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestPromptFailureWarningIsSafeAndActionable(t *testing.T) {
	failure := &promptPreparationFailure{
		Stage:    promptStageSourceRevision,
		Category: promptFailureSourceUnavailable,
		cause:    errors.New("raw upstream output"),
	}
	var out bytes.Buffer
	writePromptFailure(&out, "prompt source resolution failed", failure, "agent handoff bundle with TODO template")
	text := out.String()
	for _, want := range []string{"source revision resolution", "agent handoff bundle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("warning missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, failure.cause.Error()) {
		t.Fatalf("warning exposed raw failure: %s", text)
	}
}

func TestPromptPreparationStageLabels(t *testing.T) {
	stages := map[promptPreparationStage]string{
		promptStageSourceRevision:        "source revision resolution",
		promptStageFinalPromptValidation: "final rendering and prompt validation",
	}
	for stage, want := range stages {
		if got := stage.label(); got != want {
			t.Errorf("stage %q label = %q, want %q", stage, got, want)
		}
	}
}
