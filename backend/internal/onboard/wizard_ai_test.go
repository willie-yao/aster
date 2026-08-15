package onboard

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
)

const usePromptDefault = "<default>"

type queuedWizardUI struct {
	inputs         []string
	selects        []string
	confirms       []bool
	inputPrompts   []inputPrompt
	selectPrompts  []selectPrompt
	confirmPrompts []confirmPrompt
}

func (u *queuedWizardUI) Input(_ context.Context, prompt inputPrompt) (string, error) {
	u.inputPrompts = append(u.inputPrompts, prompt)
	if len(u.inputs) == 0 {
		return "", fmt.Errorf("unexpected input prompt %q", prompt.Title)
	}
	value := u.inputs[0]
	u.inputs = u.inputs[1:]
	if value == usePromptDefault {
		value = prompt.Value
	}
	if prompt.Required && strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s was empty", prompt.Title)
	}
	if prompt.Validate != nil {
		if err := prompt.Validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (u *queuedWizardUI) Select(_ context.Context, prompt selectPrompt) (string, error) {
	u.selectPrompts = append(u.selectPrompts, prompt)
	if len(u.selects) == 0 {
		return "", fmt.Errorf("unexpected select prompt %q", prompt.Title)
	}
	value := u.selects[0]
	u.selects = u.selects[1:]
	if value == usePromptDefault {
		value = prompt.Value
	}
	if prompt.Validate != nil {
		if err := prompt.Validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (u *queuedWizardUI) Confirm(_ context.Context, prompt confirmPrompt) (bool, error) {
	u.confirmPrompts = append(u.confirmPrompts, prompt)
	if len(u.confirms) == 0 {
		return false, fmt.Errorf("unexpected confirmation prompt %q", prompt.Title)
	}
	value := u.confirms[0]
	u.confirms = u.confirms[1:]
	return value, nil
}

func TestWizardDeploymentAI_PublicPresets(t *testing.T) {
	for _, preset := range aiProviderPresets {
		if preset.Endpoint == "" {
			continue
		}
		t.Run(string(preset.ID), func(t *testing.T) {
			enabled := true
			opts := Options{Mode: modeK8s, AIEnabled: &enabled}
			ui := &queuedWizardUI{
				selects: []string{string(preset.ID)},
				inputs:  []string{usePromptDefault, "account-model"},
			}
			if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
				t.Fatalf("wizardDeploymentAI: %v", err)
			}
			if len(ui.selectPrompts) == 0 || ui.selectPrompts[0].Value != string(aiProviderChoose) {
				t.Fatalf("new provider selection had an implicit default: %+v", ui.selectPrompts)
			}
			if opts.DeploymentAIAPI != preset.API || opts.DeploymentAIEndpoint != preset.Endpoint || opts.DeploymentAIModel != "account-model" {
				t.Fatalf("coordinates = %q %q %q", opts.DeploymentAIAPI, opts.DeploymentAIEndpoint, opts.DeploymentAIModel)
			}
			if len(ui.inputPrompts) != 2 || ui.inputPrompts[1].Value != "" {
				t.Fatalf("model prompt unexpectedly had a preset default: %+v", ui.inputPrompts)
			}
		})
	}
}

func TestWizardDeploymentAI_CustomizableProviders(t *testing.T) {
	tests := []struct {
		name     string
		provider aiProviderID
		api      string
		endpoint string
	}{
		{name: "self-hosted chat", provider: aiProviderSelfHosted, api: project.AIAPIChatCompletions, endpoint: "https://self-hosted.example/v1/chat/completions"},
		{name: "Azure responses", provider: aiProviderAzure, api: project.AIAPIResponses, endpoint: "https://azure-gateway.example/v1/responses"},
		{name: "custom responses", provider: aiProviderCustom, api: project.AIAPIResponses, endpoint: "https://custom.example/v1/responses"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enabled := true
			opts := Options{Mode: modeK8s, AIEnabled: &enabled}
			ui := &queuedWizardUI{
				selects: []string{string(test.provider), test.api},
				inputs:  []string{test.endpoint, "custom-model"},
			}
			if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
				t.Fatalf("wizardDeploymentAI: %v", err)
			}
			if opts.DeploymentAIAPI != test.api || opts.DeploymentAIEndpoint != test.endpoint || opts.DeploymentAIModel != "custom-model" {
				t.Fatalf("coordinates = %+v", opts)
			}
		})
	}
}

func TestWizardDeploymentAI_ConfigureLaterDisablesInitialAI(t *testing.T) {
	enabled := true
	opts := Options{
		Mode: modePages, AIEnabled: &enabled,
		DeploymentAIAPI: project.AIAPIResponses, DeploymentAIEndpoint: "https://custom.example/v1/responses", DeploymentAIModel: "private-model",
	}
	out := &bytes.Buffer{}
	ui := &queuedWizardUI{selects: []string{string(aiProviderConfigureLater)}}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, out); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	if effectiveAIEnabled(opts) || opts.DeploymentAIAPI != "" || opts.DeploymentAIEndpoint != "" || opts.DeploymentAIModel != "" {
		t.Fatalf("configure later retained deployed AI: %+v", opts)
	}
	if !strings.Contains(out.String(), "disabled in the initial scaffold") {
		t.Fatalf("configure-later note missing: %q", out.String())
	}
	if len(ui.inputPrompts) != 0 || len(ui.confirmPrompts) != 0 {
		t.Fatalf("configure later requested provider details: inputs=%v confirms=%v", ui.inputPrompts, ui.confirmPrompts)
	}
}

