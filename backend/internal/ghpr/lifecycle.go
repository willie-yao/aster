package ghpr

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PullRequest is the lifecycle metadata needed by remediation reconciliation.
type PullRequest struct {
	Number         int
	HTMLURL        string
	State          string
	Draft          bool
	Merged         bool
	MergeCommitSHA string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       time.Time
	MergedAt       time.Time
	Head           PullRequestRef
	Base           PullRequestRef
}

// PullRequestRef identifies one side of a pull request.
type PullRequestRef struct {
	SHA  string
	Ref  string
	Repo string
}

// GetPullRequest returns current lifecycle and revision metadata.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var raw struct {
		Number         int    `json:"number"`
		HTMLURL        string `json:"html_url"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		CreatedAt      string `json:"created_at"`
		UpdatedAt      string `json:"updated_at"`
		ClosedAt       string `json:"closed_at"`
		MergedAt       string `json:"merged_at"`
		Head           struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := c.get(ctx, c.url(owner, repo, fmt.Sprintf("pulls/%d", number)), &raw); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{
		Number: raw.Number, HTMLURL: raw.HTMLURL, State: strings.ToLower(raw.State),
		Draft: raw.Draft, Merged: raw.Merged, MergeCommitSHA: raw.MergeCommitSHA,
		CreatedAt: parseGitHubTime(raw.CreatedAt), UpdatedAt: parseGitHubTime(raw.UpdatedAt),
		ClosedAt: parseGitHubTime(raw.ClosedAt), MergedAt: parseGitHubTime(raw.MergedAt),
		Head: PullRequestRef{SHA: raw.Head.SHA, Ref: raw.Head.Ref, Repo: raw.Head.Repo.FullName},
		Base: PullRequestRef{SHA: raw.Base.SHA, Ref: raw.Base.Ref, Repo: raw.Base.Repo.FullName},
	}, nil
}

// CompareCommits reports whether head contains base in the same repository.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (bool, string, error) {
	if base == "" || head == "" {
		return false, "", fmt.Errorf("compare commits requires base and head")
	}
	var raw struct {
		Status string `json:"status"`
	}
	path := "compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
	if err := c.get(ctx, c.url(owner, repo, path), &raw); err != nil {
		return false, "", err
	}
	status := strings.ToLower(raw.Status)
	return status == "ahead" || status == "identical", status, nil
}

// PullRequestSearchResult is one marker-matched pull request.
type PullRequestSearchResult struct {
	Number    int
	HTMLURL   string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SearchPullRequests returns marker-matched pull requests in update order.
func (c *Client) SearchPullRequests(ctx context.Context, owner, repo, queryToken, confirmMarker string) ([]PullRequestSearchResult, error) {
	q := fmt.Sprintf("repo:%s/%s is:pr %s in:body", owner, repo, queryToken)
	searchURL := c.base + "/search/issues?per_page=10&sort=updated&order=desc&q=" + url.QueryEscape(q)
	var out struct {
		Items []struct {
			Number    int    `json:"number"`
			HTMLURL   string `json:"html_url"`
			Body      string `json:"body"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"items"`
	}
	if err := c.get(ctx, searchURL, &out); err != nil {
		return nil, err
	}
	var results []PullRequestSearchResult
	for _, item := range out.Items {
		if !strings.Contains(item.Body, confirmMarker) {
			continue
		}
		results = append(results, PullRequestSearchResult{
			Number: item.Number, HTMLURL: item.HTMLURL, State: item.State,
			CreatedAt: parseGitHubTime(item.CreatedAt), UpdatedAt: parseGitHubTime(item.UpdatedAt),
		})
	}
	return results, nil
}

// SearchPR finds a pull request in any state whose body contains confirmMarker.
func (c *Client) SearchPR(ctx context.Context, owner, repo, queryToken, confirmMarker string) (number int, htmlURL string, found bool, err error) {
	results, err := c.SearchPullRequests(ctx, owner, repo, queryToken, confirmMarker)
	if err != nil {
		return 0, "", false, err
	}
	if len(results) == 0 {
		return 0, "", false, nil
	}
	return results[0].Number, results[0].HTMLURL, true, nil
}

// CommentPullRequest posts a timeline comment through the issues API.
func (c *Client) CommentPullRequest(ctx context.Context, owner, repo string, number int, body string) error {
	return c.post(ctx, c.url(owner, repo, fmt.Sprintf("issues/%d/comments", number)), map[string]string{"body": body}, nil)
}

func parseGitHubTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
