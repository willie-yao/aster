package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubTokenEndpoint points the OAuth token exchange at a local stub.
func stubTokenEndpoint(t *testing.T) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"user-tok","scope":"read:user"}`))
	}))
	orig := githubTokenURL
	githubTokenURL = srv.URL + "/login/oauth/access_token"
	return func() {
		githubTokenURL = orig
		srv.Close()
	}
}

func testOAuth(t *testing.T, admins []string) *OAuth {
	return testOAuthWithSecureCookies(t, admins, false)
}

func testOAuthWithSecureCookies(t *testing.T, admins []string, secure bool) *OAuth {
	return testOAuthWithWriteToken(t, admins, secure, "")
}

func testOAuthWithWriteToken(t *testing.T, admins []string, secure bool, writeToken string) *OAuth {
	t.Helper()
	o, err := NewOAuth(OAuthConfig{
		ClientID:      "cid",
		ClientSecret:  "secret",
		RedirectURL:   "http://localhost/api/auth/callback",
		Scope:         "read:user",
		WriteToken:    writeToken,
		Admins:        admins,
		SessionKey:    "k",
		SecureCookies: secure,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOAuth_SecureTemporaryCookies(t *testing.T) {
	o := testOAuthWithSecureCookies(t, []string{"alice"}, true)
	rec := httptest.NewRecorder()
	o.handleLogin(rec, httptest.NewRequest("GET", "/api/auth/login?redirect=%2Fjob%2Fx", nil))

	var state string
	seen := map[string]bool{}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != stateCookieName && cookie.Name != returnCookieName {
			continue
		}
		seen[cookie.Name] = true
		if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s attributes = Secure:%t HttpOnly:%t SameSite:%v", cookie.Name, cookie.Secure, cookie.HttpOnly, cookie.SameSite)
		}
		if cookie.Name == stateCookieName {
			state = cookie.Value
		}
	}
	if !seen[stateCookieName] || !seen[returnCookieName] {
		t.Fatalf("temporary cookies = %v, want state and return", seen)
	}

	callback := httptest.NewRequest("GET", "/api/auth/callback?state="+state, nil)
	callback.AddCookie(&http.Cookie{Name: stateCookieName, Value: state})
	clearRec := httptest.NewRecorder()
	o.handleCallback(clearRec, callback)
	var cleared bool
	for _, cookie := range clearRec.Result().Cookies() {
		if cookie.Name == stateCookieName {
			cleared = true
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != -1 {
				t.Errorf("cleared state attributes = Secure:%t HttpOnly:%t SameSite:%v MaxAge:%d", cookie.Secure, cookie.HttpOnly, cookie.SameSite, cookie.MaxAge)
			}
		}
	}
	if !cleared {
		t.Fatal("callback did not clear the state cookie")
	}
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
	tok, scope, err := o.exchange(context.Background(), "code123")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "user-tok" || scope != "read:user" {
		t.Errorf("token = %q scope = %q", tok, scope)
	}
}

func TestOAuth_AuthenticateSessionAndAllowlist(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	// Admin session authenticates.
	rec := httptest.NewRecorder()
	if err := o.codec.write(rec, "alice"); err != nil {
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
	if id.Login != "alice" || id.Token != "" {
		t.Errorf("identity = %+v", id)
	}

	// The OAuth token in a legacy session is ignored in favor of the configured
	// server-side bot credential.
	botOAuth := testOAuthWithWriteToken(t, []string{"alice"}, false, "bot-token")
	botRequest := httptest.NewRequest("GET", "/api/auth/user", nil)
	botRequest.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: sealLegacySession(t, botOAuth.codec, "alice", "legacy-user-token", time.Now().Add(time.Hour).Unix()),
	})
	botIdentity, err := botOAuth.Authenticate(context.Background(), botRequest)
	if err != nil {
		t.Fatal(err)
	}
	if botIdentity.Login != "alice" || botIdentity.Token != "bot-token" {
		t.Errorf("bot identity = %+v", botIdentity)
	}

	// A non-admin session is rejected even if the cookie is valid.
	rec2 := httptest.NewRecorder()
	o.codec.write(rec2, "mallory")
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
	b := NewBotAuthenticator("X-Auth-Request-Email", "bot-tok", []string{"alice"}, "")
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

func TestBot_FailsClosed(t *testing.T) {
	b := NewBotAuthenticator("", "bot-tok", nil, "")
	if _, err := b.Authenticate(context.Background(), httptest.NewRequest("POST", "/", nil)); err == nil {
		t.Error("empty header must fail closed")
	}

	// Header set but no admins configured -> reject.
	b = NewBotAuthenticator("X-Auth-Request-Email", "bot-tok", nil, "")
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Auth-Request-Email", "alice")
	if _, err := b.Authenticate(context.Background(), req); err == nil {
		t.Error("empty allowlist must fail closed")
	}
}

func TestBot_SharedSecret(t *testing.T) {
	b := NewBotAuthenticator("X-Auth-Request-Email", "bot-tok", []string{"alice"}, "s3cret")

	// Correct identity but missing/wrong secret -> reject.
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Auth-Request-Email", "alice")
	if _, err := b.Authenticate(context.Background(), req); err == nil {
		t.Error("missing proxy secret must be rejected")
	}
	req.Header.Set("X-Proxy-Secret", "wrong")
	if _, err := b.Authenticate(context.Background(), req); err == nil {
		t.Error("wrong proxy secret must be rejected")
	}

	// Correct secret + admin identity -> authorized.
	req.Header.Set("X-Proxy-Secret", "s3cret")
	id, err := b.Authenticate(context.Background(), req)
	if err != nil || id.Login != "alice" || id.Token != "bot-tok" {
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

func TestValidateGrantedScope(t *testing.T) {
	if err := validateGrantedScope("read:user", "read:user"); err != nil {
		t.Fatal(err)
	}
	if err := validateGrantedScope("read:user", "public_repo"); err == nil {
		t.Fatal("repository grant was accepted for identity-only policy")
	}
	if err := validateGrantedScope("read:user", "read:user,gist"); err == nil {
		t.Fatal("unrelated retained grant was accepted")
	}
}

func TestOAuthRejectsSessionFromDifferentPolicy(t *testing.T) {
	old, err := newSessionCodec("k", false, time.Hour, "oauth:public_repo")
	if err != nil {
		t.Fatal(err)
	}
	current, err := newSessionCodec("k", false, time.Hour, "oauth:read:user")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := old.write(recorder, "alice"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range recorder.Result().Cookies() {
		request.AddCookie(cookie)
	}
	if _, err := current.read(request); err == nil {
		t.Fatal("session survived OAuth policy change")
	}
}

func TestOAuthUserUsesPrivateCacheHeaders(t *testing.T) {
	o := testOAuth(t, []string{"alice"})
	for _, authenticated := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
		if authenticated {
			cookieRecorder := httptest.NewRecorder()
			if err := o.codec.write(cookieRecorder, "alice"); err != nil {
				t.Fatal(err)
			}
			for _, cookie := range cookieRecorder.Result().Cookies() {
				request.AddCookie(cookie)
			}
		}
		o.handleUser(recorder, request)
		if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("authenticated=%t Cache-Control=%q", authenticated, got)
		}
		vary := strings.Join(recorder.Header().Values("Vary"), ",")
		if !strings.Contains(vary, "Cookie") || !strings.Contains(vary, "Authorization") {
			t.Fatalf("authenticated=%t Vary=%q", authenticated, vary)
		}
	}
}

func TestOAuthMissingScope(t *testing.T) {
	_, err := NewOAuth(OAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURL: "https://example.test/callback", SessionKey: "key"})
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("missing scope error = %v", err)
	}
}

// stubTokenEndpointWith points the exchange at a stub returning body.
func stubTokenEndpointWith(t *testing.T, body string) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	orig := githubTokenURL
	githubTokenURL = srv.URL + "/login/oauth/access_token"
	return func() {
		githubTokenURL = orig
		srv.Close()
	}
}

// callbackWithState drives handleCallback with a matching state cookie.
func callbackWithState(o *OAuth) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "s"})
	rec := httptest.NewRecorder()
	o.handleCallback(rec, req)
	return rec
}

// A failed token exchange and a refused scope are different operator problems:
// the first is a bad credential or unreachable endpoint, the second is a
// GitHub-side grant. They must not report the same status or message.
func TestOAuthCallbackDistinguishesExchangeFromScopeFailure(t *testing.T) {
	o := testOAuth(t, []string{"alice"})

	cleanup := stubTokenEndpointWith(t, `{"error":"incorrect_client_credentials"}`)
	exchange := callbackWithState(o)
	cleanup()
	if exchange.Code != http.StatusBadGateway {
		t.Errorf("exchange failure status = %d, want %d", exchange.Code, http.StatusBadGateway)
	}

	cleanup = stubTokenEndpointWith(t, `{"access_token":"t","scope":"read:user,repo"}`)
	scope := callbackWithState(o)
	cleanup()
	if scope.Code != http.StatusForbidden {
		t.Errorf("scope failure status = %d, want %d", scope.Code, http.StatusForbidden)
	}

	if strings.TrimSpace(exchange.Body.String()) == strings.TrimSpace(scope.Body.String()) {
		t.Errorf("both failures report the same message %q", strings.TrimSpace(exchange.Body.String()))
	}
}

// The exchange error must name GitHub's reason; it is the only thing that makes
// a bad client secret diagnosable from a log line.
func TestOAuthExchangeErrorNamesUpstreamReason(t *testing.T) {
	cleanup := stubTokenEndpointWith(t, `{"error":"incorrect_client_credentials"}`)
	defer cleanup()
	o := testOAuth(t, []string{"alice"})
	_, _, err := o.exchange(context.Background(), "code123")
	if err == nil || !strings.Contains(err.Error(), "incorrect_client_credentials") {
		t.Fatalf("exchange error = %v, want it to name incorrect_client_credentials", err)
	}
}
