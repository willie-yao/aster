package analysisruntime

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

type patternBrowser struct {
	root     string
	buildIDs []string
	browsers map[string]artifacts.Browser
}

// NewPatternBrowser returns one read-only Browser namespaced by exact build IDs.
func NewPatternBrowser(factory artifacts.Factory, builds []analysischat.ArtifactBuild) artifacts.Browser {
	browser := &patternBrowser{root: "recurring pattern builds", browsers: map[string]artifacts.Browser{}}
	for _, build := range builds {
		id := strings.TrimSpace(build.Build.BuildID)
		if id == "" || strings.Contains(id, "/") || browser.browsers[id] != nil {
			continue
		}
		browser.buildIDs = append(browser.buildIDs, id)
		browser.browsers[id] = factory.ForBuild(build.BuildPrefix, build.Build.JobName+"/"+id)
	}
	slices.Sort(browser.buildIDs)
	return browser
}

func (b *patternBrowser) BuildRoot() string { return b.root }

func (b *patternBrowser) List(ctx context.Context, dir string) (*artifacts.Listing, error) {
	dir = strings.TrimSpace(dir)
	if strings.HasPrefix(dir, "/") {
		return nil, fmt.Errorf("pattern artifact path must be relative")
	}
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return &artifacts.Listing{Dirs: []string{"builds/"}}, nil
	}
	if dir == "builds" {
		dirs := make([]string, 0, len(b.buildIDs))
		for _, id := range b.buildIDs {
			dirs = append(dirs, id+"/")
		}
		return &artifacts.Listing{Dir: "builds/", Dirs: dirs}, nil
	}
	id, sub, browser, err := b.resolve(dir)
	if err != nil {
		return nil, err
	}
	listing, err := browser.List(ctx, sub)
	if err != nil {
		return nil, err
	}
	listing.Dir = "builds/" + id + "/"
	if sub != "" {
		listing.Dir += strings.TrimSuffix(sub, "/") + "/"
	}
	return listing, nil
}

func (b *patternBrowser) ListTree(ctx context.Context, maxPaths int) ([]string, bool, error) {
	var out []string
	truncated := false
	for _, id := range b.buildIDs {
		remaining := maxPaths - len(out)
		if remaining <= 0 {
			return out, true, nil
		}
		paths, cut, err := b.browsers[id].ListTree(ctx, remaining)
		if err != nil {
			return nil, false, err
		}
		for _, path := range paths {
			out = append(out, "builds/"+id+"/"+path)
		}
		truncated = truncated || cut
	}
	return out, truncated, nil
}

func (b *patternBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	_, sub, browser, err := b.resolve(file)
	if err != nil {
		return nil, -1, err
	}
	return browser.Read(ctx, sub, offset, length)
}

func (b *patternBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*artifacts.TailResult, error) {
	_, sub, browser, err := b.resolve(file)
	if err != nil {
		return nil, err
	}
	return browser.Tail(ctx, sub, lines, maxBytes)
}

func (b *patternBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen, maxBytes int) (*artifacts.GrepResult, error) {
	_, sub, browser, err := b.resolve(file)
	if err != nil {
		return nil, err
	}
	return browser.Grep(ctx, sub, re, contextLines, maxMatches, maxLineLen, maxBytes)
}

func (b *patternBrowser) resolve(value string) (string, string, artifacts.Browser, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "/") {
		return "", "", nil, fmt.Errorf("pattern artifact path must be relative")
	}
	value = strings.TrimSuffix(value, "/")
	parts := strings.SplitN(value, "/", 3)
	if len(parts) < 2 || parts[0] != "builds" {
		return "", "", nil, fmt.Errorf("pattern artifact path must be builds/<build-id>/<path>")
	}
	browser := b.browsers[parts[1]]
	if browser == nil {
		return "", "", nil, fmt.Errorf("pattern artifact build %q is unavailable", parts[1])
	}
	sub := ""
	if len(parts) == 3 {
		sub = parts[2]
	}
	return parts[1], sub, browser, nil
}

func newPatternBrowser(factory artifacts.Factory, builds []analysischat.ArtifactBuild) artifacts.Browser {
	return NewPatternBrowser(factory, builds)
}
