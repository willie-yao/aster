package ai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestGitHubRepoReaderReadsBoundedSourceArchive(t *testing.T) {
	const token = "read-token-value"
	body := sourceArchive(t, map[string]string{
		"go.mod":     "module example/repo\n",
		"pkg/fix.go": "package fix\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/private/tarball/commit-sha" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("archive authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	reader := NewGitHubRepoReader("owner", "private", "commit-sha", token).(*githubRepoReader)
	archive, err := reader.ReadSourceArchive(context.Background())
	if err != nil || !archive.Paths["go.mod"] || !archive.Paths["pkg/fix.go"] || archive.GoFiles["pkg/fix.go"] != "package fix\n" {
		t.Fatalf("archive=%v err=%v", archive, err)
	}
	if _, ok := archive.GoFiles["go.mod"]; ok {
		t.Fatal("non-Go file content was retained")
	}
}

func sourceArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for path, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: "owner-private-sha/" + path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestGitHubRepoReaderForwardsPrivateTokenToCodeload(t *testing.T) {
	const token = "read-token-value"
	archive := sourceArchive(t, map[string]string{"go.mod": "module example/repo\n"})
	var apiAuth, archiveAuth string
	archiveServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveAuth = r.Header.Get("Authorization")
		_, _ = w.Write(archive)
	}))
	defer archiveServer.Close()
	archiveURL, err := url.Parse(archiveServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, archiveServer.URL+"/archive.tar.gz", http.StatusFound)
	}))
	defer apiServer.Close()

	oldAPI, oldArchiveHost := githubAPIBase, githubArchiveHost
	githubAPIBase, githubArchiveHost = apiServer.URL, archiveURL.Hostname()
	t.Cleanup(func() { githubAPIBase, githubArchiveHost = oldAPI, oldArchiveHost })
	reader := NewGitHubRepoReader("owner", "private", "commit-sha", token).(*githubRepoReader)
	reader.client = archiveServer.Client()
	archiveResult, err := reader.ReadSourceArchive(context.Background())
	if err != nil || !archiveResult.Paths["go.mod"] {
		t.Fatalf("archive=%v err=%v", archiveResult, err)
	}
	if apiAuth != "Bearer "+token || archiveAuth != "Bearer "+token {
		t.Fatalf("authorization api=%q archive=%q", apiAuth, archiveAuth)
	}
}

func TestGitHubRepoReaderRejectsUnexpectedPrivateArchiveRedirect(t *testing.T) {
	redirected := false
	archiveServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer archiveServer.Close()
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, archiveServer.URL+"/archive.tar.gz", http.StatusFound)
	}))
	defer apiServer.Close()

	oldAPI, oldArchiveHost := githubAPIBase, githubArchiveHost
	githubAPIBase, githubArchiveHost = apiServer.URL, "codeload.github.com"
	t.Cleanup(func() { githubAPIBase, githubArchiveHost = oldAPI, oldArchiveHost })
	reader := NewGitHubRepoReader("owner", "private", "commit-sha", "read-token-value").(*githubRepoReader)
	reader.client = archiveServer.Client()
	if _, err := reader.ReadSourceArchive(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing authenticated source archive redirect") {
		t.Fatalf("ReadSourceArchive error = %v", err)
	}
	if redirected {
		t.Fatal("unexpected redirect received authenticated request")
	}
}
