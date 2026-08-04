package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var githubAPIBaseURL = "https://api.github.com"

var (
	repoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	scpGitHubPattern = regexp.MustCompile(`(?i)^(?:[^@]+@)?github\.com:([^/]+)/(.+)$`)
)

var errNoGitOrigin = errors.New("current checkout has no GitHub origin remote")

// NormalizeGitHubRepo accepts owner/name and common GitHub remote URL forms.
func NormalizeGitHubRepo(input string) (Repo, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Repo{}, fmt.Errorf("repository is required")
	}
	if match := scpGitHubPattern.FindStringSubmatch(raw); match != nil {
		return repoFromRemoteParts(match[1], match[2])
	}
	if !strings.Contains(raw, "://") {
		if strings.Count(raw, "/") != 1 {
			return Repo{}, fmt.Errorf("GitHub repository must be owner/name, got %q", raw)
		}
		owner, name, _ := strings.Cut(raw, "/")
		return repoFromParts(owner, name)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Repo{}, fmt.Errorf("invalid GitHub repository %q", raw)
	}
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return Repo{}, fmt.Errorf("repository host %q is not github.com", u.Hostname())
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Repo{}, fmt.Errorf("GitHub repository URL must not contain a query or fragment")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return Repo{}, fmt.Errorf("GitHub repository must identify owner/name, got %q", raw)
	}
	return repoFromRemoteParts(parts[0], parts[1])
}

func repoFromRemoteParts(owner, name string) (Repo, error) {
	return repoFromParts(owner, strings.TrimSuffix(strings.TrimSpace(name), ".git"))
}

func repoFromParts(owner, name string) (Repo, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if !repoOwnerPattern.MatchString(owner) || !repoNamePattern.MatchString(name) || len(name) > 100 || name == "." || name == ".." {
		return Repo{}, fmt.Errorf("GitHub repository must be owner/name, got %q", strings.Trim(owner+"/"+name, "/"))
	}
	return Repo{Owner: owner, Name: name, FullName: owner + "/" + name}, nil
}

type gitRemoteDetector struct{}

func (gitRemoteDetector) Root(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("detecting Git checkout root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git checkout root is empty")
	}
	return filepath.Clean(root), nil
}

func (gitRemoteDetector) Origin(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return "", errNoGitOrigin
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return "", errNoGitOrigin
	}
	if _, err := NormalizeGitHubRepo(remote); err != nil {
		return "", fmt.Errorf("origin remote is not a supported GitHub repository: %w", err)
	}
	return remote, nil
}

type githubRepositoryClient struct {
	client *http.Client
}

func (c githubRepositoryClient) AuthenticatedLogin(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBaseURL+"/user", nil)
	if err != nil {
		return "", err
	}
	setGitHubRequestHeaders(req, token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading authenticated GitHub login: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading authenticated GitHub login: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authenticated GitHub user request returned HTTP %d", resp.StatusCode)
	}
	var response struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decoding authenticated GitHub login: %w", err)
	}
	if !repoOwnerPattern.MatchString(response.Login) {
		return "", fmt.Errorf("authenticated GitHub user response omitted a valid login")
	}
	return response.Login, nil
}

func setGitHubRequestHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (c githubRepositoryClient) Repository(ctx context.Context, repo Repo, token string) (RepositoryMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBaseURL+"/repos/"+repo.FullName, nil)
	if err != nil {
		return RepositoryMetadata{}, err
	}
	setGitHubRequestHeaders(req, token)
	resp, err := c.client.Do(req)
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("reading GitHub metadata for %s: %w", repo.FullName, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("reading GitHub metadata for %s: %w", repo.FullName, err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && token == "" {
			return RepositoryMetadata{}, fmt.Errorf("GitHub repository %s was not found or is private; set GITHUB_TOKEN for private repository metadata", repo.FullName)
		}
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return RepositoryMetadata{}, fmt.Errorf("GitHub API rate limit exceeded while reading %s; set GITHUB_TOKEN or retry after the rate limit resets", repo.FullName)
		}
		return RepositoryMetadata{}, fmt.Errorf("GitHub metadata request for %s returned HTTP %d", repo.FullName, resp.StatusCode)
	}
	type repositoryRef struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	var response struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		Visibility    string `json:"visibility"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
		Parent *repositoryRef `json:"parent"`
		Source *repositoryRef `json:"source"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return RepositoryMetadata{}, fmt.Errorf("decoding GitHub metadata for %s: %w", repo.FullName, err)
	}
	resolved, err := repoFromParts(response.Owner.Login, response.Name)
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("GitHub metadata for %s omitted a valid repository identity", repo.FullName)
	}
	resolved.Branch = response.DefaultBranch
	resolved.Visibility = response.Visibility
	if resolved.Visibility == "" {
		if response.Private {
			resolved.Visibility = "private"
		} else {
			resolved.Visibility = "public"
		}
	}
	var upstream *Repo
	ref := response.Source
	if ref == nil {
		ref = response.Parent
	}
	if ref != nil {
		if value, err := repoFromParts(ref.Owner.Login, ref.Name); err == nil {
			upstream = &value
		}
	}
	return RepositoryMetadata{Repo: resolved, Private: response.Private, Upstream: upstream}, nil
}
