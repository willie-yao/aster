package analysisruntime

import (
	"context"
	"regexp"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type patternBrowserFactoryStub struct {
	browsers map[string]*patternBrowserStub
}

func (f patternBrowserFactoryStub) ForBuild(prefix, _ string) artifacts.Browser {
	return f.browsers[prefix]
}

type patternBrowserStub struct{ content []byte }

func (b *patternBrowserStub) BuildRoot() string { return "build" }
func (b *patternBrowserStub) List(context.Context, string) (*artifacts.Listing, error) {
	return &artifacts.Listing{Files: []artifacts.FileInfo{{Name: "build-log.txt", Size: int64(len(b.content))}}}, nil
}
func (b *patternBrowserStub) ListTree(context.Context, int) ([]string, bool, error) {
	return []string{"build-log.txt"}, false, nil
}
func (b *patternBrowserStub) Read(_ context.Context, _ string, _, _ int) ([]byte, int64, error) {
	return b.content, int64(len(b.content)), nil
}
func (b *patternBrowserStub) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return &artifacts.TailResult{Content: b.content, FileSize: int64(len(b.content))}, nil
}
func (b *patternBrowserStub) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return &artifacts.GrepResult{}, nil
}

func TestPatternBrowserRoutesBuildPrefixedPaths(t *testing.T) {
	factory := patternBrowserFactoryStub{browsers: map[string]*patternBrowserStub{
		"logs/job/104/": {content: []byte("build 104")},
		"logs/job/103/": {content: []byte("build 103")},
	}}
	browser := newPatternBrowser(factory, []analysischat.ArtifactBuild{
		{BuildPrefix: "logs/job/104/", Build: models.BuildInfo{BuildID: "104", JobName: "job"}},
		{BuildPrefix: "logs/job/103/", Build: models.BuildInfo{BuildID: "103", JobName: "job"}},
	})
	paths, truncated, err := browser.ListTree(t.Context(), 10)
	if err != nil || truncated || len(paths) != 2 || paths[0] != "builds/103/build-log.txt" || paths[1] != "builds/104/build-log.txt" {
		t.Fatalf("paths=%v truncated=%t err=%v", paths, truncated, err)
	}
	content, _, err := browser.Read(t.Context(), "builds/104/build-log.txt", 0, 100)
	if err != nil || string(content) != "build 104" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if _, _, err := browser.Read(t.Context(), "/builds/104/build-log.txt", 0, 100); err == nil {
		t.Fatal("absolute pattern artifact path was accepted")
	}
	listing, err := browser.List(t.Context(), "builds")
	if err != nil || len(listing.Dirs) != 2 {
		t.Fatalf("listing=%+v err=%v", listing, err)
	}
}
