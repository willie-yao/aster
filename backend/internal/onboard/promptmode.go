package onboard

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	promptModeAgent    = "agent"
	promptModeHandoff  = "handoff"
	promptModeTemplate = "todo-template"
)

func effectivePromptMode(opts Options) string {
	if opts.NoPrompt {
		return promptModeTemplate
	}
	if opts.PromptMode != "" {
		return opts.PromptMode
	}
	return promptModeHandoff
}

func validatePromptAgentModel(model string) error {
	model = strings.TrimSpace(model)
	provider, name, ok := strings.Cut(model, "/")
	if !ok || provider == "" || name == "" || strings.IndexFunc(model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("--prompt-agent-model must be an OpenCode provider/model reference")
	}
	return nil
}
