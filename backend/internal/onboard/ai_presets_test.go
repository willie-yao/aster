package onboard

import (
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestAIProviderPresetTable(t *testing.T) {
	if err := validateAIPresetTable(); err != nil {
		t.Fatalf("validateAIPresetTable: %v", err)
	}
	wantOrder := []aiProviderID{
		aiProviderGitHubCopilotResponses,
		aiProviderGitHubCopilot,
		aiProviderOpenAIResponse,
		aiProviderOpenAIChat,
		aiProviderNVIDIA,
		aiProviderSelfHosted,
		aiProviderAzure,
		aiProviderCustom,
		aiProviderConfigureLater,
	}
	gotOrder := make([]aiProviderID, 0, len(aiProviderPresets))
	for _, preset := range aiProviderPresets {
		gotOrder = append(gotOrder, preset.ID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("preset order = %v, want %v", gotOrder, wantOrder)
	}

	wantCoordinates := map[aiProviderID]struct {
		api      string
		endpoint string
	}{
		aiProviderGitHubCopilotResponses: {project.AIAPIResponses, "https://api.githubcopilot.com/responses"},
		aiProviderGitHubCopilot:          {project.AIAPIChatCompletions, "https://api.githubcopilot.com/chat/completions"},
		aiProviderOpenAIResponse:         {project.AIAPIResponses, "https://api.openai.com/v1/responses"},
		aiProviderOpenAIChat:             {project.AIAPIChatCompletions, "https://api.openai.com/v1/chat/completions"},
		aiProviderNVIDIA:                 {project.AIAPIChatCompletions, "https://integrate.api.nvidia.com/v1/chat/completions"},
	}
	for id, want := range wantCoordinates {
		preset, ok := aiProviderPresetForID(id)
		if !ok {
			t.Fatalf("preset %q missing", id)
		}
		if preset.API != want.api || preset.Endpoint != want.endpoint {
			t.Fatalf("preset %q coordinates = %q %q, want %q %q", id, preset.API, preset.Endpoint, want.api, want.endpoint)
		}
	}
}

func TestAIProviderOptionsDescribeDeploymentReachability(t *testing.T) {
	pages := aiProviderOptions(modePages)
	k8s := aiProviderOptions(modeK8s)
	if pages[0].Value != string(aiProviderChoose) || k8s[0].Value != string(aiProviderChoose) {
		t.Fatalf("provider sentinel missing: pages=%v k8s=%v", pages[0], k8s[0])
	}
	find := func(options []selectOption, id aiProviderID) selectOption {
		t.Helper()
		for _, option := range options {
			if option.Value == string(id) {
				return option
			}
		}
		t.Fatalf("option %q missing", id)
		return selectOption{}
	}
	if got := find(pages, aiProviderSelfHosted).Description; !strings.Contains(got, "GitHub Actions") {
		t.Fatalf("Pages self-hosted description = %q", got)
	}
	if got := find(k8s, aiProviderSelfHosted).Description; !strings.Contains(got, "Cluster-local") {
		t.Fatalf("Kubernetes self-hosted description = %q", got)
	}
	if got := find(pages, aiProviderAzure).Description; !strings.Contains(got, "bearer token") {
		t.Fatalf("Pages Azure description = %q", got)
	}
}

func TestCopilotProviderOptionsExposeBothAPIs(t *testing.T) {
	options := aiProviderOptions(modePages)
	want := map[string]bool{
		"GitHub Copilot Responses":        false,
		"GitHub Copilot Chat Completions": false,
	}
	for _, option := range options {
		if _, ok := want[option.Label]; ok {
			want[option.Label] = true
		}
	}
	for label, found := range want {
		if !found {
			t.Fatalf("provider option %q missing: %v", label, options)
		}
	}
}

func TestMatchAIProviderPresetAndLabel(t *testing.T) {
	for _, preset := range aiProviderPresets {
		if preset.Endpoint == "" {
			continue
		}
		if got := matchAIProviderPreset(preset.API, preset.Endpoint+"/"); got != preset.ID {
			t.Errorf("match %q = %q", preset.ID, got)
		}
		if got := providerLabel(preset.API, preset.Endpoint); got != preset.Label {
			t.Errorf("label %q = %q, want %q", preset.ID, got, preset.Label)
		}
	}
	if got := matchAIProviderPreset(project.AIAPIResponses, "https://custom.example/v1/responses"); got != aiProviderCustom {
		t.Fatalf("custom match = %q", got)
	}
	if got := providerLabel(project.AIAPIResponses, "https://custom.example/v1/responses"); got != "Custom provider" {
		t.Fatalf("custom label = %q", got)
	}
	if got := matchAIProviderPreset("", "https://api.openai.com/v1/responses"); got != aiProviderOpenAIResponse {
		t.Fatalf("endpoint-only match = %q", got)
	}
	if got := matchAIProviderPreset("", ""); got != aiProviderChoose {
		t.Fatalf("empty match = %q", got)
	}
}

func TestPagesEndpointWarnings(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     []string
	}{
		{name: "public HTTPS", endpoint: "https://api.example.com/v1/chat/completions"},
		{name: "public HTTP", endpoint: "http://api.example.com/v1/chat/completions", want: []string{"without TLS"}},
		{name: "localhost", endpoint: "http://localhost:8000/v1/chat/completions", want: []string{"without TLS", "localhost"}},
		{name: "localhost subdomain", endpoint: "https://api.localhost/v1/chat/completions", want: []string{"localhost"}},
		{name: "IPv4 unspecified", endpoint: "http://0.0.0.0:8000/v1/chat/completions", want: []string{"without TLS", "unspecified"}},
		{name: "IPv6 unspecified", endpoint: "http://[::]:8000/v1/chat/completions", want: []string{"without TLS", "unspecified"}},
		{name: "IPv4 loopback", endpoint: "http://127.0.0.1:8000/v1/chat/completions", want: []string{"without TLS", "loopback"}},
		{name: "IPv6 loopback", endpoint: "http://[::1]:8000/v1/chat/completions", want: []string{"without TLS", "loopback"}},
		{name: "IPv4 link-local", endpoint: "https://169.254.1.1/v1/chat/completions", want: []string{"link-local"}},
		{name: "IPv6 link-local", endpoint: "https://[fe80::1]/v1/chat/completions", want: []string{"link-local"}},
		{name: "private IPv4", endpoint: "https://10.1.2.3/v1/chat/completions", want: []string{"private IP"}},
		{name: "private IPv6", endpoint: "https://[fd00::1]/v1/chat/completions", want: []string{"private IP"}},
		{name: "local DNS", endpoint: "https://model.local/v1/chat/completions", want: []string{"local-only DNS"}},
		{name: "service DNS", endpoint: "http://model.namespace.svc:8000/v1/chat/completions", want: []string{"without TLS", "cluster-local"}},
		{name: "cluster DNS", endpoint: "http://model.namespace.svc.cluster.local:8000/v1/chat/completions", want: []string{"without TLS", "cluster-local"}},
		{name: "invalid", endpoint: "://invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			warnings := pagesEndpointWarnings(test.endpoint)
			if len(warnings) != len(test.want) {
				t.Fatalf("warnings = %v, want %v", warnings, test.want)
			}
			for i, fragment := range test.want {
				if !strings.Contains(warnings[i], fragment) {
					t.Fatalf("warning[%d] = %q, want fragment %q", i, warnings[i], fragment)
				}
			}
		})
	}
}
