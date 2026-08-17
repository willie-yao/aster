package ghpr

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func rawFile(name string, patchBytes int) map[string]any {
	return map[string]any{
		"filename": name, "status": "modified", "additions": 3, "deletions": 1,
		"patch": strings.Repeat("a", patchBytes),
	}
}

func filesHandler(t *testing.T, pages [][]map[string]any, seen *[]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7/files" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		*seen = append(*seen, r.URL.RawQuery)
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 || page > len(pages) {
			writeJSON(w, http.StatusOK, []map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, pages[page-1])
	}
}

func changedFiles(t *testing.T, pages [][]map[string]any) (ChangedFileSet, []string) {
	t.Helper()
	var seen []string
	client := lifecycleClient(t, filesHandler(t, pages, &seen))
	set, err := client.ChangedFiles(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	return set, seen
}

func patchFor(set ChangedFileSet, path string) (ChangedFile, bool) {
	for _, file := range set.Files {
		if file.Path == path {
			return file, true
		}
	}
	return ChangedFile{}, false
}

func TestChangedFilesReturnsPathsAndPatches(t *testing.T) {
	set, _ := changedFiles(t, [][]map[string]any{{
		rawFile("azure/scope/cluster.go", 100),
		rawFile("azure/scope/cluster_test.go", 200),
	}})

	if set.TotalFiles != 2 || set.FilesTruncated {
		t.Fatalf("set = %+v", set)
	}
	if got := set.Paths(); len(got) != 2 || got[0] != "azure/scope/cluster.go" {
		t.Fatalf("paths = %v", got)
	}
	for _, file := range set.Files {
		if file.Patch == "" || file.PatchOmitted {
			t.Errorf("%s should keep its patch: %+v", file.Path, file)
		}
	}
	if set.PatchBytes != 300 {
		t.Errorf("patch bytes = %d, want 300", set.PatchBytes)
	}
}

// Regenerated mocks and vendored trees produce large diffs that say nothing
// about intent, so their patches are dropped but their paths are kept.
func TestGeneratedFilesKeepPathsButDropPatches(t *testing.T) {
	generated := []string{
		"vendor/github.com/pkg/errors/errors.go",
		"azure/services/privatelinks/mock_privatelinks/privatelinks_mock.go",
		"api/v1beta1/zz_generated.deepcopy.go",
		"api/v1beta1/types_generated.go",
		"api/v1beta1/service.pb.go",
		"testdata/output.golden",
	}
	raws := []map[string]any{rawFile("azure/scope/cluster.go", 50)}
	for _, name := range generated {
		raws = append(raws, rawFile(name, 5000))
	}
	set, _ := changedFiles(t, [][]map[string]any{raws})

	if len(set.Paths()) != len(generated)+1 {
		t.Fatalf("every changed path must be reported: %v", set.Paths())
	}
	for _, name := range generated {
		file, ok := patchFor(set, name)
		if !ok || !file.Generated || file.Patch != "" || !file.PatchOmitted {
			t.Errorf("%s = %+v, want a generated path with no patch", name, file)
		}
	}
	source, _ := patchFor(set, "azure/scope/cluster.go")
	if source.Generated || source.Patch == "" {
		t.Errorf("hand-written file = %+v", source)
	}
}

func TestPatchBudgetPrefersCoveringMoreFiles(t *testing.T) {
	// Each file fits on its own, but together they exceed the budget. The
	// smallest ones must win so the retained text covers the most files.
	var raws []map[string]any
	for i := 0; i < 4; i++ {
		raws = append(raws, rawFile(fmt.Sprintf("large%d.go", i), MaxFilePatchBytes))
	}
	for i := 0; i < 6; i++ {
		raws = append(raws, rawFile(fmt.Sprintf("small%d.go", i), 200))
	}
	set, _ := changedFiles(t, [][]map[string]any{raws})

	if set.PatchBytes > MaxPatchBytes {
		t.Fatalf("patch bytes = %d, over budget %d", set.PatchBytes, MaxPatchBytes)
	}
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("small%d.go", i)
		if file, _ := patchFor(set, name); file.Patch == "" {
			t.Errorf("%s should fit the budget ahead of the large files", name)
		}
	}
	omitted := 0
	for _, file := range set.Files {
		if file.PatchOmitted {
			omitted++
		}
	}
	if omitted == 0 {
		t.Error("some large patches should have been dropped")
	}
	// Every path is still reported even when its patch was dropped.
	if len(set.Paths()) != len(raws) {
		t.Errorf("paths = %d, want %d", len(set.Paths()), len(raws))
	}
}

