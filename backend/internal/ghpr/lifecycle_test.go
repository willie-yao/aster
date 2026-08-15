package ghpr

import (
	"context"
	"net/http"
	"net/http/httptest"
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
