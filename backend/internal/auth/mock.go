package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// mockSessionKey seals mock sessions. It is a fixed constant so a session
// survives a server restart during frontend iteration. Every value this
// authenticator protects is fabricated, so the key guards nothing.
const mockSessionKey = "aster-mock-server-session-key-not-a-secret"

// mockSessionTTL bounds a mock session.
const mockSessionTTL = 12 * time.Hour

// MockAuthenticator reproduces the OAuth sign-in surface without GitHub. Login
// establishes the session immediately and redirects back, so the frontend can
// exercise the anonymous and authenticated states the deployed site has. It
// authenticates whoever asks and carries no write credential, so it must only
// ever back the local mock server.
type MockAuthenticator struct {
	login string
	codec *sessionCodec
}

// NewMockAuthenticator builds mock mode signing in as login.
func NewMockAuthenticator(login string) (*MockAuthenticator, error) {
	codec, err := newSessionCodec(mockSessionKey, false, mockSessionTTL)
	if err != nil {
		return nil, err
	}
	return &MockAuthenticator{login: login, codec: codec}, nil
}

// Authenticate returns the mock admin when a session cookie is present.
func (m *MockAuthenticator) Authenticate(_ context.Context, r *http.Request) (*Identity, error) {
	s, err := m.codec.read(r)
	if err != nil {
		return nil, ErrNoToken
	}
	login := strings.TrimSpace(s.Login)
	if login == "" {
		return nil, ErrInvalidToken
	}
	return &Identity{Login: login}, nil
}

// Register mounts the same auth routes the OAuth authenticator serves.
func (m *MockAuthenticator) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/login", m.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", m.handleLogout)
	mux.HandleFunc("GET /api/auth/user", m.handleUser)
}

// handleLogin signs in and returns to the page the request came from.
func (m *MockAuthenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	if err := m.codec.write(w, m.login, ""); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, safeRelativePath(r.URL.Query().Get("redirect")), http.StatusFound)
}

// handleLogout clears the session.
func (m *MockAuthenticator) handleLogout(w http.ResponseWriter, _ *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	m.codec.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleUser reports the signed-in admin, for the frontend to gate its UI.
func (m *MockAuthenticator) handleUser(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	id, err := m.Authenticate(r.Context(), r)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"login": id.Login})
}
