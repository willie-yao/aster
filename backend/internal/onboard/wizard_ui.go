package onboard

import (
	"context"
	"os"
)

type wizardUI interface {
	Input(context.Context, inputPrompt) (string, error)
	Select(context.Context, selectPrompt) (string, error)
	Confirm(context.Context, confirmPrompt) (bool, error)
}

type inputPrompt struct {
	Title       string
	Description string
	Value       string
	Required    bool
	Validate    func(string) error
}

type selectPrompt struct {
	Title       string
	Description string
	Options     []selectOption
	Value       string
}

type selectOption struct {
	Value       string
	Label       string
	Description string
}

type confirmPrompt struct {
	Title       string
	Description string
	Value       bool
}

func newWizardUI(terminal Terminal) wizardUI {
	if os.Getenv("TERM") == "dumb" {
		return newAccessibleWizardUI(terminal)
	}
	return newHuhWizardUI(terminal)
}
