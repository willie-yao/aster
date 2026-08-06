package onboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type promptPreparationRequest string

const (
	promptRequestTemplate promptPreparationRequest = "todo-template"
	promptRequestAgent    promptPreparationRequest = "agent"
	promptRequestHandoff  promptPreparationRequest = "handoff"
)

type promptPreparationStatus string

const (
	promptStatusTemplate      promptPreparationStatus = "todo-template"
	promptStatusAgentDraft    promptPreparationStatus = "agent-draft"
	promptStatusAgentFallback promptPreparationStatus = "agent-fallback"
	promptStatusHandoff       promptPreparationStatus = "handoff"
)

type promptOutputKind string

const (
	promptOutputTemplate   promptOutputKind = "todo-template"
	promptOutputAgentDraft promptOutputKind = "agent-draft"
)

type promptPreparationStage string

const (
	promptStageSourceRevision        promptPreparationStage = "source-revision-resolution"
	promptStageAgentExecution        promptPreparationStage = "agent-execution"
	promptStageFinalPromptValidation promptPreparationStage = "final-rendering-and-prompt-validation"
)

func (s promptPreparationStage) label() string {
	switch s {
	case promptStageSourceRevision:
		return "source revision resolution"
	case promptStageAgentExecution:
		return "agent execution"
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
	promptFailureAgentExecution    promptFailureCategory = "agent-execution-failed"
	promptFailureTimedOut          promptFailureCategory = "timed-out"
)

func (c promptFailureCategory) reason() string {
	switch c {
	case promptFailureSourceUnavailable:
		return "the source revision could not be resolved"
	case promptFailurePromptValidation:
		return "the generated prompt failed deterministic validation"
	case promptFailureAgentExecution:
		return "OpenCode prompt authoring could not run to completion"
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
	case promptFailureAgentExecution:
		return "Verify pinned srt and OpenCode availability, authentication, provider domains, and repository access, then retry or use the handoff bundle."
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
	switch r.Status {
	case promptStatusAgentDraft:
		return "OpenCode agent draft"
	case promptStatusAgentFallback, promptStatusHandoff:
		return "Agent handoff bundle with TODO template"
	default:
		return "TODO template"
	}
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
	if r.Requested == promptRequestAgent {
		plan.Runtime = "opencode"
		plan.Model = effectivePromptAgentModel(opts)
		plan.Timeout = effectivePromptDraftTimeout(opts).String()
	}
	return plan
}

func validatePromptPlan(plan PromptPlan) error {
	switch plan.RequestedMode {
	case string(promptRequestTemplate), string(promptRequestAgent), string(promptRequestHandoff):
	default:
		return fmt.Errorf("onboarding plan prompt request %q is invalid", plan.RequestedMode)
	}
	if plan.RequestedMode == string(promptRequestAgent) {
		timeout, err := time.ParseDuration(plan.Timeout)
		if err != nil || timeout < minPromptDraftTimeout || timeout > maxPromptDraftTimeout {
			return fmt.Errorf("onboarding plan prompt timeout is invalid")
		}
	} else if plan.Timeout != "" {
		return fmt.Errorf("onboarding plan prompt retained an inapplicable timeout")
	}

	switch plan.FinalStatus {
	case string(promptStatusTemplate):
		if plan.RequestedMode != string(promptRequestTemplate) || plan.Output != string(promptOutputTemplate) || plan.Source != "TODO template" {
			return fmt.Errorf("onboarding plan TODO prompt result is inconsistent")
		}
	case string(promptStatusAgentDraft):
		if plan.RequestedMode != string(promptRequestAgent) || plan.Output != string(promptOutputAgentDraft) || plan.Source != "OpenCode agent draft" || plan.Runtime != "opencode" || validatePromptAgentModel(plan.Model) != nil {
			return fmt.Errorf("onboarding plan agent prompt result is inconsistent")
		}
	case string(promptStatusAgentFallback):
		if plan.RequestedMode != string(promptRequestAgent) || plan.Output != string(promptOutputTemplate) || plan.Source != "Agent handoff bundle with TODO template" || plan.Runtime != "opencode" || validatePromptAgentModel(plan.Model) != nil {
			return fmt.Errorf("onboarding plan agent fallback result is inconsistent")
		}
		if err := validatePromptFailureDiagnostics(plan); err != nil {
			return err
		}
	case string(promptStatusHandoff):
		if plan.RequestedMode != string(promptRequestHandoff) || plan.Output != string(promptOutputTemplate) || plan.Source != "Agent handoff bundle with TODO template" {
			return fmt.Errorf("onboarding plan handoff result is inconsistent")
		}
	default:
		return fmt.Errorf("onboarding plan prompt status %q is invalid", plan.FinalStatus)
	}
	if plan.RequestedMode != string(promptRequestAgent) && (plan.Runtime != "" || plan.Model != "") {
		return fmt.Errorf("onboarding plan non-agent prompt result retained agent coordinates")
	}
	if plan.FinalStatus != string(promptStatusAgentFallback) && (plan.FailureStage != "" || plan.FailureCategory != "" || plan.FailureAction != "") {
		return fmt.Errorf("onboarding plan successful prompt result retained failure diagnostics")
	}
	return nil
}

func validatePromptFailureDiagnostics(plan PromptPlan) error {
	stage := promptPreparationStage(plan.FailureStage)
	category := promptFailureCategory(plan.FailureCategory)
	allowed := false
	switch stage {
	case promptStageSourceRevision:
		allowed = category == promptFailureSourceUnavailable || category == promptFailureTimedOut
	case promptStageAgentExecution:
		allowed = category == promptFailureAgentExecution || category == promptFailureTimedOut
	case promptStageFinalPromptValidation:
		allowed = category == promptFailurePromptValidation
	}
	if !allowed || plan.FailureAction != category.action() {
		return fmt.Errorf("onboarding plan fallback diagnostics are invalid")
	}
	return nil
}

func promptPlanIncludesHandoff(plan PromptPlan) bool {
	return plan.FinalStatus == string(promptStatusHandoff) || plan.FinalStatus == string(promptStatusAgentFallback)
}

func sourcePromptFailure(stage promptPreparationStage, err error) *promptPreparationFailure {
	category := promptFailureSourceUnavailable
	if errors.Is(err, context.DeadlineExceeded) {
		category = promptFailureTimedOut
	}
	return &promptPreparationFailure{Stage: stage, Category: category, cause: err}
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

type requiredPromptDraftError struct {
	failure *promptPreparationFailure
}

func (e *requiredPromptDraftError) Error() string {
	if e == nil || e.failure == nil {
		return "required agent prompt draft was not produced"
	}
	return fmt.Sprintf("required agent prompt draft was not produced: %s: %s", e.failure.Stage.label(), e.failure.Category.reason())
}

func (e *requiredPromptDraftError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.failure
}
