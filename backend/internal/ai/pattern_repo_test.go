package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubRepoReaderAnonymousPublicAccess(t *testing.T) {
	var treeCalls, fileCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("anonymous request carried Authorization")
		}
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/HEAD"):
			treeCalls++
			_, _ = w.Write([]byte(`{"tree":[{"path":"config/source.yaml","type":"blob"},{"path":"config","type":"tree"}]}`))
		case r.URL.Path == "/owner/repo/HEAD/config/source.yaml":
			fileCalls++
			_, _ = w.Write([]byte("enabled: true\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI, oldRaw := githubAPIBase, rawContentBase
	githubAPIBase, rawContentBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, rawContentBase = oldAPI, oldRaw })

	reader := NewGitHubRepoReader("owner", "repo", "", "")
	paths, err := reader.ListTree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "config/source.yaml" || treeCalls != 1 {
		t.Fatalf("paths=%v treeCalls=%d", paths, treeCalls)
	}
	content, found, err := reader.ReadFile(context.Background(), "config/source.yaml")
	if err != nil || !found || content != "enabled: true\n" || fileCalls != 1 {
		t.Fatalf("content=%q found=%t fileCalls=%d err=%v", content, found, fileCalls, err)
	}
}

func TestGitHubRepoReaderAuthenticatedPrivateAccess(t *testing.T) {
	const token = "read-token-value"
	var treeAuth, fileAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/commit-sha"):
			treeAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"tree":[{"path":"private/file.go","type":"blob"}]}`))
		case r.URL.Path == "/repos/owner/private/contents/private/file.go":
			fileAuth = r.Header.Get("Authorization")
			if r.URL.Query().Get("ref") != "commit-sha" || r.Header.Get("Accept") != "application/vnd.github.raw+json" {
				t.Fatalf("authenticated file request ref=%q accept=%q", r.URL.Query().Get("ref"), r.Header.Get("Accept"))
			}
			_, _ = w.Write([]byte("package private\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	reader := NewGitHubRepoReader("owner", "private", "commit-sha", token)
	if _, err := reader.ListTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, found, err := reader.ReadFile(context.Background(), "private/file.go")
	if err != nil || !found || content != "package private\n" {
		t.Fatalf("content=%q found=%t err=%v", content, found, err)
	}
	if treeAuth != "Bearer "+token || fileAuth != "Bearer "+token {
		t.Fatalf("authorization headers tree=%q file=%q", treeAuth, fileAuth)
	}
}
