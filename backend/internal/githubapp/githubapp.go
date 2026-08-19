// Package githubapp authenticates as a GitHub App installation. It mints the
// short-lived RS256 JWT the App API requires, resolves the App's bot identity
// and its installation on a repository, and exchanges both for an installation
// access token.
//
// An installation token is scoped to one repository and the App's declared
// permissions and expires within an hour, so it is a much narrower credential
// than a personal access token carrying its creator's full access.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/textutil"
)

// apiBase is the GitHub REST API root, overridable per Client for tests.
const apiBase = "https://api.github.com"

// maxResponseBytes bounds a single GitHub API response body.
const maxResponseBytes = 1 << 20

// jwtLifetime is how long a minted App assertion stays valid. GitHub rejects
// anything above ten minutes.
const jwtLifetime = 9 * time.Minute

// jwtBackdate offsets the issued-at claim so minor clock drift between the
// engine and GitHub cannot produce an assertion that is not yet valid.
const jwtBackdate = 60 * time.Second

// tokenRefreshSkew is how long before expiry a cached installation token is
// discarded, so a token cannot lapse mid-request.
const tokenRefreshSkew = 5 * time.Minute

// Credentials identify the GitHub App the engine posts as.
type Credentials struct {
	// AppID is the App's numeric identifier, as shown on its settings page.
	AppID string
	// PrivateKey is the PEM the App generated, in PKCS#1 or PKCS#8 form.
	PrivateKey string
}

// statusError is a non-success GitHub response.
type statusError struct {
	statusCode int
	status     string
	body       string
}

func (e *statusError) Error() string { return fmt.Sprintf("%s: %s", e.status, e.body) }

// Client authenticates as one GitHub App. It caches the App's identity and
// per-repository installation tokens, so a pass costs one exchange rather than
// one per call. Safe for concurrent use.
type Client struct {
	httpClient *http.Client
	base       string
	appID      string
	key        *rsa.PrivateKey

	mu    sync.Mutex
	login string
	// tokens caches installation tokens keyed by "owner/repo".
	tokens map[string]cachedToken
	// now is a seam for tests.
	now func() time.Time
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// New builds a Client from the App's credentials. A nil httpClient defaults to
// a 30s client.
func New(httpClient *http.Client, creds Credentials) (*Client, error) {
	appID := strings.TrimSpace(creds.AppID)
	if appID == "" {
		return nil, fmt.Errorf("githubapp: app id is required")
	}
	key, err := parsePrivateKey(creds.PrivateKey)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		httpClient: httpClient,
		base:       apiBase,
		appID:      appID,
		key:        key,
		tokens:     map[string]cachedToken{},
		now:        time.Now,
	}, nil
}

// parsePrivateKey decodes the App's PEM. GitHub issues PKCS#1 ("RSA PRIVATE
// KEY"), but PKCS#8 is accepted so a key converted by openssl still works.
func parsePrivateKey(value string) (*rsa.PrivateKey, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("githubapp: private key is required")
	}
	block, _ := pem.Decode([]byte(trimmed))
	if block == nil {
		return nil, fmt.Errorf("githubapp: private key is not PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parsing private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("githubapp: private key is %T, want RSA", parsed)
	}
	return key, nil
}

// mintJWT builds the RS256 assertion the App-level endpoints authenticate with.
func (c *Client) mintJWT() (string, error) {
	now := c.now()
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": c.appID,
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: signing assertion: %w", err)
	}
	return signing + "." + enc.EncodeToString(signature), nil
}

// Login returns the App's bot account name, "{slug}[bot]". This is the author
// GitHub attributes the App's comments to, so callers use it both to report the
// posting identity and to recognize the App's own pull requests.
func (c *Client) Login(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.login
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	var out struct {
		Slug string `json:"slug"`
	}
	if err := c.appRequest(ctx, http.MethodGet, c.base+"/app", nil, &out); err != nil {
		return "", fmt.Errorf("githubapp: resolving app identity: %w", err)
	}
	slug := strings.TrimSpace(out.Slug)
	if slug == "" {
		return "", fmt.Errorf("githubapp: app identity response omitted the slug")
	}
	login := slug + "[bot]"

	c.mu.Lock()
	c.login = login
	c.mu.Unlock()
	return login, nil
}

// InstallationToken returns a token scoped to owner/repo, minting one when no
// unexpired token is cached.
func (c *Client) InstallationToken(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo

	c.mu.Lock()
	cached, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && c.now().Add(tokenRefreshSkew).Before(cached.expiresAt) {
		return cached.token, nil
	}

	id, err := c.installationID(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.base, id)
	// Scope the token to the one repository it is minted for. An installation
	// may cover many repositories, and an unscoped token would carry write
	// access to every one of them.
	body := map[string]any{"repositories": []string{repo}}
	if err := c.appRequest(ctx, http.MethodPost, url, body, &out); err != nil {
		return "", fmt.Errorf("githubapp: minting installation token for %s: %w", key, err)
	}
	token := strings.TrimSpace(out.Token)
	if token == "" {
		return "", fmt.Errorf("githubapp: installation token response for %s was empty", key)
	}
	expiresAt, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		// An unreadable expiry must not be treated as long-lived. Expire the
		// entry immediately so the next call re-mints rather than reusing a
		// token whose lifetime is unknown.
		expiresAt = c.now()
	}

	c.mu.Lock()
	c.tokens[key] = cachedToken{token: token, expiresAt: expiresAt}
	c.mu.Unlock()
	return token, nil
}

// installationID resolves the App's installation on one repository. A 404 means
// the App exists but was never installed there, which is the most common setup
// mistake, so it is reported in those terms.
func (c *Client) installationID(ctx context.Context, owner, repo string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	url := fmt.Sprintf("%s/repos/%s/%s/installation", c.base, owner, repo)
	if err := c.appRequest(ctx, http.MethodGet, url, nil, &out); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.statusCode == http.StatusNotFound {
			return 0, fmt.Errorf("githubapp: app %s is not installed on %s/%s", c.appID, owner, repo)
		}
		return 0, fmt.Errorf("githubapp: resolving installation on %s/%s: %w", owner, repo, err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("githubapp: installation response for %s/%s omitted the id", owner, repo)
	}
	return out.ID, nil
}

// appRequest performs one App-authenticated call, signing a fresh assertion.
func (c *Client) appRequest(ctx context.Context, method, url string, body, out any) error {
	assertion, err := c.mintJWT()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aster")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &statusError{
			statusCode: resp.StatusCode,
			status:     resp.Status,
			body:       textutil.Truncate(string(payload), 300),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}
