package onboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func TestNewWizardUI_SelectsTerminalImplementation(t *testing.T) {
	terminal := Terminal{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}
	t.Run("accessible", func(t *testing.T) {
		t.Setenv("TERM", "dumb")
		if _, ok := newWizardUI(terminal).(*accessibleWizardUI); !ok {
			t.Fatalf("TERM=dumb did not select accessible UI")
		}
	})
	t.Run("accessible environment override", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		t.Setenv("ACCESSIBLE", "1")
		if _, ok := newWizardUI(terminal).(*accessibleWizardUI); !ok {
			t.Fatalf("ACCESSIBLE did not select accessible UI")
		}
	})
	t.Run("interactive", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		t.Setenv("ACCESSIBLE", "")
		if _, ok := newWizardUI(terminal).(*huhWizardUI); !ok {
			t.Fatalf("interactive terminal did not select Huh UI")
		}
	})
}

func TestAccessibleWizardUI_InputAcceptsEditableDefault(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
	value, err := ui.Input(context.Background(), inputPrompt{
		Title: "Project ID", Description: "Editable inferred value.", Value: "kind", Required: true,
	})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if value != "kind" {
		t.Fatalf("value = %q", value)
	}
	for _, want := range []string{"Editable inferred value.", "Project ID [kind]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q: %q", want, out.String())
		}
	}
}

func TestAccessibleWizardUI_InputCanReplaceDefault(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("custom\n"), Out: out, Err: out})
	value, err := ui.Input(context.Background(), inputPrompt{
		Title: "Project ID", Value: "kind", Required: true,
	})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if value != "custom" {
		t.Fatalf("value = %q", value)
	}
}

func TestAccessibleWizardUI_InputValidatesRequiredValue(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("\naccepted\n"), Out: out, Err: out})
	value, err := ui.Input(context.Background(), inputPrompt{
		Title: "Required", Required: true,
	})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if value != "accepted" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(strings.ToLower(out.String()), "required") {
		t.Fatalf("validation output missing: %q", out.String())
	}
}

