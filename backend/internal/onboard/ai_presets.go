package onboard

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type aiProviderID string

const (
	aiProviderChoose                 aiProviderID = "choose"
	aiProviderGitHubCopilotResponses aiProviderID = "github_copilot_responses"
	aiProviderGitHubCopilot          aiProviderID = "github_copilot"
	aiProviderOpenAIResponse         aiProviderID = "openai_responses"
	aiProviderOpenAIChat             aiProviderID = "openai_chat_completions"
	aiProviderNVIDIA                 aiProviderID = "nvidia_api"
	aiProviderSelfHosted             aiProviderID = "self_hosted"
	aiProviderAzure                  aiProviderID = "azure_openai"
	aiProviderCustom                 aiProviderID = "custom"
	aiProviderConfigureLater         aiProviderID = "configure_later"
)

type aiProviderPreset struct {
	ID             aiProviderID
	Label          string
	Description    string
	API            string
	Endpoint       string
	AskAPI         bool
	ConfigureLater bool
}

var aiProviderPresets = []aiProviderPreset{
	{
		ID:          aiProviderGitHubCopilotResponses,
		Label:       "GitHub Copilot Responses",
		Description: "Uses Copilot's Responses endpoint for Responses-only models and requires a fine-grained PAT with copilot_chat permission.",
		API:         project.AIAPIResponses,
		Endpoint:    "https://api.githubcopilot.com/responses",
	},
	{
		ID:          aiProviderGitHubCopilot,
		Label:       "GitHub Copilot Chat Completions",
		Description: "Uses Copilot's Chat Completions endpoint and requires a fine-grained PAT with copilot_chat permission.",
		API:         project.AIAPIChatCompletions,
		Endpoint:    "https://api.githubcopilot.com/chat/completions",
	},
	{
		ID:          aiProviderOpenAIResponse,
		Label:       "OpenAI Responses",
		Description: "Uses OpenAI's Responses API with store disabled.",
		API:         project.AIAPIResponses,
		Endpoint:    "https://api.openai.com/v1/responses",
	},
	{
		ID:          aiProviderOpenAIChat,
		Label:       "OpenAI Chat Completions",
		Description: "Uses OpenAI's Chat Completions API.",
		API:         project.AIAPIChatCompletions,
		Endpoint:    "https://api.openai.com/v1/chat/completions",
	},
	{
		ID:          aiProviderNVIDIA,
		Label:       "NVIDIA API",
		Description: "Uses the public NVIDIA API with an OpenAI-compatible request contract.",
		API:         project.AIAPIChatCompletions,
		Endpoint:    "https://integrate.api.nvidia.com/v1/chat/completions",
	},
	{
		ID:          aiProviderSelfHosted,
		Label:       "Self-hosted OpenAI-compatible",
		Description: "Use Ollama, vLLM, NIM, Ray Serve, or another compatible endpoint.",
		AskAPI:      true,
	},
	{
		ID:          aiProviderAzure,
		Label:       "Azure OpenAI or Azure gateway",
		Description: "Azure deployments often need a trusted gateway for api-key authentication.",
		AskAPI:      true,
	},
	{
		ID:          aiProviderCustom,
		Label:       "Other custom endpoint",
		Description: "Enter any compatible Chat Completions or Responses endpoint.",
		AskAPI:      true,
	},
	{
		ID:             aiProviderConfigureLater,
		Label:          "Configure later",
		Description:    "Generate a valid initial scaffold with deployed AI disabled.",
		ConfigureLater: true,
	},
}

func aiProviderOptions(mode string) []selectOption {
	options := []selectOption{{
		Value:       string(aiProviderChoose),
		Label:       "Choose a provider",
		Description: "Select a provider or choose Configure later.",
	}}
	for _, preset := range aiProviderPresets {
		description := preset.Description
		switch preset.ID {
		case aiProviderSelfHosted, aiProviderCustom:
			if mode == modePages {
				description += " The endpoint must be reachable from GitHub Actions."
			} else {
				description += " Cluster-local endpoints are supported."
			}
		case aiProviderAzure:
			if mode == modePages {
				description += " The stock Pages workflow sends a bearer token, not an Azure api-key header."
			}
		}
		options = append(options, selectOption{
			Value:       string(preset.ID),
			Label:       preset.Label,
			Description: description,
		})
	}
	return options
}

