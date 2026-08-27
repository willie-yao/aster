package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mockServer(t *testing.T, login string) (*MockAuthenticator, http.Handler) {
	t.Helper()
	authenticator, err := NewMockAuthenticator(login)
	if err != nil {
		t.Fatalf("NewMockAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	authenticator.Register(mux)
	return authenticator, mux
}

func TestMockAuthenticatorSignsInAndOut(t *testing.T) {
	authenticator, mux := mockServer(t, "mock-admin")

	// Nothing is authenticated until the operator signs in, so the frontend can
	// render the anonymous state the deployed dashboard has.
	anonymous := httptest.NewRecorder()
	mux.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/auth/user", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("user before sign-in = %d, want 401", anonymous.Code)
	}

	login := httptest.NewRecorder()
	mux.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/api/auth/login?redirect=%2Fjobs", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login = %d, want 302", login.Code)
	}
	if got := login.Header().Get("Location"); got != "/jobs" {
		t.Fatalf("login redirect = %q, want /jobs", got)
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no session cookie")
	}

	authed := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	for _, cookie := range cookies {
		authed.AddCookie(cookie)
	}
	user := httptest.NewRecorder()
	mux.ServeHTTP(user, authed)
	if user.Code != http.StatusOK {
		t.Fatalf("user after sign-in = %d, want 200", user.Code)
	}
	identity, err := authenticator.Authenticate(context.Background(), authed)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Login != "mock-admin" {
		t.Fatalf("identity login = %q, want mock-admin", identity.Login)
	}
	if identity.Token != "" {
		t.Fatal("mock identity carries a write credential")
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	for _, cookie := range cookies {
		logoutRequest.AddCookie(cookie)
	}
	logout := httptest.NewRecorder()
	mux.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", logout.Code)
	}
	cleared := logout.Result().Cookies()
	if len(cleared) == 0 || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout did not expire the session cookie: %+v", cleared)
	}
}

func TestMockAuthenticatorRejectsOffSiteRedirect(t *testing.T) {
	_, mux := mockServer(t, "mock-admin")

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/auth/login?redirect=https%3A%2F%2Fexample.com", nil))
	if got := recorder.Header().Get("Location"); got != "/" {
		t.Fatalf("off-site redirect = %q, want /", got)
	}
}

func TestMockAuthenticatorRejectsForeignSession(t *testing.T) {
	authenticator, _ := mockServer(t, "mock-admin")

	request := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-sealed-session"})
	if _, err := authenticator.Authenticate(context.Background(), request); err != ErrNoToken {
		t.Fatalf("Authenticate with a tampered cookie = %v, want ErrNoToken", err)
	}
}
