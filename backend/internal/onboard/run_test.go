package onboard

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestRunRejectsInvalidInputsBeforeConstructingPrompter(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{name: "contradictory selectors", opts: Options{TestGrid: "dashboard", Bucket: "bucket"}},
		{name: "noninteractive missing inputs", opts: Options{NonInteractive: true}},
		{name: "invalid guided endpoint", opts: Options{SourceRepo: "example/project", AIEndpoint: "not-a-url"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(t.Context(), tc.opts, nil, func() Prompter {
				t.Fatal("constructed prompter before input validation")
				return nil
			})
			if err == nil {
				t.Fatal("invalid inputs were accepted")
			}
		})
	}
}

type closingPrompter struct {
	Prompter
	closes int
}

func (p *closingPrompter) Close() error {
	p.closes++
	return nil
}

func TestRunGuidedPrompterLifecycle(t *testing.T) {
	successInput := strings.Join([]string{"", "", defaultTestDashboardRepo, "", "", "", "n", "", "y"}, "\n") + "\n"
	for _, tc := range []struct {
		name      string
		input     string
		preflight error
		writes    int
	}{
		{name: "success", input: successInput, writes: 1},
		{name: "cancellation", input: "q\n"},
		{name: "error", input: successInput, preflight: errors.New("destination unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, out, writer, _ := wizardDependencies(tc.input)
			writer.validateErr = tc.preflight
			prompter := &closingPrompter{Prompter: deps.newPrompter()}
			constructions := 0
			deps.newPrompter = func() Prompter {
				constructions++
				return prompter
			}
			err := run(t.Context(), Options{SourceRepo: "example/project", EngineRef: "main", PromptMode: promptModeTemplate}, deps)
			if !errors.Is(err, tc.preflight) {
				t.Fatalf("run = %v, want %v\n%s", err, tc.preflight, out)
			}
			if constructions != 1 || prompter.closes != 1 || writer.writes != tc.writes {
				t.Fatalf("constructions=%d closes=%d writes=%d, want 1, 1, %d", constructions, prompter.closes, writer.writes, tc.writes)
			}
		})
	}
}

func TestRunReportsUnavailablePrompter(t *testing.T) {
	err := Run(t.Context(), Options{SourceRepo: "example/project"}, io.Discard, func() Prompter { return nil })
	if err == nil || err.Error() != "interactive onboarding UI is unavailable" {
		t.Fatalf("run = %v", err)
	}
}

func TestHeadlessOnboardingDependencies(t *testing.T) {
	if defaultDependencies(Options{}, io.Discard).newPrompter != nil {
		t.Fatal("headless defaults include an interactive prompter factory")
	}
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", ".", "../kubernetesdeploy")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list headless dependencies: %v\n%s", err, output)
	}
	for dependency := range strings.FieldsSeq(string(output)) {
		if dependency == "github.com/willie-yao/aster/backend/internal/onboard/terminal" ||
			dependency == "github.com/muesli/cancelreader" ||
			strings.HasPrefix(dependency, "charm.land/") ||
			strings.HasPrefix(dependency, "github.com/charmbracelet/") {
			t.Errorf("headless dependency includes terminal implementation %s", dependency)
		}
	}
}
