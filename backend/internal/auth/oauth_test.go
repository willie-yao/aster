package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTokenEndpoint points the OAuth token exchange at a local stub.
func stubTokenEndpoint(t *testing.T) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"user-tok"}`))
	}))
	orig := githubTokenURL
	githubTokenURL = srv.URL + "/login/oauth/access_token"
	return func() {
		githubTokenURL = orig
		srv.Close()
	}
}

func testOAuth(t *testing.T, admins []string) *OAuth {
	t.Helper()
	o, err := NewOAuth(OAuthConfig{
		ClientID:      "cid",
		ClientSecret:  "secret",
		RedirectURL:   "http://localhost/api/auth/callback",
		Admins:        admins,
		SessionKey:    "k",
		SecureCookies: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOAuth_LoginSetsStateAndRedirects(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	rec := httptest.NewRecorder()
	o.handleLogin(rec, httptest.NewRequest("GET", "/api/auth/login", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.Contains(loc, "client_id=cid") || !strings.Contains(loc, "state=") {
		t.Errorf("authorize URL missing params: %s", loc)
	}
	var hasState bool
	for _, c := range res.Cookies() {
		if c.Name == stateCookieName && c.Value != "" {
			hasState = true
		}
	}
	if !hasState {
		t.Error("login must set a state cookie")
	}
}

func TestOAuth_CallbackRejectsBadState(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	req := httptest.NewRequest("GET", "/api/auth/callback?state=evil&code=x", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "real"})
	rec := httptest.NewRecorder()
	o.handleCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for state mismatch", rec.Code)
	}
}

func TestOAuth_Exchange(t *testing.T) {
	cleanup := stubTokenEndpoint(t)
	defer cleanup()
	o := testOAuth(t, []string{"alice"})
	tok, err := o.exchange(context.Background(), "code123")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "user-tok" {
		t.Errorf("token = %q", tok)
	}
}

func TestOAuth_AuthenticateSessionAndAllowlist(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	// Admin session authenticates.
	rec := httptest.NewRecorder()
	if err := o.codec.write(rec, "alice", "user-tok"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/auth/user", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	id, err := o.Authenticate(context.Background(), req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Login != "alice" || id.Token != "user-tok" {
		t.Errorf("identity = %+v", id)
	}

	// A non-admin session is rejected even if the cookie is valid.
	rec2 := httptest.NewRecorder()
	o.codec.write(rec2, "mallory", "user-tok")
	req2 := httptest.NewRequest("GET", "/api/auth/user", nil)
	for _, c := range rec2.Result().Cookies() {
		req2.AddCookie(c)
	}
	if _, err := o.Authenticate(context.Background(), req2); err == nil {
		t.Error("non-admin session must be rejected")
	}

	// No cookie -> ErrNoToken.
	if _, err := o.Authenticate(context.Background(), httptest.NewRequest("GET", "/", nil)); err != ErrNoToken {
		t.Errorf("no cookie err = %v, want ErrNoToken", err)
	}
}

func TestOAuth_MissingConfig(t *testing.T) {
	if _, err := NewOAuth(OAuthConfig{SessionKey: "k", RedirectURL: "x"}); err == nil {
		t.Error("expected error without client id/secret")
	}
}

func TestBot_TrustedHeaderAllowlist(t *testing.T) {
	b := NewBotAuthenticator("X-Auth-Request-Email", "bot-tok", []string{"alice"})
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Auth-Request-Email", "alice")
	id, err := b.Authenticate(context.Background(), req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Token != "bot-tok" || id.Login != "alice" {
		t.Errorf("identity = %+v", id)
	}

	// Missing header -> ErrNoToken.
	if _, err := b.Authenticate(context.Background(), httptest.NewRequest("POST", "/", nil)); err != ErrNoToken {
		t.Errorf("missing header err = %v", err)
	}
	// Non-admin header -> ErrNotAdmin.
	req2 := httptest.NewRequest("POST", "/", nil)
	req2.Header.Set("X-Auth-Request-Email", "mallory")
	if _, err := b.Authenticate(context.Background(), req2); err == nil {
		t.Error("non-admin header must be rejected")
	}
}

func TestBot_NoHeaderNoAllowlist(t *testing.T) {
	// No trusted header configured: authorize with the bot token (network-isolated
	// deployment).
	b := NewBotAuthenticator("", "bot-tok", nil)
	id, err := b.Authenticate(context.Background(), httptest.NewRequest("POST", "/", nil))
	if err != nil || id.Token != "bot-tok" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}

func TestSafeRelativePath(t *testing.T) {
	cases := map[string]string{
		"/job/x":           "/job/x",
		"/job/x?run=1":     "/job/x?run=1",
		"":                 "/",
		"relative":         "/",
		"//evil.com":       "/",
		`/\evil.com`:       "/",
		"https://evil.com": "/",
		"/path#frag":       "/path#frag",
	}
	for in, want := range cases {
		if got := safeRelativePath(in); got != want {
			t.Errorf("safeRelativePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOAuth_LoginRecordsReturnPath(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	rec := httptest.NewRecorder()
	o.handleLogin(rec, httptest.NewRequest("GET", "/api/auth/login?redirect=%2Fjob%2Fx", nil))
	var ret string
	for _, c := range rec.Result().Cookies() {
		if c.Name == returnCookieName {
			ret = c.Value
		}
	}
	if ret != "/job/x" {
		t.Errorf("return cookie = %q, want /job/x", ret)
	}
}

func TestOAuth_LoginRejectsExternalReturn(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	rec := httptest.NewRecorder()
	o.handleLogin(rec, httptest.NewRequest("GET", "/api/auth/login?redirect=https%3A%2F%2Fevil.com", nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == returnCookieName && c.Value != "/" {
			t.Errorf("external return not sanitized: %q", c.Value)
		}
	}
}
