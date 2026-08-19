package githubapp

import (
	"os"
	"strings"
)

// Environment variables carrying the App credentials.
const (
	EnvAppID      = "ASTER_APP_ID"
	EnvPrivateKey = "ASTER_APP_PRIVATE_KEY"
)

// CredentialsFromEnv reads the App credentials from the environment. The second
// result is false when neither variable is set, which callers treat as "the App
// is not configured" rather than as an error.
func CredentialsFromEnv() (Credentials, bool) {
	creds := Credentials{
		AppID:      strings.TrimSpace(os.Getenv(EnvAppID)),
		PrivateKey: strings.TrimSpace(os.Getenv(EnvPrivateKey)),
	}
	if creds.AppID == "" && creds.PrivateKey == "" {
		return Credentials{}, false
	}
	return creds, true
}
