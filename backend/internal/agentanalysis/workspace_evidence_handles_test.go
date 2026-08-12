package agentanalysis

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildWorkspaceEvidenceHandlesUsesObservedLinesOnly(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 3},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
	)
	want := []WorkspaceEvidenceHandle{
		{ID: "artifact-001", Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
		{ID: "artifact-002", Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 3, LineEnd: 3},
		{ID: "source-001", Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
	}
	if !slices.Equal(handles, want) {
		t.Fatalf("handles=%+v want=%+v", handles, want)
	}
	instruction, err := WorkspaceFinalizationInstruction(handles)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"artifact-001", "artifacts/logs/build.log line 2", "source-001", "source/pkg/controller.go line 3"} {
		if !strings.Contains(instruction, value) {
			t.Fatalf("instruction missing %q: %s", value, instruction)
		}
	}
	if strings.Contains(instruction, "artifact-only-marker") || strings.Contains(instruction, "func reconcile") {
		t.Fatalf("instruction retained evidence content: %s", instruction)
	}
}

func TestBuildWorkspaceEvidenceHandlesReportsBoundedDiagnostics(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	ranges := make([]WorkspaceEvidenceRange, maxWorkspaceEvidenceRanges+1)
	for index := range ranges {
		ranges[index] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}
	}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings || diagnostics.ObservedRangeCount != maxWorkspaceEvidenceRanges+1 || diagnostics.AcceptedArtifactHandleCount != 1 || diagnostics.DroppedRangeCount != maxWorkspaceEvidenceRanges || !diagnostics.Truncated || !slices.Equal(diagnostics.Codes, []string{WorkspaceEvidenceHandleDuplicate, WorkspaceEvidenceHandleTruncated, WorkspaceEvidenceRangeOverflow}) {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesClassifiesRejectedRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		rangeValue WorkspaceEvidenceRange
		code       string
	}{
		{name: "root", rangeValue: WorkspaceEvidenceRange{Root: "other", Path: "logs/build.log", LineStart: 1, LineEnd: 1}, code: WorkspaceEvidenceRangeRootInvalid},
		{name: "path", rangeValue: WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "../escape.log", LineStart: 1, LineEnd: 1}, code: WorkspaceEvidenceRangePathInvalid},
		{name: "unreadable", rangeValue: WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "missing.log", LineStart: 1, LineEnd: 1}, code: WorkspaceEvidenceRangeUnreadable},
		{name: "line", rangeValue: WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 99, LineEnd: 99}, code: WorkspaceEvidenceRangeLineInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{test.rangeValue})
			if err == nil || diagnostics.Status != WorkspaceEvidenceHandlesRejected || !slices.Contains(diagnostics.Codes, test.code) {
				t.Fatalf("err=%v diagnostics=%+v", err, diagnostics)
			}
		})
	}
}

func TestBuildWorkspaceEvidenceHandlesPrioritizesFocusedRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	var content strings.Builder
	for line := 1; line <= 100; line++ {
		fmt.Fprintf(&content, "line-%03d\n", line)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "long.log"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "long.log", LineStart: 1, LineEnd: 100},
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "long.log", LineStart: 90, LineEnd: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != maxWorkspaceEvidencePerRoot {
		t.Fatalf("handle count=%d", len(handles))
	}
	if diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings || diagnostics.AcceptedArtifactHandleCount != maxWorkspaceEvidencePerRoot || !diagnostics.Truncated || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceHandleTruncated) {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	workspaceHandleID(t, handles, WorkspaceArtifactsDir, "long.log", 90)
}

