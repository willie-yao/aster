package auth

import (
	"context"
	"crypto/subtle"
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
// not be reachable except through the proxy. Set a shared secret so the backend
// can reject requests that did not pass through the proxy even if that network
// isolation is imperfect.
type BotAuthenticator struct {
	header       string
	botToken     string
	admins       map[string]struct{}
	secretHeader string
	secret       string
}

// proxySecretHeader carries the shared secret the proxy injects and the backend
// verifies.
const proxySecretHeader = "X-Proxy-Secret"

// NewBotAuthenticator builds bot mode. header is the trusted identity header and
// admins the allowlist of identities that may act; both are required and this
// authenticator fails closed if either is empty. secret, when non-empty, must be
// presented in the X-Proxy-Secret header on every request.
func NewBotAuthenticator(header, botToken string, admins []string, secret string) *BotAuthenticator {
	return &BotAuthenticator{
		header:       http.CanonicalHeaderKey(strings.TrimSpace(header)),
		botToken:     botToken,
		admins:       adminSet(admins),
		secretHeader: proxySecretHeader,
		secret:       secret,
	}
}

// Authenticate validates the optional shared secret and the trusted identity
// header against the allowlist, then returns the bot token as the write
// credential. It fails closed: a missing identity header, an empty allowlist, or
// an identity not on the allowlist is rejected.
func (b *BotAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	if b.secret != "" {
		got := r.Header.Get(b.secretHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(b.secret)) != 1 {
			return nil, ErrInvalidToken
		}
	}
	if b.header == "" {
		return nil, fmt.Errorf("%w: proxy mode requires a trusted identity header", ErrNoToken)
	}
	user := strings.TrimSpace(r.Header.Get(b.header))
	if user == "" {
		return nil, ErrNoToken
	}
	if len(b.admins) == 0 {
		return nil, fmt.Errorf("%w: no admins configured", ErrNotAdmin)
	}
	if _, ok := b.admins[strings.ToLower(user)]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotAdmin, user)
	}
	return &Identity{Login: user, Token: b.botToken}, nil
}
