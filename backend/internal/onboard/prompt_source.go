package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type promptJobSummary struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	ConfigFile string   `json:"config_file,omitempty"`
	Repo       string   `json:"repository,omitempty"`
	Branches   []string `json:"branches_or_refs,omitempty"`
	Dashboards []string `json:"testgrid_dashboards,omitempty"`
}

type promptDraftInput struct {
	ProjectName          string
	SourceRepo           Repo
	SourceRevision       string
	SourceRevisionStatus string
	Jobs                 []promptJobSummary
}

func defaultBranch(ctx context.Context, client *http.Client, owner, repo, token string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s", githubAPIBaseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving source repository: %s", resp.Status)
	}
	var result struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.DefaultBranch == "" {
		return "", fmt.Errorf("source repository response did not include a default branch")
	}
	return result.DefaultBranch, nil
}

func resolvePromptSourceRevision(ctx context.Context, client *http.Client, owner, repo, branch, token string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", githubAPIBaseURL, owner, repo, url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving source revision: %s", resp.Status)
	}
	revision := strings.TrimSpace(string(body))
	if len(revision) < 7 || strings.IndexFunc(revision, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF", r)
	}) >= 0 {
		return "", fmt.Errorf("source revision response was invalid")
	}
	return revision, nil
}
