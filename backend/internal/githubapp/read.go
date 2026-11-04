package githubapp

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// ReadAuth supplies repository-scoped credentials for GitHub reads.
// A nil ReadAuth makes anonymous requests.
type ReadAuth struct {
	app   *Client
	token string
}

// NewReadAuthFromEnv prefers a configured App over the caller's static token.
// Partial or invalid App credentials are errors, never a fallback signal.
func NewReadAuthFromEnv(client *http.Client, fallbackToken string) (*ReadAuth, error) {
	creds, configured := CredentialsFromEnv()
	if !configured {
		return StaticReadAuth(fallbackToken), nil
	}
	app, err := New(client, creds)
	if err != nil {
		return nil, fmt.Errorf("configuring GitHub read authentication: %w", err)
	}
	return &ReadAuth{app: app}, nil
}

// StaticReadAuth uses a fixed token, or anonymous access when it is empty.
func StaticReadAuth(token string) *ReadAuth {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &ReadAuth{token: token}
}

// Configured reports whether requests require authentication.
func (a *ReadAuth) Configured() bool { return a != nil }

// UsesApp reports whether reads use renewable installation tokens.
func (a *ReadAuth) UsesApp() bool { return a != nil && a.app != nil }

// Do authenticates one read, renewing installation tokens before they expire.
// Redirects must stay within the repository, including GitHub archive downloads.
func (a *ReadAuth) Do(client *http.Client, req *http.Request, owner, repo, permission string) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("GitHub read authentication requires GET or HEAD")
	}
	if client == nil {
		client = http.DefaultClient
	}
	if a == nil {
		return client.Do(req)
	}
	if !repositoryPath(req.URL, owner, repo, false) {
		return nil, fmt.Errorf("GitHub read request does not address the selected repository")
	}
	switch permission {
	case "contents", "metadata", "pull_requests":
	default:
		return nil, fmt.Errorf("unsupported GitHub read permission")
	}
	request := req.Clone(req.Context())
	if err := a.authorize(request, owner, repo, permission); err != nil {
		return nil, err
	}
	readClient := *client
	readClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 GitHub read redirects")
		}
		if client.CheckRedirect != nil {
			if err := client.CheckRedirect(next, via); err != nil {
				return err
			}
		}
		sameOrigin := strings.EqualFold(next.URL.Scheme, req.URL.Scheme) && strings.EqualFold(next.URL.Host, req.URL.Host)
		archive := req.URL.Host == "api.github.com" && strings.Contains(req.URL.Path, "/tarball/") &&
			next.URL.Scheme == "https" && next.URL.Host == "codeload.github.com"
		allowed := sameOrigin && repositoryPath(next.URL, owner, repo, false) || archive && repositoryPath(next.URL, owner, repo, true)
		if !allowed {
			return fmt.Errorf("refusing GitHub read redirect outside the selected repository")
		}
		return a.authorize(next, owner, repo, permission)
	}
	return readClient.Do(request)
}

func (a *ReadAuth) authorize(req *http.Request, owner, repo, permission string) error {
	token := a.token
	if a.app != nil {
		var err error
		token, err = a.app.installationToken(req.Context(), owner, repo, permission)
		if err != nil {
			return err
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func repositoryPath(u *url.URL, owner, repo string, archive bool) bool {
	for _, part := range []string{owner, repo} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\%?#") {
			return false
		}
	}
	prefix := "/repos/" + owner + "/" + repo
	if archive {
		prefix = "/" + owner + "/" + repo
	}
	p := strings.ToLower(path.Clean(u.Path))
	prefix = strings.ToLower(prefix)
	return u.User == nil && (p == prefix || strings.HasPrefix(p, prefix+"/"))
}
