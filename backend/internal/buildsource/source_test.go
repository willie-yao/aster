package buildsource

import (
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestResolve(t *testing.T) {
	sha := strings.Repeat("a", 40)
	upper := strings.ToUpper(sha)
	other := strings.Repeat("b", 40)
	cases := []struct {
		name  string
		build models.BuildInfo
		owner string
		repo  string
		want  string
	}{
		{name: "exact SHA", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha}}, owner: "example", repo: "repo", want: sha},
		{name: "periodic ref SHA", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:" + sha}}, owner: "example", repo: "repo", want: sha},
		{name: "mutable ref matching checkout", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo", want: sha},
		{name: "case insensitive repository", build: models.BuildInfo{RepoRefs: map[string]string{"Example/Repo": sha}}, owner: "EXAMPLE", repo: "REPO", want: sha},
		{name: "uppercase SHA normalization", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": upper}}, owner: "example", repo: "repo", want: sha},
		{name: "missing commit", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "missing repo version", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, Commit: sha}, owner: "example", repo: "repo"},
		{name: "mismatched checkout metadata", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, Commit: sha, RepoVersion: other}, owner: "example", repo: "repo"},
		{name: "non SHA checkout metadata", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, Commit: "main", RepoVersion: "main"}, owner: "example", repo: "repo"},
		{name: "multiple repositories with mutable ref", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main", "example/other": "main"}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "wrong repository", build: models.BuildInfo{RepoRefs: map[string]string{"example/other": sha}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "empty repository revision", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": ""}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "malformed SHA", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:01234567"}}, owner: "example", repo: "repo"},
		{name: "short bare SHA", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "01234567"}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "composite presubmit", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:" + sha + ",pull:" + other}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "conflicting revisions for repository", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha, "Example/Repo": other}}, owner: "example", repo: "repo"},
		{name: "exact repository revision amid other repositories", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha, "example/other": "main"}}, owner: "example", repo: "repo", want: sha},
		{name: "no repository refs", build: models.BuildInfo{Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
		{name: "empty owner", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha}}, repo: "repo"},
		{name: "empty name", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha}}, owner: "example"},
		{name: "persisted ambiguous sentinel", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "ambiguous"}, Commit: sha, RepoVersion: sha}, owner: "example", repo: "repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(tc.build, tc.owner, tc.repo)
			if ok != (tc.want != "") {
				t.Fatalf("Resolve() ok=%t source=%+v", ok, got)
			}
			if ok && got.Revision != tc.want {
				t.Fatalf("revision=%q, want %q", got.Revision, tc.want)
			}
		})
	}
}

func TestNormalizeRevision(t *testing.T) {
	sha := strings.Repeat("A", 40)
	want := strings.ToLower(sha)
	for _, value := range []string{sha, "main:" + sha} {
		if got, ok := NormalizeRevision(value); !ok || got != want {
			t.Fatalf("NormalizeRevision(%q) = %q, %t", value, got, ok)
		}
	}
	for _, value := range []string{"", "main", "01234567", "main:01234567", ":" + sha, "main:" + sha + ",pull:" + sha} {
		if got, ok := NormalizeRevision(value); ok {
			t.Fatalf("NormalizeRevision(%q) = %q, true", value, got)
		}
	}
}

func TestBranch(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, testCase := range []struct {
		name  string
		build models.BuildInfo
		want  string
	}{
		{name: "mutable checkout", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main"}, Commit: sha, RepoVersion: sha}, want: "main"},
		{name: "qualified revision", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "release-1.2:" + sha}}, want: "release-1.2"},
		{name: "heads prefix", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "refs/heads/main"}, Commit: sha, RepoVersion: sha}, want: "main"},
		{name: "duplicate identical branch", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:" + sha, "Example/Repo": "main:" + sha}}, want: "main"},
		{name: "conflicting branches", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:" + sha, "Example/Repo": "release-1.2:" + sha}}, want: ""},
		{name: "branch and bare revision", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": "main:" + sha, "Example/Repo": sha}}, want: ""},
		{name: "exact revision", build: models.BuildInfo{RepoRefs: map[string]string{"example/repo": sha}}, want: ""},
		{name: "wrong repository", build: models.BuildInfo{RepoRefs: map[string]string{"example/other": "main"}, Commit: sha, RepoVersion: sha}, want: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := Branch(testCase.build, "example", "repo")
			if ok != (testCase.want != "") || got != testCase.want {
				t.Fatalf("Branch() = %q, %t, want %q", got, ok, testCase.want)
			}
		})
	}
}

func TestVerifiedPaths(t *testing.T) {
	sha := strings.Repeat("a", 40)
	source := Source{Owner: "example", Name: "repo", Revision: sha}
	got := VerifiedPaths(map[string]string{
		"pkg/controller.go": "https://github.com/example/repo/blob/" + sha + "/pkg/controller.go",
		"wrong-revision.go": "https://github.com/example/repo/blob/" + strings.Repeat("b", 40) + "/wrong-revision.go",
		"wrong-repo.go":     "https://github.com/example/other/blob/" + sha + "/wrong-repo.go",
		"unsafe.go":         "https://github.com/example/repo/blob/" + sha + "/../unsafe.go",
		"insecure.go":       "http://github.com/example/repo/blob/" + sha + "/insecure.go",
	}, source)
	if len(got) != 1 || got[0] != "pkg/controller.go" {
		t.Fatalf("VerifiedPaths() = %v", got)
	}
	// The path comes from the blob URL, not the cited file-link key. Chat fix
	// eligibility in the frontend mirrors this rule.
	prefixed := VerifiedPaths(map[string]string{
		"repo/pkg/controller.go": "https://github.com/example/repo/blob/" + sha + "/pkg/controller.go",
		"./pkg/util.go":          "https://github.com/example/repo/blob/" + sha + "/pkg/./util.go",
		"escaped.go":             "https://github.com/example/repo/blob/" + sha + "/pkg%2Fescaped.go",
		"backslash.go":           "https://github.com/example/repo/blob/" + sha + "/pkg%5Cbackslash.go",
	}, source)
	if !slices.Equal(prefixed, []string{"pkg/controller.go", "pkg/escaped.go", "pkg/util.go"}) {
		t.Fatalf("VerifiedPaths() = %v", prefixed)
	}
}
