package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
)

// GitHub OAuth endpoints, overridable in tests.
var (
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
)

const stateCookieName = "pad_oauth_state"
const returnCookieName = "pad_oauth_return"

func oauthCookie(name, value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// OAuthConfig configures the GitHub OAuth App login flow.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback registered on the OAuth App, e.g.
	// https://dashboard.example.com/api/auth/callback.
	RedirectURL string
	// Scope granted at login. Callers derive it from the repository access policy.
	Scope string
	// Admins is the allowlist of GitHub logins (case-insensitive).
	Admins []string
	// SessionKey seeds the session cookie encryption.
	SessionKey string
	// SecureCookies sets the Secure flag; disable only for local http.
	SecureCookies bool
	// SessionTTL is the login lifetime. Zero defaults to 12h.
	SessionTTL time.Duration
	// HTTPClient is used for the token exchange. Nil uses a 30s client.
	HTTPClient *http.Client
}

// OAuth implements the GitHub OAuth App login flow and a session-cookie
// Authenticator. The admin's own OAuth token performs GitHub writes, so actions
// are attributed to them.
type OAuth struct {
	cfg    OAuthConfig
	codec  *sessionCodec
	admins map[string]struct{}
	client *http.Client
}

// NewOAuth validates config and builds the flow.
func NewOAuth(cfg OAuthConfig) (*OAuth, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("auth: OAuth client id and secret are required")
	}
	if cfg.RedirectURL == "" {
		return nil, fmt.Errorf("auth: OAuth redirect URL is required")
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	if strings.TrimSpace(cfg.Scope) == "" {
		return nil, fmt.Errorf("auth: OAuth scope is required")
	}
	cfg.Scope = strings.TrimSpace(cfg.Scope)
	codec, err := newSessionCodec(cfg.SessionKey, cfg.SecureCookies, ttl, "oauth:"+cfg.Scope)
	if err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuth{cfg: cfg, codec: codec, admins: adminSet(cfg.Admins), client: client}, nil
}

// Authenticate reads the session cookie and returns the admin identity. It is
// the Authenticator seam used by the action middleware.
func (o *OAuth) Authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	s, err := o.codec.read(r)
	if err != nil {
		return nil, ErrNoToken
	}
	if _, ok := o.admins[strings.ToLower(s.Login)]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotAdmin, s.Login)
	}
	return &Identity{Login: s.Login, Token: s.Token}, nil
}

// Register mounts the auth routes on mux.
func (o *OAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/login", o.handleLogin)
	mux.HandleFunc("GET /api/auth/callback", o.handleCallback)
	mux.HandleFunc("POST /api/auth/logout", o.handleLogout)
	mux.HandleFunc("GET /api/auth/user", o.handleUser)
}

// handleLogin sets a random state cookie and redirects to GitHub. It records a
// same-origin return path so the callback can send the admin back to the page
// they started from.
func (o *OAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	state, err := randomState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, oauthCookie(stateCookieName, state, int((10*time.Minute).Seconds()), o.cfg.SecureCookies))
	http.SetCookie(w, oauthCookie(returnCookieName, safeRelativePath(r.URL.Query().Get("redirect")), int((10*time.Minute).Seconds()), o.cfg.SecureCookies))
	q := url.Values{
		"client_id":    {o.cfg.ClientID},
		"redirect_uri": {o.cfg.RedirectURL},
		"scope":        {o.cfg.Scope},
		"state":        {state},
		"allow_signup": {"false"},
	}
	http.Redirect(w, r, githubAuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// handleCallback verifies state, exchanges the code, checks the allowlist, and
// establishes the session.
func (o *OAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	// The state cookie is single-use.
	http.SetCookie(w, oauthCookie(stateCookieName, "", -1, o.cfg.SecureCookies))

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	token, grantedScope, err := o.exchange(r.Context(), code)
	if err != nil {
		log.Printf("auth: token exchange failed")
		http.Error(w, "oauth authorization did not grant the configured access", http.StatusBadGateway)
		return
	}
	if err := validateGrantedScope(o.cfg.Scope, grantedScope); err != nil {
		log.Printf("auth: OAuth scope validation failed: %v", err)
		http.Error(w, "oauth authorization did not grant the configured access", http.StatusForbidden)
		return
	}
	login, err := ghpr.NewClient(nil, token).AuthedLogin(r.Context())
	if err != nil || login == "" {
		http.Error(w, "could not resolve user", http.StatusBadGateway)
		return
	}
	if _, ok := o.admins[strings.ToLower(login)]; !ok {
		http.Error(w, "not an admin", http.StatusForbidden)
		return
	}
	if err := o.codec.write(w, login, token); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Send the admin back to the page they signed in from, defaulting to the
	// home page. The path is validated to be same-origin.
	dest := "/"
	if rc, err := r.Cookie(returnCookieName); err == nil {
		dest = safeRelativePath(rc.Value)
	}
	http.SetCookie(w, oauthCookie(returnCookieName, "", -1, o.cfg.SecureCookies))
	http.Redirect(w, r, dest, http.StatusFound)
}

// handleLogout clears the session.
func (o *OAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	o.codec.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleUser reports the signed-in admin, for the frontend to gate its UI.
func (o *OAuth) handleUser(w http.ResponseWriter, r *http.Request) {
	SetPrivateResponseHeaders(w.Header())
	id, err := o.Authenticate(r.Context(), r)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"login": id.Login})
}

// exchange trades an authorization code for a user access token.
func (o *OAuth) exchange(ctx context.Context, code string) (string, string, error) {
	form := url.Values{
		"client_id":     {o.cfg.ClientID},
		"client_secret": {o.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {o.cfg.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	if out.AccessToken == "" {
		return "", "", fmt.Errorf("no access token (%s)", out.Error)
	}
	return out.AccessToken, out.Scope, nil
}

func validateGrantedScope(expected, granted string) error {
	grants := map[string]bool{}
	for _, value := range strings.FieldsFunc(granted, func(r rune) bool { return r == ',' || r == ' ' }) {
		grants[strings.TrimSpace(value)] = true
	}
	if len(grants) != 1 || !grants[expected] {
		return fmt.Errorf("granted scopes do not exactly match %q", expected)
	}
	return nil
}

// randomState returns a URL-safe random string for CSRF protection.
func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// safeRelativePath returns p only if it is a same-origin absolute path (a single
// leading "/"), otherwise "/". This blocks open redirects to external or
// protocol-relative destinations after login.
func safeRelativePath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/"
	}
	// Reject protocol-relative ("//host") and backslash tricks ("/\\host").
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

// adminSet lowercases and indexes the allowlist.
func adminSet(admins []string) map[string]struct{} {
	set := make(map[string]struct{}, len(admins))
	for _, a := range admins {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" {
			set[a] = struct{}{}
		}
	}
	return set
}
