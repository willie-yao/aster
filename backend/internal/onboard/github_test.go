package onboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeGitHubRepo(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "owner name", input: "kubernetes-sigs/cluster-api", want: "kubernetes-sigs/cluster-api"},
		{name: "literal dot git name", input: "example/project.git", want: "example/project.git"},
		{name: "https", input: "https://github.com/kubernetes-sigs/cluster-api.git", want: "kubernetes-sigs/cluster-api"},
		{name: "ssh url", input: "ssh://git@github.com/kubernetes-sigs/cluster-api.git", want: "kubernetes-sigs/cluster-api"},
		{name: "scp remote", input: "git@github.com:kubernetes-sigs/cluster-api.git", want: "kubernetes-sigs/cluster-api"},
		{name: "scp mixed-case host", input: "git@GitHub.com:kubernetes-sigs/cluster-api.git", want: "kubernetes-sigs/cluster-api"},
		{name: "non github", input: "https://gitlab.com/example/project", wantErr: "not github.com"},
		{name: "invalid repo", input: "not-a-repository", wantErr: "owner/name"},
		{name: "missing name", input: "https://github.com/example", wantErr: "owner/name"},
		{name: "query", input: "https://github.com/example/project?token=value", wantErr: "query or fragment"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, err := NormalizeGitHubRepo(test.input)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeGitHubRepo: %v", err)
			}
			if repo.FullName != test.want {
				t.Fatalf("repo = %q, want %q", repo.FullName, test.want)
			}
		})
	}
}

func TestGitHubRepositoryClient_PrivateWithoutAuthentication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		http.Error(w, `{"message":"private details"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	_, err := (githubRepositoryClient{client: srv.Client()}).Repository(context.Background(), Repo{FullName: "private/project"}, "")
	if err == nil || !strings.Contains(err.Error(), "set GITHUB_TOKEN") {
		t.Fatalf("error = %v, want credential guidance", err)
	}
	if strings.Contains(err.Error(), "private details") {
		t.Fatalf("private response body leaked into error: %v", err)
	}
}

func TestGitHubRepositoryClient_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	_, err := (githubRepositoryClient{client: srv.Client()}).Repository(context.Background(), Repo{FullName: "example/project"}, "fixture-token")
	if err == nil || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("error = %v, want rate-limit guidance", err)
	}
	if strings.Contains(err.Error(), "fixture-token") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

func TestGitHubRepositoryClient_ReportsForkUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{
  "name": "project",
  "default_branch": "main",
  "private": false,
  "visibility": "public",
  "owner": {"login": "fork-owner"},
  "source": {"name": "project", "owner": {"login": "upstream-owner"}}
}`))
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	metadata, err := (githubRepositoryClient{client: srv.Client()}).Repository(context.Background(), Repo{FullName: "fork-owner/project"}, "fixture-token")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if metadata.Repo.FullName != "fork-owner/project" || metadata.Upstream == nil || metadata.Upstream.FullName != "upstream-owner/project" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestGitRemoteDetector(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	tests := []struct {
		name    string
		remote  string
		want    string
		wantErr string
	}{
		{name: "missing remote", wantErr: "no GitHub origin"},
		{name: "github remote", remote: "git@github.com:example/project.git", want: "git@github.com:example/project.git"},
		{name: "non github remote", remote: "https://gitlab.com/example/project.git", wantErr: "not a supported GitHub"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if output, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			if test.remote != "" {
				if output, err := exec.Command("git", "remote", "add", "origin", test.remote).CombinedOutput(); err != nil {
					t.Fatalf("git remote: %v: %s", err, output)
				}
			}
			got, err := (gitRemoteDetector{}).Origin(context.Background())
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("Origin = %q, %v; want %q", got, err, test.want)
			}
			if err := os.Chdir(originalDir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGitHubRepositoryClientAuthenticatedLogin(t *testing.T) {
	const token = "fixture-token"
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/user" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"dashboard-owner"}`))
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	login, err := (githubRepositoryClient{client: srv.Client()}).AuthenticatedLogin(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if login != "dashboard-owner" || requests != 1 {
		t.Fatalf("login=%q requests=%d", login, requests)
	}
}

func TestGitHubRepositoryClientAuthenticatedLoginWithoutTokenDoesNotRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	login, err := (githubRepositoryClient{client: srv.Client()}).AuthenticatedLogin(context.Background(), "")
	if err != nil || login != "" || requests != 0 {
		t.Fatalf("login=%q requests=%d error=%v", login, requests, err)
	}
}

func TestGitHubRepositoryClientAuthenticatedLoginErrorDoesNotLeakToken(t *testing.T) {
	const token = "fixture-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, token+" private response", http.StatusUnauthorized)
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = old })

	_, err := (githubRepositoryClient{client: srv.Client()}).AuthenticatedLogin(context.Background(), token)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v", err)
	}
}

func TestGitRemoteDetectorRootFromNestedDirectory(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	nested := filepath.Join(dir, "backend", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	root, err := (gitRemoteDetector{}).Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(rootInfo, dirInfo) {
		t.Fatalf("root = %q, want checkout %q", root, dir)
	}
}
