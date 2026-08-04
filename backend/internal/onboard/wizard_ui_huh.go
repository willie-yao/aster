package onboard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/huh/v2"
)

type huhWizardUI struct {
	terminal Terminal
}

func newHuhWizardUI(terminal Terminal) wizardUI {
	return &huhWizardUI{terminal: terminal}
}

func (u *huhWizardUI) Input(ctx context.Context, prompt inputPrompt) (string, error) {
	value := prompt.Value
	field := newClearableHuhInput(huh.NewInput().
		Title(prompt.Title).
		Description(prompt.Description).
		Value(&value).
		Validate(func(value string) error {
			value = strings.TrimSpace(value)
			if prompt.Required && value == "" {
				return fmt.Errorf("a value is required")
			}
			if prompt.Validate != nil {
				return prompt.Validate(value)
			}
			return nil
		}), &value)
	if err := u.run(ctx, prompt.Title, field); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (u *huhWizardUI) Select(ctx context.Context, prompt selectPrompt) (string, error) {
	if len(prompt.Options) == 0 {
		return "", fmt.Errorf("%s: no options are available", prompt.Title)
	}
	value := prompt.Value
	options := make([]huh.Option[string], 0, len(prompt.Options))
	for _, option := range prompt.Options {
		options = append(options, huh.NewOption(option.Label, option.Value))
	}
	field := huh.NewSelect[string]().
		Title(prompt.Title).
		Options(options...).
		Value(&value).
		Validate(func(value string) error {
			available := false
			for _, option := range prompt.Options {
				if option.Value == value {
					available = true
					break
				}
			}
			if !available {
				return fmt.Errorf("select an available option")
			}
			if prompt.Validate != nil {
				return prompt.Validate(value)
			}
			return nil
		})
	field.DescriptionFunc(func() string {
		for _, option := range prompt.Options {
			if option.Value == value && option.Description != "" {
				return option.Description
			}
		}
		return prompt.Description
	}, &value)
	if err := u.run(ctx, prompt.Title, field); err != nil {
		return "", err
	}
	return value, nil
}

func (u *huhWizardUI) Confirm(ctx context.Context, prompt confirmPrompt) (bool, error) {
	value := prompt.Value
	field := huh.NewConfirm().
		Title(prompt.Title).
		Description(prompt.Description).
		Value(&value)
	if err := u.run(ctx, prompt.Title, field); err != nil {
		return false, err
	}
	return value, nil
}

func (u *huhWizardUI) run(ctx context.Context, title string, field huh.Field) error {
	form := huh.NewForm(huh.NewGroup(field)).
		WithInput(u.terminal.In).
		WithOutput(u.terminal.Out).
		WithAccessible(false)
	if err := form.RunWithContext(ctx); err != nil {
		return normalizeWizardUIError(title, err)
	}
	return nil
}

func normalizeWizardUIError(title string, err error) error {
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
		return ErrCancelled
	}
	return fmt.Errorf("%s: %w", title, err)
}
