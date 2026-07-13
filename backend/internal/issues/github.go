package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// defaultAPIBase is the GitHub REST API root. Overridable per-client so tests
// can point at an httptest server.
const defaultAPIBase = "https://api.github.com"

// Client is a minimal GitHub REST client scoped to one target repo, with just
// the issue operations this package needs.
type Client struct {
	httpClient *http.Client
	token      string
	owner      string
	repo       string
	apiBase    string
}

// NewClient returns a Client for owner/repo authenticated with token.
func NewClient(token, owner, repo string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		token:      token,
		owner:      owner,
		repo:       repo,
		apiBase:    defaultAPIBase,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, rb, nil
}

// SearchOpenIssue finds an open issue whose body contains confirmMarker.
// queryToken narrows the search, and the full marker check avoids token-level
// false positives when local state is lost.
func (c *Client) SearchOpenIssue(ctx context.Context, queryToken, confirmMarker string) (number int, htmlURL string, found bool, err error) {
	q := fmt.Sprintf("repo:%s/%s is:issue is:open %s in:body", c.owner, c.repo, queryToken)
	path := "/search/issues?per_page=5&q=" + url.QueryEscape(q)
	resp, rb, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, "", false, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", false, fmt.Errorf("search issues: %s: %s", resp.Status, textutil.Truncate(string(rb), 300))
	}
	var out struct {
		Items []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return 0, "", false, fmt.Errorf("decode search response: %w", err)
	}
	// GitHub search tokenizes bodies, so confirm the full marker before adoption.
	for _, it := range out.Items {
		if strings.Contains(it.Body, confirmMarker) {
			return it.Number, it.HTMLURL, true, nil
		}
	}
	return 0, "", false, nil
}

// CreateIssue opens a new issue and returns its number and html URL.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (number int, htmlURL string, err error) {
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	resp, rb, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo), payload)
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return 0, "", fmt.Errorf("create issue: %s: %s", resp.Status, textutil.Truncate(string(rb), 300))
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return 0, "", fmt.Errorf("decode create response: %w", err)
	}
	return out.Number, out.HTMLURL, nil
}

// CommentIssue posts a comment on an existing issue.
func (c *Client) CommentIssue(ctx context.Context, number int, body string) error {
	resp, rb, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.owner, c.repo, number),
		map[string]any{"body": body})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("comment issue #%d: %s: %s", number, resp.Status, textutil.Truncate(string(rb), 300))
	}
	return nil
}

// CloseIssue closes an existing issue.
func (c *Client) CloseIssue(ctx context.Context, number int) error {
	resp, rb, err := c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%d", c.owner, c.repo, number),
		map[string]any{"state": "closed"})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("close issue #%d: %s: %s", number, resp.Status, textutil.Truncate(string(rb), 300))
	}
	return nil
}
