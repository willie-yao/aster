package terminal

import (
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/onboard"
)

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("unexpected terminal read")
}

func TestRunWithTerminalInputRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		opts        onboard.Options
		want        string
	}{
		{name: "non-TTY", want: "stdin is not an interactive terminal"},
		{name: "noninteractive override", interactive: true, opts: onboard.Options{NonInteractive: true}, want: "non-interactive onboarding requires"},
		{name: "contradictory selectors", interactive: true, opts: onboard.Options{TestGrid: "dashboard", Bucket: "bucket"}, want: "provide exactly one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RunWithTerminal(t.Context(), tc.opts, Terminal{In: panicReader{}, Interactive: tc.interactive})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want %q", err, tc.want)
			}
		})
	}
}
