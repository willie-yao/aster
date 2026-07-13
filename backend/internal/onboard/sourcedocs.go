package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// Doc bounds for grounding generation: keep the prompt budget sane while still
// giving the model real project context.
const (
	maxDocFiles     = 6
	maxDocFileBytes = 20_000
	maxDocTotal     = 80_000
	// maxDocAttempts caps ranked candidates so large repos do not cause long fetch
	// sequences.
	maxDocAttempts = 18
)

// sourceDoc is one fetched markdown doc from the source repo.
type sourceDoc struct {
	Path string
	Text string
}

// fetchSourceDocs pulls bounded markdown docs from the source repo to ground
// prompt generation. An empty result is not an error.
func fetchSourceDocs(ctx context.Context, client *http.Client, owner, repo, token string) ([]sourceDoc, error) {
	branch, err := defaultBranch(ctx, client, owner, repo, token)
	if err != nil {
		return nil, err
	}
	paths, err := listMarkdownPaths(ctx, client, owner, repo, branch, token)
	if err != nil {
		return nil, err
	}
	ranked := rankDocPaths(paths)

	var docs []sourceDoc
	total := 0
	attempts := 0
	for _, p := range ranked {
		if len(docs) >= maxDocFiles || total >= maxDocTotal || attempts >= maxDocAttempts {
			break
		}
		attempts++
		text, err := fetchRaw(ctx, client, owner, repo, branch, p, token)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		// Trim the last file to stay within the total budget.
		remaining := maxDocTotal - total
		if len(text) > remaining {
			text = text[:remaining] + "\n…(truncated)"
		}
		if len(text) > maxDocFileBytes {
			text = text[:maxDocFileBytes] + "\n…(truncated)"
		}
		docs = append(docs, sourceDoc{Path: p, Text: text})
		total += len(text)
	}
	return docs, nil
}

// defaultBranch resolves the repo's default branch via the GitHub API.
func defaultBranch(ctx context.Context, client *http.Client, owner, repo, token string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := ghJSON(ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo), token, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "main", nil
	}
	return out.DefaultBranch, nil
}

// listMarkdownPaths returns markdown file paths from one recursive git tree.
func listMarkdownPaths(ctx context.Context, client *http.Client, owner, repo, branch, token string) ([]string, error) {
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, branch)
	if err := ghJSON(ctx, client, url, token, &out); err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range out.Tree {
		if e.Type == "blob" && strings.EqualFold(path.Ext(e.Path), ".md") && !excludedDocDir(e.Path) {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}

// excludedDocDir filters out vendored, generated, and meta markdown.
func excludedDocDir(p string) bool {
	lp := strings.ToLower(p)
	for _, prefix := range []string{"vendor/", "third_party/", "thirdparty/", ".github/", "node_modules/"} {
		if strings.HasPrefix(lp, prefix) {
			return true
		}
	}
	return false
}

// rankDocPaths orders markdown paths by likely usefulness for a CI-failure
// prompt: README first, then top-level docs, preferring architecture/design/
// testing/contributing material, and shallower paths over deep ones.
func rankDocPaths(paths []string) []string {
	score := func(p string) int {
		lp := strings.ToLower(p)
		base := strings.ToLower(path.Base(p))
		s := 0
		switch {
		case lp == "readme.md": // root README only
			s += 100
		case base == "readme.md": // a nested README is still useful, just less
			s += 15
		case strings.HasPrefix(lp, "docs/"):
			s += 50
		}
		for kw, w := range map[string]int{
			"architect": 40, "design": 30, "test": 25, "e2e": 25,
			"contribut": 20, "troubleshoot": 30, "debug": 25, "flake": 35,
		} {
			if strings.Contains(lp, kw) {
				s += w
			}
		}
		// Prefer shallower files.
		s -= strings.Count(p, "/") * 5
		return s
	}
	sort.SliceStable(paths, func(i, j int) bool {
		si, sj := score(paths[i]), score(paths[j])
		if si != sj {
			return si > sj
		}
		return paths[i] < paths[j]
	})
	return paths
}

// fetchRaw fetches a file's raw content from raw.githubusercontent.com.
func fetchRaw(ctx context.Context, client *http.Client, owner, repo, branch, p, token string) (string, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, branch, p)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("raw %s: %s", p, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocFileBytes+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ghJSON does a GET against the GitHub API and decodes the JSON body.
func ghJSON(ctx context.Context, client *http.Client, url, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, textutil.Truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}
