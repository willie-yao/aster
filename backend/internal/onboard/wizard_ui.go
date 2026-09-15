package onboard

import "context"

// Prompter supplies interactive input for the guided workflow.
type Prompter interface {
	Input(context.Context, InputPrompt) (string, error)
	Select(context.Context, SelectPrompt) (string, error)
	Confirm(context.Context, ConfirmPrompt) (bool, error)
}

// InputPrompt describes one editable text value.
type InputPrompt struct {
	Title       string
	Description string
	Value       string
	Required    bool
	Validate    func(string) error
}

// SelectPrompt describes a choice among stable option values.
type SelectPrompt struct {
	Title       string
	Description string
	Options     []SelectOption
	Value       string
	Validate    func(string) error
}

// SelectOption pairs a stable value with its display text.
type SelectOption struct {
	Value       string
	Label       string
	Description string
}

// ConfirmPrompt describes a yes-or-no choice.
type ConfirmPrompt struct {
	Title       string
	Description string
	Value       bool
}
