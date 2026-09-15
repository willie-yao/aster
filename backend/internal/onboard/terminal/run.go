// Package terminal provides terminal adapters for guided onboarding.
package terminal

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/willie-yao/aster/backend/internal/onboard"
	"golang.org/x/term"
)

// Terminal supplies injected input and output for the interactive wizard.
type Terminal struct {
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Interactive bool
}

// Run executes onboarding using the process terminal.
func Run(ctx context.Context, opts onboard.Options) error {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	return RunWithTerminal(ctx, opts, Terminal{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: interactive})
}

// RunWithTerminal executes onboarding with injected terminal streams.
func RunWithTerminal(ctx context.Context, opts onboard.Options, terminal Terminal) error {
	if terminal.In == nil {
		terminal.In = strings.NewReader("")
	}
	if terminal.Out == nil {
		terminal.Out = io.Discard
	}
	if terminal.Err == nil {
		terminal.Err = terminal.Out
	}
	var newPrompter func() onboard.Prompter
	if terminal.Interactive {
		newPrompter = func() onboard.Prompter { return newWizardUI(terminal) }
	}
	return onboard.Run(ctx, opts, terminal.Out, newPrompter)
}

func newWizardUI(terminal Terminal) onboard.Prompter {
	if os.Getenv("TERM") == "dumb" || os.Getenv("ACCESSIBLE") != "" {
		return newAccessibleWizardUI(terminal)
	}
	return newHuhWizardUI(terminal)
}
