package onboard

const (
	promptModeAgent    = "agent"
	promptModeHandoff  = "handoff"
	promptModeAPI      = "api-experimental"
	promptModeTemplate = "todo-template"
)

func effectivePromptMode(opts Options) string {
	if opts.NoPrompt {
		return promptModeTemplate
	}
	if opts.PromptMode != "" {
		return opts.PromptMode
	}
	if opts.AIToken == "" && !opts.RequirePromptDraft {
		return promptModeTemplate
	}
	return promptModeAPI
}
