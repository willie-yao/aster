package auth

import (
	"context"
	"net/http"
)

// DevAuthenticator authenticates every request as a fixed admin identity, for
// local development only. It performs no authentication and grants admin to any
// caller, so it must never be used in a deployment reachable by untrusted
// clients. It exists so `make dev-actions` can exercise the admin action UI
// without an SSO proxy in front of the server.
type DevAuthenticator struct {
	login string
	token string
}

// NewDevAuthenticator builds dev mode. login is the identity attributed to every
// request; token is the GitHub token used to perform writes.
func NewDevAuthenticator(login, token string) *DevAuthenticator {
	return &DevAuthenticator{login: login, token: token}
}

// Authenticate always returns the configured admin identity.
func (d *DevAuthenticator) Authenticate(_ context.Context, _ *http.Request) (*Identity, error) {
	return &Identity{Login: d.login, Token: d.token}, nil
}
