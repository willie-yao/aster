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

	pulls, _, err := client.ListOpenPullRequests(context.Background(), "o", "r", 0)
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
func TestHasCommentBy(t *testing.T) {
	cases := []struct {
		name   string
		logins []string
		want   bool
	}{
		{name: "present", logins: []string{"someone", "aster[bot]"}, want: true},
		{name: "case insensitive", logins: []string{"ASTER[BOT]"}, want: true},
		{name: "absent", logins: []string{"someone", "another"}},
		{name: "no comments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/issues/7/comments" {
					http.Error(w, "bad path", http.StatusNotFound)
					return
				}
				items := []map[string]any{}
				for _, login := range tc.logins {
					items = append(items, map[string]any{"user": map[string]any{"login": login}})
				}
				writeJSON(w, http.StatusOK, items)
			})
			got, err := client.HasCommentBy(context.Background(), "o", "r", 7, "aster[bot]")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("HasCommentBy = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestHasCommentByFailsOnUnboundedTimeline proves absence is never asserted
// from a truncated read. Reporting "no comment" wrongly means a duplicate.
func TestHasCommentByFailsOnUnboundedTimeline(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		items := []map[string]any{}
		for range commentPageSize {
			items = append(items, map[string]any{"user": map[string]any{"login": "someone"}})
		}
		writeJSON(w, http.StatusOK, items)
	})
	if _, err := client.HasCommentBy(context.Background(), "o", "r", 7, "aster[bot]"); err == nil {
		t.Fatal("expected an error when the timeline exceeds the page bound")
	}
}

func TestHasCommentByRequiresLogin(t *testing.T) {
	called := false
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, []map[string]any{})
	})
	if _, err := client.HasCommentBy(context.Background(), "o", "r", 7, " "); err == nil {
		t.Fatal("expected an error for an empty login")
	}
	if called {
		t.Fatal("an empty login still queried GitHub")
	}
}

// TestHighestPullRequestNumberIncludesEveryState proves the watermark query is
// not filtered to open, non-draft pull requests. A draft or closed pull request
// above the bound would otherwise be treated as new later.
func TestHighestPullRequestNumberIncludesEveryState(t *testing.T) {
	var gotQuery string
	client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(w, http.StatusOK, []map[string]any{
			{"number": 500, "draft": true, "state": "open"},
		})
	})

	got, err := client.HighestPullRequestNumber(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if got != 500 {
		t.Fatalf("HighestPullRequestNumber = %d, want 500", got)
	}
	for _, want := range []string{"state=all", "sort=created", "direction=desc"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
}

func TestHighestPullRequestNumberOnEmptyRepo(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{})
	})
	got, err := client.HighestPullRequestNumber(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("HighestPullRequestNumber = %d, want 0", got)
	}
}

// TestListOpenPullRequestsReportsTruncation proves callers can distinguish a
// complete listing from a capped one. The returned count cannot: drafts are
// filtered after paging, so a short result does not prove the end was reached.
func TestListOpenPullRequestsReportsTruncation(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		pages     int
		lastShort bool
		want      bool
	}{
		{name: "single short page is complete", pages: 1, lastShort: true},
		{name: "stopped at the caller's limit", limit: 5, pages: 1, want: true},
		{name: "exhausted the page ceiling", pages: maxPullRequestPages, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				size := pullRequestPageSize
				if tc.lastShort && page >= tc.pages {
					size = 1
				}
				raws := []map[string]any{}
				for i := range size {
					raws = append(raws, map[string]any{
						"number": (page-1)*pullRequestPageSize + i + 1, "state": "open",
					})
				}
				writeJSON(w, http.StatusOK, raws)
			})

			_, truncated, err := client.ListOpenPullRequests(context.Background(), "o", "r", tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if truncated != tc.want {
				t.Fatalf("truncated = %t, want %t", truncated, tc.want)
			}
		})
	}
}

func TestListPullRequestsCommentedBy(t *testing.T) {
	var gotQuery string
	client := lifecycleClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		writeJSON(w, http.StatusOK, map[string]any{
			"total_count": 2, "items": []map[string]any{{"number": 11}, {"number": 12}},
		})
	})

	got, err := client.ListPullRequestsCommentedBy(context.Background(), "o", "r", "aster[bot]")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[11] || !got[12] {
		t.Fatalf("commented = %v, want 11 and 12", got)
	}
	for _, want := range []string{"repo:o/r", "is:pr", "is:open", "commenter:aster[bot]"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
}

// TestListPullRequestsCommentedByFailsClosed proves a partial or unreliable
// search is an error rather than a short set. A missing entry would drop the
// retention keeping a commented pull request's triage page alive.
func TestListPullRequestsCommentedByFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		wantSub string
	}{
		{
			name:    "incomplete results",
			body:    map[string]any{"total_count": 1, "incomplete_results": true, "items": []map[string]any{{"number": 1}}},
			wantSub: "incomplete",
		},
		{
			name:    "beyond the enumerable window",
			body:    map[string]any{"total_count": maxSearchResults + 1, "items": []map[string]any{{"number": 1}}},
			wantSub: "exceed",
		},
		{
			name:    "short final page",
			body:    map[string]any{"total_count": 5, "items": []map[string]any{{"number": 1}}},
			wantSub: "returned 1 of 5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, tc.body)
			})
			if _, err := client.ListPullRequestsCommentedBy(context.Background(), "o", "r", "aster[bot]"); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestListPullRequestsCommentedByRequiresLogin(t *testing.T) {
	called := false
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 0, "items": []map[string]any{}})
	})
	if _, err := client.ListPullRequestsCommentedBy(context.Background(), "o", "r", " "); err == nil {
		t.Fatal("expected an error for an empty login")
	}
	if called {
		t.Fatal("an empty login still queried GitHub, which would match every commenter")
	}
}
