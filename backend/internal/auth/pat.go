package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
)

// loginResolver verifies a token and returns the GitHub login it authenticates
// as. It is a seam so tests can avoid a real GitHub call.
type loginResolver func(ctx context.Context, token string) (string, error)

// PATAuthenticator authenticates a request by the GitHub personal access token
// it carries and authorizes it against an admin login allowlist. The token
// performs the eventual GitHub write, so actions are attributed to the admin.
type PATAuthenticator struct {
	admins  map[string]struct{}
	resolve loginResolver
}

// NewPATAuthenticator builds an authenticator whose admins are the given GitHub
// logins (case-insensitive). An empty list fails closed: no one is authorized.
func NewPATAuthenticator(admins []string) *PATAuthenticator {
	return &PATAuthenticator{admins: adminSet(admins), resolve: resolveGitHubLogin}
}

// resolveGitHubLogin verifies the token against GitHub and returns its login.
func resolveGitHubLogin(ctx context.Context, token string) (string, error) {
	return ghpr.NewClient(nil, token).AuthedLogin(ctx)
}

// Authenticate reads the request's bearer token, verifies it against GitHub,
// and checks the resolved login against the admin allowlist.
func (p *PATAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, ErrNoToken
	}
	login, err := p.resolve(ctx, token)
	if err != nil || login == "" {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if _, ok := p.admins[strings.ToLower(login)]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotAdmin, login)
	}
	return &Identity{Login: login, Token: token}, nil
}

// bearerToken extracts a token from the Authorization header, accepting both
// "Bearer <t>" and GitHub's "token <t>" forms.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	for _, prefix := range []string{"Bearer ", "token "} {
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	return ""
}
