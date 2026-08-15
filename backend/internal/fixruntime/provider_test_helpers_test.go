package fixruntime

import "github.com/willie-yao/aster/backend/internal/modelprovider"

func testGatewayProvider(endpoint, model string) modelprovider.Config {
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

func testDirectUnauthenticatedProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect,
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