// A single oversized patch is truncated rather than dropped, so it still fits
// alongside other files.
func TestOversizedPatchIsTruncatedNotDropped(t *testing.T) {
	set, _ := changedFiles(t, [][]map[string]any{{
		rawFile("huge.go", MaxPatchBytes*2),
		rawFile("small.go", 100),
	}})

	huge, _ := patchFor(set, "huge.go")
	if huge.Patch == "" || len(huge.Patch) > MaxFilePatchBytes+64 {
		t.Fatalf("huge patch = %d bytes, want truncated to about %d", len(huge.Patch), MaxFilePatchBytes)
	}
	if small, _ := patchFor(set, "small.go"); small.Patch == "" {
		t.Error("the small file should still fit")
	}
}

func TestSingleFilePatchIsCapped(t *testing.T) {
	set, _ := changedFiles(t, [][]map[string]any{{rawFile("big.go", MaxFilePatchBytes*2)}})

	file, _ := patchFor(set, "big.go")
	if len(file.Patch) > MaxFilePatchBytes+64 {
		t.Fatalf("patch = %d bytes, want capped near %d", len(file.Patch), MaxFilePatchBytes)
	}
	if !strings.Contains(file.Patch, "patch truncated") {
		t.Error("a truncated patch must say so")
	}
}

func TestTruncatePatchPrefersAHunkBoundary(t *testing.T) {
	hunk := "@@ -1,3 +1,3 @@\n" + strings.Repeat("x", 100) + "\n"
	patch := strings.Repeat(hunk, (MaxFilePatchBytes/len(hunk))+5)
	got := truncatePatch(patch)

	if len(got) > MaxFilePatchBytes+64 {
		t.Fatalf("truncated patch = %d bytes", len(got))
	}
	body := strings.TrimSuffix(got, "\n... patch truncated ...\n")
	if strings.HasSuffix(body, "x") && strings.Count(body, "@@")%2 != 0 {
		t.Error("truncation should not split a hunk header")
	}
}

func TestChangedFilesCapsTheFileList(t *testing.T) {
	var pages [][]map[string]any
	for page := 0; page < 4; page++ {
		var batch []map[string]any
		for i := 0; i < changedFilesPageSize; i++ {
			batch = append(batch, rawFile(fmt.Sprintf("p%d_f%d.go", page, i), 10))
		}
		pages = append(pages, batch)
	}
	set, seen := changedFiles(t, pages)

	if !set.FilesTruncated || len(set.Files) != MaxChangedFiles {
		t.Fatalf("files = %d truncated = %t, want %d and true", len(set.Files), set.FilesTruncated, MaxChangedFiles)
	}
	if len(seen) > maxChangedFilePages {
		t.Errorf("requests = %d, over the page bound", len(seen))
	}
}

func TestChangedFilesPaginatesUntilShortPage(t *testing.T) {
	full := make([]map[string]any, changedFilesPageSize)
	for i := range full {
		full[i] = rawFile(fmt.Sprintf("f%d.go", i), 10)
	}
	set, seen := changedFiles(t, [][]map[string]any{full, {rawFile("last.go", 10)}})

	if set.TotalFiles != changedFilesPageSize+1 {
		t.Fatalf("total = %d", set.TotalFiles)
	}
	if len(seen) != 2 || !strings.Contains(seen[1], "page=2") {
		t.Errorf("requests = %v", seen)
	}
}

func TestChangedFilesHandlesFilesWithoutPatches(t *testing.T) {
	set, _ := changedFiles(t, [][]map[string]any{{
		{"filename": "assets/logo.png", "status": "added", "additions": 0, "deletions": 0},
	}})

	file, ok := patchFor(set, "assets/logo.png")
	if !ok || file.Patch != "" || file.PatchOmitted {
		t.Fatalf("binary file = %+v, want no patch and no omission flag", file)
	}
}

func TestChangedFilesReportsAPIFailure(t *testing.T) {
	client := lifecycleClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := client.ChangedFiles(context.Background(), "o", "r", 7); err == nil {
		t.Fatal("want an error when the files endpoint fails")
	}
}
