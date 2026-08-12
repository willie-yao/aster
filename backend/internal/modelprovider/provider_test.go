package modelprovider

import (
	"strings"
	"testing"
)

func TestValidateOpenCodeModes(t *testing.T) {
	directNone := Normalize(Config{CredentialMode: CredentialModeDirect, API: APIChatCompletions, Endpoint: "https://provider.example/v1/chat/completions", Model: "fixture", Auth: Auth{Type: AuthTypeNone}})
	directBearer := directNone
	directBearer.Auth = Auth{Type: AuthTypeBearer, TokenEnv: TokenEnv}
	gateway := Normalize(Config{CredentialMode: CredentialModeGateway, API: APIChatCompletions, Endpoint: "https://gateway.platform.svc.cluster.local/v1/chat/completions", Model: "fixture", Auth: Auth{Type: AuthTypeNone}})
	responses := directBearer
	responses.API = APIResponses
	responses.Endpoint = "https://provider.example/v1/responses"
	for _, config := range []Config{directNone, directBearer, responses, gateway} {
		if err := ValidateOpenCode(config); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CredentialMode = "ambient" },
		func(c *Config) { c.API = "completions" },
		func(c *Config) { c.Endpoint = "http://provider.example/v1/chat/completions" },
		func(c *Config) { c.Endpoint = "https://user@provider.example/v1/chat/completions" },
		func(c *Config) { c.Model = "" },
		func(c *Config) { c.Auth = Auth{Type: AuthTypeBearer, TokenEnv: "OTHER_TOKEN"} },
		func(c *Config) { c.PublicCAPrivateDNS = true },
	} {
		candidate := directNone
		mutate(&candidate)
		if err := ValidateOpenCode(candidate); err == nil {
			t.Fatalf("invalid provider accepted: %+v", candidate)
		}
	}
	responsesNone := responses
	responsesNone.Auth = Auth{Type: AuthTypeNone}
	if err := ValidateOpenCode(responsesNone); err == nil {
		t.Fatal("Responses without direct bearer auth accepted")
	}
	gatewayResponses := gateway
	gatewayResponses.API = APIResponses
	gatewayResponses.Endpoint = "https://gateway.platform.svc.cluster.local/v1/responses"
	if err := ValidateOpenCode(gatewayResponses); err == nil {
		t.Fatal("gateway Responses accepted without a provider credential")
	}
	gatewayBearer := gateway
	gatewayBearer.Auth = Auth{Type: AuthTypeBearer, TokenEnv: TokenEnv}
	if err := ValidateOpenCode(gatewayBearer); err == nil {
		t.Fatal("gateway bearer auth accepted")
	}
}

func TestOpenCodeAdapterFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		api      string
		endpoint string
		base     string
		npm      string
	}{
		{name: "chat", api: APIChatCompletions, endpoint: "https://provider.example/v1/chat/completions", base: "https://provider.example/v1", npm: "@ai-sdk/openai-compatible"},
		{name: "responses", api: APIResponses, endpoint: "https://provider.example/v1/responses", base: "https://provider.example/v1", npm: "@ai-sdk/openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := Auth{Type: AuthTypeNone}
			if tc.api == APIResponses {
				auth = Auth{Type: AuthTypeBearer}
			}
			config := Normalize(Config{API: tc.api, Endpoint: tc.endpoint, Model: "fixture", Auth: auth})
			adapter, err := OpenCodeAdapterFor(config)
			if err != nil {
				t.Fatal(err)
			}
			if adapter.BaseURL != tc.base || adapter.NPM != tc.npm {
				t.Fatalf("adapter = %+v", adapter)
			}
		})
	}
	for _, config := range []Config{
		Normalize(Config{API: APIChatCompletions, Endpoint: "https://provider.example/v1/responses", Model: "fixture", Auth: Auth{Type: AuthTypeNone}}),
		Normalize(Config{API: APIResponses, Endpoint: "https://provider.example/v1/chat/completions", Model: "fixture", Auth: Auth{Type: AuthTypeBearer}}),
		Normalize(Config{API: APIResponses, Endpoint: "https://provider.example/v1", Model: "fixture", Auth: Auth{Type: AuthTypeBearer}}),
	} {
		if _, err := OpenCodeAdapterFor(config); err == nil {
			t.Fatalf("mismatched endpoint accepted: %+v", config)
		}
	}
}

func TestCredentialGuard(t *testing.T) {
	config := Normalize(Config{Endpoint: "https://provider.example/v1/chat/completions", Model: "fixture", Auth: Auth{Type: AuthTypeBearer}})
	credential := strings.Repeat("fixture-credential-", 2)
	guard, err := NewCredentialGuard(config, func(name string) (string, bool) {
		if name != TokenEnv {
			t.Fatal("unexpected environment lookup")
		}
		return credential, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(guard.Environment()) != 1 || !strings.HasPrefix(guard.Environment()[0], TokenEnv+"=") {
		t.Fatal("credential environment was not isolated")
	}
	if guard.CheckStrings("safe") != nil || guard.CheckStrings("prefix"+credential+"suffix") == nil {
		t.Fatal("whole-value credential detection failed")
	}
	detector := guard.NewDetector()
	mid := len(credential) / 2
	_, _ = detector.Write([]byte("prefix" + credential[:mid]))
	_, _ = detector.Write([]byte(credential[mid:] + "suffix"))
	if !detector.Detected() {
		t.Fatal("streaming credential detection failed")
	}
}