func TestBuildWorkspaceEvidenceHandlesDeduplicatesObservedRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	rangeValue := WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{rangeValue, rangeValue})
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || diagnostics.DroppedRangeCount != 1 || diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceHandleDuplicate) {
		t.Fatalf("handles=%+v diagnostics=%+v", handles, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesDeduplicatesOverlappingRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{
		{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 3},
		{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 3 || diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings || diagnostics.DroppedRangeCount != 0 || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceHandleDuplicate) {
		t.Fatalf("handles=%+v diagnostics=%+v", handles, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesRejectsUnsafeRangeBeyondCap(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	ranges := make([]WorkspaceEvidenceRange, maxWorkspaceEvidenceRanges+1)
	for index := range maxWorkspaceEvidenceRanges {
		ranges[index] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}
	}
	ranges[maxWorkspaceEvidenceRanges] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "../escape.log", LineStart: 1, LineEnd: 1}
	_, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err == nil || diagnostics.Status != WorkspaceEvidenceHandlesRejected || !slices.Equal(diagnostics.Codes, []string{WorkspaceEvidenceRangePathInvalid}) {
		t.Fatalf("err=%v diagnostics=%+v", err, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesPreservesSourceAfterArtifactOverflow(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	ranges := make([]WorkspaceEvidenceRange, maxWorkspaceEvidenceRanges+2)
	for index := range maxWorkspaceEvidenceRanges + 1 {
		ranges[index] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}
	}
	ranges[len(ranges)-1] = WorkspaceEvidenceRange{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.AcceptedArtifactHandleCount != 1 || diagnostics.AcceptedSourceHandleCount != 1 || !diagnostics.Truncated || diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings {
		t.Fatalf("handles=%+v diagnostics=%+v", handles, diagnostics)
	}
	workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)
}

func TestBuildWorkspaceEvidenceHandlesRejectsEscapingSymlinkBeyondCap(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(artifactRoot, "zz-escape.log")); err != nil {
		t.Fatal(err)
	}
	ranges := make([]WorkspaceEvidenceRange, maxWorkspaceEvidenceRanges+1)
	for index := range maxWorkspaceEvidenceRanges {
		ranges[index] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}
	}
	ranges[maxWorkspaceEvidenceRanges] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "zz-escape.log", LineStart: 1, LineEnd: 1}
	_, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err == nil || diagnostics.Status != WorkspaceEvidenceHandlesRejected || !slices.Equal(diagnostics.Codes, []string{WorkspaceEvidenceRangePathInvalid}) {
		t.Fatalf("err=%v diagnostics=%+v", err, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesKeepsValidArtifactWithInvalidOptionalSource(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{
		{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1},
		{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 99, LineEnd: 99},
		{Root: WorkspaceSourceDir, Path: "pkg/missing.go", LineStart: 1, LineEnd: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].Root != WorkspaceArtifactsDir || diagnostics.Status != WorkspaceEvidenceHandlesAcceptedWithWarnings || diagnostics.AcceptedArtifactHandleCount != 1 || diagnostics.AcceptedSourceHandleCount != 0 || diagnostics.DroppedRangeCount != 2 || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceRangeLineInvalid) || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceRangeUnreadable) {
		t.Fatalf("handles=%+v diagnostics=%+v", handles, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesAcceptsFinalLineWithTrailingNewline(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	if err := os.WriteFile(filepath.Join(artifactRoot, "trailing.log"), []byte("first\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "trailing.log", LineStart: 2, LineEnd: 2},
	)
	workspaceHandleID(t, handles, WorkspaceArtifactsDir, "trailing.log", 2)
}

func TestBuildWorkspaceEvidenceHandlesAcceptsArtifactWithoutSource(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1},
	)
	if len(handles) != 1 || handles[0].ID != "artifact-001" {
		t.Fatalf("handles=%+v", handles)
	}
}

func TestBuildWorkspaceEvidenceHandlesCachesSharedFileContent(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	var content strings.Builder
	for line := 1; line <= 100; line++ {
		fmt.Fprintf(&content, "line-%03d\n", line)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "cache.log"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	ranges := make([]WorkspaceEvidenceRange, maxWorkspaceEvidencePerRoot)
	for index := range ranges {
		ranges[index] = WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "cache.log", LineStart: index + 1, LineEnd: index + 1}
	}
	reads := 0
	handles, diagnostics, err := buildWorkspaceEvidenceHandles(workspaceRoot, ranges, time.Time{}, func(root, relative string, limit int64) (string, error) {
		reads++
		return readWorkspaceText(root, relative, limit)
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(handles) != maxWorkspaceEvidencePerRoot || diagnostics.AcceptedArtifactHandleCount != maxWorkspaceEvidencePerRoot {
		t.Fatalf("reads=%d handles=%d diagnostics=%+v", reads, len(handles), diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesHonorsDeadline(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err := BuildWorkspaceEvidenceHandlesWithDeadline(workspaceRoot, []WorkspaceEvidenceRange{{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1}}, time.Now().Add(-time.Second))
	if err == nil || diagnostics.Status != WorkspaceEvidenceHandlesRejected || !slices.Equal(diagnostics.Codes, []string{WorkspaceEvidenceHandleTimeout}) {
		t.Fatalf("err=%v diagnostics=%+v", err, diagnostics)
	}
}

func TestBuildWorkspaceEvidenceHandlesStopsWhenDeadlineExpiresDuringCanonicalization(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	var content strings.Builder
	for line := 1; line <= 100; line++ {
		fmt.Fprintf(&content, "line-%03d\n", line)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "deadline.log"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	afterRead := false
	checksAfterRead := 0
	now := func() time.Time {
		if !afterRead {
			return base
		}
		checksAfterRead++
		if checksAfterRead > 2 {
			return base.Add(20 * time.Millisecond)
		}
		return base
	}
	_, diagnostics, err := buildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{{Root: WorkspaceArtifactsDir, Path: "deadline.log", LineStart: 1, LineEnd: 64}}, base.Add(10*time.Millisecond), func(root, relative string, limit int64) (string, error) {
		value, readErr := readWorkspaceText(root, relative, limit)
		afterRead = true
		return value, readErr
	}, now)
	if err == nil || diagnostics.Status != WorkspaceEvidenceHandlesRejected || !slices.Contains(diagnostics.Codes, WorkspaceEvidenceHandleTimeout) {
		t.Fatalf("err=%v diagnostics=%+v checks_after_read=%d", err, diagnostics, checksAfterRead)
	}
}

func TestIndexWorkspaceEvidenceFileBoundsShortLineMemory(t *testing.T) {
	content := strings.Repeat("\n", int(maxWorkspaceFileBytes))
	indexed, err := indexWorkspaceEvidenceFile(content, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	maxOffsets := int(maxWorkspaceFileBytes)/workspaceEvidenceIndexStride + 1
	maxBytes := maxWorkspaceFileBytes + int64(maxOffsets*8)
	if indexed.lineCount != int(maxWorkspaceFileBytes) || len(indexed.lineOffsets) > maxOffsets || indexed.cacheBytes > maxBytes {
		t.Fatalf("line_count=%d offsets=%d cache_bytes=%d max_offsets=%d max_bytes=%d", indexed.lineCount, len(indexed.lineOffsets), indexed.cacheBytes, maxOffsets, maxBytes)
	}
}

func TestBuildWorkspaceEvidenceHandlesRejectsUnsafeRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	for _, ranges := range [][]WorkspaceEvidenceRange{
		{{Root: WorkspaceArtifactsDir, Path: "../escape.log", LineStart: 1, LineEnd: 1}},
		{{Root: WorkspaceSourceDir, Path: ".git/config", LineStart: 1, LineEnd: 1}},
		{{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 0, LineEnd: 1}},
		{{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 1, LineEnd: 99}},
	} {
		if _, _, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges); err == nil {
			t.Fatalf("ranges were accepted: %+v", ranges)
		}
	}
}

func workspaceDefaultHandles(t *testing.T, sourceRoot, artifactRoot string) []WorkspaceEvidenceHandle {
	t.Helper()
	return workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 3},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 1, LineEnd: 3},
	)
}

func workspaceTestHandles(t *testing.T, sourceRoot, artifactRoot string, ranges ...WorkspaceEvidenceRange) []WorkspaceEvidenceHandle {
	t.Helper()
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	handles, _, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err != nil {
		t.Fatal(err)
	}
	return handles
}

func workspaceHandleID(t *testing.T, handles []WorkspaceEvidenceHandle, root, path string, line int) string {
	t.Helper()
	for _, handle := range handles {
		if handle.Root == root && handle.Path == path && handle.LineStart == line && handle.LineEnd == line {
			return handle.ID
		}
	}
	t.Fatalf("handle unavailable: root=%s path=%s line=%d handles=%+v", root, path, line, handles)
	return ""
}

func workspaceCitationSelection(id string) string { return id }
