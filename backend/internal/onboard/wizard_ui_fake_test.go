package onboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type lineWizardUI struct {
	reader *bufio.Reader
	out    io.Writer
}

func newLineWizardUI(terminal Terminal) wizardUI {
	return &lineWizardUI{reader: bufio.NewReader(terminal.In), out: terminal.Out}
}

func (u *lineWizardUI) Input(_ context.Context, prompt inputPrompt) (string, error) {
	for {
		fmt.Fprint(u.out, prompt.Title)
		if prompt.Value != "" {
			fmt.Fprintf(u.out, " [%s]", prompt.Value)
		}
		fmt.Fprint(u.out, ": ")
		line, readErr := u.reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		line = strings.TrimSpace(line)
		if isLineCancel(line) || errors.Is(readErr, io.EOF) && line == "" {
			return "", ErrCancelled
		}
		if line == "" {
			line = prompt.Value
		}
		if prompt.Required && line == "" {
			fmt.Fprintln(u.out, "A value is required. Enter q to cancel.")
			if errors.Is(readErr, io.EOF) {
				return "", ErrCancelled
			}
			continue
		}
		if prompt.Validate != nil {
			if err := prompt.Validate(line); err != nil {
				return "", err
			}
		}
		return line, nil
	}
}

func (u *lineWizardUI) Select(_ context.Context, prompt selectPrompt) (string, error) {
	fmt.Fprintln(u.out, prompt.Title)
	defaultIndex := 0
	for i, option := range prompt.Options {
		fmt.Fprintf(u.out, "  %d. %s\n", i+1, option.Label)
		if option.Value == prompt.Value {
			defaultIndex = i
		}
	}
	for {
		fmt.Fprintf(u.out, "Select [%d]: ", defaultIndex+1)
		line, readErr := u.reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		line = strings.TrimSpace(line)
		if isLineCancel(line) || errors.Is(readErr, io.EOF) && line == "" {
			return "", ErrCancelled
		}
		if line == "" {
			return prompt.Options[defaultIndex].Value, nil
		}
		selected, convErr := strconv.Atoi(line)
		if convErr == nil && selected >= 1 && selected <= len(prompt.Options) {
			return prompt.Options[selected-1].Value, nil
		}
		fmt.Fprintf(u.out, "Enter a number from 1 to %d, or q to cancel.\n", len(prompt.Options))
	}
}

func (u *lineWizardUI) Confirm(_ context.Context, prompt confirmPrompt) (bool, error) {
	suffix := " [y/N]: "
	if prompt.Value {
		suffix = " [Y/n]: "
	}
	for {
		fmt.Fprint(u.out, prompt.Title+suffix)
		line, readErr := u.reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, readErr
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if isLineCancel(line) || errors.Is(readErr, io.EOF) && line == "" {
			return false, ErrCancelled
		}
		switch line {
		case "":
			return prompt.Value, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(u.out, "Enter y, n, or q to cancel.")
		}
	}
}

func isLineCancel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "q", "quit", "cancel":
		return true
	default:
		return false
	}
}

type panicWizardUI struct{}

func (panicWizardUI) Input(context.Context, inputPrompt) (string, error) {
	panic("wizard input was requested")
}

func (panicWizardUI) Select(context.Context, selectPrompt) (string, error) {
	panic("wizard selection was requested")
}

func (panicWizardUI) Confirm(context.Context, confirmPrompt) (bool, error) {
	panic("wizard confirmation was requested")
}
