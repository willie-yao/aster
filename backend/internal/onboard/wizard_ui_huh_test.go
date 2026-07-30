package onboard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/huh/v2"
)

func TestHuhWizardUI_InputAcceptsEditableDefault(t *testing.T) {
	t.Setenv("TERM", "dumb")
	out := &bytes.Buffer{}
	ui := newHuhWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
	value, err := ui.Input(context.Background(), inputPrompt{
		Title: "Project ID", Value: "kind", Required: true,
	})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if value != "kind" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(out.String(), "Project ID") {
		t.Fatalf("output did not use injected writer: %q", out.String())
	}
}

func TestHuhWizardUI_InputCanReplaceDefault(t *testing.T) {
	t.Setenv("TERM", "dumb")
	out := &bytes.Buffer{}
	ui := newHuhWizardUI(Terminal{In: strings.NewReader("custom\n"), Out: out, Err: out})
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

func TestHuhWizardUI_InputValidatesRequiredValue(t *testing.T) {
	t.Setenv("TERM", "dumb")
	out := &bytes.Buffer{}
	ui := newHuhWizardUI(Terminal{In: strings.NewReader("\naccepted\n"), Out: out, Err: out})
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

func TestHuhWizardUI_SelectUsesStableValue(t *testing.T) {
	t.Setenv("TERM", "dumb")
	out := &bytes.Buffer{}
	ui := newHuhWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
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

func TestHuhWizardUI_ConfirmPreservesDefaultNo(t *testing.T) {
	t.Setenv("TERM", "dumb")
	out := &bytes.Buffer{}
	ui := newHuhWizardUI(Terminal{In: strings.NewReader("\n"), Out: out, Err: out})
	value, err := ui.Confirm(context.Background(), confirmPrompt{
		Title: "Create this scaffold?", Value: false,
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if value {
		t.Fatal("default confirmation was true")
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