func aiProviderPresetForID(id aiProviderID) (aiProviderPreset, bool) {
	for _, preset := range aiProviderPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return aiProviderPreset{}, false
}

func matchAIProviderPreset(api, endpoint string) aiProviderID {
	endpoint = normalizeAIEndpointForMatch(endpoint)
	if endpoint == "" {
		return aiProviderChoose
	}
	api = strings.ToLower(strings.TrimSpace(api))
	for _, preset := range aiProviderPresets {
		if preset.Endpoint == "" {
			continue
		}
		if normalizeAIEndpointForMatch(preset.Endpoint) == endpoint && (api == "" || preset.API == api) {
			return preset.ID
		}
	}
	return aiProviderCustom
}

func providerLabel(api, endpoint string) string {
	id := matchAIProviderPreset(api, endpoint)
	if preset, ok := aiProviderPresetForID(id); ok && preset.Endpoint != "" {
		return preset.Label
	}
	return "Custom provider"
}

func normalizeAIEndpointForMatch(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}

func validateAIPresetTable() error {
	seen := map[aiProviderID]struct{}{}
	for _, preset := range aiProviderPresets {
		if preset.ID == "" || preset.Label == "" {
			return fmt.Errorf("AI provider preset has an empty ID or label")
		}
		if _, ok := seen[preset.ID]; ok {
			return fmt.Errorf("AI provider preset ID %q is duplicated", preset.ID)
		}
		seen[preset.ID] = struct{}{}
		if preset.ConfigureLater {
			if preset.API != "" || preset.Endpoint != "" || preset.AskAPI {
				return fmt.Errorf("configure-later preset must not carry provider coordinates")
			}
			continue
		}
		if preset.AskAPI {
			if preset.API != "" || preset.Endpoint != "" {
				return fmt.Errorf("customizable preset %q must not carry fixed coordinates", preset.ID)
			}
			continue
		}
		if err := project.ValidateAIAPI(preset.API); err != nil {
			return fmt.Errorf("AI provider preset %q: %w", preset.ID, err)
		}
		if preset.API == "" || preset.Endpoint == "" {
			return fmt.Errorf("AI provider preset %q is missing fixed coordinates", preset.ID)
		}
		if err := validateAIEndpoint(preset.Endpoint); err != nil {
			return fmt.Errorf("AI provider preset %q: %w", preset.ID, err)
		}
	}
	return nil
}

func pagesEndpointWarnings(endpoint string) []string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	var warnings []string
	if strings.EqualFold(parsed.Scheme, "http") {
		warnings = append(warnings, "The endpoint uses HTTP, so a bearer token would be sent without TLS.")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		warnings = append(warnings, "GitHub-hosted Actions runners cannot reach a localhost name on your machine.")
		return warnings
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsUnspecified():
			warnings = append(warnings, "An unspecified IP address does not identify a reachable model service.")
		case ip.IsLoopback():
			warnings = append(warnings, "GitHub-hosted Actions runners cannot reach a loopback address.")
		case ip.IsLinkLocalUnicast():
			warnings = append(warnings, "GitHub-hosted Actions runners cannot reach your model through a link-local address.")
		case ip.IsPrivate():
			warnings = append(warnings, "GitHub-hosted Actions runners normally cannot reach a private IP address.")
		}
		return warnings
	}
	switch {
	case strings.HasSuffix(host, ".cluster.local"), strings.HasSuffix(host, ".svc"):
		warnings = append(warnings, "GitHub-hosted Actions runners cannot resolve a cluster-local service name.")
	case strings.HasSuffix(host, ".local"):
		warnings = append(warnings, "GitHub-hosted Actions runners normally cannot resolve a local-only DNS name.")
	}
	return warnings
}
