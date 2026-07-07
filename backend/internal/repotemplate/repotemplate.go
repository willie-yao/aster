// Package repotemplate fetches a GitHub repo's issue and pull-request markdown
// templates and, with an AI completer, reformats a generated body to follow
// them. It is best-effort: when no template exists, no completer is available,
// or a call fails, callers fall back to their default body unchanged. GitHub
// YAML issue forms (.github/ISSUE_TEMPLATE/*.yml) are intentionally ignored;
// only markdown templates are used.
package repotemplate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Template is one issue template: a display name plus its markdown body with
// front-matter removed.
type Template struct {
	Name string
	Body string
}

// Fetcher reads templates from a GitHub repo over the REST contents API.
type Fetcher struct {
	client *http.Client
	token  string
	base   string
}

// NewFetcher builds a Fetcher. The token may be empty for public repos.
func NewFetcher(token string) *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: 30 * time.Second}, token: token, base: "https://api.github.com"}
}

// prTemplatePaths are the conventional single-file PR template locations.
var prTemplatePaths = []string{
	".github/PULL_REQUEST_TEMPLATE.md",
	".github/pull_request_template.md",
	"PULL_REQUEST_TEMPLATE.md",
	"docs/PULL_REQUEST_TEMPLATE.md",
}

// legacyIssueTemplatePaths are single-file issue template locations used when a
// repo has no .github/ISSUE_TEMPLATE directory.
var legacyIssueTemplatePaths = []string{
	".github/ISSUE_TEMPLATE.md",
	"ISSUE_TEMPLATE.md",
	"docs/ISSUE_TEMPLATE.md",
}

// maxIssueTemplates caps how many templates are considered.
const maxIssueTemplates = 12

// PRTemplate returns the repo's PR template body, or ("", false) when none
// exists. Errors are returned so callers can log and fall back.
func (f *Fetcher) PRTemplate(ctx context.Context, owner, repo string) (string, bool, error) {
	for _, p := range prTemplatePaths {
		body, found, err := f.fileContent(ctx, owner, repo, p)
		if err != nil {
			return "", false, err
		}
		if found && strings.TrimSpace(body) != "" {
			return stripFrontMatter(body), true, nil
		}
	}
	return "", false, nil
}

// IssueTemplates returns the repo's markdown issue templates. It reads the
// .github/ISSUE_TEMPLATE directory, then falls back to legacy single-file
// locations. YAML forms are skipped.
func (f *Fetcher) IssueTemplates(ctx context.Context, owner, repo string) ([]Template, error) {
	entries, err := f.listDir(ctx, owner, repo, ".github/ISSUE_TEMPLATE")
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(strings.ToLower(e.Name), ".md") {
			continue // skip YAML forms, config.yml, and non-files
		}
		body, found, err := f.fileContent(ctx, owner, repo, e.Path)
		if err != nil {
			return nil, err
		}
		if !found || strings.TrimSpace(body) == "" {
			continue
		}
		out = append(out, Template{Name: templateName(e.Name, body), Body: stripFrontMatter(body)})
		if len(out) >= maxIssueTemplates {
			break
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	for _, p := range legacyIssueTemplatePaths {
		body, found, err := f.fileContent(ctx, owner, repo, p)
		if err != nil {
			return nil, err
		}
		if found && strings.TrimSpace(body) != "" {
			return []Template{{Name: "default", Body: stripFrontMatter(body)}}, nil
		}
	}
	return nil, nil
}

// dirEntry is one item in a contents-API directory listing.
type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// listDir returns the entries of a repo directory, or nil when it does not
// exist.
func (f *Fetcher) listDir(ctx context.Context, owner, repo, dir string) ([]dirEntry, error) {
	rb, status, err := f.get(ctx, owner, repo, dir)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("listing %s: status %d", dir, status)
	}
	var entries []dirEntry
	if err := json.Unmarshal(rb, &entries); err != nil {
		// A file path returns a JSON object, not an array; treat as no directory.
		return nil, nil
	}
	return entries, nil
}

// fileContent returns a repo file's decoded content, or ("", false) when
// absent.
func (f *Fetcher) fileContent(ctx context.Context, owner, repo, path string) (string, bool, error) {
	rb, status, err := f.get(ctx, owner, repo, path)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("reading %s: status %d", path, status)
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
	}
	if err := json.Unmarshal(rb, &out); err != nil || out.Type != "file" || out.Encoding != "base64" {
		return "", false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", false, nil
	}
	return string(decoded), true, nil
}

// get performs a contents-API GET and returns the body and status.
func (f *Fetcher) get(ctx context.Context, owner, repo, path string) ([]byte, int, error) {
	escaped := strings.Join(mapStrings(strings.Split(path, "/"), url.PathEscape), "/")
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", f.base, owner, repo, escaped)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return rb, resp.StatusCode, nil
}

// stripFrontMatter removes a leading YAML front-matter block (--- ... ---).
func stripFrontMatter(s string) string {
	t := strings.TrimLeft(s, "\ufeff \t\r\n")
	if !strings.HasPrefix(t, "---") {
		return s
	}
	rest := strings.TrimPrefix(t, "---")
	if i := strings.Index(rest, "\n---"); i >= 0 {
		after := rest[i+len("\n---"):]
		return strings.TrimLeft(after, "\r\n")
	}
	return s
}

// templateName returns the front-matter "name:" value, or the filename stem.
func templateName(filename, body string) string {
	t := strings.TrimLeft(body, "\ufeff \t\r\n")
	if strings.HasPrefix(t, "---") {
		rest := strings.TrimPrefix(t, "---")
		if end := strings.Index(rest, "\n---"); end >= 0 {
			for _, line := range strings.Split(rest[:end], "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "name:") {
					if v := strings.TrimSpace(strings.TrimPrefix(line, "name:")); v != "" {
						return strings.Trim(v, `"'`)
					}
				}
			}
		}
	}
	return strings.TrimSuffix(filename, ".md")
}

func mapStrings(in []string, fn func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fn(s)
	}
	return out
}
