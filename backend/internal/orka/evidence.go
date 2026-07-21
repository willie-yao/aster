package orka

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

const (
	artifactTreeMaxPaths = 500
	artifactTreeMaxBytes = 48 * 1024
)

var artifactTreeNoiseExt = map[string]bool{
	".png": true, ".svg": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".gz": true, ".tar": true, ".tgz": true, ".zip": true, ".bz2": true,
}

// ArtifactTreeSeed lists bounded model-readable paths for the initial prompt.
func ArtifactTreeSeed(ctx context.Context, browser artifacts.Browser) (string, error) {
	if browser == nil {
		return "", nil
	}
	raw, rawTruncated, err := browser.ListTree(ctx, artifactTreeMaxPaths*2)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(raw))
	for _, artifactPath := range raw {
		if artifactTreeNoiseExt[strings.ToLower(path.Ext(artifactPath))] {
			continue
		}
		paths = append(paths, artifactPath)
	}
	if len(paths) == 0 {
		return "", nil
	}
	sort.Strings(paths)
	truncated := rawTruncated
	if len(paths) > artifactTreeMaxPaths {
		paths = paths[:artifactTreeMaxPaths]
		truncated = true
	}

	var lines strings.Builder
	kept := 0
	for _, artifactPath := range paths {
		if lines.Len()+len(artifactPath)+1 > artifactTreeMaxBytes {
			truncated = true
			break
		}
		lines.WriteString(artifactPath)
		lines.WriteByte('\n')
		kept++
	}
	if kept == 0 {
		return "", nil
	}

	var seed strings.Builder
	fmt.Fprintf(&seed, "Artifact paths for this build (%d file(s)). Use these exact paths with the artifact tools. Do not guess paths or rediscover entries already listed here:\n", kept)
	seed.WriteString(lines.String())
	if truncated {
		seed.WriteString("... [list truncated; use list_artifacts only for subtrees not shown above]\n")
	}
	return seed.String(), nil
}

// WithArtifactTreeSeed prepends deterministic build context to a failure prompt.
func WithArtifactTreeSeed(prompt, seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return prompt
	}
	return seed + "\n\n---\n\n" + prompt
}
