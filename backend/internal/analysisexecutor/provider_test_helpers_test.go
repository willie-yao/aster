package analysisexecutor

import (
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
)

func testOpenCodeProvider(endpoint, model string) modelprovider.Config {
	if endpoint == "" {
		endpoint = "http://127.0.0.1/v1/chat/completions"
	}
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
}

func testGatewayProvider(endpoint, model string) modelprovider.Config {
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
	}
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
}

func testDirectBearerProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}

func testResponsesProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIResponses,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}
