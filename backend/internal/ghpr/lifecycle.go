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
	Title          string
	Body           string
	Author         string
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

// pullRequestJSON is the subset of GitHub's pull request representation used by
// both the single-pull and list endpoints.
type pullRequestJSON struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	ClosedAt       string `json:"closed_at"`
	MergedAt       string `json:"merged_at"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Head pullRequestRefJSON `json:"head"`
	Base pullRequestRefJSON `json:"base"`
}

type pullRequestRefJSON struct {
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

func (raw pullRequestJSON) toPullRequest() PullRequest {
	return PullRequest{
		Number: raw.Number, Title: raw.Title, Body: raw.Body, Author: raw.User.Login,
		HTMLURL: raw.HTMLURL, State: strings.ToLower(raw.State),
		Draft: raw.Draft, Merged: raw.Merged, MergeCommitSHA: raw.MergeCommitSHA,
		CreatedAt: parseGitHubTime(raw.CreatedAt), UpdatedAt: parseGitHubTime(raw.UpdatedAt),
		ClosedAt: parseGitHubTime(raw.ClosedAt), MergedAt: parseGitHubTime(raw.MergedAt),
		Head: PullRequestRef{SHA: raw.Head.SHA, Ref: raw.Head.Ref, Repo: raw.Head.Repo.FullName},
		Base: PullRequestRef{SHA: raw.Base.SHA, Ref: raw.Base.Ref, Repo: raw.Base.Repo.FullName},
	}
}

// GetPullRequest returns current lifecycle and revision metadata.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var raw pullRequestJSON
	if err := c.get(ctx, c.url(owner, repo, fmt.Sprintf("pulls/%d", number)), &raw); err != nil {
		return PullRequest{}, err
	}
	return raw.toPullRequest(), nil
}

// pullRequestPageSize is GitHub's maximum page size for the pulls endpoint.
const pullRequestPageSize = 100

// maxPullRequestPages bounds pagination so an unexpectedly large repository
// cannot stall a refresh pass.
const maxPullRequestPages = 20

// ListOpenPullRequests returns open, non-draft pull requests most recently
// updated first, stopping at limit. A non-positive limit returns every open
// non-draft pull request within the page bound.
//
// The second result reports that the listing was cut short, either by limit or
// by the client's own page ceiling, so more open pull requests exist than were
// returned. Callers that must reason about every open pull request have to
// check it: the returned count alone cannot distinguish a complete listing from
// a truncated one, because drafts are filtered out after paging.
func (c *Client) ListOpenPullRequests(ctx context.Context, owner, repo string, limit int) ([]PullRequest, bool, error) {
	var out []PullRequest
	for page := 1; page <= maxPullRequestPages; page++ {
		query := fmt.Sprintf("pulls?state=open&sort=updated&direction=desc&per_page=%d&page=%d",
			pullRequestPageSize, page)
		var raws []pullRequestJSON
		if err := c.get(ctx, c.url(owner, repo, query), &raws); err != nil {
			return nil, false, fmt.Errorf("listing open pull requests for %s/%s: %w", owner, repo, err)
		}
		for _, raw := range raws {
			if raw.Draft {
				continue
			}
			out = append(out, raw.toPullRequest())
			if limit > 0 && len(out) >= limit {
				return out, true, nil
			}
		}
		if len(raws) < pullRequestPageSize {
			return out, false, nil
		}
	}
	// Pagination ran out before a short page proved the end was reached.
	return out, true, nil
}

// HighestPullRequestNumber returns the largest pull request number the
// repository has ever assigned, open or closed, draft or not.
//
// It is the watermark commenting activates above. Deriving that bound from the
// triage listing would be wrong: that listing is capped and excludes drafts, so
// a draft or a rarely-updated pull request could sit above the watermark and be
// treated as new once it is updated. Numbers are assigned monotonically by
// GitHub, so anything at or below this already existed.
func (c *Client) HighestPullRequestNumber(ctx context.Context, owner, repo string) (int, error) {
	query := "pulls?state=all&sort=created&direction=desc&per_page=1"
	var raws []pullRequestJSON
	if err := c.get(ctx, c.url(owner, repo, query), &raws); err != nil {
		return 0, fmt.Errorf("resolving the newest pull request in %s/%s: %w", owner, repo, err)
	}
	if len(raws) == 0 {
		return 0, nil // the repository has no pull requests yet
	}
	return raws[0].Number, nil
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

// commentPageSize is GitHub's maximum page size for the comments endpoint.
const commentPageSize = 100

// maxCommentPages bounds comment pagination on one pull request.
const maxCommentPages = 20

// HasCommentBy reports whether login has already commented on one pull request.
//
// Unlike the search index, a pull request's own comment timeline is immediately
// consistent, so this sees a comment posted moments ago. It is the last check
// before an unattended write, where a stale answer means a duplicate comment on
// a contributor's thread.
func (c *Client) HasCommentBy(ctx context.Context, owner, repo string, number int, login string) (bool, error) {
	if strings.TrimSpace(login) == "" {
		return false, fmt.Errorf("checking for an existing comment requires a login")
	}
	for page := 1; page <= maxCommentPages; page++ {
		query := fmt.Sprintf("issues/%d/comments?per_page=%d&page=%d", number, commentPageSize, page)
		var out []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := c.get(ctx, c.url(owner, repo, query), &out); err != nil {
			return false, fmt.Errorf("listing comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, comment := range out {
			if strings.EqualFold(comment.User.Login, login) {
				return true, nil
			}
		}
		if len(out) < commentPageSize {
			return false, nil
		}
	}
	// The timeline is longer than the page bound, so absence cannot be proven.
	return false, fmt.Errorf("%s/%s#%d has more than %d comments, cannot confirm whether %s already commented",
		owner, repo, number, commentPageSize*maxCommentPages, login)
}

// searchPageSize is GitHub's maximum page size for the search endpoint.
const searchPageSize = 100

// maxSearchResults is the hard ceiling the search API paginates to.
const maxSearchResults = 1000

// maxSearchPages bounds pagination to the reachable result window.
const maxSearchPages = maxSearchResults / searchPageSize

// ListPullRequestsCommentedBy returns the numbers of open pull requests that
// login has already commented on.
//
// It is used once, when commenting activates, to recover which pull requests a
// previous deployment commented on after the local records were lost. Per-pass
// deduplication does not use it, because each write is confirmed against the
// pull request itself, which the search index can lag behind.
//
// It fails rather than returning a partial set: a missing entry would drop the
// retention that keeps a commented pull request's triage page alive.
func (c *Client) ListPullRequestsCommentedBy(ctx context.Context, owner, repo, login string) (map[int]bool, error) {
	if strings.TrimSpace(login) == "" {
		return nil, fmt.Errorf("listing commented pull requests requires a login")
	}
	found := map[int]bool{}
	total := 0
	for page := 1; page <= maxSearchPages; page++ {
		q := fmt.Sprintf("repo:%s/%s is:pr is:open commenter:%s", owner, repo, login)
		searchURL := fmt.Sprintf("%s/search/issues?per_page=%d&page=%d&q=%s",
			c.base, searchPageSize, page, url.QueryEscape(q))
		var out struct {
			TotalCount        int  `json:"total_count"`
			IncompleteResults bool `json:"incomplete_results"`
			Items             []struct {
				Number int `json:"number"`
			} `json:"items"`
		}
		if err := c.get(ctx, searchURL, &out); err != nil {
			return nil, fmt.Errorf("searching pull requests commented on by %s in %s/%s: %w", login, owner, repo, err)
		}
		// GitHub sets this when the query timed out and returned only the
		// matches found so far.
		if out.IncompleteResults {
			return nil, fmt.Errorf("GitHub returned incomplete search results for %s/%s", owner, repo)
		}
		if out.TotalCount > maxSearchResults {
			return nil, fmt.Errorf("%d matches in %s/%s exceed the %d the search API can enumerate",
				out.TotalCount, owner, repo, maxSearchResults)
		}
		for _, item := range out.Items {
			found[item.Number] = true
		}
		if len(out.Items) < searchPageSize {
			if len(found) < out.TotalCount {
				return nil, fmt.Errorf("search returned %d of %d matches", len(found), out.TotalCount)
			}
			return found, nil
		}
		total = out.TotalCount
	}
	if len(found) < total {
		return nil, fmt.Errorf("search returned %d of %d matches", len(found), total)
	}
	return found, nil
}

// CommentPullRequest posts a timeline comment through the issues API.
func (c *Client) CommentPullRequest(ctx context.Context, owner, repo string, number int, body string) error {
	return c.post(ctx, c.url(owner, repo, fmt.Sprintf("issues/%d/comments", number)), map[string]string{"body": body}, nil)
}

func parseGitHubTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
