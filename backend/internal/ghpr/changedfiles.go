package ghpr

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Changed-file bounds. A pull request's diff is untrusted input sized by its
// author, so both the file list and the patch text are capped.
const (
	// MaxChangedFiles bounds the returned file list.
	MaxChangedFiles = 300
	// MaxPatchBytes bounds the total patch text across all files.
	MaxPatchBytes = 32 * 1024
	// MaxFilePatchBytes bounds one file's patch so a single large file cannot
	// consume the whole budget.
	MaxFilePatchBytes = 8 * 1024
	// changedFilesPageSize is GitHub's maximum page size for the files endpoint.
	changedFilesPageSize = 100
	// maxChangedFilePages bounds pagination independently of MaxChangedFiles.
	maxChangedFilePages = 10
)

// generatedPathRE matches paths that are normally machine-generated. Their
// patches are dropped first because regenerating mocks or vendored trees
// produces large diffs that say nothing about intent.
var generatedPathRE = regexp.MustCompile(
	`(^|/)(vendor|third_party|node_modules)/|(^|/)(zz_generated|mock_)|_generated\.go$|\.pb\.go$|\.golden$|generated\.deepcopy`)

// ChangedFile is one file a pull request modifies.
type ChangedFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	// Patch is the unified diff hunk, present only when it fit the budget.
	Patch string
	// Generated reports that the path looks machine-generated.
	Generated bool
	// PatchOmitted reports that a patch existed but was dropped, either because
	// the path is generated or because the budget was exhausted.
	PatchOmitted bool
}

// ChangedFileSet is the bounded view of a pull request's diff.
type ChangedFileSet struct {
	Files []ChangedFile
	// FilesTruncated reports that Files is incomplete, either because the pull
	// request changes more files than MaxChangedFiles or because pagination hit
	// its page bound.
	FilesTruncated bool
	// TotalFiles is the number of files observed. When FilesTruncated is set it
	// is a lower bound rather than the pull request's true file count.
	TotalFiles int
	// PatchBytes is the total patch text retained.
	PatchBytes int
}

// Paths returns every changed path, including files whose patch was dropped.
// The overlap check needs the complete list, so it must not depend on patches.
func (s ChangedFileSet) Paths() []string {
	out := make([]string, 0, len(s.Files))
	for _, file := range s.Files {
		out = append(out, file.Path)
	}
	return out
}

type changedFileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// ChangedFiles returns the files a pull request modifies, with patch text
// bounded by MaxPatchBytes. The complete path list is always returned up to
// MaxChangedFiles even when no patch fits, because callers correlate on paths.
func (c *Client) ChangedFiles(ctx context.Context, owner, repo string, number int) (ChangedFileSet, error) {
	var raws []changedFileJSON
	truncated := false
	for page := 1; page <= maxChangedFilePages; page++ {
		query := fmt.Sprintf("pulls/%d/files?per_page=%d&page=%d", number, changedFilesPageSize, page)
		var batch []changedFileJSON
		if err := c.get(ctx, c.url(owner, repo, query), &batch); err != nil {
			return ChangedFileSet{}, fmt.Errorf("listing changed files for %s/%s#%d: %w", owner, repo, number, err)
		}
		raws = append(raws, batch...)
		// A short page is the definitive end of the list. Stopping on any other
		// condition means more files may exist that were never observed.
		if len(batch) < changedFilesPageSize {
			break
		}
		if len(raws) >= MaxChangedFiles || page == maxChangedFilePages {
			truncated = true
			break
		}
	}
	return newChangedFileSet(raws, truncated), nil
}

// newChangedFileSet applies the file cap, then fills the patch budget with the
// smallest non-generated patches first so the retained text covers as many
// distinct files as possible.
func newChangedFileSet(raws []changedFileJSON, truncated bool) ChangedFileSet {
	set := ChangedFileSet{TotalFiles: len(raws), FilesTruncated: truncated}
	if len(raws) > MaxChangedFiles {
		set.FilesTruncated = true
		raws = raws[:MaxChangedFiles]
	}
	set.Files = make([]ChangedFile, len(raws))
	// Candidates carry their already-truncated patch so ordering reflects the
	// bytes actually retained, not the raw size.
	type candidate struct {
		index int
		patch string
	}
	var candidates []candidate
	for i, raw := range raws {
		generated := generatedPathRE.MatchString(raw.Filename)
		set.Files[i] = ChangedFile{
			Path:      raw.Filename,
			Status:    raw.Status,
			Additions: raw.Additions,
			Deletions: raw.Deletions,
			Generated: generated,
		}
		if raw.Patch == "" {
			continue
		}
		if generated {
			set.Files[i].PatchOmitted = true
			continue
		}
		candidates = append(candidates, candidate{index: i, patch: truncatePatch(raw.Patch)})
	}
	sort.Slice(candidates, func(a, b int) bool {
		if len(candidates[a].patch) != len(candidates[b].patch) {
			return len(candidates[a].patch) < len(candidates[b].patch)
		}
		return raws[candidates[a].index].Filename < raws[candidates[b].index].Filename
	})
	for _, item := range candidates {
		if set.PatchBytes+len(item.patch) > MaxPatchBytes {
			set.Files[item.index].PatchOmitted = true
			continue
		}
		set.Files[item.index].Patch = item.patch
		set.PatchBytes += len(item.patch)
	}
	return set
}

// truncatePatch caps one file's patch at a hunk boundary where possible so the
// retained text stays readable as a diff.
func truncatePatch(patch string) string {
	if len(patch) <= MaxFilePatchBytes {
		return patch
	}
	clipped := patch[:MaxFilePatchBytes]
	if idx := strings.LastIndex(clipped, "\n@@"); idx > 0 {
		clipped = clipped[:idx]
	} else if idx := strings.LastIndex(clipped, "\n"); idx > 0 {
		clipped = clipped[:idx]
	}
	return clipped + "\n... patch truncated ...\n"
}
