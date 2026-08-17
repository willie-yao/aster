package onboard

import (
	"fmt"
	"io"
)

type promptPreparationRequest string

const (
	promptRequestTemplate promptPreparationRequest = "todo-template"
	promptRequestHandoff  promptPreparationRequest = "handoff"
)

type promptPreparationStatus string

const (
	promptStatusTemplate promptPreparationStatus = "todo-template"
	promptStatusHandoff  promptPreparationStatus = "handoff"
)

type promptOutputKind string

const (
	promptOutputTemplate promptOutputKind = "todo-template"
)

type promptPreparationStage string

const (
	promptStageSourceRevision        promptPreparationStage = "source-revision-resolution"
	promptStageFinalPromptValidation promptPreparationStage = "final-rendering-and-prompt-validation"
)

func (s promptPreparationStage) label() string {
	switch s {
	case promptStageSourceRevision:
		return "source revision resolution"
	case promptStageFinalPromptValidation:
		return "final rendering and prompt validation"
	default:
		return "prompt preparation"
	}
}

type promptFailureCategory string

const (
	promptFailureSourceUnavailable promptFailureCategory = "source-unavailable"
	promptFailurePromptValidation  promptFailureCategory = "prompt-validation-failed"
	promptFailureTimedOut          promptFailureCategory = "timed-out"
)

func (c promptFailureCategory) reason() string {
	switch c {
	case promptFailureSourceUnavailable:
		return "the source revision could not be resolved"
	case promptFailurePromptValidation:
		return "the generated prompt failed deterministic validation"
	case promptFailureTimedOut:
		return "prompt preparation exceeded its time limit"
	default:
		return "prompt preparation could not complete safely"
	}
}

func (c promptFailureCategory) action() string {
	switch c {
	case promptFailureSourceUnavailable:
		return "Verify GitHub repository access and the default branch, then retry or use the handoff bundle."
	case promptFailurePromptValidation:
		return "Use the handoff bundle and inspect the generated prompt against the deterministic validation contract."
	case promptFailureTimedOut:
		return "Retry with a larger --prompt-timeout or use the handoff bundle."
	default:
		return "Continue with the reviewable TODO template and handoff bundle."
	}
}

type promptPreparationFailure struct {
	Stage    promptPreparationStage
	Category promptFailureCategory
	cause    error
}

func (f *promptPreparationFailure) Error() string {
	if f == nil {
		return "prompt preparation failed"
	}
	return fmt.Sprintf("%s: %s", f.Stage.label(), f.Category.reason())
}

func (f *promptPreparationFailure) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, f.Error())
}

func (f *promptPreparationFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.cause
}

type promptPreparationResult struct {
	Requested promptPreparationRequest
	Status    promptPreparationStatus
	Output    promptOutputKind
	Failure   *promptPreparationFailure
	Handoff   string
}

func newTemplatePromptResult() promptPreparationResult {
	return promptPreparationResult{Requested: promptRequestTemplate, Status: promptStatusTemplate, Output: promptOutputTemplate}
}

func (r promptPreparationResult) reviewLabel() string {
	if r.Status == promptStatusHandoff {
		return "Agent handoff bundle with TODO template"
	}
	return "TODO template"
}

func (r promptPreparationResult) promptPlan(opts Options) PromptPlan {
	plan := PromptPlan{
		RequestedMode: string(r.Requested),
		FinalStatus:   string(r.Status),
		Output:        string(r.Output),
		Source:        r.reviewLabel(),
	}
	if r.Failure != nil {
		plan.FailureStage = string(r.Failure.Stage)
		plan.FailureCategory = string(r.Failure.Category)
		plan.FailureAction = r.Failure.Category.action()
	}
	return plan
}

func validatePromptPlan(plan PromptPlan) error {
	if plan.BaselineStatus != promptBaselineSourceOnly {
		return fmt.Errorf("onboarding plan prompt baseline status is invalid")
	}
	if _, err := parseSHA256Digest(plan.CandidateSHA256, "candidate prompt digest"); err != nil {
		return fmt.Errorf("onboarding plan candidate prompt digest is invalid")
	}
	if plan.ExistingSHA256 != "" {
		if _, err := parseSHA256Digest(plan.ExistingSHA256, "existing prompt digest"); err != nil {
			return fmt.Errorf("onboarding plan existing prompt digest is invalid")
		}
	}
	switch plan.RequestedMode {
	case string(promptRequestTemplate), string(promptRequestHandoff):
	default:
		return fmt.Errorf("onboarding plan prompt request %q is invalid", plan.RequestedMode)
	}
	if plan.Timeout != "" {
		return fmt.Errorf("onboarding plan prompt retained an inapplicable timeout")
	}

	switch plan.FinalStatus {
	case string(promptStatusTemplate):
		if plan.RequestedMode != string(promptRequestTemplate) || plan.Output != string(promptOutputTemplate) || plan.Source != "TODO template" {
			return fmt.Errorf("onboarding plan TODO prompt result is inconsistent")
		}
	case string(promptStatusHandoff):
		if plan.RequestedMode != string(promptRequestHandoff) || plan.Output != string(promptOutputTemplate) || plan.Source != "Agent handoff bundle with TODO template" {
			return fmt.Errorf("onboarding plan handoff result is inconsistent")
		}
	default:
		return fmt.Errorf("onboarding plan prompt status %q is invalid", plan.FinalStatus)
	}
	if plan.Runtime != "" || plan.Model != "" || plan.AgentRef != "" {
		return fmt.Errorf("onboarding plan prompt result retained agent coordinates")
	}
	if plan.FailureStage != "" || plan.FailureCategory != "" || plan.FailureAction != "" {
		return fmt.Errorf("onboarding plan successful prompt result retained failure diagnostics")
	}
	return nil
}

func promptPlanIncludesHandoff(plan PromptPlan) bool {
	return plan.FinalStatus == string(promptStatusHandoff)
}

func writePromptFailure(out io.Writer, title string, failure *promptPreparationFailure, fallback string) {
	if out == nil || failure == nil {
		return
	}
	fmt.Fprintf(out, "[warn] %s\n", title)
	fmt.Fprintf(out, "       stage: %s\n", failure.Stage.label())
	fmt.Fprintf(out, "       reason: %s\n", failure.Category.reason())
	fmt.Fprintf(out, "       fallback: %s\n", fallback)
	if action := failure.Category.action(); action != "" {
		fmt.Fprintf(out, "       action: %s\n", action)
	}
}
