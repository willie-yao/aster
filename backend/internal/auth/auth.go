// Package auth guards the server's write actions. It authenticates a request to
// a GitHub user and authorizes that user against an admin allowlist. The
// Authenticator interface is the seam: a per-user PAT implementation ships
// first, and an OAuth flow can replace it later without touching the handlers.
package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
)

// Identity is an authenticated admin. Token is the GitHub token used to perform
// writes as this user, so actions are attributed to a real person.
type Identity struct {
	Login string
	Token string
}

// Errors returned by Authenticate, mapped to HTTP status by Middleware.
var (
	// ErrNoToken means the request carried no credential. -> 401.
	ErrNoToken = errors.New("auth: no token provided")
	// ErrInvalidToken means the credential did not resolve to a user. -> 401.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrNotAdmin means a valid user is not on the admin allowlist. -> 403.
	ErrNotAdmin = errors.New("auth: not an admin")
)

// Authenticator resolves and authorizes a request to an admin Identity.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*Identity, error)
}

// SetPrivateResponseHeaders prevents authenticated responses from being cached.
func SetPrivateResponseHeaders(header http.Header) {
	header.Set("Cache-Control", "private, no-store")
	appendVary(header, "Cookie")
	appendVary(header, "Authorization")
}

func appendVary(header http.Header, value string) {
	for _, current := range header.Values("Vary") {
		for _, item := range strings.Split(current, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

type ctxKey struct{}

// withIdentity returns a context carrying the authenticated identity.
func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom returns the identity stored by Middleware, if any.
func IdentityFrom(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok
}

// Middleware requires an authenticated admin before calling next. On success it
// stores the Identity in the request context for the handler to read. Failures
// return 401 (no or invalid token) or 403 (valid user, not an admin) without
// leaking the token.
func Middleware(a Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetPrivateResponseHeaders(w.Header())
		id, err := a.Authenticate(r.Context(), r)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotAdmin):
				http.Error(w, "forbidden", http.StatusForbidden)
			case errors.Is(err, ErrNoToken), errors.Is(err, ErrInvalidToken):
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			default:
				log.Printf("auth: unexpected authentication failure")
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}
