package onboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/muesli/cancelreader"
)

type accessibleWizardUI struct {
	terminal Terminal
	reader   *bufio.Reader
	cancel   cancelreader.CancelReader
}

func newAccessibleWizardUI(terminal Terminal) wizardUI {
	return &accessibleWizardUI{terminal: terminal}
}

func (u *accessibleWizardUI) Input(ctx context.Context, prompt inputPrompt) (string, error) {
	if prompt.Description != "" {
		fmt.Fprintln(u.terminal.Out, prompt.Description)
	}
	for {
		fmt.Fprint(u.terminal.Out, prompt.Title)
		if prompt.Value != "" {
			fmt.Fprintf(u.terminal.Out, " [%s]", prompt.Value)
		}
		fmt.Fprint(u.terminal.Out, ": ")
		value, err := u.readLine(ctx)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = prompt.Value
		}
		if prompt.Required && value == "" {
			fmt.Fprintln(u.terminal.Out, "A value is required.")
			continue
		}
		if prompt.Validate != nil {
			if err := prompt.Validate(value); err != nil {
				fmt.Fprintln(u.terminal.Out, err)
				continue
			}
		}
		return value, nil
	}
}

func (u *accessibleWizardUI) Select(ctx context.Context, prompt selectPrompt) (string, error) {
	if len(prompt.Options) == 0 {
		return "", fmt.Errorf("%s: no options are available", prompt.Title)
	}
	defaultIndex := 0
	fmt.Fprintln(u.terminal.Out, prompt.Title)
	if prompt.Description != "" {
		fmt.Fprintln(u.terminal.Out, prompt.Description)
	}
	for index, option := range prompt.Options {
		if option.Value == prompt.Value {
			defaultIndex = index
		}
		fmt.Fprintf(u.terminal.Out, "  %d. %s", index+1, option.Label)
		if option.Description != "" {
			fmt.Fprintf(u.terminal.Out, " - %s", option.Description)
		}
		fmt.Fprintln(u.terminal.Out)
	}
	for {
		fmt.Fprintf(u.terminal.Out, "Select [%d]: ", defaultIndex+1)
		value, err := u.readLine(ctx)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return prompt.Options[defaultIndex].Value, nil
		}
		selected, err := strconv.Atoi(value)
		if err == nil && selected >= 1 && selected <= len(prompt.Options) {
			return prompt.Options[selected-1].Value, nil
		}
		fmt.Fprintf(u.terminal.Out, "Enter a number from 1 to %d.\n", len(prompt.Options))
	}
}

func (u *accessibleWizardUI) Confirm(ctx context.Context, prompt confirmPrompt) (bool, error) {
	if prompt.Description != "" {
		fmt.Fprintln(u.terminal.Out, prompt.Description)
	}
	suffix := " [y/N]: "
	if prompt.Value {
		suffix = " [Y/n]: "
	}
	for {
		fmt.Fprint(u.terminal.Out, prompt.Title+suffix)
		value, err := u.readLine(ctx)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
			return prompt.Value, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(u.terminal.Out, "Enter y or n.")
		}
	}
}

func (u *accessibleWizardUI) readLine(ctx context.Context) (string, error) {
	if err := u.ensureReader(); err != nil {
		return "", err
	}
	type result struct {
		value string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		value, err := u.reader.ReadString('\n')
		resultCh <- result{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		if u.cancel.Cancel() {
			<-resultCh
		}
		return "", ErrCancelled
	case result := <-resultCh:
		if errors.Is(result.err, cancelreader.ErrCanceled) {
			return "", ErrCancelled
		}
		if result.err != nil && !errors.Is(result.err, io.EOF) {
			return "", result.err
		}
		if errors.Is(result.err, io.EOF) && result.value == "" {
			return "", ErrCancelled
		}
		return result.value, nil
	}
}

func (u *accessibleWizardUI) ensureReader() error {
	if u.reader != nil {
		return nil
	}
	reader, err := cancelreader.NewReader(u.terminal.In)
	if err != nil {
		return fmt.Errorf("prepare accessible terminal input: %w", err)
	}
	u.cancel = reader
	u.reader = bufio.NewReader(reader)
	return nil
}

func (u *accessibleWizardUI) Close() error {
	if u.cancel == nil {
		return nil
	}
	return u.cancel.Close()
}
