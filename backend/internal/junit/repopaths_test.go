package junit

import (
	"fmt"
	"reflect"
	"testing"
)

const capz = "kubernetes-sigs/cluster-api-provider-azure"

func TestRepoFailurePaths(t *testing.T) {
	// A Ginkgo stack that enters cluster-api before reaching the repository
	// under test, which is the shape junit_test.go already records.
	body := `sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115
sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412
sigs.k8s.io/cluster-api-provider-azure/azure/scope/cluster.go:88`

	got := RepoFailurePaths(body, capz)
	want := []string{"test/e2e/azure_test.go", "azure/scope/cluster.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoFailurePaths = %v, want %v", got, want)
	}
}

func TestRepoFailurePathsSelectsByRepository(t *testing.T) {
	body := `sigs.k8s.io/cluster-api/test@v1.12.3/framework/helpers.go:115
sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412`

	if got := RepoFailurePaths(body, "kubernetes-sigs/cluster-api"); len(got) != 0 {
		t.Errorf("a version-qualified dependency frame is not a working-tree path: %v", got)
	}
	if got := RepoFailurePaths(body, capz); len(got) != 1 {
		t.Errorf("capz paths = %v, want one", got)
	}
}

func TestRepoFailurePathsSkipsVersionQualifiedReferences(t *testing.T) {
	body := "sigs.k8s.io/cluster-api-provider-azure@v1.12.3/test/e2e/azure_test.go:412"

	if got := RepoFailurePaths(body, capz); len(got) != 0 {
		t.Fatalf("RepoFailurePaths = %v, want none for a tagged copy", got)
	}
}

func TestRepoFailurePathsDeduplicatesAndBounds(t *testing.T) {
	body := ""
	for i := 0; i < maxRepoFailurePaths*3; i++ {
		body += "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412\n"
	}
	if got := RepoFailurePaths(body, capz); len(got) != 1 {
		t.Fatalf("repeated frames should collapse: %v", got)
	}

	body = ""
	var paths []string
	for i := 0; i < maxRepoFailurePaths*2; i++ {
		path := fmt.Sprintf("test/e2e/f%02d.go", maxRepoFailurePaths*2-i)
		paths = append(paths, path)
		body += "sigs.k8s.io/cluster-api-provider-azure/" + path + ":1\n"
	}
	want := paths[:maxRepoFailurePaths]
	if got := RepoFailurePaths(body, capz); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want the first %d encountered paths: %v", got, maxRepoFailurePaths, want)
	}
}

func TestRepoFailurePathsRejectsEmptyInput(t *testing.T) {
	if got := RepoFailurePaths("", capz); got != nil {
		t.Errorf("empty body = %v", got)
	}
	if got := RepoFailurePaths("sigs.k8s.io/cluster-api-provider-azure/a.go:1", ""); got != nil {
		t.Errorf("empty repo = %v", got)
	}
	if got := RepoFailurePaths("no location here", capz); got != nil {
		t.Errorf("no match = %v", got)
	}
}
