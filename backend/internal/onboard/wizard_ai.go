package onboard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/aster/backend/internal/project"
)

func wizardDeploymentAI(ctx context.Context, prompt wizardUI, opts *Options, out io.Writer) error {
	if opts.AIEnabled == nil {
		enabled, err := prompt.Confirm(ctx, confirmPrompt{
			Title:       "Enable AI failure analysis in the deployed dashboard?",
			Description: "Configure a provider now or choose Configure later on the next screen.",
			Value:       true,
		})
		if err != nil {
			return err
		}
		opts.AIEnabled = &enabled
	}
	if !effectiveAIEnabled(*opts) {
		return nil
	}

	selected := matchAIProviderPreset(deploymentAIAPI(*opts), deploymentAIEndpoint(*opts))
	modelDefault := deploymentAIModel(*opts)
	for {
		choice, err := prompt.Select(ctx, selectPrompt{
			Title:       "Deployed AI provider",
			Description: "Presets fill the API and endpoint. Tokens remain environment-only.",
			Options:     aiProviderOptions(opts.Mode),
			Value:       string(selected),
			Validate: func(value string) error {
				if aiProviderID(value) == aiProviderChoose {
					return fmt.Errorf("choose a provider or Configure later")
				}
				return nil
			},
		})
		if err != nil {
			return err
		}
		providerID := aiProviderID(choice)
		preset, ok := aiProviderPresetForID(providerID)
		if !ok {
			return fmt.Errorf("AI provider %q is unavailable", providerID)
		}
		if preset.ConfigureLater {
			disabled := false
			opts.AIEnabled = &disabled
			opts.DeploymentAIAPI = ""
			opts.DeploymentAIEndpoint = ""
			opts.DeploymentAIModel = ""
			opts.deferDeploymentAI = true
			fmt.Fprintln(out, "Deployed AI will be disabled in the initial scaffold and can be configured later.")
			return nil
		}

		existingSelection := selected == providerID
		api := preset.API
		endpoint := preset.Endpoint
		if existingSelection {
			if value := deploymentAIEndpoint(*opts); value != "" {
				endpoint = value
			}
		}
		if preset.AskAPI {
			api = project.AIAPIChatCompletions
			if existingSelection && deploymentAIAPI(*opts) == project.AIAPIResponses {
				api = project.AIAPIResponses
			}
			api, err = prompt.Select(ctx, selectPrompt{
				Title:       "Deployed AI API",
				Description: "Choose the request contract supported by the endpoint.",
				Options: []selectOption{
					{Value: project.AIAPIChatCompletions, Label: "Chat Completions"},
					{Value: project.AIAPIResponses, Label: "Responses"},
				},
				Value: api,
			})
			if err != nil {
				return err
			}
		}
		endpoint, err = prompt.Input(ctx, inputPrompt{
			Title:       "Deployed AI endpoint",
			Description: "Absolute HTTP or HTTPS endpoint reachable from the deployment.",
			Value:       endpoint,
			Required:    true,
			Validate:    validateAIEndpoint,
		})
		if err != nil {
			return err
		}
		modelValue := ""
		if existingSelection {
			modelValue = modelDefault
		}
		model, err := prompt.Input(ctx, inputPrompt{
			Title:       "Deployed AI model",
			Description: "Exact model identifier available to your account or deployment.",
			Value:       modelValue,
			Required:    true,
		})
		if err != nil {
			return err
		}

		opts.DeploymentAIAPI = api
		opts.DeploymentAIEndpoint = endpoint
		opts.DeploymentAIModel = model
		opts.deferDeploymentAI = false
		modelDefault = model
		selected = providerID

		if opts.Mode == modePages {
			warnings := pagesEndpointWarnings(endpoint)
			if len(warnings) > 0 {
				proceed, err := prompt.Confirm(ctx, confirmPrompt{
					Title:       "Continue with this endpoint for GitHub Pages?",
					Description: strings.Join(warnings, "\n"),
					Value:       false,
				})
				if err != nil {
					return err
				}
				if !proceed {
					continue
				}
			}
		}
		return nil
	}
}
