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
	for _, config := range []Config{directNone, directBearer, gateway} {
		if err := ValidateOpenCode(config); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CredentialMode = "ambient" },
		func(c *Config) { c.API = APIResponses },
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
	gatewayBearer := gateway
	gatewayBearer.Auth = Auth{Type: AuthTypeBearer, TokenEnv: TokenEnv}
	if err := ValidateOpenCode(gatewayBearer); err == nil {
		t.Fatal("gateway bearer auth accepted")
	}
}

func TestOpenCodeBaseURL(t *testing.T) {
	config := Normalize(Config{Endpoint: "https://provider.example/v1/chat/completions", Model: "fixture", Auth: Auth{Type: AuthTypeNone}})
	base, err := OpenCodeBaseURL(config)
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://provider.example/v1" {
		t.Fatalf("base = %q", base)
	}
	config.Endpoint = "https://provider.example/v1"
	if _, err := OpenCodeBaseURL(config); err == nil {
		t.Fatal("ambiguous endpoint accepted")
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