func TestWizardDeploymentAI_PagesWarningRejectionReturnsToProviderSelection(t *testing.T) {
	enabled := true
	opts := Options{Mode: modePages, AIEnabled: &enabled}
	ui := &queuedWizardUI{
		selects: []string{
			string(aiProviderCustom), project.AIAPIChatCompletions,
			string(aiProviderCustom), project.AIAPIChatCompletions,
		},
		inputs: []string{
			"http://localhost:8000/v1/chat/completions", "local-model",
			"https://public.example/v1/chat/completions", "public-model",
		},
		confirms: []bool{false},
	}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	if opts.DeploymentAIEndpoint != "https://public.example/v1/chat/completions" || opts.DeploymentAIModel != "public-model" {
		t.Fatalf("final coordinates = %+v", opts)
	}
	providerPromptCount := 0
	for _, prompt := range ui.selectPrompts {
		if prompt.Title == "Deployed AI provider" {
			providerPromptCount++
		}
	}
	if providerPromptCount != 2 {
		t.Fatalf("provider prompt count = %d", providerPromptCount)
	}
	if len(ui.confirmPrompts) != 1 || ui.confirmPrompts[0].Value {
		t.Fatalf("Pages warning confirmation = %+v", ui.confirmPrompts)
	}
}

func TestWizardDeploymentAI_ProviderSwitchClearsRejectedModelDefault(t *testing.T) {
	enabled := true
	opts := Options{Mode: modePages, AIEnabled: &enabled}
	ui := &queuedWizardUI{
		selects: []string{
			string(aiProviderCustom), project.AIAPIChatCompletions,
			string(aiProviderOpenAIResponse),
		},
		inputs: []string{
			"http://localhost:8000/v1/chat/completions", "local-model",
			usePromptDefault, "openai-model",
		},
		confirms: []bool{false},
	}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	var modelPrompts []inputPrompt
	for _, prompt := range ui.inputPrompts {
		if prompt.Title == "Deployed AI model" {
			modelPrompts = append(modelPrompts, prompt)
		}
	}
	if len(modelPrompts) != 2 || modelPrompts[0].Value != "" || modelPrompts[1].Value != "" {
		t.Fatalf("model defaults = %+v", modelPrompts)
	}
	if opts.DeploymentAIEndpoint != "https://api.openai.com/v1/responses" || opts.DeploymentAIModel != "openai-model" {
		t.Fatalf("final coordinates = %+v", opts)
	}
}

func TestWizardDeploymentAI_PagesWarningCanBeAcceptedExplicitly(t *testing.T) {
	enabled := true
	opts := Options{Mode: modePages, AIEnabled: &enabled}
	ui := &queuedWizardUI{
		selects:  []string{string(aiProviderCustom), project.AIAPIChatCompletions},
		inputs:   []string{"http://localhost:8000/v1/chat/completions", "local-model"},
		confirms: []bool{true},
	}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	if opts.DeploymentAIEndpoint != "http://localhost:8000/v1/chat/completions" {
		t.Fatalf("accepted endpoint = %q", opts.DeploymentAIEndpoint)
	}
	if len(ui.confirmPrompts) != 1 || ui.confirmPrompts[0].Value {
		t.Fatalf("warning confirmation = %+v", ui.confirmPrompts)
	}
}

func TestWizardDeploymentAI_KubernetesAllowsClusterLocalHTTP(t *testing.T) {
	enabled := true
	opts := Options{Mode: modeK8s, AIEnabled: &enabled}
	ui := &queuedWizardUI{
		selects: []string{string(aiProviderSelfHosted), project.AIAPIChatCompletions},
		inputs:  []string{"http://model.namespace.svc.cluster.local:8000/v1/chat/completions", "cluster-model"},
	}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	if len(ui.confirmPrompts) != 0 {
		t.Fatalf("Kubernetes endpoint triggered Pages warning: %+v", ui.confirmPrompts)
	}
}

func TestWizardDeploymentAI_PreselectsExistingProviderCoordinates(t *testing.T) {
	enabled := true
	opts := Options{
		Mode: modePages, AIEnabled: &enabled,
		DeploymentAIAPI:      project.AIAPIResponses,
		DeploymentAIEndpoint: "https://api.openai.com/v1/responses",
		DeploymentAIModel:    "existing-model",
	}
	ui := &queuedWizardUI{
		selects: []string{usePromptDefault},
		inputs:  []string{usePromptDefault, usePromptDefault},
	}
	if err := wizardDeploymentAI(context.Background(), ui, &opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("wizardDeploymentAI: %v", err)
	}
	if len(ui.selectPrompts) == 0 || ui.selectPrompts[0].Value != string(aiProviderOpenAIResponse) {
		t.Fatalf("provider default = %+v", ui.selectPrompts)
	}
	if got := []string{opts.DeploymentAIAPI, opts.DeploymentAIEndpoint, opts.DeploymentAIModel}; !reflect.DeepEqual(got, []string{project.AIAPIResponses, "https://api.openai.com/v1/responses", "existing-model"}) {
		t.Fatalf("coordinates = %v", got)
	}
}
