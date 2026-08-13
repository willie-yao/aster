package ai

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestResolveBuildSourceDelegatesSharedResolver(t *testing.T) {
	sha := strings.Repeat("a", 40)
	build := models.BuildInfo{
		RepoRefs:    map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"},
		Commit:      sha,
		RepoVersion: sha,
	}
	got, ok := ResolveBuildSource(build, "kubernetes-sigs", "cluster-api-provider-azure")
	if !ok || got.Revision != sha {
		t.Fatalf("ResolveBuildSource() ok=%t source=%+v", ok, got)
	}
}