func TestAccessibleWizardUI_SelectUsesStableValue(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
	value, err := ui.Select(context.Background(), selectPrompt{
		Title: "Deployment",
		Options: []selectOption{
			{Value: modePages, Label: "GitHub Pages"},
			{Value: modeK8s, Label: "Kubernetes"},
		},
		Value: modeK8s,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if value != modeK8s {
		t.Fatalf("value = %q", value)
	}
}

func TestAccessibleWizardUI_SelectValidationRejectsSentinel(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("\n2\n"), Out: out, Err: out})
	value, err := ui.Select(context.Background(), selectPrompt{
		Title: "Provider",
		Options: []selectOption{
			{Value: "choose", Label: "Choose a provider"},
			{Value: "provider", Label: "Provider"},
		},
		Value: "choose",
		Validate: func(value string) error {
			if value == "choose" {
				return errors.New("choose a provider")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if value != "provider" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(out.String(), "choose a provider") {
		t.Fatalf("validation output missing: %q", out.String())
	}
}

func TestAccessibleWizardUI_ConfirmPreservesDefaultNo(t *testing.T) {
	out := &bytes.Buffer{}
	ui := newAccessibleWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
	value, err := ui.Confirm(context.Background(), confirmPrompt{
		Title: "Create this scaffold?", Description: "This permits a write.", Value: false,
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if value {
		t.Fatal("default confirmation was true")
	}
	if !strings.Contains(out.String(), "This permits a write.") {
		t.Fatalf("confirmation description missing: %q", out.String())
	}
}

func TestAccessibleWizardUI_EOFCancelsInsteadOfAcceptingDefaults(t *testing.T) {
	for name, run := range map[string]func(wizardUI) error{
		"input": func(ui wizardUI) error {
			_, err := ui.Input(context.Background(), inputPrompt{Title: "Input", Value: "default"})
			return err
		},
		"select": func(ui wizardUI) error {
			_, err := ui.Select(context.Background(), selectPrompt{
				Title: "Select", Value: "a", Options: []selectOption{{Value: "a", Label: "A"}},
			})
			return err
		},
		"confirm": func(ui wizardUI) error {
			_, err := ui.Confirm(context.Background(), confirmPrompt{Title: "Confirm", Value: true})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ui := newAccessibleWizardUI(Terminal{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
			if err := run(ui); !errors.Is(err, ErrCancelled) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func TestAccessibleWizardUI_ContextCancellationUnblocksPrompt(t *testing.T) {
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ui := newAccessibleWizardUI(Terminal{In: reader, Out: io.Discard, Err: io.Discard}).(*accessibleWizardUI)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := ui.Input(ctx, inputPrompt{Title: "Blocked", Value: "default"})
		result <- err
	}()
	<-reader.started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt did not return after context cancellation")
	}
	close(reader.release)
	if err := ui.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNormalizeWizardUIError(t *testing.T) {
	for _, err := range []error{huh.ErrUserAborted, context.Canceled} {
		if got := normalizeWizardUIError("Prompt", err); !errors.Is(got, ErrCancelled) {
			t.Fatalf("normalize(%v) = %v", err, got)
		}
	}
	sentinel := errors.New("sentinel")
	got := normalizeWizardUIError("Prompt", sentinel)
	if !errors.Is(got, sentinel) || !strings.Contains(got.Error(), "Prompt") {
		t.Fatalf("normalize(sentinel) = %v", got)
	}
}

func TestClearableHuhInput_EscapeClearsWithoutSubmitting(t *testing.T) {
	value := "prefilled"
	validationCalls := 0
	field := newClearableHuhInput(huh.NewInput().
		Value(&value).
		Validate(func(string) error {
			validationCalls++
			return nil
		}), &value)
	form := huh.NewForm(huh.NewGroup(field))
	field.Focus()

	model, _ := form.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	form = model.(*huh.Form)

	if value != "" {
		t.Fatalf("value = %q", value)
	}
	if form.State != huh.StateNormal {
		t.Fatalf("state = %v", form.State)
	}
	if validationCalls != 0 {
		t.Fatalf("validation calls = %d", validationCalls)
	}
}

func TestClearableHuhInput_EscapeUsesNormalValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		required bool
		wantErr  bool
	}{
		{name: "required", required: true, wantErr: true},
		{name: "optional", required: false, wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := "prefilled"
			field := newClearableHuhInput(huh.NewInput().
				Value(&value).
				Validate(func(value string) error {
					if test.required && strings.TrimSpace(value) == "" {
						return errors.New("a value is required")
					}
					return nil
				}), &value)
			form := huh.NewForm(huh.NewGroup(field))
			field.Focus()

			model, _ := form.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			form = model.(*huh.Form)
			model, _ = form.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			form = model.(*huh.Form)

			if value != "" {
				t.Fatalf("value = %q", value)
			}
			if got := form.GetFocusedField().Error() != nil; got != test.wantErr {
				t.Fatalf("validation error = %v, want error %t", form.GetFocusedField().Error(), test.wantErr)
			}
		})
	}
}

func TestClearableHuhInput_CtrlCCancels(t *testing.T) {
	value := "prefilled"
	field := newClearableHuhInput(huh.NewInput().Value(&value), &value)
	form := huh.NewForm(huh.NewGroup(field))

	model, _ := form.Update(tea.KeyPressMsg(tea.Key{Mod: tea.ModCtrl, Code: 'c'}))
	form = model.(*huh.Form)

	if form.State != huh.StateAborted {
		t.Fatalf("state = %v", form.State)
	}
	if value != "prefilled" {
		t.Fatalf("value = %q", value)
	}
}

func TestClearableHuhInput_ShowsClearHint(t *testing.T) {
	value := "prefilled"
	field := newClearableHuhInput(huh.NewInput().Value(&value), &value)
	bindings := field.KeyBinds()
	if len(bindings) == 0 {
		t.Fatal("no key bindings")
	}
	help := bindings[0].Help()
	if help.Key != "esc" || help.Desc != "clear" {
		t.Fatalf("clear help = %q %q", help.Key, help.Desc)
	}
}
