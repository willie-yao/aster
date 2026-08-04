package ai

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestResolveBuildSource(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	cases := []struct {
		name  string
		build models.BuildInfo
		ok    bool
	}{
		{name: "periodic repo refs", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main:" + sha}}, ok: true},
		{name: "exact repo version fallback", build: models.BuildInfo{RepoVersion: sha}, ok: true},
		{name: "mutable ref with matching checkout", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"}, Commit: sha, RepoVersion: sha}, ok: true},
		{name: "mutable ref with parallel repo", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main", "kubernetes-sigs/cloud-provider-azure": "master"}, Commit: sha, RepoVersion: sha}},
		{name: "mutable ref missing checkout commit", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"}}},
		{name: "mutable ref mismatched checkout metadata", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"}, Commit: sha, RepoVersion: other}},
		{name: "composite presubmit", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main:" + sha + ",123:" + sha}, Commit: sha, RepoVersion: sha}},
		{name: "repo mismatch with exact checkout", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api": "main"}, Commit: sha, RepoVersion: sha}},
		{name: "mutable ref with noncommit checkout", build: models.BuildInfo{RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"}, Commit: "main", RepoVersion: "main"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveBuildSource(tc.build, "kubernetes-sigs", "cluster-api-provider-azure")
			if ok != tc.ok {
				t.Fatalf("ResolveBuildSource ok=%t source=%+v", ok, got)
			}
			if ok && got.Revision != sha {
				t.Fatalf("revision=%q, want %q", got.Revision, sha)
			}
		})
	}
}
