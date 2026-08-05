package onboard

const (
	promptModeAgent    = "agent"
	promptModeHandoff  = "handoff"
	promptModeAPI      = "api-experimental"
	promptModeTemplate = "todo-template"
)

func effectivePromptMode(opts Options) string {
	if opts.PromptMode != "" {
		return opts.PromptMode
	}
	if opts.NoPrompt || opts.AIToken == "" {
		return promptModeTemplate
	}
	return promptModeAPI
}
