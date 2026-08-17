package buildsource

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	baseSHA = "3333333333333333333333333333333333333333"
	pullSHA = "1111111111111111111111111111111111111111"
)

func presubmitBuild(refs map[string]string) models.BuildInfo {
	return models.BuildInfo{RepoRefs: refs, PullNumber: "42"}
}

func compositeRefs() map[string]string {
	return map[string]string{"example/repo": "main:" + baseSHA + ",42:" + pullSHA}
}

func TestResolvePullHead(t *testing.T) {
	cases := []struct {
		name       string
		refs       map[string]string
		owner      string
		repo       string
		pullNumber string
		want       string
	}{
		{name: "composite presubmit ref", refs: compositeRefs(), owner: "example", repo: "repo", pullNumber: "42", want: pullSHA},
		{name: "case insensitive repository", refs: map[string]string{"Example/Repo": "main:" + baseSHA + ",42:" + pullSHA},
			owner: "EXAMPLE", repo: "REPO", pullNumber: "42", want: pullSHA},
		{name: "merge commit suffix", refs: map[string]string{"example/repo": "main:" + baseSHA + ",42:" + pullSHA + "444444444444444444444444"},
			owner: "example", repo: "repo", pullNumber: "42", want: pullSHA + "444444444444444444444444"},
		{name: "wrong pull number", refs: compositeRefs(), owner: "example", repo: "repo", pullNumber: "43"},
		{name: "wrong repository", refs: compositeRefs(), owner: "example", repo: "other", pullNumber: "42"},
		{name: "periodic ref has no pull segment", refs: map[string]string{"example/repo": "main:" + baseSHA},
			owner: "example", repo: "repo", pullNumber: "42"},
		{name: "malformed revision", refs: map[string]string{"example/repo": "main:" + baseSHA + ",42:notasha"},
			owner: "example", repo: "repo", pullNumber: "42"},
		{name: "empty owner", refs: compositeRefs(), repo: "repo", pullNumber: "42"},
		{name: "empty name", refs: compositeRefs(), owner: "example", pullNumber: "42"},
		{name: "empty pull number", refs: compositeRefs(), owner: "example", repo: "repo"},
		{name: "no refs", owner: "example", repo: "repo", pullNumber: "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolvePullHead(presubmitBuild(tc.refs), tc.owner, tc.repo, tc.pullNumber)
			if tc.want == "" {
				if ok {
					t.Fatalf("ResolvePullHead = %+v, want no source", got)
				}
				return
			}
			if !ok || got.Revision != tc.want {
				t.Fatalf("ResolvePullHead = %+v ok=%t, want revision %s", got, ok, tc.want)
			}
			if got.Owner != "example" && got.Owner != "EXAMPLE" {
				t.Errorf("owner = %q", got.Owner)
			}
		})
	}
}

// Resolve must keep failing closed on composite presubmit refs. The fix pull
// request write paths depend on it: a fix must never be based on another pull
// request's head commit. ResolvePullHead is the only opt-in to that revision.
func TestResolveStillFailsClosedOnPresubmitRefs(t *testing.T) {
	build := presubmitBuild(compositeRefs())

	if source, ok := Resolve(build, "example", "repo"); ok {
		t.Fatalf("Resolve = %+v, want fail-closed on a composite presubmit ref", source)
	}
	if _, ok := Branch(build, "example", "repo"); ok {
		t.Fatal("Branch must not resolve a composite presubmit ref either")
	}
	// The read-only pull request path still gets the head.
	if _, ok := ResolvePullHead(build, "example", "repo", "42"); !ok {
		t.Fatal("ResolvePullHead should resolve the same build")
	}
}

// A periodic build must not be reinterpreted as a pull request checkout.
func TestResolvePullHeadIgnoresPeriodicBuilds(t *testing.T) {
	build := models.BuildInfo{RepoRefs: map[string]string{"example/repo": baseSHA}}

	if source, ok := ResolvePullHead(build, "example", "repo", "42"); ok {
		t.Fatalf("ResolvePullHead = %+v, want no source for a periodic build", source)
	}
	// Resolve still handles the periodic case as before.
	if _, ok := Resolve(build, "example", "repo"); !ok {
		t.Fatal("Resolve should still resolve a bare periodic revision")
	}
}
