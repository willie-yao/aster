package agentanalysis

import (
	"strings"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

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

func testResponsesProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
		API:            modelprovider.APIResponses,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}
