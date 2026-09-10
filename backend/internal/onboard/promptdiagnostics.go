package onboard

import "fmt"

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

type promptPreparationResult struct {
	Requested promptPreparationRequest
	Status    promptPreparationStatus
	Output    promptOutputKind
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

func (r promptPreparationResult) promptPlan() PromptPlan {
	return PromptPlan{
		RequestedMode: string(r.Requested),
		FinalStatus:   string(r.Status),
		Output:        string(r.Output),
		Source:        r.reviewLabel(),
	}
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
	return nil
}

func promptPlanIncludesHandoff(plan PromptPlan) bool {
	return plan.FinalStatus == string(promptStatusHandoff)
}
