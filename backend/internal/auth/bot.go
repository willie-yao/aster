package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// BotAuthenticator skips per-user OAuth: an upstream SSO proxy authenticates the
// user and passes their identity in a trusted header, and a single bot token
// performs the GitHub write. Writes are attributed to the bot account.
//
// The trusted header (e.g. X-Auth-Request-Email from oauth2-proxy) is only
// meaningful when the proxy strips any client-supplied copy, so the server must
// not be reachable except through the proxy.
type BotAuthenticator struct {
	header   string
	botToken string
	admins   map[string]struct{}
}

// NewBotAuthenticator builds bot mode. header is the trusted identity header;
// when empty, requests are authorized without an identity check (rely on
// network isolation). admins, when set, restricts which header identities may
// act.
func NewBotAuthenticator(header, botToken string, admins []string) *BotAuthenticator {
	return &BotAuthenticator{
		header:   http.CanonicalHeaderKey(strings.TrimSpace(header)),
		botToken: botToken,
		admins:   adminSet(admins),
	}
}

// Authenticate validates the trusted header against the allowlist and returns
// the bot token as the write credential.
func (b *BotAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	login := "bot"
	if b.header != "" {
		user := strings.TrimSpace(r.Header.Get(b.header))
		if user == "" {
			return nil, ErrNoToken
		}
		if len(b.admins) > 0 {
			if _, ok := b.admins[strings.ToLower(user)]; !ok {
				return nil, fmt.Errorf("%w: %s", ErrNotAdmin, user)
			}
		}
		login = user
	}
	return &Identity{Login: login, Token: b.botToken}, nil
}
