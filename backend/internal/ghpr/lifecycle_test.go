package ghpr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func lifecycleClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(srv.Client(), "token")
	client.base = srv.URL
	return client
}

func TestGetPullRequest(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"number": 7, "html_url": "https://github.com/o/r/pull/7", "state": "closed", "merged": true,
			"draft": false, "merge_commit_sha": "merge", "merged_at": "2026-07-20T02:00:00Z",
			"head": map[string]any{"sha": "head", "ref": "fix", "repo": map[string]any{"full_name": "fork/r"}},
			"base": map[string]any{"sha": "base", "ref": "main", "repo": map[string]any{"full_name": "o/r"}},
		})
	})
	got, err := client.GetPullRequest(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Merged || got.MergeCommitSHA != "merge" || got.Head.Repo != "fork/r" || got.Base.Repo != "o/r" {
		t.Fatalf("pull request = %+v", got)
	}
}

func TestCompareCommits(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/compare/merge...build") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ahead"})
	})
	contains, status, err := client.CompareCommits(context.Background(), "o", "r", "merge", "build")
	if err != nil || !contains || status != "ahead" {
		t.Fatalf("contains=%v status=%q err=%v", contains, status, err)
	}
}

// listPullsHandler serves paginated open pull requests from pages.
func listPullsHandler(t *testing.T, pages [][]map[string]any, seen *[]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		*seen = append(*seen, r.URL.RawQuery)
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 || page > len(pages) {
			writeJSON(w, http.StatusOK, []map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, pages[page-1])
	}
}

func rawPull(number int, draft bool) map[string]any {
	return map[string]any{
		"number": number, "title": "title", "state": "open", "draft": draft,
		"html_url":   "https://github.com/o/r/pull/" + strconv.Itoa(number),
		"created_at": "2026-07-19T02:00:00Z", "updated_at": "2026-07-20T02:00:00Z",
		"user": map[string]any{"login": "octocat"},
		"head": map[string]any{"sha": "head", "ref": "fix", "repo": map[string]any{"full_name": "fork/r"}},
		"base": map[string]any{"sha": "base", "ref": "main", "repo": map[string]any{"full_name": "o/r"}},
	}
}

func TestListOpenPullRequestsExcludesDrafts(t *testing.T) {
	var seen []string
	client := lifecycleClient(t, listPullsHandler(t,
		[][]map[string]any{{rawPull(7, false), rawPull(8, true), rawPull(9, false)}}, &seen))

	pulls, err := client.ListOpenPullRequests(context.Background(), "o", "r", 0)
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if len(pulls) != 2 || pulls[0].Number != 7 || pulls[1].Number != 9 {
		t.Fatalf("pulls = %+v, want only the non-draft 7 and 9", pulls)
	}
	if pulls[0].Title != "title" || pulls[0].Author != "octocat" || pulls[0].Base.Ref != "main" {
		t.Errorf("pull identity = %+v", pulls[0])
	}
	if pulls[0].CreatedAt.IsZero() || pulls[0].UpdatedAt.IsZero() {
		t.Errorf("timestamps not parsed: %+v", pulls[0])
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "state=open") || !strings.Contains(seen[0], "sort=updated") {
		t.Errorf("query = %v, want a single open+updated listing", seen)
	}
}

func TestListOpenPullRequestsStopsAtLimit(t *testing.T) {
	var seen []string
	client := lifecycleClient(t, listPullsHandler(t,
		[][]map[string]any{{rawPull(7, false), rawPull(8, false), rawPull(9, false)}}, &seen))

	pulls, err := client.ListOpenPullRequests(context.Background(), "o", "r", 2)
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if len(pulls) != 2 {
		t.Fatalf("pulls = %d, want the limit of 2", len(pulls))
	}
}

func TestListOpenPullRequestsPaginatesUntilShortPage(t *testing.T) {
	full := make([]map[string]any, pullRequestPageSize)
	for i := range full {
		full[i] = rawPull(i+1, false)
	}
	var seen []string
	client := lifecycleClient(t, listPullsHandler(t,
		[][]map[string]any{full, {rawPull(500, false)}}, &seen))

	pulls, err := client.ListOpenPullRequests(context.Background(), "o", "r", 0)
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if len(pulls) != pullRequestPageSize+1 {
		t.Fatalf("pulls = %d, want %d across both pages", len(pulls), pullRequestPageSize+1)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want one per page", seen)
	}
	if !strings.Contains(seen[1], "page=2") {
		t.Errorf("second request = %q, want page=2", seen[1])
	}
}

func TestListOpenPullRequestsReportsAPIFailure(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := client.ListOpenPullRequests(context.Background(), "o", "r", 0); err == nil {
		t.Fatal("want an error when the pulls endpoint fails")
	}
}
