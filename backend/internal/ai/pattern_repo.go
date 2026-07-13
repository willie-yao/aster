package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// githubRepoReader is a bound tools.RepoReader over one GitHub repo at a ref.
// It backs the pattern agent's repotree tools so the correlation model can grep
// and read real source files before naming a fix target.
//
// ListTree uses the recursive git-trees API (one call, memoized by the tool
// Cache), so it needs a token to clear GitHub's 60/hr anonymous limit on a busy
// run. ReadFile uses the raw.githubusercontent.com CDN, which serves public
// files without a token or API-rate limit, so file reads never spend the token
// budget.
type githubRepoReader struct {
	owner  string
	repo   string
	ref    string
	token  string
	client *http.Client
}

// NewGitHubRepoReader binds a reader to owner/repo at ref. Empty ref means the
// default branch (HEAD). Empty token falls back to anonymous access. The
// returned reader grounds the pattern agent's repotree tools.
func NewGitHubRepoReader(owner, repo, ref, token string) tools.RepoReader {
	if ref == "" {
		ref = "HEAD"
	}
	return &githubRepoReader{
		owner:  owner,
		repo:   repo,
		ref:    ref,
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// ListTree returns the repo's blob paths at the bound ref via the recursive
// git-trees API.
func (r *githubRepoReader) ListTree(ctx context.Context) ([]string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1",
		r.owner, r.repo, url.PathEscape(r.ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s/%s tree: %s", r.owner, r.repo, resp.Status)
	}
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("decoding %s/%s tree: %w", r.owner, r.repo, err)
	}
	paths := make([]string, 0, len(out.Tree))
	for _, e := range out.Tree {
		if e.Type == "blob" && e.Path != "" {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}

// ReadFile returns a file's content at the bound ref via the raw CDN. found is
// false (no error) when the file does not exist. The CDN serves public repos
// only; on a private source repo reads 404 (the tree listing still works with a
// token), so grounding assumes a public repository.
func (r *githubRepoReader) ReadFile(ctx context.Context, path string) (string, bool, error) {
	escaped := strings.Join(mapSegments(strings.Split(path, "/"), url.PathEscape), "/")
	u := fmt.Sprintf("%s/%s/%s/%s/%s", rawContentBase, r.owner, r.repo, url.PathEscape(r.ref), escaped)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("reading %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	return string(body), true, nil
}

// mapSegments applies f to each element, used to escape path segments while
// preserving the slashes between them.
func mapSegments(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}
