package onboard

const (
	promptModeHandoff  = "handoff"
	promptModeTemplate = "todo-template"
)

// effectivePromptMode selects the agent handoff bundle unless the caller asked
// for the bare TODO template. Prompt generation itself is delegated to the
// operator's own coding agent.
func effectivePromptMode(opts Options) string {
	if opts.PromptMode != "" {
		return opts.PromptMode
	}
	return promptModeHandoff
}
