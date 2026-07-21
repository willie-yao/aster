package orka

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type treeSeedBrowser struct {
	paths     []string
	truncated bool
	err       error
}

func (b treeSeedBrowser) BuildRoot() string { return "build" }

func (b treeSeedBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return nil, nil
}

func (b treeSeedBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return append([]string(nil), b.paths...), b.truncated, b.err
}

func (b treeSeedBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return nil, 0, nil
}

func (b treeSeedBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return nil, nil
}

func (b treeSeedBrowser) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return nil, nil
}

func TestArtifactTreeSeedPrioritizesReadablePaths(t *testing.T) {
	seed, err := ArtifactTreeSeed(context.Background(), treeSeedBrowser{
		paths: []string{
			"artifacts/screenshot.png",
			"build-log.txt",
			"artifacts/clusters/c1/machines/node/kubelet.log",
		},
		truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"build-log.txt", "artifacts/clusters/c1/machines/node/kubelet.log", "list truncated"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed missing %q: %s", want, seed)
		}
	}
	if strings.Contains(seed, "screenshot.png") {
		t.Fatalf("seed included non-text artifact: %s", seed)
	}
	if strings.Index(seed, "artifacts/clusters") > strings.Index(seed, "build-log.txt") {
		t.Fatalf("seed paths are not sorted: %s", seed)
	}
}

func TestWithArtifactTreeSeed(t *testing.T) {
	if got := WithArtifactTreeSeed("failure", ""); got != "failure" {
		t.Fatalf("empty seed changed prompt: %q", got)
	}
	got := WithArtifactTreeSeed("failure", "tree")
	if !strings.HasPrefix(got, "tree\n\n---\n\n") || !strings.HasSuffix(got, "failure") {
		t.Fatalf("seeded prompt = %q", got)
	}
}
